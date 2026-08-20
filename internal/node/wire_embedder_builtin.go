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
	client := egress.For("embeddings")

	// Bounded: a stalled mirror must not hold start-up open forever.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	dir, err := embedder.Ensure(ctx, client.HTTPClient(), n.cfg.DataDir, cfg.Model, cfg.DownloadURL)
	if err != nil {
		return nil, fmt.Errorf("builtin embeddings: %w", err)
	}

	m, err := embedder.Load(dir)
	if err != nil {
		return nil, fmt.Errorf("builtin embeddings: load %s: %w", cfg.Model, err)
	}

	// dims is CHECKED against the checkpoint rather than trusted from
	// config. For a remote embedder nothing can verify it until the
	// first call fails; here the answer is on disk, so a mismatch is a
	// start-up error instead of a corpus of vectors at the wrong width.
	if cfg.Dims != 0 && cfg.Dims != m.Dim() {
		return nil, fmt.Errorf("builtin embeddings: [compute.embeddings] dims = %d but %s produces %d",
			cfg.Dims, cfg.Model, m.Dim())
	}

	n.log.Info("compute: builtin embedding model ready",
		"model", cfg.Model, "dims", m.Dim(), "max_seq", m.MaxSeq(),
		"pooling", string(m.Pooling()), "kernel", embedder.Kernel())

	// NOT REGISTERED AS THE NODE'S EMBEDDER YET.
	//
	// EmbeddingProvider takes TEXT, and turning text into token ids
	// needs the SentencePiece tokenizer, which is the next piece of
	// work. Returning a provider whose Embed always failed would be
	// worse than returning none: memory_search and the context engine
	// fall back to lexical on an embedding error (see #165), so every
	// single turn would take the slow path and log a warning to say so.
	//
	// Returning nil takes the same lexical path silently and honestly,
	// having still proved at boot that the model downloads, loads, and
	// is the width the operator claimed.
	n.log.Info("compute: builtin embeddings await the tokenizer — recall stays lexical for now")
	return nil, nil
}
