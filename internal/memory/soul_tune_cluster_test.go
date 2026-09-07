package memory_test

import (
	"context"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestSoulTuneFollowerReplicationAndConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("multi-node raft integration")
	}
	c := newForwardingCluster(t)
	follower := c.follower(t)
	leader := c.leader(t)
	f := memory.NewSoulTuneService(follower.raft, follower.store)
	l := memory.NewSoulTuneService(leader.raft, leader.store)
	ctx := context.Background()
	name := "first"
	if _, err := f.Put(ctx, &lobslawv1.SoulTuneState{Name: &name}, 0); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 5*time.Second, func() bool {
		r, err := f.Get(ctx)
		return err == nil && r.GetRevision() == 1
	}, "soul did not replicate back to follower")
	results := make(chan error, 2)
	for i, svc := range []*memory.SoulTuneService{f, l} {
		go func() {
			next := []string{"follower edit", "leader edit"}[i]
			_, err := svc.Put(ctx, &lobslawv1.SoulTuneState{Name: &next}, 1)
			results <- err
		}()
	}
	successes := 0
	for range 2 {
		if <-results == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("%d conflicting writes reported success; want exactly one", successes)
	}
	waitFor(t, 5*time.Second, func() bool {
		r, err := f.Get(ctx)
		return err == nil && r.GetRevision() == 2
	}, "winning soul edit did not replicate")
	r, err := l.Get(ctx)
	if err != nil || len(r.History) != 2 {
		t.Fatalf("history lost during conflict: %v, %v", r, err)
	}
}
