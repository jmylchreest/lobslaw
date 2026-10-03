package memory

import (
	"context"
	"errors"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestIntegrationStateCASAndEncryption(t *testing.T) {
	t.Parallel()
	stack := newTestServiceStack(t)
	key, _ := crypto.GenerateKey()
	s := NewIntegrationStateStore(stack.raft, stack.store, key)
	r := &lobslawv1.IntegrationStateRecord{Id: "random-id", Owner: "alice", Kind: "oauth", Data: []byte("private-verifier"), ExpiresAt: timestamppb.New(time.Now().Add(time.Minute))}
	ctx := context.Background()
	if err := s.Save(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err := s.Save(ctx, r); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale save: %v", err)
	}
	raw, _ := stack.store.Get(BucketIntegrationState, r.Id)
	if containsBytes(raw, r.Data) {
		t.Fatal("state persisted plaintext")
	}
	got, err := s.Get(ctx, r.Id)
	if err != nil || string(got.Data) != string(r.Data) || got.Revision != 1 {
		t.Fatalf("get: %v %v", got, err)
	}
	got.Data = []byte("consumed")
	if err := s.Save(ctx, got); err != nil {
		t.Fatal(err)
	}
}
