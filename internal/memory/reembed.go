package memory

import (
	"context"
	"time"

	"github.com/jmylchreest/lobslaw/internal/ids"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Re-embedding the corpus, ON THE NODE, THROUGH RAFT.
//
// The offline backfill-embeddings tool writes state.db directly. The
// node rebuilds state from its raft log on boot, so a PUT still in the
// log re-applies and resurrects anything that tool deleted: its writes
// survived and its deletions did not. Measured on a real store — two
// --force runs back to back converge, and a restart between them
// brings the same keys back, every time.
//
// That meant a model change could never fully land offline. A single
// resurrected vector carrying the old model's stamp is enough for
// CheckEmbeddingModel to refuse the next boot, and no amount of
// re-running the tool fixed it.
//
// Doing it here costs nothing extra. The node already holds the
// embedder, the store and the log, and every other mutation in this
// package already goes through raft — this one was the exception, for
// no better reason than having started life as a migration script.

// ReembedEmbedder is the minimum this needs: text in, vector out, plus
// the identity to stamp. Deliberately narrower than the compute
// package's provider so this package keeps no dependency on it.
type ReembedEmbedder interface {
	Embed(ctx context.Context, text string) ([]float32, error)
	Model() string
}

// SetEmbedder attaches the model used by Reembed.
//
// Set after construction because the embedder is wired later than the
// memory service — and it may be absent entirely, which is a supported
// configuration rather than an error.
func (s *Service) SetEmbedder(e ReembedEmbedder) { s.embedder = e }

// Reembed rewrites every episodic record's vector with the current
// model, and removes the ones the previous model left behind.
func (s *Service) Reembed(ctx context.Context, req *lobslawv1.ReembedRequest) (*lobslawv1.ReembedResponse, error) {
	if s.raft == nil {
		return nil, status.Error(codes.FailedPrecondition, "this node hosts no raft, so nothing can be written")
	}
	if s.embedder == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"no embedder is configured — set [compute.embeddings] and restart")
	}
	model := s.embedder.Model()

	// Which vector currently embeds which record. Built first so a
	// record's old vector can be deleted once its replacement is in.
	existing := map[string][]string{}
	if _, err := s.store.ForEachDecryptable(BucketVectorRecords, func(k string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		// EXACTLY ONE source, or it is not this record's vector.
		//
		// Dream's consolidations are vector records too, and they list
		// every record they merged. Keying on each source in turn made
		// re-embedding any one of them delete the consolidation that
		// contained it — destroying dream's output on every run. Caught
		// on a real store: the second pass reported 77 superseded
		// vectors for 57 records, and the extra 20 were consolidations.
		//
		// A consolidation is not re-embedded here either. Its text is a
		// summary this package cannot regenerate; dream owns it, and
		// dream will rewrite it on its next pass.
		if len(v.SourceIds) == 1 {
			existing[v.SourceIds[0]] = append(existing[v.SourceIds[0]], k)
		}
		return nil
	}); err != nil {
		return nil, status.Errorf(codes.Internal, "scan vector records: %v", err)
	}

	type job struct {
		rec  *lobslawv1.EpisodicRecord
		text string
	}
	var todo []job
	limit := int(req.GetLimit())
	unreadable, err := s.store.ForEachDecryptable(BucketEpisodicRecords, func(_ string, raw []byte) error {
		if limit > 0 && len(todo) >= limit {
			return nil
		}
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return nil
		}
		text := rec.Context
		if text == "" {
			text = rec.Event
		}
		if text == "" {
			return nil
		}
		todo = append(todo, job{rec: proto.Clone(&rec).(*lobslawv1.EpisodicRecord), text: text})
		return nil
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "scan episodic records: %v", err)
	}

	resp := &lobslawv1.ReembedResponse{Model: model, Unreadable: int32(unreadable)} //nolint:gosec // counts, not attacker-controlled
	for _, j := range todo {
		vec, eerr := s.embedder.Embed(ctx, j.text)
		if eerr != nil {
			return resp, status.Errorf(codes.Internal, "embed %s: %v", j.rec.Id, eerr)
		}
		if err := s.putVector(j.rec, j.text, vec, model); err != nil {
			return resp, err
		}
		resp.Reembedded++
		// Deleted AFTER the replacement is in, so an interrupted run
		// leaves a record with a superseded vector rather than none.
		for _, old := range existing[j.rec.Id] {
			if err := s.deleteVector(old); err != nil {
				return resp, err
			}
			resp.Replaced++
		}
	}

	orphans, err := s.sweepOrphanVectors(model)
	if err != nil {
		return resp, err
	}
	resp.Orphans = int32(orphans) //nolint:gosec // a count
	s.logger.Info("memory: re-embedded corpus", "model", model,
		"reembedded", resp.Reembedded, "replaced", resp.Replaced,
		"orphans", resp.Orphans, "unreadable", resp.Unreadable)
	return resp, nil
}

// putVector proposes a replacement vector.
func (s *Service) putVector(rec *lobslawv1.EpisodicRecord, text string, vec []float32, model string) error {
	v := &lobslawv1.VectorRecord{
		Id:        ids.New(),
		Embedding: vec,
		Text:      text,
		Scope:     "episodic",
		Retention: rec.Retention,
		// The vector carries the episodic record's ownership. It has
		// to: search reads vectors, so an unowned vector over owned
		// text is the leak wearing a different hat.
		Owner:          rec.Owner,
		Visibility:     rec.Visibility,
		CreatedAt:      rec.Timestamp,
		SourceIds:      []string{rec.Id},
		EmbeddingModel: model,
	}
	return s.apply(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      v.Id,
		Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: v},
	})
}

func (s *Service) deleteVector(id string) error {
	return s.apply(&lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_DELETE,
		Id:      id,
		Payload: &lobslawv1.LogEntry_VectorRecord{VectorRecord: &lobslawv1.VectorRecord{Id: id}},
	})
}

// sweepOrphanVectors removes vectors stamped with a DIFFERENT model.
//
// A vector no episodic record points at is never revisited by the loop
// above — a consolidation whose sources were pruned, or a row left by
// an interrupted run. It keeps the old stamp, and one is enough for
// CheckEmbeddingModel to refuse the next boot.
//
// Only a stamp that DISAGREES is removed. One carrying the current
// model is current, and an unstamped vector predates the field —
// destroying that on a guess would lose something nothing said was
// wrong.
func (s *Service) sweepOrphanVectors(model string) (int, error) {
	var stale []string
	if _, err := s.store.ForEachDecryptable(BucketVectorRecords, func(k string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		if v.EmbeddingModel != "" && v.EmbeddingModel != model {
			stale = append(stale, k)
		}
		return nil
	}); err != nil {
		return 0, status.Errorf(codes.Internal, "scan vector records: %v", err)
	}
	for _, id := range stale {
		if err := s.deleteVector(id); err != nil {
			return 0, err
		}
	}
	return len(stale), nil
}

func (s *Service) apply(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return status.Errorf(codes.Internal, "marshal log entry: %v", err)
	}
	if _, err := s.raft.Apply(data, 5*time.Second); err != nil {
		return status.Errorf(codes.Internal, "apply %s: %v", entry.Id, err)
	}
	return nil
}
