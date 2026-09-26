package memory

import (
	"context"
	"errors"
	"testing"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestDeletedBotIdentityCannotExposeRetainedInbox(t *testing.T) {
	t.Parallel()
	svc := newTestBots(t)
	ctx := context.Background()
	bot, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: "worker", Owner: "user:alice", Enabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	inbox := NewInboxService(svc.raft, svc.store, 0)
	item := post(t, inbox, "worker", "Alice's private context", 0)
	if err := svc.Delete(ctx, bot.Id); err != nil {
		t.Fatal(err)
	}
	// Rebuild the service: retirement must not be an in-process denylist.
	svc = NewBotService(svc.raft, svc.store)
	for _, owner := range []string{"user:bob", "user:alice"} {
		if _, err := svc.Put(ctx, &lobslawv1.BotRecord{Id: bot.Id, Owner: owner}, 0); !errors.Is(err, ErrClaimConflict) {
			t.Fatalf("identity reused by %s: %v", owner, err)
		}
	}
	if _, err := svc.Put(ctx, bot, bot.Revision); !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale update revived bot: %v", err)
	}
	if _, err := svc.Get(ctx, bot.Id); !errors.Is(err, ErrBotNotFound) {
		t.Fatalf("retired bot selectable: %v", err)
	}
	list, err := svc.List(ctx)
	if err != nil || len(list) != 0 {
		t.Fatalf("retired bot in roster: %v %v", list, err)
	}
	if _, err := inbox.Get(ctx, "worker", item.Id); err != nil {
		t.Fatalf("history was destroyed: %v", err)
	}
	raw, err := svc.store.Get(BucketBots, bot.Id)
	if err != nil {
		t.Fatal(err)
	}
	var retired lobslawv1.BotRecord
	if err := proto.Unmarshal(raw, &retired); err != nil {
		t.Fatal(err)
	}
	if !retired.Deleted || retired.Enabled || retired.Owner != "user:alice" {
		t.Fatalf("invalid tombstone: %v", &retired)
	}
}

func TestConcurrentInboxRetriesShareCapacity(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 1)
	ctx := context.Background()
	var items []*lobslawv1.BotInboxItem
	for range 2 {
		item := post(t, svc, "worker", "retry me", 0)
		if _, err := svc.Cancel(ctx, "worker", item.Id); err != nil {
			t.Fatal(err)
		}
		items = append(items, item)
	}
	start := make(chan struct{})
	results := make(chan error, len(items))
	for _, item := range items {
		go func() {
			<-start
			_, err := svc.Retry(ctx, "worker", item.Id)
			results <- err
		}()
	}
	close(start)
	succeeded, full := 0, 0
	for range items {
		err := <-results
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, ErrInboxFull):
			full++
		default:
			t.Fatal(err)
		}
	}
	if succeeded != 1 || full != 1 {
		t.Fatalf("success=%d full=%d", succeeded, full)
	}
}

func TestInboxPruneCannotDeleteRetriedRevision(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 1)
	ctx := context.Background()
	item := post(t, svc, "worker", "retry me", 0)
	stale, err := svc.Cancel(ctx, "worker", item.Id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Retry(ctx, "worker", item.Id); err != nil {
		t.Fatal(err)
	}
	// Deterministic interleaving: prune scanned the cancelled version, then
	// retry committed before the prune proposal reached the FSM.
	revision := stale.Revision
	err = svc.apply(ctx, lobslawv1.LogOp_LOG_OP_DELETE, stale, &revision, "")
	if !errors.Is(err, ErrClaimConflict) {
		t.Fatalf("stale prune accepted: %v", err)
	}
	current, err := svc.Get(ctx, "worker", item.Id)
	if err != nil || current.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_PENDING {
		t.Fatalf("retry lost: %v %v", current, err)
	}
}

func TestInboxAdmissionChecksCapacityAtApply(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 1)
	post(t, svc, "worker", "fills last slot", 0)
	// A post which passed its local preflight before another writer filled
	// the queue must be refused by the serialized state-machine boundary.
	err := svc.apply(context.Background(), lobslawv1.LogOp_LOG_OP_PUT, &lobslawv1.BotInboxItem{
		Id: "late", Recipient: "worker", Body: "late post", Status: lobslawv1.InboxStatus_INBOX_STATUS_PENDING,
	}, nil, "")
	if !errors.Is(err, ErrInboxFull) {
		t.Fatalf("late post exceeded capacity: %v", err)
	}
}
