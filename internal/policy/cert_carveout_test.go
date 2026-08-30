package policy

import (
	"strings"
	"testing"
)

// This is a LOOSENING of a compiled-in floor, so the table that
// matters is the one asserting what still gets refused. A miss in the
// permissive direction is a private key leaving the node.
func TestCertificateCarveOutNeverReachesKeyMaterial(t *testing.T) {
	t.Parallel()
	denied := []string{
		// The pair this exists for: the cert carves out, the key next
		// to it must not.
		"/etc/lobslaw/certs/ca-key.pem",
		"/etc/lobslaw/certs/node-key.pem",
		"/etc/lobslaw/certs/key.pem",
		"/etc/letsencrypt/live/example.com/privkey.pem",
		// Names that mention a certificate AND a key. The key half
		// wins, which is the whole ordering argument.
		"/x/ca-cert-key.pem",
		"/x/root-ca-private.pem",
		"/x/chain-secret.pem",
		// Not cert-ish at all: the default is the floor.
		"/x/id_rsa.pem",
		"/x/backup.pem",
		"/x/dump.pem",
		// A different extension is not covered by the carve-out.
		"/etc/lobslaw/certs/ca.key",
	}
	for _, p := range denied {
		v, err := CheckPath(p)
		if v != PathDenied {
			t.Errorf("CheckPath(%q) = %v, want PathDenied (err=%v)", p, v, err)
		}
	}
}

// And the half it exists to permit — confirm, never allow.
func TestCertificateCarveOutDowngradesToConfirm(t *testing.T) {
	t.Parallel()
	confirm := []string{
		"/etc/lobslaw/certs/ca.pem",
		"/etc/lobslaw/certs/node-cert.pem",
		"/etc/letsencrypt/live/example.com/fullchain.pem",
		"/etc/ssl/certs/chain.pem",
		"/x/root.pem",
		"/x/ca-bundle.pem",
		"/x/issuer.pem",
	}
	for _, p := range confirm {
		v, err := CheckPath(p)
		if v != PathConfirm {
			t.Errorf("CheckPath(%q) = %v, want PathConfirm (err=%v)", p, v, err)
			continue
		}
		// The reason has to say why it is not a flat refusal, or the
		// agent reports it as one.
		if err == nil || !strings.Contains(err.Error(), "public half") {
			t.Errorf("CheckPath(%q) reason does not explain the downgrade: %v", p, err)
		}
	}
}

// The predicate on its own, because it is the part a future edit will
// touch and the part a mistake hides in.
func TestLooksLikeCertificate(t *testing.T) {
	t.Parallel()
	cases := map[string]bool{
		"ca.pem":           true,
		"cert.pem":         true,
		"fullchain.pem":    true,
		"node-cert.pem":    true,
		"ca-bundle.pem":    true,
		"root.pem":         true,
		"CA.PEM":           true, // case-insensitive
		"ca-key.pem":       false,
		"cert-key.pem":     false,
		"privkey.pem":      false,
		"ca-private.pem":   false,
		"secret-chain.pem": false,
		"random.pem":       false,
		"cacophony.pem":    false, // "ca" as a substring is not "ca"
		"monkey-cert.pem":  false, // contains "key"
	}
	for name, want := range cases {
		if got := looksLikeCertificate(name); got != want {
			t.Errorf("looksLikeCertificate(%q) = %v, want %v", name, got, want)
		}
	}
}

// A shell has nowhere to ask mid-command, so CheckCommandPaths refuses
// a confirm verdict. That means ca.pem becomes readable via read_file
// and stays refused inside a shell command — asymmetric, deliberate,
// and pinned here so nobody "fixes" it into a silent allow.
func TestAShellStillRefusesACertificate(t *testing.T) {
	t.Parallel()
	if err := CheckCommandPaths("openssl x509 -in /etc/lobslaw/certs/ca.pem -noout"); err == nil {
		t.Error("a shell command reached a confirm-tier path; a shell cannot ask, so it must refuse")
	}
}

// The operator procedure this unblocks, from the homelab runbook:
// list a node's certs and confirm the CA private key is NOT there.
// Every file that check expects to see must be reachable, and the one
// it expects to be absent must not be.
func TestTheCertVerificationProcedureIsReachable(t *testing.T) {
	t.Parallel()
	for _, p := range []string{
		"/etc/lobslaw/certs/ca.pem",
		"/etc/lobslaw/certs/node-cert.pem",
	} {
		if v, _ := CheckPath(p); v == PathDenied {
			t.Errorf("%s is part of the documented verification and is refused outright", p)
		}
	}
	if v, _ := CheckPath("/etc/lobslaw/certs/ca-key.pem"); v != PathDenied {
		t.Errorf("ca-key.pem = %v, want PathDenied — it is the file the procedure checks is absent", v)
	}
}
