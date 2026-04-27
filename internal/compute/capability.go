package compute

import (
	"sort"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

// Capability tokens consumed by the modality builtins. Free-form
// strings on the wire (operators can declare anything in their
// config) but these are the ones lobslaw itself looks for.
const (
	CapabilityChat            = "chat"
	CapabilityFunctionCalling = "function-calling"
	CapabilityVision          = "vision"
	CapabilityAudioTranscribe = "audio-transcription"
	CapabilityAudioMultimodal = "audio-multimodal"
	CapabilityPDF             = "pdf"
	CapabilityEmbeddings      = "embeddings"
)

// SelectByCapability returns providers carrying any of the given
// capabilities, sorted by Priority (highest first) with declaration
// order as the tiebreak. Empty caps list is a programmer error
// (returns nil) — callers always pass at least one capability.
//
// One match per capability list is enough for the current "pick
// the best registered provider" wiring; the returned slice is
// kept ordered for the future fallback-chain layer that will try
// each in turn on transient failures.
func SelectByCapability(providers []config.ProviderConfig, anyOf ...string) []config.ProviderConfig {
	if len(anyOf) == 0 {
		return nil
	}
	want := make(map[string]struct{}, len(anyOf))
	for _, c := range anyOf {
		want[c] = struct{}{}
	}

	type indexed struct {
		p   config.ProviderConfig
		idx int
	}
	var hits []indexed
	for i, p := range providers {
		for _, c := range p.Capabilities {
			if _, ok := want[c]; ok {
				hits = append(hits, indexed{p: p, idx: i})
				break
			}
		}
	}
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].p.Priority != hits[j].p.Priority {
			return hits[i].p.Priority > hits[j].p.Priority
		}
		return hits[i].idx < hits[j].idx
	})
	out := make([]config.ProviderConfig, len(hits))
	for i, h := range hits {
		out[i] = h.p
	}
	return out
}
