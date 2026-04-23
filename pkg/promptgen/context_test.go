package promptgen

import (
	"strings"
	"testing"
)

func TestWrapContextEmptyBlocks(t *testing.T) {
	t.Parallel()
	if got := WrapContext(nil); got != "" {
		t.Errorf("nil blocks → empty string; got %q", got)
	}
	if got := WrapContext([]ContextBlock{{Content: ""}}); got != "" {
		t.Errorf("empty content elided; got %q", got)
	}
}

func TestWrapContextTrustedIsNotDelimited(t *testing.T) {
	t.Parallel()
	got := WrapContext([]ContextBlock{
		{Source: "operator", Trust: TrustTrusted, Content: "hello"},
	})
	if strings.Contains(got, "<untrusted>") {
		t.Error("trusted content should NOT be wrapped in <untrusted>")
	}
	if !strings.Contains(got, "operator") {
		t.Error("trusted source label should surface (in comment form)")
	}
}

// TestWrapContextUntrustedUsesDelimiter is the security-critical
// assertion: anything not explicitly trusted must be wrapped in
// <untrusted> ... </untrusted> so the model's safety training can
// refuse embedded instructions.
func TestWrapContextUntrustedUsesDelimiter(t *testing.T) {
	t.Parallel()
	got := WrapContext([]ContextBlock{
		{Source: "tool:bash:stdout", Trust: TrustUntrusted, Content: "some output"},
	})
	if !strings.Contains(got, "<untrusted ") {
		t.Error("untrusted block must open with <untrusted>")
	}
	if !strings.Contains(got, "</untrusted>") {
		t.Error("untrusted block must close with </untrusted>")
	}
	if !strings.Contains(got, `source="tool:bash:stdout"`) {
		t.Error("source attribute should identify the origin")
	}
}

func TestWrapContextUntrustedUserHasDistinctTag(t *testing.T) {
	t.Parallel()
	got := WrapContext([]ContextBlock{
		{Source: "channel:telegram", Trust: TrustUntrustedUser, Content: "hi"},
	})
	if !strings.Contains(got, "<untrusted-user ") {
		t.Error("user content should use untrusted-user tag for attribution")
	}
}

// TestWrapContextUnknownTrustDowngradesToUntrusted confirms the
// fail-safe posture: an unrecognised TrustLevel MUST NOT be treated
// as trusted. Defaults to the strictest tag shape.
func TestWrapContextUnknownTrustDowngradesToUntrusted(t *testing.T) {
	t.Parallel()
	got := WrapContext([]ContextBlock{
		{Source: "weird", Trust: TrustLevel(99), Content: "X"},
	})
	if !strings.Contains(got, "<untrusted ") {
		t.Error("SECURITY: unknown trust level must be treated as untrusted")
	}
}

func TestWrapContextPreservesBlockOrder(t *testing.T) {
	t.Parallel()
	got := WrapContext([]ContextBlock{
		{Source: "memory", Trust: TrustUntrusted, Content: "first"},
		{Source: "tool", Trust: TrustUntrusted, Content: "second"},
	})
	pos1 := strings.Index(got, "first")
	pos2 := strings.Index(got, "second")
	if !(pos1 < pos2) {
		t.Errorf("order preserved violated; got %q", got)
	}
}

func TestWrapContextElidesEmptyBlocksInMixedList(t *testing.T) {
	t.Parallel()
	got := WrapContext([]ContextBlock{
		{Source: "tool-a", Trust: TrustUntrusted, Content: ""},
		{Source: "tool-b", Trust: TrustUntrusted, Content: "present"},
		{Source: "tool-c", Trust: TrustUntrusted, Content: ""},
	})
	if strings.Contains(got, "tool-a") || strings.Contains(got, "tool-c") {
		t.Errorf("empty blocks should elide entirely; got %q", got)
	}
	if !strings.Contains(got, "tool-b") {
		t.Error("non-empty blocks should still render")
	}
}
