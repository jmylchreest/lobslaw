package memory

import (
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// newTestGroupService builds a service over the shared raft harness,
// the same way newTestBots does.
func newTestGroupService(t *testing.T) (*GroupService, func()) {
	t.Helper()
	raft, fsm := newTestRaft(t)
	return NewGroupService(raft, fsm.store), func() {}
}

// Ownership is set once and cannot be carried by an update.
//
// An update that could set Owner would be a way to take somebody's
// team by editing its name — the same class of problem as an update
// that could set is_default, and guarded the same way.
func TestOwnershipCannotBeTakenByAnUpdate(t *testing.T) {
	t.Parallel()

	svc, cleanup := newTestGroupService(t)
	defer cleanup()

	created, err := svc.Put(t.Context(), &lobslawv1.GroupRecord{
		Id: "core", Name: "Core", Owner: "user:james",
	}, 0)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if created.GetOwner() != "user:james" {
		t.Fatalf("owner = %q on create", created.GetOwner())
	}

	updated, err := svc.Put(t.Context(), &lobslawv1.GroupRecord{
		Id: "core", Name: "Core", Owner: "user:sam",
	}, created.GetRevision())
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.GetOwner() != "user:james" {
		t.Errorf("owner = %q after an update claiming it; a rename took the team",
			updated.GetOwner())
	}
}
