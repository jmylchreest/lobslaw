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
	"encoding/base64"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	cryptorand "crypto/rand"

	"github.com/oklog/ulid/v2"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/compute"
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
	flag.StringVar(&cfgPath, "config", "", "path to lobslaw config.toml")
	// 10 RPM = 6s gap. MiniMax's published docs don't state the
	// embo-01 rate limit; empirically the Token Plan trips 1002
	// at low tens of requests within a burst. Conservative default;
	// bump via --rpm if your tier allows more. Retry-on-1002 saves
	// us if we undershoot.
	flag.IntVar(&rpm, "rpm", 10, "embedding requests per minute (respect provider rate limit)")
	flag.Parse()
	if cfgPath == "" {
		fmt.Fprintln(os.Stderr, "--config required")
		os.Exit(1)
	}

	cfg, err := config.Load(config.LoadOptions{Path: cfgPath})
	if err != nil {
		die("load config: %v", err)
	}
	if cfg.Compute.Embeddings.Endpoint == "" {
		die("compute.embeddings.endpoint is empty — nothing to backfill against")
	}

	keyRaw := os.Getenv("LOBSLAW_MEMORY_KEY")
	if keyRaw == "" {
		die("LOBSLAW_MEMORY_KEY env required")
	}
	keyBytes, err := base64.StdEncoding.DecodeString(keyRaw)
	if err != nil {
		die("decode memory key: %v", err)
	}
	var key crypto.Key
	copy(key[:], keyBytes)

	statePath := filepath.Join(cfg.Cluster.DataDir, "state.db")
	store, err := memory.OpenStore(statePath, key)
	if err != nil {
		die("open state.db at %s: %v (is the node running? stop it first)", statePath, err)
	}
	defer store.Close()

	apiKey, err := config.ResolveSecret(cfg.Compute.Embeddings.APIKeyRef)
	if err != nil {
		die("resolve embedding api key: %v", err)
	}
	ec, err := compute.NewEmbeddingClient(compute.EmbeddingClientConfig{
		Endpoint: cfg.Compute.Embeddings.Endpoint,
		APIKey:   apiKey,
		Model:    cfg.Compute.Embeddings.Model,
		Dims:     cfg.Compute.Embeddings.Dims,
		Format:   compute.EmbeddingFormat(cfg.Compute.Embeddings.Format),
	})
	if err != nil {
		die("embed client: %v", err)
	}

	indexed := loadVectorIndex(store)
	var (
		total      int
		alreadyHas int
		backfilled int
		failed     int
	)
	entropy := ulid.Monotonic(cryptorand.Reader, 0)

	// Collect records needing embedding first, then batch the
	// HTTP calls. 1 HTTP round-trip per batch instead of N.
	type pending struct {
		rec  *lobslawv1.EpisodicRecord
		text string
	}
	var todo []pending
	err = store.ForEach(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
		total++
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			failed++
			return nil
		}
		if indexed[rec.Id] {
			alreadyHas++
			return nil
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
			vecID := ulid.MustNew(ulid.Now(), entropy).String()
			vrec := &lobslawv1.VectorRecord{
				Id:        vecID,
				Embedding: vec,
				Text:      p.text,
				Scope:     "episodic",
				Retention: p.rec.Retention,
				CreatedAt: p.rec.Timestamp,
				SourceIds: []string{p.rec.Id},
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
	fmt.Printf("Backfilled:  %d\n", backfilled)
	fmt.Printf("Failed:      %d\n", failed)
}

// embedWithRetry wraps Embed with backoff on MiniMax's RPM
// rate-limit error (status_code 1002). Other errors bubble
// immediately — only the rate-limit case is worth retrying.
func embedWithRetry(ec *compute.EmbeddingClient, text string) ([]float32, error) {
	var lastErr error
	for attempt := 0; attempt < 5; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		vec, err := ec.Embed(ctx, text)
		cancel()
		if err == nil {
			return vec, nil
		}
		lastErr = err
		msg := err.Error()
		if !isRateLimited(msg) {
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

// embedBatchWithRetry is the batch analogue of embedWithRetry.
// One HTTP round-trip per call; retry the whole batch on rate-limit.
func embedBatchWithRetry(ec *compute.EmbeddingClient, texts []string) ([][]float32, error) {
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
func loadVectorIndex(store *memory.Store) map[string]bool {
	out := map[string]bool{}
	_ = store.ForEach(memory.BucketVectorRecords, func(_ string, raw []byte) error {
		var v lobslawv1.VectorRecord
		if err := proto.Unmarshal(raw, &v); err != nil {
			return nil
		}
		for _, sid := range v.SourceIds {
			out[sid] = true
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
