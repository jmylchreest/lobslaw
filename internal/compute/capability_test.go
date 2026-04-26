package compute

import (
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func TestSelectByCapabilitySortsByPriority(t *testing.T) {
	t.Parallel()
	providers := []config.ProviderConfig{
		{Label: "low", Capabilities: []string{"vision"}, Priority: 1},
		{Label: "high", Capabilities: []string{"vision"}, Priority: 10},
		{Label: "mid", Capabilities: []string{"vision"}, Priority: 5},
		{Label: "no-vision", Capabilities: []string{"chat"}, Priority: 100},
	}
	got := SelectByCapability(providers, CapabilityVision)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	if got[0].Label != "high" || got[1].Label != "mid" || got[2].Label != "low" {
		t.Errorf("priority ordering wrong: got %s,%s,%s", got[0].Label, got[1].Label, got[2].Label)
	}
}

func TestSelectByCapabilityTiebreakOnDeclarationOrder(t *testing.T) {
	t.Parallel()
	providers := []config.ProviderConfig{
		{Label: "first", Capabilities: []string{"pdf"}, Priority: 5},
		{Label: "second", Capabilities: []string{"pdf"}, Priority: 5},
	}
	got := SelectByCapability(providers, CapabilityPDF)
	if len(got) != 2 {
		t.Fatalf("len = %d", len(got))
	}
	if got[0].Label != "first" || got[1].Label != "second" {
		t.Errorf("declaration-order tiebreak failed: got %s,%s", got[0].Label, got[1].Label)
	}
}

func TestSelectByCapabilityAnyOf(t *testing.T) {
	t.Parallel()
	providers := []config.ProviderConfig{
		{Label: "whisper", Capabilities: []string{"audio-transcription"}},
		{Label: "openrouter-audio", Capabilities: []string{"audio-multimodal"}},
		{Label: "irrelevant", Capabilities: []string{"chat"}},
	}
	got := SelectByCapability(providers, CapabilityAudioTranscribe, CapabilityAudioMultimodal)
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2 (any-of match)", len(got))
	}
}

func TestSelectByCapabilityEmptyAnyOf(t *testing.T) {
	t.Parallel()
	providers := []config.ProviderConfig{
		{Label: "any", Capabilities: []string{"vision"}},
	}
	if got := SelectByCapability(providers); got != nil {
		t.Errorf("empty caps list should return nil; got %v", got)
	}
}

func TestSelectByCapabilityNoMatches(t *testing.T) {
	t.Parallel()
	providers := []config.ProviderConfig{
		{Label: "chat-only", Capabilities: []string{"chat"}},
	}
	if got := SelectByCapability(providers, CapabilityPDF); len(got) != 0 {
		t.Errorf("expected no matches; got %v", got)
	}
}
