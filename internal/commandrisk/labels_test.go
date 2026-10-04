package commandrisk

import "testing"

func TestMergeLabelsPreservesIndependentEffects(t *testing.T) {
	t.Parallel()
	for _, other := range AllRiskLabels {
		t.Run(string(other), func(t *testing.T) {
			t.Parallel()
			got := MergeLabels(L(LabelReads), L(other, LabelReads))
			if !HasLabel(got, LabelReads) || !HasLabel(got, other) {
				t.Fatalf("merge lost an effect: %v", got)
			}
			wantLen := 2
			if other == LabelReads {
				wantLen = 1
			}
			if len(got) != wantLen {
				t.Fatalf("merge failed to deduplicate: %v", got)
			}
		})
	}
}

func TestIndependentReadsRemainInApprovalSet(t *testing.T) {
	t.Parallel()
	for _, command := range []string{"cat /etc/hosts; touch /tmp/probe", "touch /tmp/probe; cat /etc/hosts"} {
		v := ClassifyRisk(command)
		if !sameLabels(v.Labels, L(LabelWrites, LabelReads)) {
			t.Fatalf("%q: %v", command, v.Labels)
		}
		if v.Approved(map[RiskLabel]bool{LabelWrites: true}) {
			t.Fatalf("write-only approval authorized independent read: %q", command)
		}
		if !v.Approved(map[RiskLabel]bool{LabelWrites: true, LabelReads: true}) {
			t.Fatalf("all effects approved but refused: %q", command)
		}
	}
}

func TestReadEffectsSurviveNetworkAndPrivilege(t *testing.T) {
	t.Parallel()
	table := map[string]CommandRiskRule{
		"calendar": {Labels: L(LabelReads, LabelNetwork)},
	}
	segments, ok := splitRiskSegments("calendar")
	if !ok || len(segments) != 1 {
		t.Fatal("could not parse test command")
	}
	classified := classifyRiskSegment(segments[0], table)
	v := RiskVerdict{Labels: classified.Labels}
	if !sameLabels(v.Labels, L(LabelNetwork, LabelReads)) {
		t.Fatalf("read-only network operation lost effect: %v", v.Labels)
	}
	if v.Approved(map[RiskLabel]bool{LabelNetwork: true}) {
		t.Fatal("network permission implied read permission")
	}
	v = ClassifyRisk("sudo ls")
	if !sameLabels(v.Labels, L(LabelPrivilege, LabelReads)) {
		t.Fatalf("privileged read lost effect: %v", v.Labels)
	}
}
