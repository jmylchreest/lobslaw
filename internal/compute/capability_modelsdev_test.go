package compute

import (
	"reflect"
	"sort"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/modelsdev"
)

func TestCapabilitiesFromModelMapsCorrectly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   modelsdev.Model
		want []string
	}{
		{
			name: "text + tool_call",
			in: modelsdev.Model{
				ToolCall:   true,
				Modalities: modelsdev.Modalities{Input: []string{"text"}},
			},
			want: []string{"chat", "function-calling"},
		},
		{
			name: "vision",
			in: modelsdev.Model{
				ToolCall:   true,
				Modalities: modelsdev.Modalities{Input: []string{"text", "image"}},
			},
			want: []string{"chat", "function-calling", "vision"},
		},
		{
			name: "multimodal everything",
			in: modelsdev.Model{
				ToolCall:   true,
				Modalities: modelsdev.Modalities{Input: []string{"text", "image", "audio", "pdf"}},
			},
			want: []string{"audio-multimodal", "chat", "function-calling", "pdf", "vision"},
		},
		{
			name: "text-only no tools",
			in: modelsdev.Model{
				ToolCall:   false,
				Modalities: modelsdev.Modalities{Input: []string{"text"}},
			},
			want: []string{"chat"},
		},
		{
			name: "no modalities",
			in:   modelsdev.Model{},
			want: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := CapabilitiesFromModel(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCapabilitiesFromConsensusIntersects(t *testing.T) {
	t.Parallel()
	// Two providers list the same model. One mistakenly claims
	// vision; the other (correct) doesn't. Consensus drops vision.
	models := []modelsdev.Model{
		{ToolCall: true, Modalities: modelsdev.Modalities{Input: []string{"text"}}},
		{ToolCall: true, Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}},
	}
	got := CapabilitiesFromConsensus(models)
	sort.Strings(got)
	want := []string{"chat", "function-calling"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("consensus = %v, want %v (vision should be dropped)", got, want)
	}
}

func TestCapabilitiesFromConsensusSingleEntry(t *testing.T) {
	t.Parallel()
	got := CapabilitiesFromConsensus([]modelsdev.Model{
		{ToolCall: true, Modalities: modelsdev.Modalities{Input: []string{"text", "image"}}},
	})
	want := []string{"chat", "function-calling", "vision"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("single-entry got %v, want %v", got, want)
	}
}

func TestMergeCapabilitiesDedupesAndSorts(t *testing.T) {
	t.Parallel()
	got := MergeCapabilities(
		[]string{"chat", "function-calling", "custom-tag"},
		[]string{"vision", "chat"},
	)
	want := []string{"chat", "custom-tag", "function-calling", "vision"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("merge got %v, want %v", got, want)
	}
}

func TestMergeCapabilitiesDeclaredAreKept(t *testing.T) {
	t.Parallel()
	// Operator declared "audio-transcription" — discovery doesn't
	// emit that token, but it must survive the merge.
	got := MergeCapabilities(
		[]string{"audio-transcription"},
		[]string{"chat", "function-calling"},
	)
	want := []string{"audio-transcription", "chat", "function-calling"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("declared dropped: got %v, want %v", got, want)
	}
}
