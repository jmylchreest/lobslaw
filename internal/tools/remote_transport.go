package tools

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Remotes: running development work somewhere that is not this process.
//
// The gateway pod holds the model keys, the channel tokens and, on a
// cluster, the mTLS material for the whole Raft group. Compiling a
// project there — or running anything the agent wrote — puts untrusted
// code next to all of it. shell_command exists for small local work and
// denies "ssh " outright for exactly this reason: an agent that can
// reach a shell that can reach another host has an unbounded second hop
// that nothing in the tool layer can see or gate.
//
// This is that hop, made first-class instead. The difference is not
// convenience:
//
//   - the model names a DEVBOX, never a host, port, user or key, so the
//     set of reachable machines is the operator's config and cannot be
//     widened by anything the model emits;
//   - every call is a tool call, so it carries a risk tier, goes through
//     the policy engine, and lands in the audit log with its command;
//   - host keys are verified, and a remote whose key changed is refused
//     rather than silently reconnected to.
//
// Deliberately NOT a general SSH client. There is no "connect to
// $HOST" tool here and there should not be one.

const (
	// defaultRemotePort matches the unprivileged sshd a remote runs
	// under a restricted PodSecurity policy. See RemoteConfig.Port.
	defaultRemotePort = 2222
	// defaultRemoteUser is the conventional remote account.
	defaultRemoteUser = "dev"

	// remoteDefaultTimeout is generous next to shell_command's 30s
	// because the work is different in kind: a cold Rust build or a
	// full test suite is minutes, and a timeout that kills it halfway
	// wastes the whole run rather than bounding it.
	remoteDefaultTimeout = 5 * time.Minute
	// remoteMaxTimeout is the ceiling a call may ask for. An hour is
	// past any single build and short of "wedged forever".
	remoteMaxTimeout = time.Hour

	// remoteDialTimeout bounds reaching the host at all, separately
	// from the command budget. A remote that is not there should fail
	// in seconds and say so, not consume the caller's whole timeout
	// before reporting a connection error.
	remoteDialTimeout = 15 * time.Second

	// remoteMaxOutput caps what comes back. The output goes into the
	// model's context, so this is a context budget before it is a
	// memory one — a build log is megabytes and the useful part is
	// the end.
	remoteMaxOutput int64 = 256 << 10
)

// ErrUnknownRemote is returned when a call names a remote that is not
// configured. Its own error because it is the common case and it is not
// a failure of the remote: it is the model guessing a name.
var ErrUnknownRemote = errors.New("remote: not configured")

// Remote is one configured host, resolved and ready to dial.
//
// Secrets are resolved once at wiring time rather than per call. A key
// that cannot be read is a boot-time configuration error the operator
// should see immediately, not a tool failure surfacing mid-turn hours
// later.
type Remote struct {
	Name        string
	Description string

	addr   string
	user   string
	signer ssh.Signer

	// hostKeys verifies the far end. Never nil — a Remote that could
	// not build one is not constructed.
	hostKeys ssh.HostKeyCallback

	defaultTimeout time.Duration
	maxTimeout     time.Duration
}

// RemoteSet is the name → Remote lookup the builtin dispatches through.
type RemoteSet struct {
	byName map[string]*Remote
	names  []string
}

// SecretResolver is the narrow slice of internal/secrets this package
// needs. An interface so compute does not import the secrets package
// and a test can supply two lines.
type SecretResolver interface {
	Resolve(ref string) (string, error)
}

// NewRemoteSet resolves every configured remote.
//
// Fails on the FIRST bad entry rather than skipping it. A remote that
// silently did not load is a tool the model will keep trying to use and
// keep being told does not exist, which reads as a model fault; a
// refusal to boot reads as what it is.
func NewRemoteSet(cfgs []config.RemoteConfig, secrets SecretResolver) (*RemoteSet, error) {
	set := &RemoteSet{byName: make(map[string]*Remote, len(cfgs))}
	for i, c := range cfgs {
		box, err := newRemote(c, secrets)
		if err != nil {
			return nil, fmt.Errorf("remote[%d] (%q): %w", i, c.Name, err)
		}
		if _, dup := set.byName[box.Name]; dup {
			return nil, fmt.Errorf("remote[%d]: duplicate name %q", i, box.Name)
		}
		set.byName[box.Name] = box
		set.names = append(set.names, box.Name)
	}
	return set, nil
}

// Names returns the configured remote names in declaration order. Used
// to build the tool description, so the model is told what exists
// rather than left to guess.
func (s *RemoteSet) Names() []string {
	if s == nil {
		return nil
	}
	return append([]string(nil), s.names...)
}

// Describe renders "name — description" lines for the tool schema.
func (s *RemoteSet) Describe() string {
	if s == nil || len(s.names) == 0 {
		return ""
	}
	var b strings.Builder
	for _, n := range s.names {
		box := s.byName[n]
		b.WriteString("\n- ")
		b.WriteString(n)
		if box.Description != "" {
			b.WriteString(" — ")
			b.WriteString(box.Description)
		}
	}
	return b.String()
}

// Get resolves a name. Case-insensitive, because the name reaches this
// through model output and "Go" for "go" is a spelling difference
// rather than a different request.
func (s *RemoteSet) Get(name string) (*Remote, error) {
	if s == nil || len(s.byName) == 0 {
		return nil, fmt.Errorf("%w: no remotes are configured on this node", ErrUnknownRemote)
	}
	if box, ok := s.byName[strings.ToLower(strings.TrimSpace(name))]; ok {
		return box, nil
	}
	return nil, fmt.Errorf("%w: %q (configured: %s)", ErrUnknownRemote, name, strings.Join(s.names, ", "))
}

func newRemote(c config.RemoteConfig, secrets SecretResolver) (*Remote, error) {
	name := strings.ToLower(strings.TrimSpace(c.Name))
	switch {
	case name == "":
		return nil, errors.New("name is required")
	case strings.ContainsAny(name, " \t/\\"):
		// The name is a lookup key the model types, not a path.
		return nil, fmt.Errorf("name %q must not contain whitespace or slashes", c.Name)
	case strings.TrimSpace(c.Host) == "":
		return nil, errors.New("host is required")
	case strings.TrimSpace(c.KeyRef) == "":
		return nil, errors.New("key_ref is required")
	case secrets == nil:
		return nil, errors.New("no secret resolver wired; key_ref cannot be read")
	}

	port := c.Port
	if port == 0 {
		port = defaultRemotePort
	}
	if port < 1 || port > 65535 {
		return nil, fmt.Errorf("port %d out of range", port)
	}
	user := strings.TrimSpace(c.User)
	if user == "" {
		user = defaultRemoteUser
	}

	signer, err := loadDevboxKey(c, secrets)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(c.KnownHosts) == "" {
		// Verification is still on — the key is pinned for this
		// process and a change mid-run is refused. What is missing is
		// PERSISTENCE, so every restart trusts on first connect again,
		// and the window TOFU is weak in reopens each time instead of
		// closing for good.
		slog.Default().Warn("remote: no known_hosts path; the host key is pinned for this process only, "+
			"so a restart re-trusts on first connect",
			"remote", c.Name, "host", c.Host,
			"fix", "set known_hosts to a writable path in the [[remote]] block")
	}
	hostKeys, err := remoteHostKeyCallback(c.KnownHosts)
	if err != nil {
		return nil, fmt.Errorf("known_hosts %q: %w", c.KnownHosts, err)
	}

	return &Remote{
		Name:           name,
		Description:    strings.TrimSpace(c.Description),
		addr:           net.JoinHostPort(strings.TrimSpace(c.Host), strconv.Itoa(port)),
		user:           user,
		signer:         signer,
		hostKeys:       hostKeys,
		defaultTimeout: durationOrDefault(c.DefaultTimeoutSecs, remoteDefaultTimeout),
		maxTimeout:     durationOrDefault(c.MaxTimeoutSecs, remoteMaxTimeout),
	}, nil
}

func durationOrDefault(secs int, def time.Duration) time.Duration {
	if secs <= 0 {
		return def
	}
	return time.Duration(secs) * time.Second
}

func loadDevboxKey(c config.RemoteConfig, secrets SecretResolver) (ssh.Signer, error) {
	pem, err := secrets.Resolve(c.KeyRef)
	if err != nil {
		return nil, fmt.Errorf("key_ref %q: %w", c.KeyRef, err)
	}
	if strings.TrimSpace(pem) == "" {
		return nil, fmt.Errorf("key_ref %q resolved to empty", c.KeyRef)
	}
	if c.KeyPassphraseRef == "" {
		signer, perr := ssh.ParsePrivateKey([]byte(pem))
		if perr != nil {
			// Named explicitly: an encrypted key parsed without a
			// passphrase reports a generic format error, and the
			// operator's next move depends entirely on which it is.
			var passErr *ssh.PassphraseMissingError
			if errors.As(perr, &passErr) {
				return nil, fmt.Errorf("key_ref %q is encrypted; set key_passphrase_ref", c.KeyRef)
			}
			return nil, fmt.Errorf("key_ref %q: %w", c.KeyRef, perr)
		}
		return signer, nil
	}
	pass, err := secrets.Resolve(c.KeyPassphraseRef)
	if err != nil {
		return nil, fmt.Errorf("key_passphrase_ref %q: %w", c.KeyPassphraseRef, err)
	}
	signer, err := ssh.ParsePrivateKeyWithPassphrase([]byte(pem), []byte(pass))
	if err != nil {
		return nil, fmt.Errorf("key_ref %q with passphrase: %w", c.KeyRef, err)
	}
	return signer, nil
}

// remoteHostKeyCallback builds the verifier.
//
// Never ssh.InsecureIgnoreHostKey. A man in the middle on this
// connection gets the agent's commands and returns whatever output it
// likes, and the agent believes the output — that is a worse position
// than the untrusted-code problem remotes exist to solve.
func remoteHostKeyCallback(path string) (ssh.HostKeyCallback, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		// No file to persist to: pin for the process lifetime. Weaker
		// than a file across restarts, still strong within a run, and
		// unambiguously better than not checking.
		return newMemoryHostKeys().callback, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// knownhosts.New refuses a file that does not exist, and an empty
	// one is the correct starting state for trust-on-first-use.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()

	return (&tofuHostKeys{path: path}).callback, nil
}

// tofuHostKeys verifies against a known_hosts file, recording a host
// the file has never seen and refusing one whose key CHANGED.
//
// The distinction is the whole design. "Never seen" is a new remote and
// is expected every time a stack is added. "Changed" is either a pod
// that regenerated its host key — which the remote image avoids by
// keeping it on the PVC precisely so this stays meaningful — or someone
// in the middle. Both are refused, and the error says which it looked
// like.
type tofuHostKeys struct {
	path string
	mu   sync.Mutex
}

func (t *tofuHostKeys) callback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	verify, err := knownhosts.New(t.path)
	if err != nil {
		return fmt.Errorf("remote: known_hosts: %w", err)
	}
	err = verify(hostname, remote, key)
	if err == nil {
		return nil
	}

	var keyErr *knownhosts.KeyError
	if errors.As(err, &keyErr) && len(keyErr.Want) == 0 {
		// Unknown host: record it and accept, once.
		if aerr := t.append(hostname, key); aerr != nil {
			return fmt.Errorf("remote: recording host key for %s: %w", hostname, aerr)
		}
		return nil
	}
	return fmt.Errorf("remote: host key for %s changed since it was pinned in %s — "+
		"refusing. If the remote was genuinely rebuilt with a new key, remove its line "+
		"from that file; otherwise treat this as an interception: %w", hostname, t.path, err)
}

func (t *tofuHostKeys) append(hostname string, key ssh.PublicKey) error {
	f, err := os.OpenFile(t.path, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	line := knownhosts.Line([]string{knownhosts.Normalize(hostname)}, key)
	if _, err := f.WriteString(line + "\n"); err != nil {
		return err
	}
	return f.Close()
}

// memoryHostKeys is the no-file variant: first key wins for the life of
// the process.
type memoryHostKeys struct {
	mu   sync.Mutex
	seen map[string]string
}

func newMemoryHostKeys() *memoryHostKeys {
	return &memoryHostKeys{seen: make(map[string]string)}
}

func (m *memoryHostKeys) callback(hostname string, _ net.Addr, key ssh.PublicKey) error {
	fp := ssh.FingerprintSHA256(key)
	host := knownhosts.Normalize(hostname)

	m.mu.Lock()
	defer m.mu.Unlock()
	prev, seen := m.seen[host]
	if !seen {
		m.seen[host] = fp
		return nil
	}
	if prev != fp {
		return fmt.Errorf("remote: host key for %s changed mid-run (%s -> %s) — refusing", host, prev, fp)
	}
	return nil
}

// RemoteResult is one completed command.
type RemoteResult struct {
	Remote    string `json:"remote"`
	Command   string `json:"command"`
	ExitCode  int    `json:"exit_code"`
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	Truncated bool   `json:"truncated,omitempty"`
	DurationS string `json:"duration"`
}

// Exec runs one command and returns its output.
//
// A NON-ZERO EXIT IS NOT AN ERROR. A failing build is the answer to
// "does this build", and returning it as a tool error would have the
// agent retrying the transport instead of reading the compiler. The
// error return is for the transport only: unreachable, refused, timed
// out. Same split shell_command makes.
func (r *Remote) Exec(ctx context.Context, command, cwd string, timeout time.Duration) (*RemoteResult, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, errors.New("remote: command is required")
	}
	switch {
	case timeout <= 0:
		timeout = r.defaultTimeout
	case timeout > r.maxTimeout:
		timeout = r.maxTimeout
	}

	dialCtx, cancelDial := context.WithTimeout(ctx, remoteDialTimeout)
	defer cancelDial()

	client, err := r.dial(dialCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("remote %s: session: %w", r.Name, err)
	}
	defer func() { _ = session.Close() }()

	// The executor's buffer, reused: it already keeps the FIRST n
	// bytes and reports a full-length write so a truncated stream
	// does not kill the command with a broken pipe. Both properties
	// matter here — a compiler reports what went wrong at the top
	// and then repeats itself.
	stdout := compute.NewCappedBuffer(remoteMaxOutput)
	stderr := compute.NewCappedBuffer(remoteMaxOutput)
	session.Stdout = stdout
	session.Stderr = stderr

	// cd is composed here rather than being the model's problem: a
	// working directory is a parameter, and making the model write
	// its own `cd x && ...` invites quoting bugs in the one place a
	// quoting bug runs arbitrary code.
	full := command
	if cwd = strings.TrimSpace(cwd); cwd != "" {
		full = "cd " + shellQuote(cwd) + " && " + command
	}

	runCtx, cancelRun := context.WithTimeout(ctx, timeout)
	defer cancelRun()

	started := time.Now()
	exitCode, err := runSession(runCtx, session, full)
	if err != nil {
		return nil, fmt.Errorf("remote %s: %w", r.Name, err)
	}

	return &RemoteResult{
		Remote:    r.Name,
		Command:   command,
		ExitCode:  exitCode,
		Stdout:    string(stdout.Bytes()),
		Stderr:    string(stderr.Bytes()),
		Truncated: stdout.Truncated() || stderr.Truncated(),
		DurationS: time.Since(started).Round(time.Millisecond).String(),
	}, nil
}

// RemoteEgressRole is the egress ACL role for one declared remote.
//
// Per-remote rather than one shared "remote" role: the ACL is
// generated from [[remote]], so a session aimed at web01 is allowed to
// reach web01 and nothing else. A single role would let any remote be
// dialled at any other remote's host, which is most of the property
// worth having.
func RemoteEgressRole(name string) string { return "remote/" + name }

func (r *Remote) dial(ctx context.Context) (*ssh.Client, error) {
	cfg := &ssh.ClientConfig{
		User:            r.user,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(r.signer)},
		HostKeyCallback: r.hostKeys,
		Timeout:         remoteDialTimeout,
	}
	// Through the egress filter, not straight out.
	//
	// This used to be a bare net.Dialer, which made remote_ssh the one
	// tool whose entire purpose is reaching off the box and the one
	// the egress ACL never saw. A role of "remote/<name>" means the
	// hosts this remote may be dialled at are the ones the operator
	// declared for it in [[remote]] — so an agent talked into aiming a
	// session somewhere else is refused by the proxy rather than by
	// our own good intentions.
	//
	// The client is context-aware, so a cancelled turn does not leave
	// a connect attempt running to its own deadline.
	conn, err := egress.For(RemoteEgressRole(r.Name)).DialContext(ctx, r.addr)
	if err != nil {
		return nil, fmt.Errorf("remote %s: dial %s: %w", r.Name, r.addr, err)
	}
	sshConn, chans, reqs, err := ssh.NewClientConn(conn, r.addr, cfg)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("remote %s: handshake %s: %w", r.Name, r.addr, err)
	}
	return ssh.NewClient(sshConn, chans, reqs), nil
}

// runSession runs the command, honouring ctx.
//
// x/crypto/ssh has no context-aware Run, so the cancellation path is a
// goroutine that closes the session. Closing is what actually stops it:
// Signal() is advisory and OpenSSH's sshd ignores SIGTERM over an exec
// channel often enough that relying on it would leave a hung build
// holding the turn open past its deadline.
func runSession(ctx context.Context, session *ssh.Session, command string) (int, error) {
	done := make(chan error, 1)
	go func() { done <- session.Run(command) }()

	select {
	case err := <-done:
		if err == nil {
			return 0, nil
		}
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitStatus(), nil
		}
		var sigErr *ssh.ExitMissingError
		if errors.As(err, &sigErr) {
			// The command died without reporting status — killed by a
			// signal, usually the OOM killer on a big build.
			return -1, nil
		}
		return 0, err
	case <-ctx.Done():
		_ = session.Close()
		return 0, fmt.Errorf("timed out after %s (the command may still be running on the remote): %w",
			deadlineOf(ctx), ctx.Err())
	}
}

func deadlineOf(ctx context.Context) string {
	if dl, ok := ctx.Deadline(); ok {
		return time.Until(dl).Round(time.Second).String()
	}
	return "its budget"
}

// shellQuote wraps a value for POSIX sh. Single quotes, with the
// standard '"'"' escape, because that is the only form with no
// remaining metacharacters inside it.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// --- file transfer ----------------------------------------------------

// remoteMaxTransferBytes caps one file in either direction.
//
// Lower than you might expect, deliberately. This is not a bulk copy
// tool: a repository moves by git and a build artefact moves by the
// registry. What is left is a log to attach to a reply, a patch, a
// config — all small. A ceiling that would accommodate a container
// image would also accommodate exfiltrating one.
const remoteMaxTransferBytes int64 = 32 << 20

// ErrTransferTooLarge is returned rather than truncating.
//
// Truncation is the wrong failure for a file: a half-copied binary is
// corrupt in a way the model cannot see and will report as success.
// Command OUTPUT truncates because the tail of a build log is noise;
// the tail of a file is the file.
var ErrTransferTooLarge = errors.New("remote: file exceeds the transfer cap")

// Put writes body to remotePath on the far end.
//
// Implemented as `cat > path` over a session rather than by adding an
// SFTP dependency. The bytes go down stdin untouched, so this is
// binary-exact — the only thing needing care is the PATH, which is
// quoted, and which is why the path is never interpolated raw.
func (r *Remote) Put(ctx context.Context, remotePath string, body []byte) error {
	if int64(len(body)) > remoteMaxTransferBytes {
		return fmt.Errorf("%w (%d bytes, cap %d)", ErrTransferTooLarge, len(body), remoteMaxTransferBytes)
	}
	dialCtx, cancelDial := context.WithTimeout(ctx, remoteDialTimeout)
	defer cancelDial()
	client, err := r.dial(dialCtx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("remote %s: session: %w", r.Name, err)
	}
	defer func() { _ = session.Close() }()

	session.Stdin = bytes.NewReader(body)
	stderr := compute.NewCappedBuffer(8 << 10)
	session.Stderr = stderr

	runCtx, cancelRun := context.WithTimeout(ctx, r.defaultTimeout)
	defer cancelRun()

	// The redirect is the whole command, so a failure here is a real
	// failure rather than a result to report: nothing was written, or
	// something was half written, and either way the caller must not
	// be told the file arrived.
	code, err := runSession(runCtx, session, "cat > "+shellQuote(remotePath))
	if err != nil {
		return fmt.Errorf("remote %s: upload %s: %w", r.Name, remotePath, err)
	}
	if code != 0 {
		return fmt.Errorf("remote %s: upload %s failed (exit %d): %s",
			r.Name, remotePath, code, strings.TrimSpace(string(stderr.Bytes())))
	}
	return nil
}

// Get reads remotePath from the far end.
//
// Refuses at the cap rather than truncating — see ErrTransferTooLarge.
// The limit is enforced on the way in, so an oversized file costs one
// read of the cap and not one of the whole file.
func (r *Remote) Get(ctx context.Context, remotePath string) ([]byte, error) {
	dialCtx, cancelDial := context.WithTimeout(ctx, remoteDialTimeout)
	defer cancelDial()
	client, err := r.dial(dialCtx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Close() }()

	session, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("remote %s: session: %w", r.Name, err)
	}
	defer func() { _ = session.Close() }()

	// One byte over the cap, so a file exactly AT the limit reads
	// whole and only a genuinely oversized one trips it.
	out := &limitedBuffer{limit: remoteMaxTransferBytes + 1}
	stderr := compute.NewCappedBuffer(8 << 10)
	session.Stdout = out
	session.Stderr = stderr

	runCtx, cancelRun := context.WithTimeout(ctx, r.defaultTimeout)
	defer cancelRun()

	code, err := runSession(runCtx, session, "cat -- "+shellQuote(remotePath))
	if err != nil {
		return nil, fmt.Errorf("remote %s: download %s: %w", r.Name, remotePath, err)
	}
	if code != 0 {
		return nil, fmt.Errorf("remote %s: download %s failed (exit %d): %s",
			r.Name, remotePath, code, strings.TrimSpace(string(stderr.Bytes())))
	}
	if int64(out.buf.Len()) > remoteMaxTransferBytes {
		return nil, fmt.Errorf("%w (cap %d)", ErrTransferTooLarge, remoteMaxTransferBytes)
	}
	return out.buf.Bytes(), nil
}

// limitedBuffer stops at limit and remembers that it did. Unlike
// cappedBuffer it is used where truncation must become an ERROR, so
// the overflow flag is read by the caller rather than reported to the
// model as a note.
type limitedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if remaining := l.limit - int64(l.buf.Len()); remaining > 0 {
		if int64(len(p)) > remaining {
			l.buf.Write(p[:remaining])
		} else {
			l.buf.Write(p)
		}
	}
	// Full length, as cappedBuffer does: a short write aborts the ssh
	// session, and the caller wants to report "too large" rather than
	// "broken pipe".
	return len(p), nil
}
