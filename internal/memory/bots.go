package memory

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/bots"
	"github.com/jmylchreest/lobslaw/internal/identity"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// botApplyTimeout caps raft.Apply for a bot write. Creating or
// re-instructing a bot is a human-pace action, so 5s is generous —
// the same figure soul writes use, for the same reason.
const botApplyTimeout = 5 * time.Second

// MaxBotInstructions bounds a bot's standing brief.
//
// The brief rides on every one of that bot's turns, so an unbounded
// one is an unbounded per-turn tax that nothing else in the system
// would ever report as the cause. 8 KB is several screens of prose —
// far more than a role description needs, and small enough that a
// hundred bots still snapshot quickly.
const MaxBotInstructions = 8 << 10

// botIDPattern is what an id may look like. Restrictive on purpose:
// the id becomes a principal ("bot:engineering"), a soul-overlay key
// suffix, and a policy-rule subject, so a colon or a space in one
// would silently change what a rule matches.
var botIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// ErrBotNotFound is returned for an unknown bot id. Distinct from a
// store failure: callers routinely ask about a bot that may not exist.
var ErrBotNotFound = errors.New("bots: no such bot")

// BotService is the raft-backed registry of named agents.
//
// Reads are local, straight off the FSM's bbolt, so any node can
// answer "who is the engineering bot". Writes go through raft with a
// revision check, so two operators editing one bot's instructions
// cannot silently lose an edit — the same CAS contract SoulTuneService
// uses, for the same reason.
type BotService struct {
	raft  *RaftNode
	store *Store
}

// NewBotService wires the registry against an existing Raft + Store.
// Nil raft leaves reads working and writes failing, matching the
// asymmetry every other service here has.
func NewBotService(raft *RaftNode, store *Store) *BotService {
	return &BotService{raft: raft, store: store}
}

// Get returns one bot. ErrBotNotFound when the id is unknown.
func (s *BotService) Get(_ context.Context, id string) (*lobslawv1.BotRecord, error) {
	if s.store == nil {
		return nil, errors.New("bots: store not wired")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, ErrBotNotFound
	}
	raw, err := s.store.Get(BucketBots, id)
	if err != nil {
		if errors.Is(err, types.ErrNotFound) {
			return nil, fmt.Errorf("%w: %q", ErrBotNotFound, id)
		}
		return nil, err
	}
	var rec lobslawv1.BotRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("bots: unmarshal %q: %w", id, err)
	}
	return &rec, nil
}

// List returns every bot ordered by id.
func (s *BotService) List(_ context.Context) ([]*lobslawv1.BotRecord, error) {
	if s.store == nil {
		return nil, errors.New("bots: store not wired")
	}
	var out []*lobslawv1.BotRecord
	err := s.store.ForEach(BucketBots, func(key string, value []byte) error {
		var rec lobslawv1.BotRecord
		if err := proto.Unmarshal(value, &rec); err != nil {
			return fmt.Errorf("bots: unmarshal %q: %w", key, err)
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].GetId() < out[j].GetId()
	})
	return out, nil
}

// Put writes a bot, checking expectedRevision against the stored
// record. Zero is the expected revision for a create.
func (s *BotService) Put(ctx context.Context, rec *lobslawv1.BotRecord, expectedRevision uint64) (*lobslawv1.BotRecord, error) {
	if rec == nil {
		return nil, errors.New("bots: record required")
	}
	if s.raft == nil {
		return nil, errors.New("bots: raft not wired")
	}
	rec = proto.Clone(rec).(*lobslawv1.BotRecord)
	if err := validateBot(rec); err != nil {
		return nil, err
	}
	if err := s.checkMessageGraph(ctx, rec); err != nil {
		return nil, err
	}

	prev, err := s.Get(ctx, rec.GetId())
	switch {
	case err == nil:
		if prev.GetRevision() != expectedRevision {
			return nil, fmt.Errorf("%w: bot %q changed; read it again and retry", ErrClaimConflict, rec.GetId())
		}
		rec.CreatedAt = prev.GetCreatedAt()
		rec.CreatedBy = prev.GetCreatedBy()
		if prev.GetOwner() != "" {
			rec.Owner = prev.GetOwner()
		}
	case errors.Is(err, ErrBotNotFound):
		if expectedRevision != 0 {
			return nil, fmt.Errorf("%w: bot %q does not exist", ErrClaimConflict, rec.GetId())
		}
		if rec.GetCreatedAt() == nil {
			rec.CreatedAt = timestamppb.Now()
		}
	default:
		return nil, err
	}

	rec.UpdatedAt = timestamppb.Now()
	rec.Revision = expectedRevision + 1
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op:               lobslawv1.LogOp_LOG_OP_CLAIM,
		Id:               rec.GetId(),
		ExpectedRevision: &expectedRevision,
		Payload:          &lobslawv1.LogEntry_Bot{Bot: rec},
	})
	if err != nil {
		return nil, err
	}
	res, err := s.raft.ApplyOrForward(ctx, data, botApplyTimeout)
	if err != nil {
		return nil, fmt.Errorf("bots: raft apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return nil, applyErr
	}
	return rec, nil
}

// Delete removes a bot. Records the bot owned are NOT cascaded.
func (s *BotService) Delete(ctx context.Context, id string) error {
	if s.raft == nil {
		return errors.New("bots: raft not wired")
	}
	rec, err := s.Get(ctx, id)
	if err != nil {
		return err
	}
	data, err := proto.Marshal(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_DELETE,
		Id:      rec.GetId(),
		Payload: &lobslawv1.LogEntry_Bot{Bot: &lobslawv1.BotRecord{Id: rec.GetId()}},
	})
	if err != nil {
		return err
	}
	res, err := s.raft.ApplyOrForward(ctx, data, botApplyTimeout)
	if err != nil {
		return fmt.Errorf("bots: raft apply: %w", err)
	}
	if applyErr, ok := res.(error); ok && applyErr != nil {
		return applyErr
	}
	return nil
}

// AdoptUnowned assigns owner to every bot that currently has none.
// A zero owner is a no-op: unowned records stay inaccessible rather
// than becoming public.
func (s *BotService) AdoptUnowned(ctx context.Context, owner identity.Principal) error {
	if owner.IsZero() {
		return nil
	}
	list, err := s.List(ctx)
	if err != nil {
		return err
	}
	for _, rec := range list {
		if strings.TrimSpace(rec.GetOwner()) != "" {
			continue
		}
		rec.Owner = owner.String()
		if _, err := s.Put(ctx, rec, rec.GetRevision()); err != nil {
			return err
		}
	}
	return nil
}

// MayModify reports whether a principal can change a bot.
//
// Empty principal is nobody. Empty owner is nobody. There is no
// unowned-editable fallback — an unowned bot is inaccessible, not
// public.
func MayModify(rec *lobslawv1.BotRecord, principal string) bool {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return false
	}
	if rec == nil {
		return false
	}
	owner := strings.TrimSpace(rec.GetOwner())
	if owner == "" {
		return false
	}
	return owner == principal
}

func validateBot(rec *lobslawv1.BotRecord) error {
	id := strings.TrimSpace(rec.GetId())
	if !botIDPattern.MatchString(id) {
		return fmt.Errorf("bots: id %q must be lowercase letters, digits and hyphens, starting with a letter or digit, at most 63 characters", rec.GetId())
	}
	rec.Id = id
	if n := len(rec.GetInstructions()); n > MaxBotInstructions {
		return fmt.Errorf("bots: instructions are %d bytes, over the %d-byte cap; they ride on every turn this bot takes", n, MaxBotInstructions)
	}
	if err := validateBotOwner(rec.GetOwner()); err != nil {
		return err
	}
	for _, target := range rec.GetMayMessage() {
		if strings.TrimSpace(target) == id {
			return fmt.Errorf("bots: %q may not be listed as its own message target", id)
		}
		if !botIDPattern.MatchString(strings.TrimSpace(target)) {
			return fmt.Errorf("bots: may_message entry %q is not a valid bot id", target)
		}
	}
	if b := rec.GetBudget(); b != nil {
		if b.GetMaxToolCalls() < 0 || b.GetMaxSpendUsd() < 0 || b.GetMaxEgressBytes() < 0 {
			return fmt.Errorf("bots: %q has a negative budget cap; zero means inherit the node default", id)
		}
	}
	return nil
}

// checkMessageGraph refuses a may_message edge that would close a
// loop, naming the path. On write rather than at call time.
func (s *BotService) checkMessageGraph(ctx context.Context, rec *lobslawv1.BotRecord) error {
	if len(rec.GetMayMessage()) == 0 {
		return nil
	}
	existing, err := s.List(ctx)
	if err != nil {
		return err
	}
	graph := make(bots.Graph, len(existing)+1)
	for _, b := range existing {
		graph[b.GetId()] = b.GetMayMessage()
	}
	return graph.WithEdges(rec.GetId(), rec.GetMayMessage()).Validate()
}

func validateBotOwner(owner string) error {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil
	}
	kind, id, ok := strings.Cut(owner, ":")
	if !ok || kind != identity.KindUser || strings.TrimSpace(id) == "" || strings.Contains(id, ":") {
		return fmt.Errorf("bots: owner %q must be a human principal (user:<id>)", owner)
	}
	return nil
}
