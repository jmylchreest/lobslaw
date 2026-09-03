package commandrisk

import (
	"encoding/json"
	"os"
	"sort"
	"testing"
)

// The classification of 2482 commands, captured before this package
// existed and asserted unchanged after.
//
// The corpus is generated rather than written: every entry in the
// table, bare and with operands, with each of its subcommands and
// flags, plus verbs nobody catalogued to pin the fail-closed rule, plus
// the shapes the engine handles specially. That is the coverage a hand
// conversion of the table can silently corrupt — a transcription slip
// is not a compile error, it is a command that classifies as reads.
//
// Regenerate ONLY when a classification is deliberately changed, and
// review the diff as the substance of that change:
//
//	go run ./internal/commandrisk/internal/goldengen > internal/commandrisk/testdata/golden.json
type goldenEntry struct {
	Labels   []string `json:"labels"`
	Why      string   `json:"why"`
	Programs []string `json:"programs"`
	Headline string   `json:"headline"`
}

func TestGoldenCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/golden.json")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]goldenEntry
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatal(err)
	}
	if len(want) < 2000 {
		t.Fatalf("corpus has only %d commands; it is meant to cover the whole table", len(want))
	}

	cmds := make([]string, 0, len(want))
	for c := range want {
		cmds = append(cmds, c)
	}
	sort.Strings(cmds)

	var drift int
	for _, cmd := range cmds {
		v := ClassifyRisk(cmd)
		got := goldenEntry{Why: v.Why, Programs: v.Programs, Headline: RiskHeadline(v)}
		for _, l := range v.Labels {
			got.Labels = append(got.Labels, string(l))
		}
		w := want[cmd]
		if !eq(got.Labels, w.Labels) || got.Why != w.Why ||
			!eq(got.Programs, w.Programs) || got.Headline != w.Headline {
			drift++
			if drift <= 20 {
				t.Errorf("%q\n   labels %v -> %v\n   why    %q -> %q\n   head   %q -> %q",
					cmd, w.Labels, got.Labels, w.Why, got.Why, w.Headline, got.Headline)
			}
		}
	}
	if drift > 20 {
		t.Errorf("... and %d more", drift-20)
	}
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
