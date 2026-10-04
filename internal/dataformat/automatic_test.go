package dataformat

import (
	"encoding/json"
	"testing"
)

func TestAutomaticUpgradeSelectionAndRecovery(t *testing.T) {
	state := ContractState{Active: 1}
	step, reason := state.AutomaticStep()
	if reason != "" || step.Action != "prepare" || step.Target != 2 || step.ID == "" {
		t.Fatalf("%+v %s", step, reason)
	}
	tr := Transition{ID: step.ID, Target: step.Target, Epoch: step.Epoch, MembershipIndex: 1, Index: 2, Members: []string{"a"}}
	prepared, err := state.Advance("prepare", tr, SupportedContracts())
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &prepared); err != nil {
		t.Fatal(err)
	}
	next, reason := prepared.AutomaticStep()
	if reason != "" || next.Action != "finalize" || next.ID != step.ID {
		t.Fatalf("%+v %s", next, reason)
	}
	aborted, err := prepared.Advance("abort", tr, SupportedContracts())
	if err != nil {
		t.Fatal(err)
	}
	raw, err = json.Marshal(aborted)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &aborted); err != nil {
		t.Fatal(err)
	}
	if step, reason := aborted.AutomaticStep(); step.Action != "" || reason == "" {
		t.Fatal("abort immediately re-prepared", step, reason)
	}
	manual := tr
	manual.ID, manual.Epoch = "operator-recovery", aborted.Epoch
	resumed, err := aborted.Advance("prepare", manual, SupportedContracts())
	if err != nil {
		t.Fatal(err)
	}
	if step, reason := resumed.AutomaticStep(); step.Action != "" || reason == "" {
		t.Fatal("manual transition taken over", step, reason)
	}
	done, err := resumed.Advance("finalize", manual, SupportedContracts())
	if err != nil {
		t.Fatal(err)
	}
	if step, _ := done.AutomaticStep(); step.Action != "" {
		t.Fatal("replayed completed transition", step)
	}
	for _, active := range []uint32{0, 2, 3, 99} {
		if step, _ := (ContractState{Active: active}).AutomaticStep(); step.Action != "" {
			t.Fatal("inferred an unreviewed transition", step)
		}
	}
}
