package memory

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/turn"
)

// RaftApplier is what persisting a memory needs from the cluster.
type RaftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// Remember persists one episodic memory and the vector that indexes it.
//
// THE door for creating a memory. There used to be three, and only one
// of them was complete: conversation ingest stamped ownership and
// wrote the paired vector, while Service.EpisodicAdd and the memory_*
// tools each built a record by hand and applied it directly. Both of
// those produced records nobody could read — an unowned record is
// visible only to Everyone() — and that no vector indexed, so they
// were reachable by lexical fallback alone.
//
// The bugs were the symptom. Three callers each assembling a record
// from parts is the thing that made the same two mistakes twice, and
// would have made them a third time for the next caller. This is the
// one place that knows what a complete memory looks like.
//
// Ownership comes from the turn when the caller has not set it, which
// is what makes the door hard to use wrongly: a caller that forgets
// gets the right answer rather than a silent hole.
func Remember(ctx context.Context, raft RaftApplier, embedder Embedder, timeout time.Duration, rec *lobslawv1.EpisodicRecord) (string, error) {
	if raft == nil {
		return "", errors.New("remember: no raft")
	}
	if rec == nil {
		return "", errors.New("remember: no record")
	}

	stampOwnership(ctx, rec)
	if rec.Owner == "" {
		// The same refusal ingest has always made, now made for every
		// caller. A record nobody owns is a record nobody can read, so
		// storing one reports success and loses the memory — which is
		// worse than failing where somebody can see it.
		return "", errors.New("remember: this turn carries no identity, and a record nobody " +
			"owns is a record nobody can read; refusing rather than storing something unreachable")
	}

	if rec.Id == "" {
		rec.Id = ids.New()
	}
	if rec.Timestamp == nil {
		rec.Timestamp = timestamppb.Now()
	}
	if rec.Importance == 0 {
		// Dream scores by importance × recency, so zero would exclude
		// the record from consolidation rather than rank it low.
		rec.Importance = 5
	}
	if rec.Retention == lobslawv1.Retention_RETENTION_UNSPECIFIED {
		rec.Retention = lobslawv1.Retention_RETENTION_EPISODIC
	}
	if timeout <= 0 {
		timeout = DefaultRememberTimeout
	}

	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      rec.Id,
		Payload: &lobslawv1.LogEntry_EpisodicRecord{EpisodicRecord: rec},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return "", fmt.Errorf("remember: marshal: %w", err)
	}
	res, err := raft.Apply(data, timeout)
	if err != nil {
		return "", fmt.Errorf("remember: raft apply: %w", err)
	}
	if fsmErr, ok := res.(error); ok && fsmErr != nil {
		return "", fmt.Errorf("remember: fsm: %w", fsmErr)
	}

	indexVector(ctx, raft, embedder, timeout, rec)
	return rec.Id, nil
}

// DefaultRememberTimeout bounds the raft apply when a caller names no
// timeout.
const DefaultRememberTimeout = 5 * time.Second

// stampOwnership fills what the caller left blank from the turn.
//
// Only what is blank: a caller that knows better — ingest, which has
// the turn's resolved principal already — keeps its own answer.
func stampOwnership(ctx context.Context, rec *lobslawv1.EpisodicRecord) {
	id, _ := turn.IdentityFrom(ctx)
	if rec.Owner == "" {
		rec.Owner = id.Principal.String()
	}
	if rec.Visibility == lobslawv1.Visibility_VISIBILITY_UNSPECIFIED {
		rec.Visibility = ownedVisibility(rec.Owner)
	}
	if rec.SessionRef == "" {
		rec.SessionRef = sessionRefFor(id.Channel, id.ChannelID)
	}
}

// indexVector writes the vector that makes a memory findable by
// meaning rather than by wording.
//
// Best-effort, and after the episodic record commits. That record is
// the memory; this is an index over it, so a failure here costs recall
// quality rather than the memory, and a vector is never written for
// something that failed to land.
func indexVector(ctx context.Context, raft RaftApplier, embedder Embedder, timeout time.Duration, rec *lobslawv1.EpisodicRecord) {
	if embedder == nil {
		return
	}
	text := rec.Context
	if text == "" {
		text = rec.Event
	}
	if text == "" {
		return
	}
	vec, err := embedder.Embed(ctx, text)
	if err != nil {
		return
	}
	vecID := ids.New()
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: vecID,
		Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &lobslawv1.VectorRecord{
			Id:        vecID,
			Embedding: vec,
			Text:      text,
			Scope:     "episodic",
			Retention: rec.Retention,
			// The vector carries the record's ownership. Search reads
			// vectors, so an unowned vector over owned text is the
			// same leak wearing a different hat — and a vector without
			// the session its record carries is invisible in the
			// conversation that produced it.
			Owner:          rec.Owner,
			Visibility:     rec.Visibility,
			SessionRef:     rec.SessionRef,
			CreatedAt:      rec.Timestamp,
			SourceIds:      []string{rec.Id},
			EmbeddingModel: embedder.Model(),
		}},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return
	}
	_, _ = raft.Apply(data, timeout) //nolint:errcheck // best-effort; see doc comment
}
