package memory

import (
	"context"
	cryptorand "crypto/rand"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// raftApplier is the subset of *RaftNode the ingester uses.
// Kept as an interface so tests can substitute a fake.
type raftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// EpisodicTurn is the agent-facing shape duplicated here so this
// package doesn't depend on internal/compute. A thin adapter wires
// the two (see node.go).
type EpisodicTurn struct {
	Channel     string
	ChatID      string
	UserID      string
	UserMessage string
	AssistReply string
	TurnID      string
	CompletedAt time.Time
}

// EpisodicIngester writes per-turn records into the Raft-
// replicated episodic bucket. Dream consolidates them later.
type EpisodicIngester struct {
	raft    raftApplier
	entropy *ulidEntropy
	timeout time.Duration
}

type ulidEntropy struct {
	reader *ulid.MonotonicEntropy
}

// NewEpisodicIngester wires the ingester. ApplyTimeout zero picks
// 5s — long enough for a healthy Raft round-trip, short enough to
// not stall the turn's reply path.
func NewEpisodicIngester(raft raftApplier, applyTimeout time.Duration) (*EpisodicIngester, error) {
	if raft == nil {
		return nil, errors.New("episodic ingester: raft applier required")
	}
	if applyTimeout <= 0 {
		applyTimeout = 5 * time.Second
	}
	return &EpisodicIngester{
		raft:    raft,
		entropy: &ulidEntropy{reader: ulid.Monotonic(cryptorand.Reader, 0)},
		timeout: applyTimeout,
	}, nil
}

// IngestTurn writes one EpisodicRecord summarising the exchange.
// Event is a short synopsis; context carries the full reply text
// so future memory_search hits have content to match against. Tags
// carry channel + user so filtered recall works. Importance 5 is
// the neutral "keep for a while" default; dream re-scores based on
// recall frequency.
func (i *EpisodicIngester) IngestTurn(ctx context.Context, turn EpisodicTurn) error {
	id := ulid.MustNew(ulid.Now(), i.entropy.reader).String()
	tags := []string{}
	if turn.Channel != "" {
		tags = append(tags, "channel:"+turn.Channel)
	}
	if turn.UserID != "" {
		tags = append(tags, "user:"+turn.UserID)
	}
	if turn.ChatID != "" {
		tags = append(tags, "chat:"+turn.ChatID)
	}
	if turn.TurnID != "" {
		tags = append(tags, "turn:"+turn.TurnID)
	}

	event := turnEventSummary(turn.UserMessage)
	rec := &lobslawv1.EpisodicRecord{
		Id:         id,
		Event:      event,
		Context:    turn.UserMessage + "\n\n---\n\n" + turn.AssistReply,
		Importance: 5,
		Timestamp:  timestamppb.New(turn.CompletedAt),
		Tags:       tags,
		Retention:  "session",
	}

	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: id,
		Payload: &lobslawv1.LogEntry_EpisodicRecord{
			EpisodicRecord: rec,
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	res, err := i.raft.Apply(data, i.timeout)
	if err != nil {
		return fmt.Errorf("raft apply: %w", err)
	}
	if fsmErr, ok := res.(error); ok && fsmErr != nil {
		return fmt.Errorf("fsm: %w", fsmErr)
	}
	return nil
}

// turnEventSummary generates a short (max ~140-char) synopsis
// from the user message. Dream reranker will replace this with a
// better LLM-backed summary when it consolidates; this is just
// enough context for substring search to find the record.
func turnEventSummary(userMsg string) string {
	const maxLen = 140
	if len(userMsg) <= maxLen {
		return userMsg
	}
	return userMsg[:maxLen] + "…"
}
