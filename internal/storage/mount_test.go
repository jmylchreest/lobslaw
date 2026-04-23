package storage

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// fakeMount is a test double for the Mount interface.
type fakeMount struct {
	label    string
	backend  string
	path     string
	healthy  atomic.Bool
	startErr error
	stopErr  error
	starts   atomic.Int32
	stops    atomic.Int32
}

func (f *fakeMount) Label() string   { return f.label }
func (f *fakeMount) Backend() string { return f.backend }
func (f *fakeMount) Path() string    { return f.path }
func (f *fakeMount) Healthy() bool   { return f.healthy.Load() }
func (f *fakeMount) Start(_ context.Context) error {
	f.starts.Add(1)
	if f.startErr != nil {
		return f.startErr
	}
	f.healthy.Store(true)
	return nil
}
func (f *fakeMount) Stop(_ context.Context) error {
	f.stops.Add(1)
	f.healthy.Store(false)
	return f.stopErr
}

func TestManagerRegisterStartsAndResolves(t *testing.T) {
	t.Parallel()
	m := NewManager()
	mt := &fakeMount{label: "shared", backend: "local", path: "/srv/shared"}
	if err := m.Register(context.Background(), mt); err != nil {
		t.Fatal(err)
	}
	got, err := m.Resolve("shared")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/srv/shared" {
		t.Errorf("path: %q", got)
	}
	if mt.starts.Load() != 1 {
		t.Errorf("start should have run exactly once; ran %d", mt.starts.Load())
	}
}

func TestManagerResolveUnknownReturnsErrNotFound(t *testing.T) {
	t.Parallel()
	m := NewManager()
	_, err := m.Resolve("missing")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("want ErrNotFound; got %v", err)
	}
}

// TestManagerRegisterReplaceStopsOldThenStartsNew — re-registering
// the same label cleanly retires the previous mount so subscribers
// don't see two backings for one label.
func TestManagerRegisterReplaceStopsOldThenStartsNew(t *testing.T) {
	t.Parallel()
	m := NewManager()

	old := &fakeMount{label: "x", backend: "local", path: "/a"}
	newer := &fakeMount{label: "x", backend: "local", path: "/b"}

	if err := m.Register(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := m.Register(context.Background(), newer); err != nil {
		t.Fatal(err)
	}
	if old.stops.Load() != 1 {
		t.Errorf("old mount should be Stop'd on replace; stops=%d", old.stops.Load())
	}
	got, _ := m.Resolve("x")
	if got != "/b" {
		t.Errorf("resolve should return replacement path; got %q", got)
	}
}

// TestManagerRegisterRollsBackOnStartFailure — a mount whose Start
// errors must NOT remain registered. Resolve against the failed
// label returns ErrNotFound so callers don't get a broken path.
func TestManagerRegisterRollsBackOnStartFailure(t *testing.T) {
	t.Parallel()
	m := NewManager()
	mt := &fakeMount{label: "bad", startErr: errors.New("boom")}

	err := m.Register(context.Background(), mt)
	if err == nil {
		t.Fatal("Start error should propagate through Register")
	}
	if _, err := m.Resolve("bad"); !errors.Is(err, ErrNotFound) {
		t.Errorf("failed Start should leave label unregistered; got %v", err)
	}
}

func TestManagerUnregisterRemovesAndStops(t *testing.T) {
	t.Parallel()
	m := NewManager()
	mt := &fakeMount{label: "x", path: "/p"}
	_ = m.Register(context.Background(), mt)

	if err := m.Unregister(context.Background(), "x"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Resolve("x"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Unregister should remove label; got %v", err)
	}
	if mt.stops.Load() != 1 {
		t.Errorf("stops=%d; want 1", mt.stops.Load())
	}
}

// TestManagerUnregisterUnknownIsNoOp — idempotent so Raft-observed
// deletes can be re-delivered safely.
func TestManagerUnregisterUnknownIsNoOp(t *testing.T) {
	t.Parallel()
	m := NewManager()
	if err := m.Unregister(context.Background(), "not-there"); err != nil {
		t.Errorf("unregister unknown should be nil; got %v", err)
	}
}

func TestManagerListSortedByLabel(t *testing.T) {
	t.Parallel()
	m := NewManager()
	_ = m.Register(context.Background(), &fakeMount{label: "charlie", path: "/c"})
	_ = m.Register(context.Background(), &fakeMount{label: "alpha", path: "/a"})
	_ = m.Register(context.Background(), &fakeMount{label: "bravo", path: "/b"})

	list := m.List()
	if len(list) != 3 {
		t.Fatalf("len=%d", len(list))
	}
	want := []string{"alpha", "bravo", "charlie"}
	for i, w := range want {
		if list[i].Label != w {
			t.Errorf("order[%d]: %q want %q", i, list[i].Label, w)
		}
	}
}

func TestManagerStopAllRetiresEverything(t *testing.T) {
	t.Parallel()
	m := NewManager()
	mt1 := &fakeMount{label: "a", path: "/a"}
	mt2 := &fakeMount{label: "b", path: "/b"}
	_ = m.Register(context.Background(), mt1)
	_ = m.Register(context.Background(), mt2)

	errs := m.StopAll(context.Background())
	if len(errs) != 0 {
		t.Errorf("clean StopAll should return no errors; got %v", errs)
	}
	if mt1.stops.Load() != 1 || mt2.stops.Load() != 1 {
		t.Errorf("each mount should Stop once; a=%d b=%d", mt1.stops.Load(), mt2.stops.Load())
	}
	if len(m.List()) != 0 {
		t.Errorf("StopAll should clear the map; still have %d mounts", len(m.List()))
	}
}

// TestManagerRegisterRejectsNilAndEmpty — belt + braces checks
// for the two programming errors that would otherwise crash or
// produce a silently unresolvable label.
func TestManagerRegisterRejectsNilAndEmpty(t *testing.T) {
	t.Parallel()
	m := NewManager()
	if err := m.Register(context.Background(), nil); err == nil {
		t.Error("nil mount should error")
	}
	if err := m.Register(context.Background(), &fakeMount{}); err == nil {
		t.Error("empty-label mount should error")
	}
}
