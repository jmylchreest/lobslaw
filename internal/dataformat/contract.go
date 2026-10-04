package dataformat

import (
	"errors"
	"fmt"
	"slices"
)

// ControlProtocol remains stable while explicitly supported data contracts evolve.
const ControlProtocol = "lobslaw-rolling-v1"

const MaxTransitionIDBytes = 128

// SupportedContracts is an explicit compatibility set, not an inferred range.
func SupportedContracts() []uint32 { return []uint32{1, 2} }

type Transition struct {
	ID                    string   `json:"id"`
	Target                uint32   `json:"target"`
	Epoch                 uint64   `json:"epoch"`
	MembershipIndex       uint64   `json:"membership_index"`
	MembershipFingerprint string   `json:"membership_fingerprint"`
	Members               []string `json:"members"`
	Index                 uint64   `json:"index"`
}

type ContractState struct {
	Active          uint32      `json:"active"`
	Epoch           uint64      `json:"epoch"`
	Prepared        *Transition `json:"prepared,omitempty"`
	CompletedID     string      `json:"completed_id,omitempty"`
	CompletedAction string      `json:"completed_action,omitempty"`
	Completed       *Transition `json:"completed,omitempty"`
}

// CompletedMatches checks the caller-controlled part of a completed request.
// Historical records without a complete tuple remain readable, but cannot
// establish that a retry is identical and therefore fail closed.
func (s ContractState) CompletedMatches(action, id string, target uint32, epoch uint64) bool {
	return s.Completed != nil && s.CompletedID == id && s.CompletedAction == action && s.Epoch == epoch+1 && s.Completed.ID == id && s.Completed.Target == target && s.Completed.Epoch == epoch
}

func (s ContractState) Required() uint32 {
	if s.Prepared != nil {
		return s.Prepared.Target
	}
	return s.Active
}

func (s ContractState) Validate(supported []uint32) error {
	if !slices.Contains(supported, s.Active) || !slices.Contains(supported, s.Required()) {
		return fmt.Errorf("binary cannot read active/prepared contract %d/%d", s.Active, s.Required())
	}
	if s.Prepared != nil {
		p := s.Prepared
		if p.ID == "" || p.Target <= s.Active || p.Epoch != s.Epoch || p.Index == 0 || p.MembershipIndex == 0 || len(p.Members) == 0 {
			return errors.New("invalid persisted upgrade transition")
		}
	}
	return nil
}

// Advance is deterministic. The adapter validates membership and readiness before
// proposing; all replicas apply the same durable transition at the same index.
func (s ContractState) Advance(action string, t Transition, supported []uint32) (ContractState, error) {
	if t.ID == "" || len(t.ID) > MaxTransitionIDBytes {
		return s, fmt.Errorf("transition id required (maximum %d bytes)", MaxTransitionIDBytes)
	}
	if s.CompletedID == t.ID {
		if s.CompletedMatches(action, t.ID, t.Target, t.Epoch) && s.Completed.matches(t) {
			return s, nil
		}
		return s, errors.New("transition id already completed; choose a new id")
	}
	if t.Epoch != s.Epoch {
		return s, errors.New("stale upgrade epoch")
	}
	switch action {
	case "prepare":
		if s.Prepared != nil {
			p := s.Prepared
			if p.matches(t) {
				return s, nil
			}
			return s, errors.New("another transition is prepared")
		}
		if t.Target <= s.Active || !slices.Contains(supported, t.Target) || t.Index == 0 || t.MembershipIndex == 0 || len(t.Members) == 0 {
			return s, errors.New("unsupported target or invalid preparation")
		}
		if !slices.IsSorted(t.Members) {
			return s, errors.New("members must be sorted")
		}
		for i, id := range t.Members {
			if id == "" || (i > 0 && id == t.Members[i-1]) {
				return s, errors.New("invalid member set")
			}
		}
		t.Members = slices.Clone(t.Members)
		s.Prepared = &t
	case "finalize", "abort":
		p := s.Prepared
		if p == nil || !p.matches(t) {
			return s, errors.New("prepared transition does not match")
		}
		if action == "finalize" {
			s.Active = p.Target
		}
		s.Epoch++
		s.Prepared = nil
		s.CompletedID = t.ID
		s.CompletedAction = action
		completed := *p
		completed.Members = slices.Clone(p.Members)
		s.Completed = &completed
	default:
		return s, errors.New("expected prepare, finalize or abort")
	}
	return s, s.Validate(supported)
}

func (t Transition) matches(other Transition) bool {
	return t.ID == other.ID && t.Target == other.Target && t.Epoch == other.Epoch && t.MembershipIndex == other.MembershipIndex && t.MembershipFingerprint == other.MembershipFingerprint && slices.Equal(t.Members, other.Members)
}
