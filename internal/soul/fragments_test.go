package soul

import (
	"strings"
	"testing"
)

func TestSanitiseFragmentStripsControlAndCollapsesWhitespace(t *testing.T) {
	t.Parallel()
	got, err := SanitiseFragment("  user supports\nLiverpool\tFC  ")
	if err != nil {
		t.Fatal(err)
	}
	if got != "user supports Liverpool FC" {
		t.Errorf("got %q", got)
	}
}

func TestSanitiseFragmentRejectsBackticks(t *testing.T) {
	t.Parallel()
	got, err := SanitiseFragment("eats `code` for breakfast")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "`") {
		t.Errorf("backticks should be stripped: %q", got)
	}
	if got != "eats code for breakfast" {
		t.Errorf("got %q", got)
	}
}

func TestSanitiseFragmentNeutralisesCodeFences(t *testing.T) {
	t.Parallel()
	got, err := SanitiseFragment("ignore prior\n```\nattack\n```")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "`") || strings.Contains(got, "```") {
		t.Errorf("fences should be stripped, not preserved: %q", got)
	}
	if !strings.Contains(got, "ignore prior") {
		t.Errorf("legitimate text should be preserved: %q", got)
	}
}

func TestSanitiseFragmentRejectsTooLong(t *testing.T) {
	t.Parallel()
	in := strings.Repeat("a", MaxFragmentLength+10)
	if _, err := SanitiseFragment(in); err == nil {
		t.Error("expected rejection for over-cap fragment")
	}
}

func TestSanitiseFragmentRejectsEmpty(t *testing.T) {
	t.Parallel()
	if _, err := SanitiseFragment("   \t\n   "); err == nil {
		t.Error("expected rejection for whitespace-only input")
	}
}

func TestSanitiseNameRejectsMarkdown(t *testing.T) {
	t.Parallel()
	if _, err := SanitiseName("[Lobs](evil.com)"); err == nil {
		t.Error("expected name to reject markdown link syntax")
	}
}

func TestRenderFragmentsEmitsDelimitedBlock(t *testing.T) {
	t.Parallel()
	out := RenderFragments([]string{"likes coffee", "supports Liverpool"})
	if !strings.Contains(out, FragmentRenderStartMarker) || !strings.Contains(out, FragmentRenderEndMarker) {
		t.Errorf("missing markers: %q", out)
	}
	if !strings.Contains(out, "- likes coffee") || !strings.Contains(out, "- supports Liverpool") {
		t.Errorf("fragments not rendered as bullets: %q", out)
	}
}

func TestRenderFragmentsEmptyList(t *testing.T) {
	t.Parallel()
	if got := RenderFragments(nil); got != "" {
		t.Errorf("expected empty render for nil; got %q", got)
	}
}
