package compute

import (
	"context"
	"strings"
	"testing"
)

// The prompt leads with the classification and still shows the command
// verbatim and in full. Both halves matter: the headline is what makes
// a 300-character probe answerable in one glance, and the verbatim
// command is what makes the answer mean anything.
func TestShellCommandSummaryLeadsWithTheClassification(t *testing.T) {
	t.Parallel()
	const cmd = "echo start; rm -rf /etc/hosts"
	got := ShellCommandSummary(context.Background(), map[string]string{"command": cmd})

	head, rest, found := strings.Cut(got, "\n")
	if !found {
		t.Fatalf("summary has no headline line: %q", got)
	}
	if !strings.HasPrefix(head, "privilege") {
		t.Errorf("headline = %q, want it to lead with the labels", head)
	}
	if !strings.Contains(head, "rm -rf /etc/hosts") {
		t.Errorf("headline = %q, want it to name the step that caused the ask", head)
	}
	// Verbatim and in full, unchanged by the header above it.
	if !strings.Contains(rest, cmd) {
		t.Errorf("summary body = %q, want the command verbatim", rest)
	}
}

// A grantable command's summary must still contain the resource
// byte-for-byte: what the user reads is what gets minted, and a
// headline must not have disturbed that.
func TestShellCommandSummaryStillEchoesTheGrantKey(t *testing.T) {
	t.Parallel()
	params := map[string]string{"command": "git   status    --short"}
	target := ShellGrantResource(params)
	if !target.Grantable {
		t.Fatal("the fixture stopped being grantable")
	}
	got := ShellCommandSummary(context.Background(), params)
	if !strings.Contains(got, target.Resource) {
		t.Errorf("summary %q does not contain the grant key %q", got, target.Resource)
	}
}
