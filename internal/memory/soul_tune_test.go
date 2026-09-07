package memory

import (
	"context"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestSoulTuneDurableCASAndInitialRollback(t *testing.T) {
	r, f := newTestRaft(t)
	svc := NewSoulTuneService(r, f.Store())
	ctx := context.Background()
	name := "changed"
	first, err := svc.Put(ctx, &lobslawv1.SoulTuneState{Name: &name}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if first.Revision != 1 {
		t.Fatalf("revision = %d", first.Revision)
	}
	if _, err := svc.Put(ctx, &lobslawv1.SoulTuneState{}, 0); err == nil {
		t.Fatal("stale writer overwrote current soul")
	}
	// Reconstructing the service must read the persisted record and history.
	svc = NewSoulTuneService(r, f.Store())
	rec, err := svc.Get(ctx)
	if err != nil || rec.GetCurrent().GetName() != name {
		t.Fatalf("stored soul lost: %v, %v", rec, err)
	}
	restored, err := svc.Rollback(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if restored.Current.Name != nil || restored.Revision != 2 {
		t.Fatalf("first edit not undone: %v", restored)
	}
}
