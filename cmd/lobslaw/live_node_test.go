package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// liveNode is the first CLI→running-node path in the tree, so the
// tests here are mostly about the failure everybody will hit first:
// running it somewhere that is not a cluster member.

func TestResolveNamesTheMissingPieces(t *testing.T) {
	t.Parallel()
	var l liveNode
	l.configPath = ""
	_, _, _, _, err := l.resolve()
	if err == nil {
		t.Fatal("resolve succeeded with nothing configured")
	}
	// Somebody running this on their laptop is missing exactly one of
	// these, and which one is the whole answer.
	for _, want := range []string{"--addr", "--ca-cert", "--node-cert", "--node-key", "--config"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %q; it does not mention %s", err, want)
		}
	}
}

func TestExplicitFlagsNeedNoConfig(t *testing.T) {
	t.Parallel()
	l := liveNode{addr: "node:7000", caCert: "ca.pem", nodeCert: "n.pem", nodeKey: "n.key"}
	addr, ca, cert, key, err := l.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "node:7000" || ca != "ca.pem" || cert != "n.pem" || key != "n.key" {
		t.Errorf("resolve() = %q %q %q %q", addr, ca, cert, key)
	}
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConfigSuppliesTheConnection(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `
[cluster]
listen_addr = "0.0.0.0:7000"
advertise_addr = "node-a:7000"

[cluster.mtls]
ca_cert = "/certs/ca.pem"
node_cert = "/certs/node.pem"
node_key = "/certs/node.key"
`)
	l := liveNode{configPath: path}
	addr, ca, cert, key, err := l.resolve()
	if err != nil {
		t.Fatal(err)
	}
	// AdvertiseAddr, not ListenAddr: a bind address of 0.0.0.0 is not
	// somewhere to connect.
	if addr != "node-a:7000" {
		t.Errorf("addr = %q, want the advertise address", addr)
	}
	if ca != "/certs/ca.pem" || cert != "/certs/node.pem" || key != "/certs/node.key" {
		t.Errorf("mtls = %q %q %q", ca, cert, key)
	}
}

// A node with no advertise address falls back to the listen address,
// which is right for the single-node case where they are the same
// thing written once.
func TestListenAddrIsTheFallback(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `
[cluster]
listen_addr = "127.0.0.1:7000"

[cluster.mtls]
ca_cert = "/c/ca.pem"
node_cert = "/c/n.pem"
node_key = "/c/n.key"
`)
	addr, _, _, _, err := (&liveNode{configPath: path}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "127.0.0.1:7000" {
		t.Errorf("addr = %q", addr)
	}
}

func TestExplicitFlagsBeatTheConfig(t *testing.T) {
	t.Parallel()
	path := writeConfig(t, `
[cluster]
advertise_addr = "from-config:7000"

[cluster.mtls]
ca_cert = "/c/ca.pem"
node_cert = "/c/n.pem"
node_key = "/c/n.key"
`)
	addr, _, _, _, err := (&liveNode{configPath: path, addr: "from-flag:9000"}).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "from-flag:9000" {
		t.Errorf("addr = %q; the config beat an explicit flag", addr)
	}
}

// --- dispatch ------------------------------------------------------

func TestTakeOfflineStripsTheFlag(t *testing.T) {
	t.Parallel()
	rest, offline := takeOffline([]string{"skill:tidy", "--offline", "--as", "user:john"})
	if !offline {
		t.Error("--offline was not detected")
	}
	// Stripped, or the subcommand's own FlagSet rejects a flag it has
	// never heard of.
	for _, a := range rest {
		if a == "--offline" {
			t.Fatalf("--offline survived into %v", rest)
		}
	}
	if len(rest) != 3 {
		t.Errorf("rest = %v", rest)
	}
}

func TestTakeOfflineAcceptsBothDashForms(t *testing.T) {
	t.Parallel()
	if _, offline := takeOffline([]string{"-offline"}); !offline {
		t.Error("single-dash form not detected")
	}
}

func TestWithoutTheFlagItIsLive(t *testing.T) {
	t.Parallel()
	rest, offline := takeOffline([]string{"skill:tidy"})
	if offline {
		t.Error("offline was selected without the flag")
	}
	if len(rest) != 1 {
		t.Errorf("rest = %v", rest)
	}
}

// Approving against a stopped node would mean the running cluster
// never sees the decision. Somebody who passed --offline believes the
// node is down, so this is refused rather than silently done live.
func TestApproveHasNoOfflineForm(t *testing.T) {
	t.Parallel()
	err := errLiveOnly("approve")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "never sees it") {
		t.Errorf("err = %q; it does not say why", err)
	}
}

// --- attribution ---------------------------------------------------

func TestExplicitApproverWins(t *testing.T) {
	t.Parallel()
	got, err := approverName("  user:john  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "user:john" {
		t.Errorf("approver = %q", got)
	}
}

// Defaulted to the OS user rather than to something like "cli". An
// approval attributed to the tool is one nobody can be asked about,
// which is the whole reason the field exists.
func TestTheApproverDefaultsToAPerson(t *testing.T) {
	t.Parallel()
	got, err := approverName("")
	if err != nil {
		t.Skipf("no OS user available in this environment: %v", err)
	}
	if !strings.HasPrefix(got, "user:") {
		t.Errorf("approver = %q, want a user: principal", got)
	}
	if got == "user:" || got == "cli" {
		t.Errorf("approver = %q; that is not a person", got)
	}
}

// The CLI used ListenAddr verbatim when advertise_addr was unset,
// which is 0.0.0.0:7443 on every stock deployment. The handshake then
// failed with "certificate is valid for 127.0.0.1, not 0.0.0.0" —
// which reads as a certificate problem rather than as the client
// having dialled a wildcard.
func TestABindAddressIsNotSomewhereToDial(t *testing.T) {
	t.Parallel()

	for _, c := range []struct{ listen, want string }{
		{"0.0.0.0:7443", "127.0.0.1:7443"},
		{":7443", "127.0.0.1:7443"},
		{"[::]:7443", "127.0.0.1:7443"},
		// An address that names a host is already dialable; leave it.
		{"10.0.0.5:7443", "10.0.0.5:7443"},
		{"node.internal:7443", "node.internal:7443"},
		// Not host:port at all — hand it back rather than guessing.
		{"garbage", "garbage"},
	} {
		if got := dialableListenAddr(c.listen); got != c.want {
			t.Errorf("dialableListenAddr(%q) = %q, want %q", c.listen, got, c.want)
		}
	}
}
