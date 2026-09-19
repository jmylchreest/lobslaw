package memory_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
)

func TestCredentialRefreshAcrossFollowers(t *testing.T) {
	if testing.Short() {
		t.Skip("cluster test")
	}
	c := newForwardingCluster(t)
	key, err := crypto.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var services []*memory.CredentialService
	for _, n := range c.nodes {
		svc, err := memory.NewCredentialService(n.raft, n.store, key)
		if err != nil {
			t.Fatal(err)
		}
		services = append(services, svc)
	}
	if err := services[0].Put(ctx, &memory.PlaintextCredential{Provider: "example", Subject: "user", AccessToken: "old", RefreshToken: "original", ExpiresAt: time.Now().Add(-time.Minute), Scopes: []string{"read"}, AllowedSkills: []string{"skill"}, AllowedScopesPerSkill: map[string][]string{"skill": {"read"}}}); err != nil {
		t.Fatal(err)
	}
	for _, svc := range services {
		waitFor(t, 5*time.Second, func() bool { _, err := svc.Get(ctx, "example", "user"); return err == nil }, "credential not replicated")
	}
	// Explicitly exercise rejected CAS over both follower forwarding paths.
	for _, n := range c.nodes {
		raw, err := n.store.Get(memory.BucketCredentials, "example:user")
		if err != nil {
			t.Fatal(err)
		}
		var rec lobslawv1.CredentialRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			t.Fatal(err)
		}
		wrongRevision := uint64(999)
		data, err := proto.Marshal(&lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: "example:user", ExpectedRevision: &wrongRevision, Payload: &lobslawv1.LogEntry_Credential{Credential: &rec}})
		if err != nil {
			t.Fatal(err)
		}
		response, err := n.raft.ApplyOrForward(ctx, data, time.Second)
		if err == nil {
			err, _ = response.(error)
		}
		if !errors.Is(err, memory.ErrClaimConflict) {
			t.Fatalf("claim conflict lost across forwarding: %v", err)
		}
	}
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	results := make(chan error, 3)
	var calls atomic.Int32
	refresh := func(ctx context.Context, _ string) (string, string, int, string, error) {
		calls.Add(1)
		entered <- struct{}{}
		select {
		case <-release:
		case <-ctx.Done():
			return "", "", 0, "", ctx.Err()
		}
		return "fresh", "rotated", 3600, "", nil
	}
	for _, svc := range services {
		go func() {
			issued, err := svc.IssueForSkill(ctx, "example", "user", "skill", refresh)
			if err == nil && issued.AccessToken != "fresh" {
				t.Error("returned stale token")
			}
			results <- err
		}()
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Keep the exchange open while the other replicas contend for ownership.
	select {
	case <-entered:
		t.Error("second node called provider")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	for range 3 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
	for _, svc := range services {
		waitFor(t, 5*time.Second, func() bool {
			p, err := svc.Get(ctx, "example", "user")
			return err == nil && p.RefreshToken == "rotated"
		}, "rotation not replicated")
	}
}
