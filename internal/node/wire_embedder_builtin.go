package node

import (
	"context"
	"fmt"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/embedder"
)

// An embedding model this node runs itself.
//
// Prepared AT BOOT rather than on first recall, and that ordering is
// the whole point. A model is a gigabyte-scale download and a
// multi-second load; discovering at the first turn that the URL is
// wrong, the disk is full, or the checkpoint is a dtype we refuse
// would mean one user's message paying for everybody's
// misconfiguration. It also means `lobslaw` either starts correctly or
// says why.
func (n *Node) wireBuiltinEmbedder() (compute.EmbeddingProvider, error) {
	cfg := n.cfg.Compute.Embeddings

	// The download is egress like any other, so it goes through the
	// node's policy rather than around it. A node whose policy forbids
	// the host fails here, loudly, instead of quietly reaching the
	// internet from a code path nobody associated with network access.
	//
	// "embedding-model", NOT "embedding": the latter is the allowance
	// for calling an embedding API and carries the LLM provider hosts.
	// A model is fetched from a mirror, which is a different host and a
	// different decision.
	client := egress.For("embedding-model")

	// Bounded: a stalled mirror must not hold start-up open forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	dir, err := embedder.Ensure(ctx, client.HTTPClient(), n.cfg.DataDir, cfg.Model, cfg.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("builtin embeddings: %w", err)
	}

	enc, err := embedder.Open(dir)
	if err != nil {
		return nil, fmt.Errorf("builtin embeddings: load %s: %w", cfg.Model, err)
	}

	// dims is CHECKED against the checkpoint rather than trusted from
	// config. For a remote embedder nothing can verify it until the
	// first call fails; here the answer is on disk, so a mismatch is a
	// start-up error instead of a corpus of vectors at the wrong width.
	if cfg.Dims != 0 && cfg.Dims != enc.Dim() {
		_ = enc.Close()
		return nil, fmt.Errorf("builtin embeddings: [compute.embeddings] dims = %d but %s produces %d",
			cfg.Dims, cfg.Model, enc.Dim())
	}

	n.log.Info("compute: builtin embedding model ready",
		"model", cfg.Model, "dims", enc.Dim(), "max_seq", enc.MaxSeq(), "vocab", enc.VocabSize(),
		"kernel", embedder.Kernel())

	// The model name is what gets stamped on every vector, so it is
	// also what memory.CheckEmbeddingModel compares against the corpus
	// already on disk: swapping models without re-embedding is refused
	// at boot rather than silently scoring across two vector spaces.
	return compute.NewBuiltinEmbedder(enc, cfg.Model).
		WithPrefixes(cfg.QueryPrefix, cfg.PassagePrefix), nil
}
