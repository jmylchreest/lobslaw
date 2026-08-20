// Package main is a one-off migration that generates vector
// records for every episodic record that doesn't already have one.
// Run once after enabling embeddings on a deployment that has
// historical memories written without vectors.
//
// Usage:
//
//	export LOBSLAW_MEMORY_KEY=...
//	export MINIMAX_API_KEY=...   # or whichever provider is configured
//	go run ./cmd/backfill-embeddings --config ~/.config/lobslaw/config.toml
//
// Idempotent: skips episodic records that already have a
// VectorRecord pointing at them (via source_ids). Safe to re-run.
//
// WARNING: runs OUTSIDE the live cluster — reads state.db
// directly with ReadOnly semantics. Stop the node first; bbolt
// file-locks prevent concurrent writers.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/embedder"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/crypto"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func main() {
	var (
		cfgPath string
		rpm     int
	)
	var force bool
	flag.StringVar(&cfgPath, "config", "", "path to lobslaw config.toml")
	// 10 RPM = 6s gap. MiniMax's published docs don't state the
	// embo-01 rate limit; empirically the Token Plan trips 1002
	// at low tens of requests within a burst. Conservative default;
	// bump via --rpm if your tier allows more. Retry-on-1002 saves
	// us if we undershoot.
	flag.IntVar(&rpm, "rpm", 10, "embedding requests per minute (respect provider rate limit)")
	flag.BoolVar(&force, "force", false,
		"re-embed records that ALREADY have a vector, deleting the old one. "+
			"Required after changing the embedding model: vectors from two "+
			"models are not comparable, and the default gap-filling mode "+
			"skips every record precisely because it already has one.")
	flag.Parse()
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "--config required")
		os.Exit(1)
	}

	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		die("load config: %v", err)
	}
	if !cfg.Compute.Embeddings.Configured() {
		die("[compute.embeddings] is not configured — nothing to backfill against")
	}

	keyRaw := os.Getenv("LOBSLAW_MEMORY_KEY")
	if keyRaw == "" {
		die("LOBSLAW_MEMORY_KEY env required")
	}
	// crypto.ParseKey, NOT a bare base64 decode.
	//
	// ParseKey accepts hex OR base64, which is what the node uses. This
	// decoded base64 only — and a 64-character hex key IS valid base64:
	// it decodes to 48 bytes of nonsense, the first 32 become the
	// "key", and the failure surfaces as
	//
	//	decrypt failed (bad key, nonce, or ciphertext)
	//
	// on the first record, with nothing pointing at the key parsing.
	// Two implementations of one concept, and the wrong one failed
	// silently.
	key, err := crypto.ParseKey(strings.TrimSpace(keyRaw))
	if err != nil {
		die("parse memory key: %v", err)
	}

	statePath := filepath.Join(cfg.Cluster.DataDir, "state.db")
	store, err := memory.OpenStore(statePath, key)
	if err != nil {
		die("open state.db at %s: %v (is the node running? stop it first)", statePath, err)
	}
	// bbolt fsyncs on every Update, so durability does not depend on
	// this Close; it only releases the file lock and mmap at exit.
	defer func() { _ = store.Close() }()

	// Resolved through one function so main does not branch on the
	// embedder kind; see newEmbedder for why builtin had to be added.
	ec, closeEmbedder := newEmbedder(cfg)
	defer closeEmbedder()

	indexed := loadVectorIndex(store)
	var (
		total      int
		alreadyHas int
		backfilled int
		failed     int
	)

	// Collect records needing embedding first, then batch the
	// HTTP calls. 1 HTTP round-trip per batch instead of N.
	type pending struct {
		rec  *lobslawv1.EpisodicRecord
		text string
	}
	var todo []pending
	// Stale vectors are deleted only AFTER their replacements are
	// written, so an interrupted --force run leaves a record with an
	// old vector rather than none at all.
	var replacing []string
	err = store.ForEach(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
		total++
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			failed++
			return nil
		}
		if stale, has := indexed[rec.Id]; has {
			if !force {
				alreadyHas++
				return nil
			}
			replacing = append(replacing, stale)
		}
		text := rec.Context
		if text == "" {
			text = rec.Event
		}
		if text == "" {
			return nil
		}
		todo = append(todo, pending{rec: &rec, text: text})
		return nil
	})
	if err != nil {
		// A decrypt failure here is almost always the KEY, not the
		// data, and "decrypt failed (bad key, nonce, or ciphertext)"
		// does not say which. total counts the records walked before
		// the abort, so the two cases are distinguishable: nothing
		// walked means the very first record failed.
		if strings.Contains(err.Error(), "decrypt") {
			if total <= 1 {
				die("cannot decrypt the store — LOBSLAW_MEMORY_KEY does not match the key this\n"+
					"  data was written with. It must be the same value [memory.encryption]\n"+
					"  key_ref resolves to for the node.\n\n  %v", err)
			}
			die("decrypted %d records then failed on one — that record predates a key\n"+
				"  rotation, or is corrupt. Re-embedding cannot skip it without leaving\n"+
				"  a gap it would never report.\n\n  %v", total, err)
		}
		die("scan episodic: %v", err)
	}

	if len(todo) == 0 {
		fmt.Println("No records need backfilling.")
	}

	// Pacing between batches — most providers rate-limit on
	// requests, not tokens, so batching is a direct QPS win
	// even at aggressive --rpm. MiniMax is the exception (rate
	// limit is per-call regardless of payload size).
	if rpm < 1 {
		rpm = 1
	}
	gap := time.Minute / time.Duration(rpm)

	// Process in batches. 32 is the sweet spot for most
	// providers (OpenAI accepts up to 2048; Qwen / DeepInfra
	// similar). Too-large batches risk exceeding the context
	// window and tripping "input too long" errors.
	const batchSize = 32
	for start := 0; start < len(todo); start += batchSize {
		end := start + batchSize
		if end > len(todo) {
			end = len(todo)
		}
		chunk := todo[start:end]
		texts := make([]string, len(chunk))
		for i, p := range chunk {
			texts[i] = p.text
		}
		vecs, err := embedBatchWithRetry(ec, texts)
		if err != nil {
			for _, p := range chunk {
				fmt.Fprintf(os.Stderr, "  [FAIL] %s: %v\n", p.rec.Id, err)
			}
			failed += len(chunk)
			time.Sleep(gap)
			continue
		}
		for i, p := range chunk {
			vec := vecs[i]
			if len(vec) == 0 {
				failed++
				continue
			}
			vecID := ids.New()
			vrec := &lobslawv1.VectorRecord{
				Id:        vecID,
				Embedding: vec,
				Text:      p.text,
				Scope:     "episodic",
				Retention: p.rec.Retention,
				CreatedAt: p.rec.Timestamp,
				SourceIds: []string{p.rec.Id},
				// OWNERSHIP WAS MISSING ENTIRELY, and the effect was
				// silent: Audience.allows admits a record only when it
				// is SHARED or owned by the caller, so every vector
				// this tool wrote was unreadable by any scoped search.
				// It reported "Backfilled: N" and produced N rows that
				// neither memory_search nor passive recall could ever
				// return. The ingest path has always copied these two
				// fields, with the comment that an unowned vector over
				// owned text is the leak wearing a different hat.
				Owner:      p.rec.Owner,
				Visibility: p.rec.Visibility,
				// Stamped so a later model change is refused at boot
				// rather than silently scoring across two vector spaces.
				EmbeddingModel: cfg.Compute.Embeddings.Model,
			}
			vraw, err := proto.Marshal(vrec)
			if err != nil {
				failed++
				continue
			}
			if err := store.Put(memory.BucketVectorRecords, vecID, vraw); err != nil {
				fmt.Fprintf(os.Stderr, "  [WRITE-FAIL] %s: %v\n", p.rec.Id, err)
				failed++
				continue
			}
			backfilled++
			fmt.Printf("  [OK] %s → vec=%s (%d dims)\n", p.rec.Id, vecID, len(vec))
		}
		if end < len(todo) {
			time.Sleep(gap)
		}
	}

	// Note: direct store.Put writes BYPASS Raft consensus. For a
	// single-node deployment that's fine (no followers to diverge).
	// For a multi-node cluster this would desync — migration has
	// to propose each VectorRecord via Apply instead. Extension
	// left deliberate since single-node is the common case.

	fmt.Println()
	fmt.Printf("Scanned:     %d episodic records\n", total)
	fmt.Printf("Had vector:  %d (skipped)\n", alreadyHas)
	// Ordered after the writes for a reason: if the process dies
	// mid-run, a record with a superseded vector still returns
	// something, whereas one with no vector returns nothing at all.
	var removed int
	for _, id := range replacing {
		if err := store.Delete(memory.BucketVectorRecords, id); err == nil {
			removed++
		}
	}
	if removed > 0 {
		fmt.Printf("Replaced:    %d stale vector(s)\n", removed)
	}
	fmt.Printf("Backfilled:  %d\n", backfilled)
	fmt.Printf("Failed:      %d\n", failed)
}

// embedBatchWithRetry is the batch analogue of embedWithRetry.
// One HTTP round-trip per call; retry the whole batch on rate-limit.
// Takes the INTERFACE, not the HTTP client, so a builtin model goes
// through the same retry and batching path. The retries are inert for
// an in-process embedder — there is nothing transient to survive — but
// one code path is worth more than a saved branch.
func embedBatchWithRetry(ec compute.EmbeddingProvider, texts []string) ([][]float32, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		vecs, err := ec.EmbedBatch(ctx, texts)
		cancel()
		if err == nil {
			return vecs, nil
		}
		lastErr = err
		if !isRateLimited(err.Error()) {
			return nil, err
		}
		wait := time.Duration(5<<attempt) * time.Second
		if wait > 60*time.Second {
			wait = 60 * time.Second
		}
		fmt.Fprintf(os.Stderr, "  [RATE-LIMIT] %v — sleeping %s\n", err, wait)
		time.Sleep(wait)
	}
	return nil, fmt.Errorf("rate-limited after retries: %w", lastErr)
}

func isRateLimited(msg string) bool {
	// MiniMax: "minimax status 1002: rate limit exceeded(RPM)"
	// OpenAI / generic:    "HTTP 429"
	return containsAny(msg, "1002", "rate limit", "HTTP 429")
}

func containsAny(hay string, needles ...string) bool {
	for _, n := range needles {
		if n == "" {
			continue
		}
		if idx := indexOf(hay, n); idx >= 0 {
			return true
		}
	}
	return false
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// loadVectorIndex returns a set of episodic IDs that already have
// at least one VectorRecord pointing at them via source_ids.
// loadVectorIndex maps each embedded episodic id to the vector record
// that embeds it. The VECTOR ID is kept, not just a bool, because
// --force has to delete the stale vector rather than leave two rows
// pointing at one record — the second of which would be scored against
// a query it shares no vector space with.
func loadVectorIndex(store *memory.Store) map[string]string {
	out := map[string]string{}
	_ = store.ForEach(memory.BucketVectorRecords, func(key string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		for _, sid := range v.SourceIds {
			out[sid] = key
		}
		return nil
	})
	return out
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "backfill-embeddings: "+format+"\n", args...)
	os.Exit(1)
}

// suppress unused imports on some Go versions
var _ = timestamppb.Now

// backfillEmbeddingFactory picks the wire shape for this one-shot tool.
//
// A local switch rather than a DriverSet: the backfill is a standalone
// binary with no node to assemble one, and two cases here is cheaper
// than exporting the wiring layer's table for a tool that runs by hand.
func backfillEmbeddingFactory(name string) compute.EmbeddingDriverFactory {
	if strings.EqualFold(strings.TrimSpace(name), compute.DriverMiniMax) {
		return compute.MiniMaxEmbeddingFactory
	}
	return compute.OpenAIEmbeddingFactory
}

// openBuiltin resolves and loads the in-process model.
//
// Downloads it if download_url is set and it is absent, exactly as the
// node does at boot — so re-embedding after a model change does not
// require the operator to have fetched it by hand first.
func openBuiltin(cfg *config.Config) (*embedder.Encoder, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	// Through egress like the node's own fetch, not http.DefaultClient
	// — a lint rule enforces this, and rightly: a tool that reached the
	// internet outside the policy would be a hole the policy cannot see.
	dir, err := embedder.Ensure(ctx, egress.For("embedding-model").HTTPClient(), cfg.Cluster.DataDir,
		cfg.Compute.Embeddings.Model, cfg.Compute.Embeddings.DownloadURL)
	if err != nil {
		return nil, err
	}
	return embedder.Open(dir)
}

// newEmbedder builds whichever embedder the config asks for.
//
// BUILTIN MODELS HAVE TO WORK HERE, and did not. This tool required an
// endpoint, so with type = "builtin" it refused to start — while
// memory.CheckEmbeddingModel's error tells the operator to run exactly
// this command to recover from a model change. A node running a local
// model could therefore never change it: refused at boot, and refused
// by the only tool offered as the way out.
func newEmbedder(cfg *config.Config) (compute.EmbeddingProvider, func()) {
	if cfg.Compute.Embeddings.Builtin() {
		enc, err := openBuiltin(cfg)
		if err != nil {
			die("builtin embeddings: %v", err)
		}
		return compute.NewBuiltinEmbedder(enc, cfg.Compute.Embeddings.Model),
			func() { _ = enc.Close() }
	}
	apiKey, err := config.ResolveSecret(cfg.Compute.Embeddings.APIKeyRef)
	if err != nil {
		die("resolve embedding api key: %v", err)
	}
	ec, err := compute.NewEmbeddingClient(compute.EmbeddingClientConfig{
		Endpoint:      cfg.Compute.Embeddings.Endpoint,
		APIKey:        apiKey,
		Model:         cfg.Compute.Embeddings.Model,
		Dims:          cfg.Compute.Embeddings.Dims,
		DriverFactory: backfillEmbeddingFactory(cfg.Compute.Embeddings.Format),
	})
	if err != nil {
		die("embed client: %v", err)
	}
	return ec, func() {}
}
