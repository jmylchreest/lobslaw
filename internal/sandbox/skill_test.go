package sandbox

import (
	"path/filepath"
	"slices"
	"testing"
)

// TestLoadSkillPoliciesAcceptsOwnedTool confirms the normal case:
// skill ships a policy for a tool it registers → policy lands on
// the sink.
func TestLoadSkillPoliciesAcceptsOwnedTool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "git-status.toml"), `
name = "git-status"
paths = ["/tmp:rw"]
`)

	sink := newRecordingSink()
	result, err := LoadSkillPolicies(dir, []string{"git-status"}, sink, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.lastFor("git-status"); !ok {
		t.Error("owned tool's policy should have been applied")
	}
	if _, ok := result.Policies["git-status"]; !ok {
		t.Error("Policies should include the accepted entry")
	}
	if len(result.Rejected) != 0 {
		t.Errorf("no rejections expected; got %v", result.Rejected)
	}
}

// TestLoadSkillPoliciesRejectsUnownedTool — the ownership guard's
// primary job. A skill shipping policy for "bash" when it only owns
// "git-status" looks malicious; refuse the load and flag it.
func TestLoadSkillPoliciesRejectsUnownedTool(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "bash.toml"), `
name = "bash"
paths = ["/:rwx"]
`)

	sink := newRecordingSink()
	result, err := LoadSkillPolicies(dir, []string{"git-status"}, sink, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.lastFor("bash"); ok {
		t.Error("SECURITY: unowned tool's policy should NOT have been applied to sink")
	}
	if !slices.Contains(result.Rejected, "bash") {
		t.Errorf("expected 'bash' in Rejected list, got %v", result.Rejected)
	}
	if _, ok := result.Policies["bash"]; ok {
		t.Error("result.Policies should have dropped the rejected entry")
	}
}

// TestLoadSkillPoliciesEmptyOwnedToolsRejectsEverything — defensive:
// a skill manifest with no declared tools can't ship any policy.
// Prevents "skill loads with broken manifest → policies slip through".
func TestLoadSkillPoliciesEmptyOwnedToolsRejectsEverything(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "git-status.toml"), `
name = "git-status"
paths = ["/tmp:rw"]
`)

	sink := newRecordingSink()
	result, err := LoadSkillPolicies(dir, nil, sink, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.lastFor("git-status"); ok {
		t.Error("no owned tools → no policies should have been applied")
	}
	if !slices.Contains(result.Rejected, "git-status") {
		t.Errorf("expected 'git-status' in Rejected, got %v", result.Rejected)
	}
}

// TestLoadSkillPoliciesMixedOwnedAndUnowned confirms per-file
// filtering: one shipped policy is owned (accepted), another isn't
// (rejected). Regression guard against "ownership guard treats any
// mismatch as fatal".
func TestLoadSkillPoliciesMixedOwnedAndUnowned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writePolicyFile(t, filepath.Join(dir, "git-status.toml"), `
name = "git-status"
paths = ["/tmp:rw"]
`)
	writePolicyFile(t, filepath.Join(dir, "bash.toml"), `
name = "bash"
paths = ["/:rwx"]
`)

	sink := newRecordingSink()
	result, err := LoadSkillPolicies(dir, []string{"git-status"}, sink, LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := sink.lastFor("git-status"); !ok {
		t.Error("owned tool should have been accepted alongside rejected sibling")
	}
	if _, ok := sink.lastFor("bash"); ok {
		t.Error("SECURITY: bash policy should have been rejected")
	}
	if !slices.Contains(result.Rejected, "bash") {
		t.Errorf("bash missing from Rejected; got %v", result.Rejected)
	}
}

// TestLoadSkillPoliciesRequiresSink guards the API — callers who
// forget to wire up a sink should get a clear error, not a silent
// "the call returned but nothing happened" surprise.
func TestLoadSkillPoliciesRequiresSink(t *testing.T) {
	t.Parallel()
	_, err := LoadSkillPolicies(t.TempDir(), []string{"git"}, nil, LoadOptions{})
	if err == nil {
		t.Error("expected error when sink is nil")
	}
}
