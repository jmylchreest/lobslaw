package compute

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// A real SSH server, in-process. The alternative — mocking the ssh
// package — would test the mock: every property worth pinning here
// (host-key pinning, exit-code passthrough, cwd quoting, truncation)
// lives in the protocol rather than in our call sites.

type fakeRemoteServer struct {
	ln       net.Listener
	hostKey  ssh.Signer
	authKeys map[string]bool

	// lastCommand is what the client actually asked to run, so a test
	// can assert on the composed command rather than its effect.
	lastCommand chan string
	// reply decides what a command produces.
	reply func(cmd string) (stdout, stderr string, code int)
	// onStdin receives whatever the client wrote to the session, so an
	// upload can be asserted on the bytes that actually arrived.
	onStdin func([]byte)
}

func newFakeRemote(t *testing.T, clientPub ssh.PublicKey, hostKey ssh.Signer) *fakeRemoteServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeRemoteServer{
		ln:          ln,
		hostKey:     hostKey,
		authKeys:    map[string]bool{string(clientPub.Marshal()): true},
		lastCommand: make(chan string, 8),
		reply: func(string) (string, string, int) {
			return "ok\n", "", 0
		},
	}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *fakeRemoteServer) addr() (host string, port int) {
	a := s.ln.Addr().(*net.TCPAddr)
	return a.IP.String(), a.Port
}

func (s *fakeRemoteServer) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeRemoteServer) handle(nc net.Conn) {
	defer func() { _ = nc.Close() }()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if s.authKeys[string(key.Marshal())] {
				return &ssh.Permissions{}, nil
			}
			return nil, fmt.Errorf("unknown key")
		},
	}
	cfg.AddHostKey(s.hostKey)

	_, chans, reqs, err := ssh.NewServerConn(nc, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nch := range chans {
		if nch.ChannelType() != "session" {
			_ = nch.Reject(ssh.UnknownChannelType, "only sessions")
			continue
		}
		ch, chReqs, err := nch.Accept()
		if err != nil {
			return
		}
		go s.session(ch, chReqs)
	}
}

func (s *fakeRemoteServer) session(ch ssh.Channel, reqs <-chan *ssh.Request) {
	defer func() { _ = ch.Close() }()
	for req := range reqs {
		if req.Type != "exec" {
			_ = req.Reply(false, nil)
			continue
		}
		var payload struct{ Command string }
		if err := ssh.Unmarshal(req.Payload, &payload); err != nil {
			_ = req.Reply(false, nil)
			return
		}
		_ = req.Reply(true, nil)

		select {
		case s.lastCommand <- payload.Command:
		default:
		}

		if s.onStdin != nil {
			body, _ := io.ReadAll(ch)
			s.onStdin(body)
		}
		stdout, stderr, code := s.reply(payload.Command)
		_, _ = ch.Write([]byte(stdout))
		_, _ = ch.Stderr().Write([]byte(stderr))
		_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(code)}))
		return
	}
}

// --- helpers ----------------------------------------------------------

func newEd25519Signer(t *testing.T) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	der, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	return signer, string(pem.EncodeToMemory(der))
}

type mapSecrets map[string]string

func (m mapSecrets) Resolve(ref string) (string, error) {
	v, ok := m[ref]
	if !ok {
		return "", fmt.Errorf("no such ref %q", ref)
	}
	return v, nil
}

// remoteUnderTest wires a Remote at a running fake server.
func remoteUnderTest(t *testing.T, knownHosts string) (*RemoteSet, *fakeRemoteServer) {
	t.Helper()
	clientSigner, clientPEM := newEd25519Signer(t)
	hostSigner, _ := newEd25519Signer(t)
	srv := newFakeRemote(t, clientSigner.PublicKey(), hostSigner)
	host, port := srv.addr()

	set, err := NewRemoteSet([]config.RemoteConfig{{
		Name:        "go",
		Description: "Go toolchain",
		Host:        host,
		Port:        port,
		User:        "dev",
		KeyRef:      "key",
		KnownHosts:  knownHosts,
	}}, mapSecrets{"key": clientPEM})
	if err != nil {
		t.Fatalf("NewRemoteSet: %v", err)
	}
	return set, srv
}

// --- tests ------------------------------------------------------------

// A failing build is the answer to "does this build". Returning it as a
// tool error would have the agent retrying the transport instead of
// reading the compiler.
func TestRemoteNonZeroExitIsAResultNotAnError(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	srv.reply = func(string) (string, string, int) {
		return "", "undefined: Frobnicate\n", 2
	}
	box, err := set.Get("go")
	if err != nil {
		t.Fatal(err)
	}
	res, err := box.Exec(context.Background(), "go build ./...", "", 0)
	if err != nil {
		t.Fatalf("a failing command must not be a transport error: %v", err)
	}
	if res.ExitCode != 2 {
		t.Errorf("ExitCode = %d, want 2", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "Frobnicate") {
		t.Errorf("stderr lost: %q", res.Stderr)
	}
}

// The whole design rests on the model naming a remote and nothing else.
// An unknown name has to say what IS configured, or the retry is
// another guess.
func TestRemoteUnknownNameNamesWhatExists(t *testing.T) {
	set, _ := remoteUnderTest(t, "")
	_, err := set.Get("rust")
	if !errors.Is(err, ErrUnknownRemote) {
		t.Fatalf("want ErrUnknownRemote, got %v", err)
	}
	if !strings.Contains(err.Error(), "go") {
		t.Errorf("the error should name the configured remotes, got %q", err)
	}
	// Case is a spelling difference, not a different request.
	if _, err := set.Get("  GO "); err != nil {
		t.Errorf("name lookup should be case- and space-insensitive: %v", err)
	}
}

// Trust on first use, then pin. Weak exactly once; strong every time
// after — which is the property InsecureIgnoreHostKey never has.
func TestRemoteRecordsHostKeyThenRefusesAChange(t *testing.T) {
	dir := t.TempDir()
	kh := filepath.Join(dir, "known_hosts")

	set, srv := remoteUnderTest(t, kh)
	box, _ := set.Get("go")
	if _, err := box.Exec(context.Background(), "true", "", 0); err != nil {
		t.Fatalf("first connect should be accepted and recorded: %v", err)
	}

	recorded, err := os.ReadFile(kh)
	if err != nil || len(recorded) == 0 {
		t.Fatalf("host key was not persisted: %v / %d bytes", err, len(recorded))
	}

	// The remote comes back with a different host key: either it was
	// rebuilt, or somebody is in the middle. Both are refused.
	newHost, _ := newEd25519Signer(t)
	srv.hostKey = newHost
	_, err = box.Exec(context.Background(), "true", "", 0)
	if err == nil {
		t.Fatal("a changed host key was accepted")
	}
	if !strings.Contains(err.Error(), "changed") {
		t.Errorf("the error should say the key changed, got %q", err)
	}
}

// With no known_hosts file the key is still pinned, for the life of the
// process. Empty means "do not persist", never "do not verify".
func TestRemoteWithoutKnownHostsStillPinsWithinTheRun(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	box, _ := set.Get("go")
	if _, err := box.Exec(context.Background(), "true", "", 0); err != nil {
		t.Fatalf("first connect: %v", err)
	}
	newHost, _ := newEd25519Signer(t)
	srv.hostKey = newHost
	if _, err := box.Exec(context.Background(), "true", "", 0); err == nil {
		t.Fatal("a mid-run host key change was accepted with no known_hosts file")
	}
}

// cwd is composed here rather than left to the model, so a directory
// with a quote in it cannot become a second command.
func TestRemoteQuotesTheWorkingDirectory(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	box, _ := set.Get("go")
	if _, err := box.Exec(context.Background(), "go test ./...", "/workspace/tasks/it's here", 0); err != nil {
		t.Fatalf("exec: %v", err)
	}
	got := <-srv.lastCommand
	want := `cd '/workspace/tasks/it'"'"'s here' && go test ./...`
	if got != want {
		t.Errorf("composed command:\n got %q\nwant %q", got, want)
	}
}

// Output goes into the model's context, so the cap is a context budget.
// Truncation must be reported, and must not kill the command.
func TestRemoteTruncatesAndSaysSo(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	srv.reply = func(string) (string, string, int) {
		return strings.Repeat("x", int(remoteMaxOutput)+4096), "", 0
	}
	box, _ := set.Get("go")
	res, err := box.Exec(context.Background(), "cat big.log", "", 0)
	if err != nil {
		t.Fatalf("exec: %v", err)
	}
	if !res.Truncated {
		t.Error("oversized output was not reported as truncated")
	}
	if int64(len(res.Stdout)) > remoteMaxOutput {
		t.Errorf("stdout %d bytes exceeds the %d cap", len(res.Stdout), remoteMaxOutput)
	}
	if res.ExitCode != 0 {
		t.Errorf("truncation must not change the exit code, got %d", res.ExitCode)
	}
}

// The builtin's contract: a transport failure is exit 1, a bad call is
// exit 2, and a command that ran is exit 0 whatever it returned.
func TestRemoteBuiltinSeparatesTransportFromResult(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	srv.reply = func(string) (string, string, int) { return "", "boom\n", 1 }
	h := newRemoteSSHHandler(set)

	out, code, err := h(context.Background(), map[string]string{"remote": "go", "command": "make"})
	if err != nil || code != 0 {
		t.Fatalf("a command that ran and failed should be exit 0 with a result: code=%d err=%v", code, err)
	}
	var res RemoteResult
	if uerr := json.Unmarshal(out, &res); uerr != nil {
		t.Fatalf("result is not JSON: %v", uerr)
	}
	if res.ExitCode != 1 || !strings.Contains(res.Stderr, "boom") {
		t.Errorf("result did not carry the failure: %+v", res)
	}

	if _, code, err = h(context.Background(), map[string]string{"remote": "nope", "command": "make"}); code != 2 || err == nil {
		t.Errorf("an unknown remote should be a malformed call (2), got code=%d err=%v", code, err)
	}
	if _, code, err = h(context.Background(), map[string]string{"remote": "go"}); code != 2 || err == nil {
		t.Errorf("a missing command should be a malformed call (2), got code=%d err=%v", code, err)
	}
}

// A host the model could name is a host the operator did not choose.
// The schema constrains `remote` to an enum of what exists.
func TestRemoteToolDefConstrainsTheTargetToConfiguredNames(t *testing.T) {
	set, _ := remoteUnderTest(t, "")
	defs := RemoteToolDefs(set)
	if len(defs) != 2 {
		t.Fatalf("want remote_ssh and remote_scp, got %d", len(defs))
	}
	for _, def := range defs {
		schema := string(def.ParametersSchema)
		if !strings.Contains(schema, `"enum": ["go"]`) {
			t.Errorf("%s: remote should be constrained to an enum of configured names:\n%s", def.Name, schema)
		}
		// The property the whole design rests on: no field the model
		// fills can name a machine the operator did not.
		for _, forbidden := range []string{"host", "port", "user", "key"} {
			if strings.Contains(schema, `"`+forbidden+`": {`) {
				t.Errorf("%s: the schema exposes %q; the model must not be able to choose one", def.Name, forbidden)
			}
		}
		if def.RiskTier != "irreversible" {
			t.Errorf("%s: RiskTier = %q, want irreversible", def.Name, def.RiskTier)
		}
	}
}

// remote_scp touches the LOCAL filesystem, which is where the cluster
// CA, the node key and the memory key live. Both directions have to
// refuse a cluster-internal path — upload because it would leave the
// node, download because the remote would then choose what this node
// reads back.
func TestRemoteSCPRefusesInternalPathsBothWays(t *testing.T) {
	set, _ := remoteUnderTest(t, "")
	h := newRemoteSCPHandler(set)

	// A path isInternalPath matches regardless of mount configuration.
	internal := filepath.Join(t.TempDir(), "certs", "ca-key.pem")
	if err := os.MkdirAll(filepath.Dir(internal), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(internal, []byte("PRIVATE KEY"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !isInternalPath(internal) {
		t.Skipf("%q is not classed internal; the guard under test does not apply", internal)
	}

	for _, dir := range []string{"upload", "download"} {
		out, code, err := h(context.Background(), map[string]string{
			"remote": "go", "direction": dir,
			"local_path": internal, "remote_path": "/tmp/stolen",
		})
		if err != nil {
			t.Fatalf("%s: unexpected transport error: %v", dir, err)
		}
		if code == 0 {
			t.Errorf("%s of a cluster-internal path was allowed", dir)
		}
		if !strings.Contains(string(out), "internal_path") {
			t.Errorf("%s: expected an internal_path refusal, got %s", dir, out)
		}
	}
}

// A direction is required and must be one of two words. Defaulting it
// would mean guessing whether a file is arriving or leaving.
func TestRemoteSCPRequiresAnExplicitDirection(t *testing.T) {
	set, _ := remoteUnderTest(t, "")
	h := newRemoteSCPHandler(set)
	for _, dir := range []string{"", "sideways", "put"} {
		if _, code, err := h(context.Background(), map[string]string{
			"remote": "go", "direction": dir,
			"local_path": "/tmp/x", "remote_path": "/tmp/y",
		}); code != 2 || err == nil {
			t.Errorf("direction %q should be a malformed call, got code=%d err=%v", dir, code, err)
		}
	}
}

// A file round-trips byte-for-byte. `cat` over a session is only
// acceptable if it is binary-exact, so this uses bytes that would not
// survive any quoting or text handling.
func TestRemoteTransferIsBinaryExact(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	box, _ := set.Get("go")

	payload := []byte{0x00, 0xff, 0x0a, 0x0d, 0x1b, '\'', '"', '$', '`', 0x80, 0xfe}
	var got []byte
	srv.onStdin = func(b []byte) { got = append([]byte(nil), b...) }

	if err := box.Put(context.Background(), "/workspace/it's a file", payload); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if string(got) != string(payload) {
		t.Errorf("upload corrupted the bytes:\n got %x\nwant %x", got, payload)
	}
	// The path is quoted rather than interpolated, so a quote in a
	// filename cannot start a second command.
	cmd := <-srv.lastCommand
	if !strings.Contains(cmd, `cat > '/workspace/it'"'"'s a file'`) {
		t.Errorf("upload did not quote the remote path: %q", cmd)
	}
}

// Truncation is the wrong failure for a file: a half-copied binary is
// corrupt in a way the model cannot see and reports as success.
func TestRemoteTransferRefusesOversizeRatherThanTruncating(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	box, _ := set.Get("go")

	srv.reply = func(string) (string, string, int) {
		return strings.Repeat("x", int(remoteMaxTransferBytes)+1024), "", 0
	}
	if _, err := box.Get(context.Background(), "/workspace/big.bin"); !errors.Is(err, ErrTransferTooLarge) {
		t.Errorf("an oversized download should be refused, got %v", err)
	}
	if err := box.Put(context.Background(), "/tmp/x", make([]byte, remoteMaxTransferBytes+1)); !errors.Is(err, ErrTransferTooLarge) {
		t.Errorf("an oversized upload should be refused, got %v", err)
	}
}

// A configured remote that cannot be built fails at boot. A tool that
// silently did not load looks like a model fault every time it is used.
func TestNewRemoteSetRefusesABrokenEntry(t *testing.T) {
	cases := map[string]config.RemoteConfig{
		"no name":     {Host: "h", KeyRef: "key"},
		"no host":     {Name: "go", KeyRef: "key"},
		"no key":      {Name: "go", Host: "h"},
		"bad name":    {Name: "go/rust", Host: "h", KeyRef: "key"},
		"bad port":    {Name: "go", Host: "h", KeyRef: "key", Port: 99999},
		"unknown ref": {Name: "go", Host: "h", KeyRef: "missing"},
	}
	_, pemKey := newEd25519Signer(t)
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := NewRemoteSet([]config.RemoteConfig{c}, mapSecrets{"key": pemKey}); err == nil {
				t.Error("expected a boot-time refusal")
			}
		})
	}

	dup := config.RemoteConfig{Name: "go", Host: "h", KeyRef: "key"}
	if _, err := NewRemoteSet([]config.RemoteConfig{dup, dup}, mapSecrets{"key": pemKey}); err == nil {
		t.Error("duplicate names should be refused")
	}
}

// A turn that ends must not leave a build running to its own deadline.
func TestRemoteHonoursACancelledContext(t *testing.T) {
	set, srv := remoteUnderTest(t, "")
	srv.reply = func(string) (string, string, int) {
		time.Sleep(5 * time.Second)
		return "", "", 0
	}
	box, _ := set.Get("go")

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := box.Exec(ctx, "sleep 5", "", 0); err == nil {
		t.Fatal("a cancelled turn should abort the command")
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Errorf("cancellation took %s; it should not wait for the command", elapsed)
	}
}
