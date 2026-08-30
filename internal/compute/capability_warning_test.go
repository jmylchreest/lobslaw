package compute

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/modelsdev"
)

// A provider that declares a modality its model cannot do routes work
// to something that will refuse it. The catalogue already knows enough
// to say so at boot rather than at 3am.
//
// The hard part is not detecting it. It is NOT CRYING WOLF: a warning
// that is often wrong is one nobody reads, and the surrounding code
// then loses the benefit of the warnings that are right.

func textOnly(id string) modelsdev.Model {
	return modelsdev.Model{ID: id, Modalities: modelsdev.Modalities{Input: []string{"text"}}}
}

func multimodal(id string) modelsdev.Model {
	return modelsdev.Model{
		ID:         id,
		ToolCall:   true,
		Modalities: modelsdev.Modalities{Input: []string{"text", "image", "pdf"}},
	}
}

func TestADeclaredCapabilityTheModelLacksIsReported(t *testing.T) {
	t.Parallel()
	got := UnsupportedCapabilities(
		[]string{CapabilityChat, CapabilityVision},
		[]modelsdev.Model{textOnly("some/text-model")})
	if len(got) != 1 || got[0] != CapabilityVision {
		t.Errorf("got %v, want [%s]", got, CapabilityVision)
	}
}

func TestACapabilityTheModelHasIsNotReported(t *testing.T) {
	t.Parallel()
	got := UnsupportedCapabilities(
		[]string{CapabilityChat, CapabilityVision, CapabilityPDF, CapabilityFunctionCalling},
		[]modelsdev.Model{multimodal("some/omni")})
	if len(got) != 0 {
		t.Errorf("got %v, want nothing", got)
	}
}

// --- not crying wolf ---------------------------------------------------

// The catalogue has NO signal for speech, image, video, embeddings or
// transcription. Warning about those would tell an operator their
// text-to-speech provider cannot speak.
func TestCapabilitiesTheCatalogueCannotSpeakToAreNeverReported(t *testing.T) {
	t.Parallel()
	declared := []string{
		CapabilitySpeak, CapabilityImage, CapabilityVideo,
		CapabilityEmbeddings, CapabilityAudioTranscribe,
	}
	got := UnsupportedCapabilities(declared, []modelsdev.Model{textOnly("some/text-model")})
	if len(got) != 0 {
		t.Errorf("got %v; the catalogue knows nothing about these and must stay quiet", got)
	}
}

// An unknown model is not evidence of anything. Treating "I have never
// heard of this" as "this cannot do that" would warn loudest about the
// self-hosted deployments least likely to appear in a public
// catalogue.
func TestAnUnknownModelProducesNoWarning(t *testing.T) {
	t.Parallel()
	got := UnsupportedCapabilities([]string{CapabilityVision}, nil)
	if len(got) != 0 {
		t.Errorf("got %v for a model the catalogue does not list", got)
	}
}

// THE UNION, not the intersection. Two listings of the same model name
// can disagree, and one of them claiming a capability is enough to say
// nothing is obviously wrong — the intersection would fire whenever
// any two entries differed.
func TestOneListingClaimingItIsEnough(t *testing.T) {
	t.Parallel()
	got := UnsupportedCapabilities(
		[]string{CapabilityVision},
		[]modelsdev.Model{textOnly("vendor-a/m"), multimodal("vendor-b/m")})
	if len(got) != 0 {
		t.Errorf("got %v; one listing claims vision, which is enough not to warn", got)
	}
}

// And when NO listing claims it, the disagreement is not the reason —
// every entry agrees it is absent.
func TestNoListingClaimingItIsReported(t *testing.T) {
	t.Parallel()
	got := UnsupportedCapabilities(
		[]string{CapabilityVision},
		[]modelsdev.Model{textOnly("vendor-a/m"), textOnly("vendor-b/m")})
	if len(got) != 1 {
		t.Errorf("got %v; no listing claims vision", got)
	}
}

func TestNothingDeclaredIsNothingToReport(t *testing.T) {
	t.Parallel()
	if got := UnsupportedCapabilities(nil, []modelsdev.Model{textOnly("m")}); len(got) != 0 {
		t.Errorf("got %v", got)
	}
}

// The result is sorted, so two boots of the same config produce the
// same warning rather than one that appears to change.
func TestTheReportIsStable(t *testing.T) {
	t.Parallel()
	declared := []string{CapabilityVision, CapabilityPDF, CapabilityChat}
	first := UnsupportedCapabilities(declared, []modelsdev.Model{textOnly("m")})
	for i := range 20 {
		got := UnsupportedCapabilities(declared, []modelsdev.Model{textOnly("m")})
		if len(got) != len(first) {
			t.Fatalf("run %d: %v vs %v", i, got, first)
		}
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("run %d: %v vs %v", i, got, first)
			}
		}
	}
	if len(first) != 2 || first[0] != CapabilityPDF || first[1] != CapabilityVision {
		t.Errorf("got %v, want [pdf vision] sorted", first)
	}
}
