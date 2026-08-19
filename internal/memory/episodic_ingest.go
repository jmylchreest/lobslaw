package memory

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/textutil"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/promptguard"
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
	Channel string
	ChatID  string
	UserID  string
	// Owner is the canonical principal this memory belongs to. Empty
	// for an anonymous turn, which writes an unowned record — readable
	// by anyone, like every record written before ownership existed.
	Owner       string
	UserMessage string
	AssistReply string
	TurnID      string
	CompletedAt time.Time
}

// Embedder produces a vector embedding for a piece of text. Kept
// as a narrow interface so the compute layer can provide the
// implementation without internal/memory depending on it.
type Embedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	// Model is stamped onto every vector this ingester writes, so a
	// later model swap is detectable rather than silently corrupting
	// every subsequent search.
	Model() string
}

// EpisodicIngester writes per-turn records into the Raft-
// replicated episodic bucket. Dream consolidates them later.
// When an Embedder is configured, each ingest also writes a
// VectorRecord whose embedding indexes the turn body — that's
// what makes memory_search's semantic strategy work.
type EpisodicIngester struct {
	raft     raftApplier
	timeout  time.Duration
	embedder Embedder
}

// NewEpisodicIngester wires the ingester. ApplyTimeout zero picks
// 5s — long enough for a healthy Raft round-trip, short enough to
// not stall the turn's reply path. Embedder nil → substring-only
// recall, no vector records written.
func NewEpisodicIngester(raft raftApplier, applyTimeout time.Duration, embedder Embedder) (*EpisodicIngester, error) {
	if raft == nil {
		return nil, errors.New("episodic ingester: raft applier required")
	}
	if applyTimeout <= 0 {
		applyTimeout = 5 * time.Second
	}
	return &EpisodicIngester{
		raft:     raft,
		timeout:  applyTimeout,
		embedder: embedder,
	}, nil
}

// IngestTurn writes one EpisodicRecord summarising the exchange.
// Event is a short synopsis; context carries the full reply text
// so future memory_search hits have content to match against. Tags
// carry channel + user so filtered recall works. Importance 5 is
// the neutral "keep for a while" default; dream re-scores based on
// recall frequency.
func (i *EpisodicIngester) IngestTurn(ctx context.Context, turn EpisodicTurn) error {
	// An unowned record is readable by nobody, so writing one produces
	// a memory that exists, costs storage, and can never be recalled —
	// a silent hole rather than a visible failure. Every Claims
	// construction in the tree yields a user id, so reaching here
	// without an owner means a caller built a turn without identity.
	// Refuse, loudly, rather than persisting something inert.
	if turn.Owner == "" {
		return fmt.Errorf("episodic ingest: turn has no owner (channel=%q user=%q); "+
			"a record nobody owns is a record nobody can read", turn.Channel, turn.UserID)
	}
	id := ids.New()
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

	// Scan the stored text, not just the user's half: a reply that
	// quotes a fetched page carries whatever that page said. A finding
	// quarantines rather than drops — the record is usually the
	// evidence, and a silently discarded memory is undebuggable.
	stored := turn.UserMessage + "\n\n---\n\n" + turn.AssistReply
	if f, ok := promptguard.Suspicious(stored); ok {
		tags = append(tags, promptguard.Tag(f))
		slog.Default().Warn("episodic ingest: record quarantined by promptguard",
			"id", id, "detector", f.Detector, "detail", f.Detail, "owner", turn.Owner)
	}

	// Sanitised as well as truncated: this process cutting a character
	// in half was one source of invalid UTF-8, and a provider or a
	// channel handing us some is another. Either way the marshal
	// fails three layers from the cause and the memory is lost with a
	// warning nobody reads.
	event := textutil.Sanitise(turnEventSummary(turn.UserMessage))
	rec := &lobslawv1.EpisodicRecord{
		Id:         id,
		Event:      event,
		Context:    textutil.Sanitise(stored),
		Importance: 5,
		Timestamp:  timestamppb.New(turn.CompletedAt),
		Tags:       tags,
		Retention:  lobslawv1.Retention_RETENTION_SESSION,
		Owner:      turn.Owner,
		Visibility: ownedVisibility(turn.Owner),
		// A pointer back to the conversation, so a recall hit can offer
		// to read the surrounding thread. The thread, not the message:
		// ingest runs before the transcript append, so the sequence
		// number does not exist yet. Advisory either way — the
		// transcript is capped and independently forgettable, so a dead
		// pointer means the link is stale, never that the memory is.
		SessionRef: sessionRefFor(turn.Channel, turn.ChatID),
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

	// Paired vector record: embed the turn body so memory_search's
	// semantic strategy has content to match against. Embedding is
	// best-effort — a failure here doesn't roll back the episodic
	// write because losing one turn's vector is better than losing
	// the episodic content entirely.
	if i.embedder != nil {
		embedText := rec.Context
		if embedText == "" {
			embedText = rec.Event
		}
		if vec, verr := i.embedder.Embed(ctx, embedText); verr == nil {
			vecID := ids.New()
			vrec := &lobslawv1.VectorRecord{
				Id:        vecID,
				Embedding: vec,
				Text:      embedText,
				Scope:     "episodic",
				Retention: rec.Retention,
				// The vector carries the same ownership as the episodic
				// record it embeds. It has to: search reads vectors, so
				// an unowned vector over owned text is the leak wearing
				// a different hat.
				Owner:      rec.Owner,
				Visibility: rec.Visibility,
				CreatedAt:  rec.Timestamp,
				SourceIds:  []string{rec.Id},
				// Stamped here rather than in the FSM, where Norm is
				// derived: the model is NODE CONFIG, and two replicas
				// could legitimately hold different values, so an FSM
				// that stamped it would not be deterministic.
				EmbeddingModel: i.embedder.Model(),
			}
			ventry := &lobslawv1.LogEntry{
				Op: lobslawv1.LogOp_LOG_OP_PUT,
				Id: vecID,
				Payload: &lobslawv1.LogEntry_VectorRecord{
					VectorRecord: vrec,
				},
			}
			vdata, merr := proto.Marshal(ventry)
			if merr == nil {
				// Best-effort: the episodic record above is the
				// source of truth; this vector index entry is a
				// derived performance optimisation. If the raft
				// Apply fails the vector record is missing but the
				// episodic record persists — search still works via
				// the substring fallback. Surfacing this would
				// double-log the same root cause.
				_, _ = i.raft.Apply(vdata, i.timeout) //nolint:errcheck // see comment above
			}
		}
	}
	return nil
}

// turnEventSummary generates a short (max ~140-char) synopsis
// from the user message. Dream reranker will replace this with a
// better LLM-backed summary when it consolidates; this is just
// enough context for substring search to find the record.
func turnEventSummary(userMsg string) string {
	// Runes, not bytes. userMsg[:140] cut a Telegram paste inside an
	// em dash, protobuf refused the record, and the turn answered
	// with nothing remembered about it — silently, because the ingest
	// is a background goroutine whose error is logged and dropped.
	const maxRunes = 140
	return textutil.Truncate(userMsg, "…", maxRunes)
}

// ownedVisibility picks the default visibility for a newly written
// record. Anything with an owner is private to them; anything without
// one is legacy-shaped and stays readable, which is what keeps an
// upgrade from hiding a single-user node's whole memory.
func ownedVisibility(owner string) lobslawv1.Visibility {
	if owner == "" {
		return lobslawv1.Visibility_VISIBILITY_UNSPECIFIED
	}
	return lobslawv1.Visibility_VISIBILITY_PRIVATE
}

// sessionRefFor addresses the conversation a memory came from. Empty
// when the turn had no channel origin — a scheduled task has no thread
// to point at.
func sessionRefFor(channel, chatID string) string {
	if channel == "" || chatID == "" {
		return ""
	}
	return channel + ":" + chatID
}
