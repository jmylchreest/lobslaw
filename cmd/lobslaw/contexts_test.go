package main

import (
	"bytes"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Reaching a remote cluster took four flags on every invocation:
// --addr, --ca-cert, --node-cert, --node-key. An operator with two
// clusters typed eight paths a day and eventually wrote a shell alias,
// which is the same file with none of the checking.
//
// These tests exercise liveNode.resolve — the WIRING — rather than
// resolveContext alone. A context file that parses perfectly and is
// never consulted by the thing that dials is the failure worth
// catching.

// writeContexts points LOBSLAW_CONTEXTS at a file with the given body.
func writeContexts(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "contexts.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("LOBSLAW_CONTEXTS", path)
	return path
}

const twoClusters = `
default = "prod"

[contexts.prod]
addr    = "prod.example:9090"
ca_cert = "/creds/prod/ca.pem"
cert    = "/creds/prod/operator.pem"
key     = "/creds/prod/operator-key.pem"

[contexts.staging]
addr    = "staging.example:9090"
ca_cert = "/creds/staging/ca.pem"
cert    = "/creds/staging/operator.pem"
key     = "/creds/staging/operator-key.pem"
`

// boundNode builds a liveNode through its flag set, the way a
// subcommand does — so a flag that is declared but never read, or read
// under a different name, shows up here.
func boundNode(t *testing.T, args ...string) *liveNode {
	t.Helper()
	var l liveNode
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(&bytes.Buffer{})
	l.bind(fs)
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	return &l
}

// THE POINT OF THE FEATURE.
func TestANamedContextReplacesTheFourConnectionFlags(t *testing.T) {
	writeContexts(t, twoClusters)

	addr, ca, cert, key, err := boundNode(t, "--context", "staging").resolve()
	if err != nil {
		t.Fatalf("a named context did not resolve: %v", err)
	}
	if addr != "staging.example:9090" {
		t.Errorf("addr = %q", addr)
	}
	if ca != "/creds/staging/ca.pem" || cert != "/creds/staging/operator.pem" ||
		key != "/creds/staging/operator-key.pem" {
		t.Errorf("credentials = %q %q %q", ca, cert, key)
	}
}

// A bare command uses the default, which is what makes the feature
// worth having: no flag at all on the common path.
func TestNoContextFlagUsesTheDefault(t *testing.T) {
	writeContexts(t, twoClusters)

	addr, _, _, _, err := boundNode(t).resolve()
	if err != nil {
		t.Fatalf("the default context did not resolve: %v", err)
	}
	if addr != "prod.example:9090" {
		t.Errorf("addr = %q, want the default context's", addr)
	}
}

// LOBSLAW_CONTEXT is for a shell that lives in one cluster. It must
// reach the same field the flag does.
func TestTheContextCanComeFromTheEnvironment(t *testing.T) {
	writeContexts(t, twoClusters)
	t.Setenv("LOBSLAW_CONTEXT", "staging")

	addr, _, _, _, err := boundNode(t).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "staging.example:9090" {
		t.Errorf("addr = %q; LOBSLAW_CONTEXT was ignored", addr)
	}
}

// An explicit flag is somebody overriding on purpose — usually to
// reach one node of a cluster rather than whichever the context names.
func TestAnExplicitFlagBeatsTheContext(t *testing.T) {
	writeContexts(t, twoClusters)

	addr, ca, _, _, err := boundNode(t, "--context", "prod", "--addr", "node3.prod:9090").resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "node3.prod:9090" {
		t.Errorf("addr = %q; --addr did not win", addr)
	}
	// And the fields NOT overridden still come from the context, or
	// overriding one flag would mean supplying all four again.
	if ca != "/creds/prod/ca.pem" {
		t.Errorf("ca = %q; overriding --addr dropped the context's credentials", ca)
	}
}

// A config.toml on a laptop is likelier to be a leftover from running
// a node locally than the cluster the operator meant to reach.
func TestTheContextBeatsTheNodesConfigFile(t *testing.T) {
	writeContexts(t, twoClusters)
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[cluster]
advertise_addr = "localhost:9090"

[cluster.mtls]
ca_cert   = "/node/ca.pem"
node_cert = "/node/node.pem"
node_key  = "/node/node-key.pem"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, ca, _, _, err := boundNode(t, "--context", "prod", "--config", cfgPath).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "prod.example:9090" {
		t.Errorf("addr = %q; the node's config.toml overrode the named context", addr)
	}
	if ca != "/creds/prod/ca.pem" {
		t.Errorf("ca = %q; the node's own credentials overrode the operator's", ca)
	}
}

// With no context file at all, --config must still work exactly as it
// did — this feature is additive, and the node-local flow is the one
// every existing deployment uses.
func TestWithNoContextsFileTheConfigFileStillResolves(t *testing.T) {
	t.Setenv("LOBSLAW_CONTEXTS", filepath.Join(t.TempDir(), "absent.toml"))
	cfgPath := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfgPath, []byte(`
[cluster]
advertise_addr = "localhost:9090"

[cluster.mtls]
ca_cert   = "/node/ca.pem"
node_cert = "/node/node.pem"
node_key  = "/node/node-key.pem"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	addr, ca, _, _, err := boundNode(t, "--config", cfgPath).resolve()
	if err != nil {
		t.Fatalf("a missing contexts.toml broke the config path: %v", err)
	}
	if addr != "localhost:9090" || ca != "/node/ca.pem" {
		t.Errorf("addr=%q ca=%q", addr, ca)
	}
}

// A typo must not silently reach the default cluster. That is the one
// outcome worth preventing: a command aimed at staging that lands on
// production.
func TestAnUnknownContextIsRefusedRatherThanFallingBack(t *testing.T) {
	writeContexts(t, twoClusters)

	_, _, _, _, err := boundNode(t, "--context", "prd").resolve()
	if err == nil {
		t.Fatal("an unknown context name resolved; it would have used the default")
	}
	// The list is the whole answer for somebody who mistyped.
	for _, want := range []string{"prd", "prod", "staging"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// Written by hand, so "~/..." is what somebody types. A literal "~"
// directory is not what they meant.
func TestATildeInACredentialPathIsExpanded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	writeContexts(t, `
[contexts.prod]
addr    = "prod.example:9090"
ca_cert = "~/creds/ca.pem"
cert    = "~/creds/operator.pem"
key     = "~/creds/operator-key.pem"
`)

	_, ca, _, _, err := boundNode(t, "--context", "prod").resolve()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, "creds", "ca.pem"); ca != want {
		t.Errorf("ca = %q, want %q", ca, want)
	}
}

// No default is a reasonable thing to configure when an operator has
// both a staging and a production cluster. A bare command must then
// fail rather than pick one.
func TestWithNoDefaultABareCommandDoesNotPickACluster(t *testing.T) {
	writeContexts(t, `
[contexts.prod]
addr    = "prod.example:9090"
ca_cert = "/creds/ca.pem"
cert    = "/creds/operator.pem"
key     = "/creds/operator-key.pem"
`)

	if _, _, _, _, err := boundNode(t).resolve(); err == nil {
		t.Error("a bare command reached a cluster with no default configured")
	}
}

// `lobslaw --context prod memory list` must find the subcommand.
// Without --context in globalValueFlags the parser reads "prod" as the
// subcommand and every command silently falls through to the agent.
func TestTheContextFlagDoesNotHideTheSubcommand(t *testing.T) {
	args := []string{"--context", "prod", "memory", "list"}
	if got := findSubcmd(args, "memory"); got != 2 {
		t.Errorf("findSubcmd(%v) = %d, want 2", args, got)
	}
	if got := findSubcmd([]string{"--context=prod", "memory"}, "memory"); got != 1 {
		t.Error("--context=prod hid the subcommand")
	}
}

// And the global form must reach the subcommand's own flag set, or
// `lobslaw --context prod memory list` parses and then connects to
// nothing.
func TestAGlobalContextFlagReachesTheSubcommand(t *testing.T) {
	writeContexts(t, twoClusters)
	t.Setenv("LOBSLAW_CONTEXT", "")
	_ = os.Unsetenv("LOBSLAW_CONTEXT")

	hoistGlobalFlagsToEnv([]string{"--context", "staging", "memory", "list"})
	addr, _, _, _, err := boundNode(t).resolve()
	if err != nil {
		t.Fatal(err)
	}
	if addr != "staging.example:9090" {
		t.Errorf("addr = %q; the global --context did not reach the subcommand", addr)
	}
}

// --- the listing -------------------------------------------------------

// A context whose key was left behind on another machine otherwise
// fails at dial time with an error about a file, and this listing is
// where somebody looks first.
func TestTheListingReportsAMissingCredential(t *testing.T) {
	dir := t.TempDir()
	present := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(present, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeContexts(t, `
[contexts.prod]
addr    = "prod.example:9090"
ca_cert = "`+present+`"
cert    = "`+filepath.Join(dir, "gone.pem")+`"
`)

	var buf bytes.Buffer
	if err := contextList(&buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "MISSING") {
		t.Errorf("a credential file that does not exist was not flagged:\n%s", out)
	}
	if !strings.Contains(out, "prod.example:9090") {
		t.Errorf("the listing does not show the address:\n%s", out)
	}
	if !strings.Contains(out, "not set") {
		t.Errorf("an unconfigured key read the same as a present one:\n%s", out)
	}
}

func TestTheListingMarksTheDefault(t *testing.T) {
	writeContexts(t, twoClusters)

	var buf bytes.Buffer
	if err := contextList(&buf); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, "prod.example") && !strings.HasPrefix(line, "*") {
			t.Errorf("the default context is not marked: %q", line)
		}
		if strings.Contains(line, "staging.example") && strings.HasPrefix(line, "*") {
			t.Errorf("a non-default context is marked as default: %q", line)
		}
	}
}

// `cluster export-operator` prints a block to paste into contexts.toml.
// A snippet that does not parse — or that the loader reads back with
// different paths — sends the operator to a "no such file" error about
// a credential that is sitting right there.
func TestTheSignOperatorSnippetParsesAsAContext(t *testing.T) {
	snippet := operatorContextSnippet("/creds/operator.pem", "/creds/operator-key.pem", "/creds/ca.pem")
	writeContexts(t, strings.ReplaceAll(
		strings.ReplaceAll(snippet, "CLUSTER_NAME", "prod"),
		"NODE_HOST:PORT", "prod.example:9090"))

	got, err := resolveContext("prod")
	if err != nil {
		t.Fatalf("the printed snippet does not load: %v", err)
	}
	want := Context{
		Addr:   "prod.example:9090",
		CACert: "/creds/ca.pem",
		Cert:   "/creds/operator.pem",
		Key:    "/creds/operator-key.pem",
	}
	if got != want {
		t.Errorf("loaded %+v, want %+v", got, want)
	}
}

// A malformed file must say so rather than read as "no contexts", or
// an operator whose file has a typo sees their clusters vanish.
func TestAMalformedContextsFileIsAnError(t *testing.T) {
	writeContexts(t, "this is not toml [[[")

	if _, err := loadContexts(); err == nil {
		t.Fatal("a malformed contexts.toml parsed as empty")
	}
}

// Every live command labels its output with the address it dialled.
// resolve() returned the resolved address but never stored it, so a
// connection made through a context left liveNode.addr empty and the
// source line printed blank — the exact failure R28's "say where this
// came from" rule exists to prevent, in the code that implements it.
//
// Found by running the binary. The renderer tests all passed a literal
// address, so none of them could see that the caller supplied nothing.
func TestDiallingRecordsTheAddressItResolved(t *testing.T) {
	writeContexts(t, twoClusters)
	node := boundNode(t, "--context", "prod")

	if node.addr != "" {
		t.Fatalf("precondition: addr should start empty, got %q", node.addr)
	}
	// dial fails — nothing is listening — but resolution happens first
	// and is what the label depends on.
	_, _ = node.dial()

	if node.addr != "prod.example:9090" {
		t.Errorf("addr = %q after dialling; every source label would print blank", node.addr)
	}
}
