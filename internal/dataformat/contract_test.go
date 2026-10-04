package dataformat

import (
	"encoding/json"
	"testing"
)

func TestContractActivationAndRestartFence(t *testing.T) {
	supported := []uint32{1, 2}
	initial := ContractState{Active: 1}
	transition := Transition{ID: "rollout", Epoch: 0, Target: 2, MembershipIndex: 3, Members: []string{"a", "b", "c"}, Index: 8}
	prepared, err := initial.Advance("prepare", transition, supported)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Active != 1 || prepared.Required() != 2 {
		t.Fatal(prepared)
	}
	if err := prepared.Validate([]uint32{1}); err == nil {
		t.Fatal("old binary passed durable restart fence")
	}
	raw, _ := json.Marshal(prepared)
	var restored ContractState
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	if err := restored.Validate(supported); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Transition){func(x *Transition) { x.Epoch++ }, func(x *Transition) { x.ID = "other" }, func(x *Transition) { x.MembershipIndex++ }, func(x *Transition) { x.Members = []string{"a", "b"} }} {
		stale := transition
		mutate(&stale)
		if _, err := restored.Advance("finalize", stale, supported); err == nil {
			t.Fatal("accepted stale finalization", stale)
		}
	}
	next, err := restored.Advance("finalize", transition, supported)
	if err != nil {
		t.Fatal(err)
	}
	if next.Active != 2 || next.Epoch != 1 || next.Prepared != nil {
		t.Fatal(next)
	}
	if _, err := next.Advance("finalize", transition, supported); err != nil {
		t.Fatal("lost response retry", err)
	}
	if _, err := next.Advance("prepare", transition, supported); err == nil {
		t.Fatal("reused completed id")
	}
	if initial.Prepared != nil {
		t.Fatal("mutated caller")
	}
}

func TestContractAbortAndUnsupportedTarget(t *testing.T) {
	state := ContractState{Active: 1}
	tr := Transition{ID: "a", Target: 2, MembershipIndex: 1, Members: []string{"a"}, Index: 2}
	if _, err := state.Advance("prepare", tr, []uint32{1}); err == nil {
		t.Fatal("accepted unsupported target")
	}
	state, err := state.Advance("prepare", tr, []uint32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	state, err = state.Advance("abort", tr, []uint32{1, 2})
	if err != nil {
		t.Fatal(err)
	}
	if state.Active != 1 || state.Epoch != 1 || state.Prepared != nil {
		t.Fatal(state)
	}
	if _, err := state.Advance("finalize", tr, []uint32{1, 2}); err == nil {
		t.Fatal("finalized aborted transition")
	}
}

func TestCompletedTransitionRejectsChangedTuple(t *testing.T) {
	for _, action := range []string{"finalize", "abort"} {
		t.Run(action, func(t *testing.T) {
			supported := []uint32{1, 2, 3}
			tr := Transition{ID: "rollout", Target: 2, MembershipIndex: 1, MembershipFingerprint: "members", Members: []string{"a"}, Index: 2}
			state, err := (ContractState{Active: 1}).Advance("prepare", tr, supported)
			if err != nil {
				t.Fatal(err)
			}
			state, err = state.Advance(action, tr, supported)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(state)
			if err := json.Unmarshal(raw, &state); err != nil {
				t.Fatal(err)
			}
			for _, change := range []func(*Transition){
				func(t *Transition) { t.Target = 3 },
				func(t *Transition) { t.MembershipIndex++ },
				func(t *Transition) { t.MembershipFingerprint = "other" },
				func(t *Transition) { t.Members = []string{"b"} },
			} {
				other := tr
				change(&other)
				if _, err := state.Advance(action, other, supported); err == nil {
					t.Fatal("accepted changed completed tuple", other)
				}
			}
		})
	}
}
