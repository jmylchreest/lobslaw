package dataformat

import (
	"fmt"
	"strings"
)

// AutomaticTargets is a reviewed transition catalogue, not a range of readable
// formats. Add a transition only with migration, mixed-binary and runtime tests.
func AutomaticTargets() []uint32 { return []uint32{2} }

const automaticPrefix = "auto-contract-v1-"

func IsAutomaticID(id string) bool { return strings.HasPrefix(id, automaticPrefix) }

// AutomaticStep uses the existing durable transition tuple for provenance and
// recovery. Aborts hold the target until an operator completes a manual upgrade;
// the controller never takes over manual preparation or silently clears a hold.
type AutomaticStep struct {
	Action string
	ID     string
	Target uint32
	Epoch  uint64
}

func (s ContractState) AutomaticStep() (AutomaticStep, string) {
	if s.Active != 1 {
		return AutomaticStep{}, "no reviewed automatic transition from the active contract"
	}
	target := AutomaticTargets()[0]
	id := fmt.Sprintf("%s%d-%d-%d", automaticPrefix, s.Active, target, s.Epoch)
	if s.Prepared != nil {
		if s.Prepared.ID != id || s.Prepared.Target != target || s.Prepared.Epoch != s.Epoch {
			return AutomaticStep{}, "operator-owned preparation requires explicit completion or abort"
		}
		return AutomaticStep{Action: "finalize", ID: id, Target: target, Epoch: s.Epoch}, ""
	}
	if s.CompletedAction == "abort" && (s.Completed == nil || s.Completed.Target == target) {
		return AutomaticStep{}, "operator abort holds automatic activation; complete a manual prepare/finalize to resume"
	}
	return AutomaticStep{Action: "prepare", ID: id, Target: target, Epoch: s.Epoch}, ""
}
