package main

import (
	"bytes"
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// The mistake worth catching is a revoke that means something wider
// than what was typed, so the argument rules are tested without a node.
func TestGrantsRevokeRefusesAmbiguousShapes(t *testing.T) {
	t.Parallel()
	if _, err := grantsRevokeRequest(nil, "", true); err == nil {
		t.Error("naming nothing was accepted; that is a revoke by omission")
	} else if !strings.Contains(err.Error(), "--conversation") {
		t.Errorf("the error should say how to revoke a conversation: %v", err)
	}
	if _, err := grantsRevokeRequest([]string{"a"}, "slack:C1", true); err == nil {
		t.Error("ids and --conversation together were accepted")
	}
}

// Dry run is the default, and --apply is what turns it off. Getting
// this backwards means an operator believes they revoked something.
func TestGrantsRevokeIsDryRunUnlessApplied(t *testing.T) {
	t.Parallel()
	req, err := grantsRevokeRequest([]string{"a"}, "", false)
	if err != nil {
		t.Fatal(err)
	}
	if !req.GetDryRun() {
		t.Error("without --apply the request should be a dry run")
	}
	req, err = grantsRevokeRequest([]string{"a"}, "", true)
	if err != nil {
		t.Fatal(err)
	}
	if req.GetDryRun() {
		t.Error("--apply still asked for a dry run; nothing would be revoked")
	}
}

// A dry run must say so. Rendering it identically to an apply is how
// somebody walks away believing the grant is gone.
func TestGrantsRevokeRenderSaysWhichItWas(t *testing.T) {
	t.Parallel()
	res := &lobslawv1.RevokeSessionGrantsResponse{
		Revoked:  []string{"slack:C1\x00shell:run\x00git status"},
		NotFound: []string{"nope"},
	}
	var dry, applied bytes.Buffer
	renderGrantRevoke(&dry, res, false)
	renderGrantRevoke(&applied, res, true)

	if !strings.Contains(dry.String(), "DRY RUN") || !strings.Contains(dry.String(), "--apply") {
		t.Errorf("a dry run must say so and how to apply:\n%s", dry.String())
	}
	if strings.Contains(applied.String(), "DRY RUN") {
		t.Errorf("an applied revoke reported itself as a dry run:\n%s", applied.String())
	}
	// Not-found ids are named, not counted: "1 not found" leaves the
	// operator to guess, and the wrong guess is "already revoked".
	if !strings.Contains(applied.String(), "nope") {
		t.Error("not-found ids should be named")
	}
	// The NUL separator must not reach a terminal.
	if strings.Contains(applied.String(), "\x00") {
		t.Error("a raw storage key was printed")
	}
}

// An empty listing must name where it read from — an empty list is
// indistinguishable from the wrong store unless the source is on the
// page. Same reasoning as policy approvals.
func TestGrantsListNamesItsSource(t *testing.T) {
	t.Parallel()
	var b bytes.Buffer
	if err := renderGrants(&b, &lobslawv1.ListSessionGrantsResponse{}, "prod:9090", false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "prod:9090") {
		t.Errorf("an empty listing must say which node it asked:\n%s", b.String())
	}
}

func TestGrantsListGroupsByConversation(t *testing.T) {
	t.Parallel()
	res := &lobslawv1.ListSessionGrantsResponse{
		TtlSeconds: 86400,
		Grants: []*lobslawv1.SessionGrant{
			{Id: "a", SessionId: "slack:C1", Action: "shell:run", Resource: "git status"},
			{Id: "b", SessionId: "slack:C1", Action: "memory:write", Resource: "episodic"},
			{Id: "c", SessionId: "telegram:-100", Action: "shell:run", Resource: "ls"},
		},
	}
	var b bytes.Buffer
	if err := renderGrants(&b, res, "n1", false); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if strings.Count(out, "slack:C1") != 1 {
		t.Errorf("a conversation should head its own group once:\n%s", out)
	}
	if !strings.Contains(out, "24h0m0s") {
		t.Errorf("the listing should say how long a grant lasts:\n%s", out)
	}
	if !strings.Contains(out, "grants revoke --apply --conversation") {
		t.Errorf("the listing should show the next command:\n%s", out)
	}
}
