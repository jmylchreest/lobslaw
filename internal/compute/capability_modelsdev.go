package compute

import (
	"sort"

	"github.com/jmylchreest/lobslaw/internal/modelsdev"
)

// CapabilitiesFromModel translates a models.dev Model record into
// lobslaw capability tokens. Conservative by design — emits a
// capability only when the model record clearly supports it. The
// caller merges the result with operator-declared capabilities
// (declared wins on conflict; this helper never returns capabilities
// that should "remove" something the operator declared).
//
// Mapping:
//
//	modalities.input contains "text" → "chat"
//	tool_call: true                  → "function-calling"
//	modalities.input contains "image"→ "vision"
//	modalities.input contains "audio"→ "audio-multimodal"
//	modalities.input contains "pdf"  → "pdf"
//
// STT (audio-transcription) is intentionally NOT inferred — it's a
// different paradigm from chat-multimodal audio and models.dev
// doesn't distinguish them. Operators declare audio-transcription
// explicitly on their Whisper / Parakeet / speaches entries.
func CapabilitiesFromModel(m modelsdev.Model) []string {
	caps := make(map[string]struct{})
	for _, in := range m.Modalities.Input {
		switch in {
		case "text":
			caps[CapabilityChat] = struct{}{}
		case "image":
			caps[CapabilityVision] = struct{}{}
		case "audio":
			caps[CapabilityAudioMultimodal] = struct{}{}
		case "pdf":
			caps[CapabilityPDF] = struct{}{}
		}
	}
	if m.ToolCall {
		caps[CapabilityFunctionCalling] = struct{}{}
	}
	out := make([]string, 0, len(caps))
	for c := range caps {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// CapabilitiesFromConsensus takes the INTERSECTION of capabilities
// across many catalog entries for the same model name. Used when
// the operator wants conservative discovery: only claim a
// capability if EVERY listing of that model agrees on it. Avoids
// catalog-bug-induced false positives (e.g. one provider mistakenly
// claiming a text-only model is multimodal).
//
// Empty input returns nil. Single-entry input returns
// CapabilitiesFromModel of that entry.
func CapabilitiesFromConsensus(models []modelsdev.Model) []string {
	if len(models) == 0 {
		return nil
	}
	if len(models) == 1 {
		return CapabilitiesFromModel(models[0])
	}
	common := capSet(CapabilitiesFromModel(models[0]))
	for _, m := range models[1:] {
		this := capSet(CapabilitiesFromModel(m))
		for c := range common {
			if _, ok := this[c]; !ok {
				delete(common, c)
			}
		}
	}
	out := make([]string, 0, len(common))
	for c := range common {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// MergeCapabilities is the union with declared-precedence policy.
// Declared capabilities are kept verbatim; discovered capabilities
// are added if not already present. Order is alphabetical.
func MergeCapabilities(declared, discovered []string) []string {
	set := make(map[string]struct{}, len(declared)+len(discovered))
	for _, c := range declared {
		set[c] = struct{}{}
	}
	for _, c := range discovered {
		set[c] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func capSet(in []string) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for _, c := range in {
		out[c] = struct{}{}
	}
	return out
}
