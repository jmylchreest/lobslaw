package policy

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The policy engine is default-deny, which is right, but it is
// operator-configurable all the way down: there is no floor a
// misconfiguration or a persuasive prompt cannot reach. Set every rule
// to allow and turn confirmations off and `rm -rf /` is permitted.
//
// This file is that floor. It is compiled in, it reads no
// configuration, and there is no override flag. The test that no
// config path disables it is the actual feature — the pattern list is
// the easy part.
//
// WHAT THIS IS NOT: a security boundary. A shell can reach every one
// of these paths by other means — a here-doc, a base64'd script, an
// interpreter, a path the pattern does not spell. The Landlock +
// seccomp sandbox is the real boundary. This layer stops accidents and
// gives the model an unambiguous stop signal it can adapt to instead
// of retrying blindly. Claiming more than that would be worse than not
// having it.

// HardlineError is returned when a request hits the floor. Distinct
// from a policy denial so the caller can render it as a tool error
// that tells the model to stop rather than to seek approval — there is
// nobody who can approve this.
type HardlineError struct {
	// Pattern names what matched, for the operator's logs.
	Pattern string
	// Detail is the model-facing explanation.
	Detail string
}

func (e *HardlineError) Error() string {
	return fmt.Sprintf("refused by the hardline floor (%s): %s", e.Pattern, e.Detail)
}

// IsHardline reports whether err came from the floor.
func IsHardline(err error) bool {
	var h *HardlineError
	return errors.As(err, &h)
}

// hardlineCommand is one compiled-in refusal. Exactly one of re or
// match is set; match exists for the shapes RE2 cannot express.
type hardlineCommand struct {
	name  string
	re    *regexp.Regexp
	match func(string) bool
	why   string
}

// hardlineCommands are refused regardless of policy, confirmations, or
// operator allowlisting.
//
// Patterns are deliberately loose about whitespace and flag order,
// because the shapes that matter are the ones a model produces when it
// has misunderstood the task — not the ones an attacker crafts to slip
// past a regex. An attacker has the sandbox to get through.
var hardlineCommands = []hardlineCommand{
	{
		name: "filesystem-wipe",
		// rm with a recursive+force flag pair aimed at / or /* — in
		// either flag order, and including the long forms.
		re:  regexp.MustCompile(`(?i)\brm\b[^|;&\n]*?\s-{1,2}[a-z-]*\s*(?:-{1,2}[a-z-]+\s*)*?/(?:\*)?(?:\s|$)`),
		why: "this deletes the root filesystem",
	},
	{
		name: "no-preserve-root",
		// The flag exists for exactly one purpose.
		re:  regexp.MustCompile(`(?i)--no-preserve-root`),
		why: "--no-preserve-root exists only to remove the guard against deleting /",
	},
	{
		name: "fork-bomb",
		// Needs to compare the function's name against its body, which
		// is a backreference, and RE2 has none. See looksLikeForkBomb.
		match: looksLikeForkBomb,
		why:   "this is a fork bomb; it takes the host down",
	},
	{
		name: "format-block-device",
		// Anchored on a device argument rather than on the bare word,
		// so a file that happens to be named mkfs.md is not refused.
		// A destructive mkfs always names a device.
		re:  regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b[^|;&\n]*\s/dev/\w`),
		why: "formatting a block device destroys everything on it",
	},
	{
		name: "raw-block-write",
		re:   regexp.MustCompile(`(?i)\bdd\b[^|;&\n]*\bof=/dev/(sd|hd|nvme|vd|mmcblk|disk)`),
		why:  "writing raw bytes to a block device destroys the filesystem on it",
	},
	{
		name: "network-pipe-to-interpreter",
		// curl/wget piped into a shell or interpreter: arbitrary remote
		// code, with the fetch invisible to the tool layer that would
		// otherwise have policy applied to it.
		re: regexp.MustCompile(`(?i)\b(curl|wget|fetch)\b[^|]*\|\s*(sudo\s+)?(ba|z|k|a|da)?sh\b|` +
			`\b(curl|wget|fetch)\b[^|]*\|\s*(sudo\s+)?(python3?|perl|ruby|node)\b`),
		why: "piping a download into an interpreter runs unreviewed remote code; fetch the content with a tool that has policy applied, then act on it",
	},
	{
		name: "world-writable-root",
		re:   regexp.MustCompile(`(?i)\bchmod\b[^|;&\n]*\s-{1,2}[a-zA-Z]*R[a-zA-Z]*\s[^|;&\n]*\s777\s+/(?:\s|$)|\bchmod\b[^|;&\n]*\s777\s+/(?:\s|$)`),
		why:  "making the root filesystem world-writable destroys every permission boundary on the host",
	},
	{
		name: "recursive-chown-root",
		re:   regexp.MustCompile(`(?i)\bchown\b[^|;&\n]*\s-{1,2}[a-zA-Z]*R[a-zA-Z]*\s[^|;&\n]*\s/(?:\s|$)`),
		why:  "recursively reassigning ownership of / breaks the host",
	},
}

// CheckCommand refuses a shell command that matches the floor.
//
// Called from the executor before policy rules load, and again inside
// the shell builtin. Twice on purpose: the executor check is the one
// that cannot be bypassed by a new caller, and the builtin check is
// the one that still fires if some future path invokes the builtin
// directly.
func CheckCommand(cmd string) error {
	s := strings.TrimSpace(cmd)
	if s == "" {
		return nil
	}
	for _, h := range hardlineCommands {
		hit := false
		switch {
		case h.re != nil:
			hit = h.re.MatchString(s)
		case h.match != nil:
			hit = h.match(s)
		}
		if hit {
			return &HardlineError{Pattern: h.name, Detail: h.why}
		}
	}
	return nil
}

// forkBombDef finds a shell function definition and captures its name
// and body separately, so the two can be compared.
var forkBombDef = regexp.MustCompile(`([A-Za-z_:][A-Za-z0-9_:]*)\(\)\{([^}]*)\}`)

// looksLikeForkBomb reports whether cmd defines a function that pipes
// into itself and backgrounds the result — :(){:|:&};: and every
// renamed variant of it.
//
// Whitespace is stripped first, because the shape is what matters and
// the spacing is free. Matching on the literal ":(){:|:&};:" would
// miss a model that formatted it.
func looksLikeForkBomb(cmd string) bool {
	stripped := strings.Map(func(r rune) rune {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			return -1
		}
		return r
	}, cmd)

	for _, m := range forkBombDef.FindAllStringSubmatch(stripped, -1) {
		name, body := m[1], m[2]
		// Self-pipe plus a background marker. Either alone is ordinary:
		// a function that calls itself is recursion, and a backgrounded
		// call is a job.
		if strings.Contains(body, name+"|"+name) && strings.Contains(body, "&") {
			return true
		}
	}
	return false
}

// PathVerdict is what the floor says about a path.
type PathVerdict int

const (
	// PathAllowed means the floor has no opinion. Ordinary policy
	// still applies.
	PathAllowed PathVerdict = iota
	// PathConfirm means the path is sensitive but not key material.
	PathConfirm
	// PathDenied means no configuration permits this.
	PathDenied
)

// protectedPath is one compiled-in path rule.
type protectedPath struct {
	name string
	// dir, when set, matches the path being inside that directory
	// under the user's home.
	dir string
	// carveOut names basenames inside dir that downgrade to confirm
	// rather than deny. ~/.ssh/config holds no key material and
	// refusing it outright breaks ordinary work.
	carveOut []string
	// abs matches an exact absolute path.
	abs string
	// base matches a basename glob anywhere.
	base string
	// carveOutIf downgrades a base match to confirm when it returns
	// true. A predicate rather than a list because the case it exists
	// for — *.pem — is a container format whose name does not say what
	// is inside, and the safe half is describable ("says cert, never
	// says key") while the dangerous half is open-ended.
	//
	// Confirm, never allow. The bar for a compiled-in floor to stop
	// refusing something outright is higher than "we are fairly sure",
	// and one tap is a cheap place to put the remaining doubt.
	carveOutIf func(base string) bool
	why        string
}

// looksLikeCertificate reports whether a PEM basename is a
// certificate rather than a key.
//
// TWO conditions, and the second is the one doing the work. Naming
// cert-ish spellings is easy and incomplete; refusing anything that
// mentions a key is what makes a miss safe. "server-cert.pem" carves
// out, "server-key.pem" does not, and a name matching neither list
// stays refused — the default is the floor, not the exception.
//
// Deliberately does not read the file. A floor that opened files to
// classify them would be a floor with an I/O dependency and a TOCTOU
// window, and the name is what every caller already has.
func looksLikeCertificate(base string) bool {
	name := strings.TrimSuffix(strings.ToLower(base), ".pem")

	// First: anything that mentions key material is out, whatever else
	// it says. "ca-key" contains "ca"; this ordering is what makes a
	// miss in the list below safe rather than dangerous.
	for _, forbidden := range []string{"key", "priv", "secret"} {
		if strings.Contains(name, forbidden) {
			return false
		}
	}

	// Then: whole parts, not substrings. A substring test would carve
	// out "cacophony.pem" on the strength of "ca", and the parts these
	// names are built from are separated by exactly these characters.
	//
	// The concatenated spellings are enumerated rather than derived.
	// "fullchain" is a Let's Encrypt convention, not a rule about
	// English, and guessing at compounds is how "monkey" becomes a
	// certificate.
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '_' || r == '.'
	})
	for _, part := range parts {
		switch part {
		case "ca", "cert", "certs", "certificate", "crt",
			"chain", "fullchain", "cacert", "cabundle", "certchain",
			"bundle", "issuer", "root", "intermediate", "public", "pub":
			return true
		}
	}
	return false
}

var protectedPaths = []protectedPath{
	{name: "ssh-keys", dir: ".ssh", carveOut: []string{"config", "known_hosts"},
		why: "~/.ssh holds private key material"},
	{name: "aws-credentials", dir: ".aws", why: "~/.aws holds cloud credentials"},
	{name: "kube-config", dir: ".kube", why: "~/.kube holds cluster credentials"},
	{name: "gnupg", dir: ".gnupg", why: "~/.gnupg holds private key material"},
	{name: "shadow", abs: "/etc/shadow", why: "/etc/shadow holds password hashes"},
	{name: "sudoers", abs: "/etc/sudoers", why: "/etc/sudoers defines privilege escalation"},
	{name: "dotenv", base: ".env*", why: "a .env file holds secrets"},
	{name: "envrc", base: ".envrc", why: ".envrc holds environment secrets"},
	{name: "cluster-state", base: "state.db*", why: "this is lobslaw's own replicated state"},
	{name: "tls-material", base: "*.key", why: "this is private key material"},
	// *.pem is the one pattern where the extension does not say what
	// the file is. PEM is a container: a private key is PEM, and so is
	// the certificate that key signed, which is the PUBLIC half and is
	// meant to be read. Refusing both meant ca.pem was refused
	// alongside the ca-key.pem that signs with it — and the operator
	// procedure for verifying a node's certs is to list that very
	// directory.
	//
	// So a name that is unambiguously a certificate downgrades to
	// CONFIRM rather than allow: the file is public, the directory it
	// sits in is not, and asking a human costs one tap.
	{name: "tls-material", base: "*.pem", why: "this is private key material",
		carveOutIf: looksLikeCertificate},

	// lobslaw's own on-disk state. These used to live in a SECOND
	// list — internalExcludes, over in the fs builtins — which
	// overlapped this one on state.db, *.key and *.pem while
	// disagreeing about why: it called a key in somebody's home
	// directory "cluster-internal". Two lists claiming the same files
	// is one list and a drift.
	//
	// The merge also un-masks this file's verdict model. The fs list
	// was a flat deny and ran FIRST, so on every shared pattern a
	// carveOut here could never take effect — latent rather than live,
	// because none of the shared entries has one yet, and exactly the
	// kind of thing that is discovered by someone adding one.
	{name: "raft-log", dir: ".raft", why: "this is lobslaw's own Raft log"},
	{name: "raft-snapshot", dir: ".snapshot", why: "this is a Raft snapshot"},
	{name: "bearer-token", base: "*.jwt", why: "this is a bearer token"},

	// NOT ".git". It was in the fs list, where it was written for
	// lobslaw's own data directory and caught every repository on the
	// box — including .git/config, which reading a remote or a branch
	// legitimately needs and which holds no secret. If the worry is a
	// stored credential that is .git-credentials, below, and not the
	// directory around it.
	{name: "git-credentials", base: ".git-credentials", why: "this holds git push credentials"},
}

// CheckPath classifies a filesystem path against the floor.
//
// Deliberately independent of whatever sandbox or mount policy is in
// force: the point of a floor is that it does not consult
// configuration, so a mount that happens to expose ~/.aws does not
// make reading it acceptable.
func CheckPath(path string) (PathVerdict, error) {
	p := strings.TrimSpace(path)
	if p == "" {
		return PathAllowed, nil
	}
	// Clean before matching, so ".ssh/../.ssh/id_rsa" and
	// "/etc/../etc/shadow" cannot walk around a prefix comparison.
	clean := filepath.Clean(p)
	slash := filepath.ToSlash(clean)
	base := filepath.Base(clean)
	segments := strings.Split(strings.TrimPrefix(slash, "/"), "/")

	for _, pp := range protectedPaths {
		switch {
		case pp.abs != "":
			if slash == filepath.ToSlash(pp.abs) {
				return PathDenied, &HardlineError{Pattern: pp.name, Detail: pp.why}
			}
		case pp.dir != "":
			// Matched by segment rather than by home-relative prefix:
			// the process HOME is not necessarily the operator's, and a
			// mount can surface someone's .ssh at any depth.
			if !slices.Contains(segments, pp.dir) {
				continue
			}
			if base != pp.dir && slices.Contains(pp.carveOut, base) {
				return PathConfirm, &HardlineError{
					Pattern: pp.name,
					Detail:  pp.why + ", but this particular file does not — it needs confirmation, not refusal",
				}
			}
			return PathDenied, &HardlineError{Pattern: pp.name, Detail: pp.why}
		case pp.base != "":
			ok, _ := filepath.Match(pp.base, base)
			if !ok {
				continue
			}
			if pp.carveOutIf != nil && pp.carveOutIf(base) {
				return PathConfirm, &HardlineError{
					Pattern: pp.name,
					Detail: pp.why + " — but this name reads as a certificate, which is the " +
						"public half and is meant to be readable. Confirm rather than refuse",
				}
			}
			return PathDenied, &HardlineError{Pattern: pp.name, Detail: pp.why}
		}
	}
	return PathAllowed, nil
}

// CheckCommandPaths refuses a command that names a protected path.
//
// Coarse by necessity — it scans whitespace-separated words rather
// than parsing shell grammar, so quoting and expansion defeat it. That
// is the documented limitation, not an oversight: this catches the
// model reaching for ~/.aws/credentials in the obvious way.
func CheckCommandPaths(cmd string) error {
	for _, word := range strings.FieldsFunc(cmd, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '"' || r == '\'' || r == '=' || r == ';'
	}) {
		w := strings.TrimSpace(word)
		if w == "" || !strings.ContainsAny(w, "/.") {
			continue
		}
		if strings.HasPrefix(w, "~/") {
			w = filepath.Join(homeDir(), strings.TrimPrefix(w, "~/"))
		}
		verdict, err := CheckPath(w)
		// A shell has no way to ask for confirmation mid-command, so a
		// confirm verdict is refused here rather than silently allowed.
		// The fs builtins are where the carve-out is usable.
		if verdict != PathAllowed {
			return err
		}
	}
	return nil
}

func homeDir() string {
	if h, err := os.UserHomeDir(); err == nil {
		return h
	}
	return "/root"
}
