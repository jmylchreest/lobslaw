package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const inboxApplyTimeout = 5 * time.Second

// DefaultInboxMaxPending bounds one bot's unworked queue.
//
// Every queued item is a replicated record on every voter, so an
// unbounded queue is a store-growth problem that only shows up later
// as slow snapshots. The cap is per RECIPIENT rather than global
// because a runaway producer should stall the bot it is flooding, not
// the whole cluster.
const DefaultInboxMaxPending = 200

// Field caps. The subject is what a queue listing renders; the body
// becomes a turn's prompt, and an unbounded one is an unbounded
// provider call nothing else would report as the cause.
const (
	MaxInboxSubject = 200
	MaxInboxBody    = 16 << 10
	MaxInboxResult  = 16 << 10
)

// DefaultInboxMaxAttempts is how many times a failing item is retried
// before it becomes visibly FAILED.
//
// Three, because the failures worth retrying are transient (a provider
// blip, a leadership change) and those clear inside three; anything
// that survives three is a real problem, and burning a provider call
// per retry forever on a task that cannot succeed is how a queue turns
// into a bill.
const DefaultInboxMaxAttempts = 3

// InboxClaimTTL is how long a claim is honoured before another node
// may take the item over. Longer than a turn's hard timeout, so a node
// that is merely slow does not have its work stolen mid-flight.
const InboxClaimTTL = 10 * time.Minute

// ErrInboxNotFound is returned for an unknown item id.
var ErrInboxNotFound = errors.New("inbox: no such item")

// ErrInboxFull is returned when a recipient's pending queue is at the
// cap. It reaches the SENDER, deliberately: a post that silently
// vanished would be the exact failure the queue exists to prevent, and
// the sender is the only party that can do anything about it.
var ErrInboxFull = errors.New("inbox: recipient's queue is full")

// InboxService is a bot's durable work queue and journal.
//
// Claiming reuses LOG_OP_CLAIM — the same revision-plus-claimer CAS
// the scheduler uses for tasks and commitments. Deliberately not a
// second claim mechanism: two of those is how they come to disagree,
// and this one would disagree about exactly-once.
type InboxService struct {
	raft       *RaftNode
	store      *Store
	maxPending int
}

// NewInboxService wires the queue. maxPending <= 0 takes the default.
func NewInboxService(raft *RaftNode, store *Store, maxPending int) *InboxService {
	if maxPending <= 0 {
		maxPending = DefaultInboxMaxPending
	}
	return &InboxService{raft: raft, store: store, maxPending: maxPending}
}

// inboxKey is the bucket key: recipient prefix, then a ULID so arrival
// order is key order within a recipient.
func inboxKey(recipient, id string) string { return recipient + ":" + id }

// Post enqueues an item.
//
// The caller supplies recipient, kind, subject and body; everything
// else is the service's. In particular the SENDER is stamped by the
// caller from turn identity and never by the model — a bot that could
// name its own sender could post work that appears to come from you.
func (s *InboxService) Post(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error) {
	if item == nil {
		return nil, errors.New("inbox: item required")
	}
	if s.raft == nil {
		return nil, errors.New("inbox: raft not wired")
	}
	item = proto.Clone(item).(*lobslawv1.BotInboxItem)
	if err := validateInboxItem(item); err != nil {
		return nil, err
	}

	pending, err := s.countPending(item.GetRecipient())
	if err != nil {
		return nil, err
	}
	if pending >= s.maxPending {
		return nil, fmt.Errorf("%w: %q has %d pending items (cap %d); it is not keeping up",
			ErrInboxFull, item.GetRecipient(), pending, s.maxPending)
	}

	item.Id = ids.New()
	item.Status = lobslawv1.InboxStatus_INBOX_STATUS_PENDING
	item.CreatedAt = timestamppb.Now()
	item.Revision = 1
	// Claim fields are the service's, never the caller's: a caller that
	// could pre-claim an item could park work nothing will ever run.
	item.ClaimedBy = ""
	item.ClaimExpiresAt = nil
	item.Attempts = 0
	item.Result = ""
	item.Error = ""
	item.CompletedAt = nil

	if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_PUT, item, nil, ""); err != nil {
		return nil, err
	}
	return item, nil
}

// Journal records an already delivered exchange in one Raft write. It never
// exposes an intermediate PENDING state to the work drain.
func (s *InboxService) Journal(ctx context.Context, item *lobslawv1.BotInboxItem) (*lobslawv1.BotInboxItem, error) {
	if item == nil || s.raft == nil {
		return nil, errors.New("inbox: item and raft required")
	}
	item = proto.Clone(item).(*lobslawv1.BotInboxItem)
	if err := validateInboxItem(item); err != nil {
		return nil, err
	}
	item.Id = ids.New()
	item.Status = lobslawv1.InboxStatus_INBOX_STATUS_DONE
	item.CreatedAt = timestamppb.Now()
	item.CompletedAt = item.CreatedAt
	item.Revision = 1
	item.ClaimedBy, item.ClaimExpiresAt, item.Attempts = "", nil, 0
	item.Result = truncate(item.Result, MaxInboxResult)
	if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_PUT, item, nil, ""); err != nil {
		return nil, err
	}
	return item, nil
}

// Get returns one item by recipient and id.
func (s *InboxService) Get(_ context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error) {
	if s.store == nil {
		return nil, errors.New("inbox: store not wired")
	}
	raw, err := s.store.Get(BucketBotInbox, inboxKey(recipient, id))
	if err != nil {
		if errors.Is(err, types.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrInboxNotFound, id)
		}
		return nil, err
	}
	var item lobslawv1.BotInboxItem
	if err := proto.Unmarshal(raw, &item); err != nil {
		return nil, fmt.Errorf("inbox: unmarshal %q: %w", id, err)
	}
	return &item, nil
}

// InboxFilter narrows a listing.
type InboxFilter struct {
	// Statuses limits to these. Empty means every status — a queue view
	// wants pending, the GUI's history view wants everything.
	Statuses []lobslawv1.InboxStatus
	Kinds    []lobslawv1.InboxKind
	// Limit caps the result. Zero means no cap.
	Limit int
}

// List returns one bot's items, most urgent first.
//
// Ordering is priority descending then id ascending, and id is a ULID,
// so equal priorities come out in arrival order. That is the order the
// drain works them in and the order the GUI shows them: one rule, so
// "why is it doing that one next" has one answer.
func (s *InboxService) List(_ context.Context, recipient string, f InboxFilter) ([]*lobslawv1.BotInboxItem, error) {
	if s.store == nil {
		return nil, errors.New("inbox: store not wired")
	}
	recipient = strings.TrimSpace(recipient)
	if recipient == "" {
		return nil, errors.New("inbox: recipient required")
	}
	var out []*lobslawv1.BotInboxItem
	err := s.store.ForEachPrefix(BucketBotInbox, recipient+":", func(key string, value []byte) error {
		var item lobslawv1.BotInboxItem
		if err := proto.Unmarshal(value, &item); err != nil {
			return fmt.Errorf("inbox: unmarshal %q: %w", key, err)
		}
		if !matchesInboxFilter(&item, f) {
			return nil
		}
		out = append(out, &item)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].GetPriority() != out[j].GetPriority() {
			return out[i].GetPriority() > out[j].GetPriority()
		}
		return out[i].GetId() < out[j].GetId()
	})
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[:f.Limit]
	}
	return out, nil
}

// Claim takes the highest-priority pending item for a recipient and
// marks it CLAIMED by nodeID. Returns (nil, nil) when the queue holds
// nothing to work — an empty queue is the normal case, not an error.
//
// An expired claim counts as unclaimed, so a crashed node's work is
// picked up on the next pass without an operator intervening. The
// expiry check is made HERE rather than in the FSM: it reads a clock,
// and the FSM must produce the same result on every replica and on
// every replay. Same split the scheduler makes for dueness.
func (s *InboxService) Claim(ctx context.Context, recipient, nodeID string) (*lobslawv1.BotInboxItem, error) {
	if s.raft == nil {
		return nil, errors.New("inbox: raft not wired")
	}
	candidates, err := s.List(ctx, recipient, InboxFilter{
		Statuses: []lobslawv1.InboxStatus{
			lobslawv1.InboxStatus_INBOX_STATUS_PENDING,
			lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED,
		},
	})
	if err != nil {
		return nil, err
	}
	now := time.Now()
	for _, item := range candidates {
		if !claimAvailable(item, now) {
			continue
		}
		claimed := proto.Clone(item).(*lobslawv1.BotInboxItem)
		claimed.Status = lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED
		claimed.ClaimedBy = nodeID
		claimed.ClaimExpiresAt = timestamppb.New(now.Add(InboxClaimTTL))
		claimed.Attempts = item.GetAttempts() + 1
		claimed.Revision = item.GetRevision() + 1

		expected := item.GetRevision()
		err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_CLAIM, claimed, &expected, item.GetClaimedBy())
		switch {
		case err == nil:
			return claimed, nil
		case errors.Is(err, ErrClaimConflict):
			// Another node won this one. Keep walking: the queue is
			// shared, and giving up on the first loss would leave a
			// busy queue drained by whichever node happened to be
			// fastest.
			continue
		default:
			return nil, err
		}
	}
	return nil, nil
}

// Resolve moves a claimed item to a terminal state.
//
// A failure that has attempts left goes back to PENDING rather than
// FAILED, so the next pass retries it. Out of attempts, it becomes
// FAILED with the error on the record and stays visible — the whole
// point of the queue is that nothing evaporates, and a task that
// disappeared quietly is worse than one that was refused.
func (s *InboxService) Resolve(ctx context.Context, recipient, id string, outcome InboxOutcome) (*lobslawv1.BotInboxItem, error) {
	if s.raft == nil {
		return nil, errors.New("inbox: raft not wired")
	}
	item, err := s.Get(ctx, recipient, id)
	if err != nil {
		return nil, err
	}
	if outcome.ClaimRevision != 0 {
		if item.GetRevision() != outcome.ClaimRevision || item.GetClaimedBy() != outcome.Claimer || item.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED {
			return nil, ErrClaimConflict
		}
	} else if item.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_PENDING {
		return nil, ErrClaimConflict
	}
	next := proto.Clone(item).(*lobslawv1.BotInboxItem)
	next.Result = truncate(outcome.Result, MaxInboxResult)
	next.SessionId = outcome.SessionID
	// What the turn actually did, beside what it says it did.
	next.ToolsUsed = outcome.ToolsUsed
	next.TokensUsed = outcome.TokensUsed
	next.CostUsd = outcome.CostUSD
	next.ClaimedBy = ""
	next.ClaimExpiresAt = nil

	switch {
	case outcome.Err == nil:
		next.Status = lobslawv1.InboxStatus_INBOX_STATUS_DONE
		next.Error = ""
		next.CompletedAt = timestamppb.Now()
	case int(item.GetAttempts()) < outcome.MaxAttempts:
		next.Status = lobslawv1.InboxStatus_INBOX_STATUS_PENDING
		next.Error = outcome.Err.Error()
	default:
		next.Status = lobslawv1.InboxStatus_INBOX_STATUS_FAILED
		next.Error = outcome.Err.Error()
		next.CompletedAt = timestamppb.Now()
	}
	next.Revision = item.GetRevision() + 1

	expected := item.GetRevision()
	if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_CLAIM, next, &expected, item.GetClaimedBy()); err != nil {
		return nil, err
	}
	return next, nil
}

// InboxOutcome is what happened when an item was worked.
type InboxOutcome struct {
	ClaimRevision uint64
	Claimer       string
	Result        string
	SessionID     string
	// ToolsUsed is the distinct tools the turn invoked. Recorded
	// because Result is the bot's account of its work and this is the
	// record of it — the two are not always the same, and only one of
	// them is evidence.
	ToolsUsed  []string
	TokensUsed uint64
	CostUSD    float64
	// Err non-nil means the turn failed. Retried while attempts remain.
	Err         error
	MaxAttempts int
}

// Cancel marks a pending item cancelled. A CLAIMED item whose claim is
// live is refused: a handler is running it, and marking it cancelled
// would not stop the work, only lose the record of it.
func (s *InboxService) Cancel(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error) {
	if s.raft == nil {
		return nil, errors.New("inbox: raft not wired")
	}
	item, err := s.Get(ctx, recipient, id)
	if err != nil {
		return nil, err
	}
	if item.GetStatus() == lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED && !claimExpired(item, time.Now()) {
		return nil, fmt.Errorf("inbox: %q is being worked by %q right now; cancelling would lose the record, not stop the work",
			id, item.GetClaimedBy())
	}
	if isTerminalInbox(item.GetStatus()) {
		return nil, fmt.Errorf("inbox: %q is already %s", id, inboxStatusName(item.GetStatus()))
	}
	next := proto.Clone(item).(*lobslawv1.BotInboxItem)
	next.Status = lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED
	next.ClaimedBy = ""
	next.ClaimExpiresAt = nil
	next.CompletedAt = timestamppb.Now()
	next.Revision = item.GetRevision() + 1

	expected := item.GetRevision()
	if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_CLAIM, next, &expected, item.GetClaimedBy()); err != nil {
		return nil, err
	}
	return next, nil
}

// Retry puts a FAILED item back on the queue with its attempt counter
// reset. What the GUI's retry button does — a task that failed because
// a cluster was down should not need to be retyped.
func (s *InboxService) Retry(ctx context.Context, recipient, id string) (*lobslawv1.BotInboxItem, error) {
	if s.raft == nil {
		return nil, errors.New("inbox: raft not wired")
	}
	item, err := s.Get(ctx, recipient, id)
	if err != nil {
		return nil, err
	}
	if item.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_FAILED &&
		item.GetStatus() != lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED {
		return nil, fmt.Errorf("inbox: %q is %s, not failed or cancelled", id, inboxStatusName(item.GetStatus()))
	}
	next := proto.Clone(item).(*lobslawv1.BotInboxItem)
	next.Status = lobslawv1.InboxStatus_INBOX_STATUS_PENDING
	next.Attempts = 0
	next.Error = ""
	next.CompletedAt = nil
	next.Revision = item.GetRevision() + 1

	expected := item.GetRevision()
	if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_CLAIM, next, &expected, item.GetClaimedBy()); err != nil {
		return nil, err
	}
	return next, nil
}

// Recipients returns every bot id with at least one item, so the drain
// knows which queues to look at without reading the bot registry.
func (s *InboxService) Recipients(_ context.Context) ([]string, error) {
	if s.store == nil {
		return nil, errors.New("inbox: store not wired")
	}
	// Keys only. The recipient is the key's prefix and keys are stored
	// in plaintext, so decrypting every value to read one — on a
	// thirty-second tick, on every node — bought nothing.
	seen := map[string]struct{}{}
	err := s.store.ForEach(BucketBotInbox, func(key string, _ []byte) error {
		if recipient, _, ok := strings.Cut(key, ":"); ok {
			seen[recipient] = struct{}{}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(seen))
	for r := range seen {
		out = append(out, r)
	}
	sort.Strings(out)
	return out, nil
}

// PruneTerminal deletes completed items older than age, returning how
// many went.
//
// Terminal items are the queue's history, and history that grows
// forever is a snapshot that gets slower forever. Pending and claimed
// items are never touched however old — an item nobody worked is a
// problem to surface, not one to tidy away.
func (s *InboxService) PruneTerminal(ctx context.Context, age time.Duration) (int, error) {
	if s.raft == nil {
		return 0, errors.New("inbox: raft not wired")
	}
	cutoff := time.Now().Add(-age)
	var stale []*lobslawv1.BotInboxItem
	err := s.store.ForEach(BucketBotInbox, func(key string, value []byte) error {
		var item lobslawv1.BotInboxItem
		if err := proto.Unmarshal(value, &item); err != nil {
			return fmt.Errorf("inbox: unmarshal %q: %w", key, err)
		}
		if !isTerminalInbox(item.GetStatus()) {
			return nil
		}
		if item.GetCompletedAt() == nil || item.GetCompletedAt().AsTime().After(cutoff) {
			return nil
		}
		stale = append(stale, &item)
		return nil
	})
	if err != nil {
		return 0, err
	}
	pruned := 0
	for _, item := range stale {
		if err := s.apply(ctx, lobslawv1.LogOp_LOG_OP_DELETE, item, nil, ""); err != nil {
			return pruned, err
		}
		pruned++
	}
	return pruned, nil
}

func (s *InboxService) countPending(recipient string) (int, error) {
	n := 0
	err := s.store.ForEachPrefix(BucketBotInbox, recipient+":", func(key string, value []byte) error {
		var item lobslawv1.BotInboxItem
		if err := proto.Unmarshal(value, &item); err != nil {
			return fmt.Errorf("inbox: unmarshal %q: %w", key, err)
		}
		if item.GetStatus() == lobslawv1.InboxStatus_INBOX_STATUS_PENDING ||
			item.GetStatus() == lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED {
			n++
		}
		return nil
	})
	return n, err
}

// apply proposes one entry.
//
// expectedClaimer matters as much as expectedRevision: the FSM checks
// BOTH, because claimed_by alone cannot tell "nobody holds this" from
// "somebody held it, finished, and released it". Omitting it defaults
// to "expect unclaimed", which makes every write against a live claim
// — the completion path especially — fail as a conflict.
func (s *InboxService) apply(ctx context.Context, op lobslawv1.LogOp, item *lobslawv1.BotInboxItem, expectedRevision *uint64, expectedClaimer string) error {
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op:               op,
		Id:               inboxKey(item.GetRecipient(), item.GetId()),
		ExpectedRevision: expectedRevision,
		ExpectedClaimer:  expectedClaimer,
		Payload:          &lobslawv1.LogEntry_BotInbox{BotInbox: item},
	})
	if err != nil {
		return err
	}
	res, err := s.raft.ApplyOrForward(ctx, data, inboxApplyTimeout)
	if err != nil {
		return fmt.Errorf("inbox: raft apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return applyErr
	}
	return nil
}

func validateInboxItem(item *lobslawv1.BotInboxItem) error {
	item.Recipient = strings.TrimSpace(item.GetRecipient())
	if !botIDPattern.MatchString(item.GetRecipient()) {
		return fmt.Errorf("inbox: recipient %q is not a valid bot id", item.GetRecipient())
	}
	item.Subject = truncate(strings.TrimSpace(item.GetSubject()), MaxInboxSubject)
	if strings.TrimSpace(item.GetBody()) == "" {
		return errors.New("inbox: body required; an empty item is a turn with nothing to do")
	}
	if n := len(item.GetBody()); n > MaxInboxBody {
		return fmt.Errorf("inbox: body is %d bytes, over the %d-byte cap; it becomes a turn's prompt", n, MaxInboxBody)
	}
	if item.GetSubject() == "" {
		item.Subject = truncate(firstLine(item.GetBody()), MaxInboxSubject)
	}
	if item.GetKind() == lobslawv1.InboxKind_INBOX_KIND_UNSPECIFIED {
		item.Kind = lobslawv1.InboxKind_INBOX_KIND_TASK
	}
	return nil
}

func matchesInboxFilter(item *lobslawv1.BotInboxItem, f InboxFilter) bool {
	if len(f.Statuses) > 0 && !containsStatus(f.Statuses, item.GetStatus()) {
		return false
	}
	if len(f.Kinds) > 0 && !containsKind(f.Kinds, item.GetKind()) {
		return false
	}
	return true
}

func containsStatus(in []lobslawv1.InboxStatus, want lobslawv1.InboxStatus) bool {
	for _, s := range in {
		if s == want {
			return true
		}
	}
	return false
}

func containsKind(in []lobslawv1.InboxKind, want lobslawv1.InboxKind) bool {
	for _, k := range in {
		if k == want {
			return true
		}
	}
	return false
}

// claimAvailable reports whether an item can be claimed now: pending,
// or claimed by a node whose claim has lapsed.
func claimAvailable(item *lobslawv1.BotInboxItem, now time.Time) bool {
	switch item.GetStatus() {
	case lobslawv1.InboxStatus_INBOX_STATUS_PENDING:
		return true
	case lobslawv1.InboxStatus_INBOX_STATUS_CLAIMED:
		return claimExpired(item, now)
	default:
		return false
	}
}

func claimExpired(item *lobslawv1.BotInboxItem, now time.Time) bool {
	exp := item.GetClaimExpiresAt()
	return exp == nil || exp.AsTime().Before(now)
}

func isTerminalInbox(s lobslawv1.InboxStatus) bool {
	switch s {
	case lobslawv1.InboxStatus_INBOX_STATUS_DONE,
		lobslawv1.InboxStatus_INBOX_STATUS_FAILED,
		lobslawv1.InboxStatus_INBOX_STATUS_CANCELLED:
		return true
	default:
		return false
	}
}

func inboxStatusName(s lobslawv1.InboxStatus) string {
	return strings.ToLower(strings.TrimPrefix(s.String(), "INBOX_STATUS_"))
}

// InboxStatusName renders a status for a human — a log line, a tool
// result, a GUI label. One spelling, so the GUI and the bot describe
// the same item the same way.
func InboxStatusName(s lobslawv1.InboxStatus) string { return inboxStatusName(s) }

// InboxKindName renders a kind for a human.
func InboxKindName(k lobslawv1.InboxKind) string {
	return strings.ToLower(strings.TrimPrefix(k.String(), "INBOX_KIND_"))
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}
