package memory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func newTestInbox(t *testing.T, maxPending int) *InboxService {
	t.Helper()
	raft, fsm := newTestRaft(t)
	return NewInboxService(raft, fsm.store, maxPending)
}

func post(t *testing.T, svc *InboxService, recipient, body string, priority int32) *lobslawv1.BotInboxItem {
	t.Helper()
	item, err := svc.Post(context.Background(), &lobslawv1.BotInboxItem{
		Recipient: recipient,
		Sender:    "bot:coordinator",
		Body:      body,
		Priority:  priority,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	return item
}

func TestInboxPostAndList(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()

	item := post(t, svc, "engineering", "deploy the staging branch", 0)
	if item.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_PENDING {
		t.Errorf("status = %v, want pending", item.GetStatus())
	}
	if item.GetSubject() != "deploy the staging branch" {
		t.Errorf("subject = %q, want it derived from the body", item.GetSubject())
	}

	items, err := svc.List(ctx, "engineering", InboxFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("listed %d items, want 1", len(items))
	}
}

// Claim fields are the service's. A caller that could pre-claim an
// item could park work nothing will ever run.
func TestInboxPostStripsCallerSuppliedClaimFields(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	item, err := svc.Post(context.Background(), &lobslawv1.BotInboxItem{
		Recipient: "engineering",
		Body:      "x",
		ClaimedBy: "attacker-node",
		Status:    lobslawv1.InboxStatus_INBOX_STATUS_DONE,
		Attempts:  99,
	})
	if err != nil {
		t.Fatalf("Post: %v", err)
	}
	if item.GetClaimedBy() != "" {
		t.Errorf("ClaimedBy = %q, want it stripped", item.GetClaimedBy())
	}
	if item.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_PENDING {
		t.Errorf("status = %v, want pending", item.GetStatus())
	}
	if item.GetAttempts() != 0 {
		t.Errorf("attempts = %d, want 0", item.GetAttempts())
	}
}

// The queue order is one rule — priority, then arrival — so "why is it
// doing that one next" has one answer for the bot and the GUI alike.
func TestInboxOrdersByPriorityThenArrival(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)

	first := post(t, svc, "engineering", "normal one", 0)
	urgent := post(t, svc, "engineering", "urgent", 10)
	second := post(t, svc, "engineering", "normal two", 0)

	items, err := svc.List(context.Background(), "engineering", InboxFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	want := []string{urgent.GetId(), first.GetId(), second.GetId()}
	for i, id := range want {
		if items[i].GetId() != id {
			t.Fatalf("position %d = %q, want %q (order: %v)", i, items[i].GetId(), id, want)
		}
	}
}

// Exactly-once. Two schedulers wake on the same item; one claims it,
// the other gets nothing, and the work runs once.
func TestInboxClaimIsExactlyOnce(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	post(t, svc, "engineering", "the only task", 0)

	var (
		mu      sync.Mutex
		winners []string
		wg      sync.WaitGroup
	)
	for i := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			item, err := svc.Claim(context.Background(), "engineering", fmt.Sprintf("node-%d", i))
			if err != nil {
				return
			}
			if item != nil {
				mu.Lock()
				winners = append(winners, item.GetClaimedBy())
				mu.Unlock()
			}
		}()
	}
	wg.Wait()

	if len(winners) != 1 {
		t.Errorf("%d nodes claimed the same item (%v); exactly one should win", len(winners), winners)
	}
}

func TestInboxClaimOnEmptyQueueIsNotAnError(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	item, err := svc.Claim(context.Background(), "engineering", "node-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if item != nil {
		t.Errorf("claimed %v from an empty queue", item.GetId())
	}
}

// A crashed node's work has to come back. The expiry check reads a
// clock, which is why it lives here and not in the FSM.
func TestExpiredClaimIsReclaimed(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "engineering", "abandoned work", 0)

	claimed, err := svc.Claim(ctx, "engineering", "node-that-dies")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if claimed == nil {
		t.Fatal("nothing claimed")
	}
	// Expire the claim the way a dead node's would.
	stale := claimed
	stale.ClaimExpiresAt = timestamppbPast()
	expected := stale.GetRevision()
	stale.Revision++
	if err := svc.apply(ctx, lobslawv1.LogOp_LOG_OP_CLAIM, stale, &expected, "node-that-dies"); err != nil {
		t.Fatalf("expire claim: %v", err)
	}

	retaken, err := svc.Claim(ctx, "engineering", "node-that-lives")
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if retaken == nil {
		t.Fatal("a dead node's work was never picked up")
	}
	if retaken.GetId() != item.GetId() {
		t.Errorf("reclaimed %q, want %q", retaken.GetId(), item.GetId())
	}
	if retaken.GetClaimedBy() != "node-that-lives" {
		t.Errorf("ClaimedBy = %q", retaken.GetClaimedBy())
	}
}

// Nothing evaporates. A task that fails every attempt ends FAILED and
// VISIBLE with the reason on it, not deleted and not silently pending
// forever.
func TestFailingItemEndsVisiblyFailed(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "engineering", "impossible", 0)

	const maxAttempts = 3
	for attempt := 1; attempt <= maxAttempts+1; attempt++ {
		claimed, err := svc.Claim(ctx, "engineering", "node-1")
		if err != nil {
			t.Fatalf("Claim %d: %v", attempt, err)
		}
		if claimed == nil {
			break
		}
		if _, err := svc.Resolve(ctx, "engineering", item.GetId(), InboxOutcome{
			ClaimRevision: claimed.Revision, Claimer: claimed.ClaimedBy,
			Err:         errors.New("the cluster is on fire"),
			MaxAttempts: maxAttempts,
		}); err != nil {
			t.Fatalf("Resolve %d: %v", attempt, err)
		}
	}

	final, err := svc.Get(ctx, "engineering", item.GetId())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if final.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_FAILED {
		t.Errorf("status = %v, want failed", final.GetStatus())
	}
	if !strings.Contains(final.GetError(), "on fire") {
		t.Errorf("error = %q, want the reason preserved", final.GetError())
	}
	if final.GetAttempts() != maxAttempts {
		t.Errorf("attempts = %d, want %d", final.GetAttempts(), maxAttempts)
	}

	// And it is still listable — a failure nobody can see is the same
	// as a failure that never happened.
	failed, err := svc.List(ctx, "engineering", InboxFilter{
		Statuses: []lobslawv1.InboxStatus{lobslawv1.InboxStatus_INBOX_STATUS_FAILED},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(failed) != 1 {
		t.Errorf("listed %d failed items, want 1", len(failed))
	}
}

// A transient failure with attempts left goes back on the queue rather
// than terminating.
func TestTransientFailureReturnsToPending(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "engineering", "flaky", 0)

	claimed, err := svc.Claim(ctx, "engineering", "node-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	next, err := svc.Resolve(ctx, "engineering", item.GetId(), InboxOutcome{
		ClaimRevision: claimed.Revision, Claimer: claimed.ClaimedBy,
		Err: errors.New("provider blip"), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if next.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_PENDING {
		t.Errorf("status = %v, want pending for a retryable failure", next.GetStatus())
	}
	again, err := svc.Claim(ctx, "engineering", "node-1")
	if err != nil {
		t.Fatalf("second Claim: %v", err)
	}
	if again == nil {
		t.Fatal("a retryable item was not re-claimable")
	}
}

func TestSuccessRecordsTheResult(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "engineering", "deploy", 0)

	claimed, err := svc.Claim(ctx, "engineering", "node-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	done, err := svc.Resolve(ctx, "engineering", item.GetId(), InboxOutcome{
		ClaimRevision: claimed.Revision, Claimer: claimed.ClaimedBy,
		Result: "deployed at 14:02", SessionID: "sess-1", MaxAttempts: 3,
	})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if done.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_DONE {
		t.Errorf("status = %v, want done", done.GetStatus())
	}
	if done.GetResult() != "deployed at 14:02" {
		t.Errorf("result = %q", done.GetResult())
	}
	if done.GetSessionId() != "sess-1" {
		t.Errorf("session_id = %q; the GUI deep-links from it", done.GetSessionId())
	}
	if done.GetCompletedAt() == nil {
		t.Error("completed_at was not stamped")
	}
	results, err := svc.List(ctx, "coordinator", InboxFilter{})
	if err != nil || len(results) != 1 || results[0].GetCorrelationId() != item.Id || results[0].GetResult() != done.GetResult() {
		t.Fatalf("sender did not receive correlated result: %v %v", results, err)
	}
}

// Backpressure reaches the SENDER. Dropping silently would be the
// exact failure a durable queue exists to prevent, and the sender is
// the only party that can do anything about it.
func TestFullQueueRefusesTheSender(t *testing.T) {
	t.Parallel()
	const cap = 3
	svc := newTestInbox(t, cap)
	for i := range cap {
		post(t, svc, "engineering", fmt.Sprintf("task %d", i), 0)
	}
	_, err := svc.Post(context.Background(), &lobslawv1.BotInboxItem{
		Recipient: "engineering", Body: "one too many",
	})
	if !errors.Is(err, ErrInboxFull) {
		t.Fatalf("err = %v, want ErrInboxFull", err)
	}
	if !strings.Contains(err.Error(), "not keeping up") {
		t.Errorf("error does not say what is wrong: %v", err)
	}

	items, lerr := svc.List(context.Background(), "engineering", InboxFilter{})
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(items) != cap {
		t.Errorf("queue holds %d items, want the cap of %d — the refused post was written anyway", len(items), cap)
	}
}

// Finished items do not count against the cap: the bound is on
// unworked backlog, not on history.
func TestTerminalItemsDoNotBlockTheQueue(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 2)
	ctx := context.Background()
	first := post(t, svc, "engineering", "one", 0)

	claimed, err := svc.Claim(ctx, "engineering", "node-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := svc.Resolve(ctx, "engineering", first.GetId(), InboxOutcome{Result: "ok", MaxAttempts: 3, ClaimRevision: claimed.Revision, Claimer: claimed.ClaimedBy}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for i := range 2 {
		if _, err := svc.Post(ctx, &lobslawv1.BotInboxItem{
			Recipient: "engineering", Body: fmt.Sprintf("later %d", i),
		}); err != nil {
			t.Fatalf("post after completion: %v", err)
		}
	}
}

// Cancelling work a handler is running would lose the record, not stop
// the work.
func TestCancelRefusesAnItemBeingWorked(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "engineering", "in flight", 0)

	if _, err := svc.Claim(ctx, "engineering", "node-1"); err != nil {
		t.Fatalf("Claim: %v", err)
	}
	_, err := svc.Cancel(ctx, "engineering", item.GetId())
	if err == nil {
		t.Fatal("cancelled an item that was being worked")
	}
	if !strings.Contains(err.Error(), "lose the record") {
		t.Errorf("error does not say why: %v", err)
	}
}

func TestCancelAndRetry(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()
	item := post(t, svc, "engineering", "never mind", 0)

	cancelled, err := svc.Cancel(ctx, "engineering", item.GetId())
	if err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	if cancelled.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED {
		t.Errorf("status = %v, want cancelled", cancelled.GetStatus())
	}

	retried, err := svc.Retry(ctx, "engineering", item.GetId())
	if err != nil {
		t.Fatalf("Retry: %v", err)
	}
	if retried.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_PENDING {
		t.Errorf("status = %v, want pending after retry", retried.GetStatus())
	}
	if retried.GetAttempts() != 0 {
		t.Errorf("attempts = %d, want the counter reset", retried.GetAttempts())
	}
}

// Queues are per bot. A bot seeing another's work would defeat the
// isolation the whole design rests on.
func TestQueuesAreIsolatedPerBot(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	post(t, svc, "engineering", "eng work", 0)
	post(t, svc, "marketing", "mkt work", 0)

	eng, err := svc.List(context.Background(), "engineering", InboxFilter{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(eng) != 1 || eng[0].GetBody() != "eng work" {
		t.Errorf("engineering saw %d items: %v", len(eng), eng)
	}
	if _, err := svc.Claim(context.Background(), "marketing", "node-1"); err != nil {
		t.Fatalf("Claim marketing: %v", err)
	}
	// Engineering's item is untouched by marketing's drain.
	eng, _ = svc.List(context.Background(), "engineering", InboxFilter{
		Statuses: []lobslawv1.InboxStatus{lobslawv1.InboxStatus_INBOX_STATUS_PENDING},
	})
	if len(eng) != 1 {
		t.Errorf("marketing's drain touched engineering's queue")
	}
}

// History is bounded, but only history: an item nobody worked is a
// problem to surface, not one to tidy away.
func TestPruneRemovesFinishedItemsAndNothingElse(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	ctx := context.Background()

	done := post(t, svc, "engineering", "finished", 0)
	pending := post(t, svc, "engineering", "still waiting", 0)
	claimed, err := svc.Claim(ctx, "engineering", "node-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if _, err := svc.Resolve(ctx, "engineering", done.GetId(), InboxOutcome{Result: "ok", MaxAttempts: 3, ClaimRevision: claimed.Revision, Claimer: claimed.ClaimedBy}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Negative age makes everything already-terminal look old.
	pruned, err := svc.PruneTerminal(ctx, -time.Hour)
	if err != nil {
		t.Fatalf("PruneTerminal: %v", err)
	}
	if pruned != 2 {
		t.Errorf("pruned %d, want completion and sender receipt", pruned)
	}
	if _, err := svc.Get(ctx, "engineering", pending.GetId()); err != nil {
		t.Errorf("an unworked item was pruned: %v", err)
	}
}

func TestInboxRejectsAnEmptyBody(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	_, err := svc.Post(context.Background(), &lobslawv1.BotInboxItem{Recipient: "engineering", Body: "   "})
	if err == nil {
		t.Fatal("accepted an item with no body")
	}
	if !strings.Contains(err.Error(), "nothing to do") {
		t.Errorf("error does not say why: %v", err)
	}
}

func TestInboxRejectsAnOversizedBody(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	_, err := svc.Post(context.Background(), &lobslawv1.BotInboxItem{
		Recipient: "engineering",
		Body:      strings.Repeat("x", MaxInboxBody+1),
	})
	if err == nil {
		t.Fatal("accepted an oversized body")
	}
	if !strings.Contains(err.Error(), "turn's prompt") {
		t.Errorf("error does not say why the cap exists: %v", err)
	}
}

func TestRecipientsListsEveryQueue(t *testing.T) {
	t.Parallel()
	svc := newTestInbox(t, 0)
	post(t, svc, "engineering", "a", 0)
	post(t, svc, "marketing", "b", 0)
	post(t, svc, "engineering", "c", 0)

	got, err := svc.Recipients(context.Background())
	if err != nil {
		t.Fatalf("Recipients: %v", err)
	}
	if strings.Join(got, ",") != "engineering,marketing" {
		t.Errorf("Recipients = %v", got)
	}
}

// The inbox must survive a backup/restore or a restored cluster comes
// back with the team's work missing.
func TestInboxIsExportable(t *testing.T) {
	t.Parallel()
	for _, k := range archiveKinds {
		if k.bucket == BucketBotInbox {
			return
		}
	}
	t.Errorf("%q is not in archiveKinds; queued work would not survive a restore", BucketBotInbox)
}

// timestamppbPast is a claim expiry already in the past — what a dead
// node's claim looks like once its TTL has run out.
func timestamppbPast() *timestamppb.Timestamp {
	return timestamppb.New(time.Now().Add(-time.Hour))
}
