package main

import (
	"context"
	"flag"
	"fmt"
	"math"
	"os"
	"sort"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/egress"
	"github.com/jmylchreest/lobslaw/internal/embedder"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Judging an embedding model BEFORE committing a corpus to it.
//
// Changing the model is close to a one-way door: vectors from two
// models are not comparable, the node refuses to start until the whole
// corpus is re-embedded, and re-embedding a large store is not quick.
// So the one thing an operator wants — "is this model better for MY
// memories?" — was the one thing there was no way to find out.
//
// Published benchmarks do not answer it. They measure retrieval over
// web documents and academic passages; this corpus is short personal
// facts, queried by paraphrase, with a few thousand records. A model
// can be excellent at one and mediocre at the other.

const embedEvalUsage = `lobslaw embed-eval — measure an embedding model against this node's own memories

  lobslaw embed-eval --config config.toml <model> [<model>...]

Runs each model over the episodic records in this node's store and
reports how well it tells them apart. Use it BEFORE changing
[compute.embeddings] model, because that change is refused at boot
until the corpus is re-embedded.

The node must be STOPPED: bbolt takes an exclusive lock.

THE MEASUREMENT. Each record carries an event (a short summary) and a
context (the fuller text). The event is used as a query and the context
as the document, so a model that understands the record ranks that
record's own context first out of all of them. No hand-written
question set is needed and no judgement is involved — the ground truth
is the pairing already in the store.

  recall@1   how often the right record ranks first
  recall@3   how often it lands in the top three, which is what the
             context engine actually puts in front of the model
  margin     how far ahead the right answer sits. A model with good
             recall and a thin margin makes any similarity threshold
             a coin toss.

Models are named as directory names under <data_dir>/models, and
fetched with --download-url if absent — the same rules as
[compute.embeddings].

  --download-url URL   where to fetch a model that is not present
  --limit N            evaluate at most N records (default 200)
`

func dispatchEmbedEval(args []string) bool {
	idx := findSubcmd(args, "embed-eval")
	if idx < 0 {
		return false
	}
	if err := runEmbedEval(args[idx+1:]); err != nil {
		fmt.Fprintln(os.Stderr, "lobslaw: embed-eval:", err)
		os.Exit(1)
	}
	return true
}

func runEmbedEval(args []string) error {
	fs := flag.NewFlagSet("embed-eval", flag.ExitOnError)
	var store offlineStore
	store.bind(fs)
	downloadURL := fs.String("download-url", "", "where to fetch a model that is not already present")
	limit := fs.Int("limit", 200, "evaluate at most this many records")

	models, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		fmt.Fprint(os.Stderr, embedEvalUsage)
		os.Exit(2)
	}

	st, path, err := store.open()
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	pairs, skipped, err := evalPairs(st, *limit)
	if err != nil {
		return err
	}
	if len(pairs) < 2 {
		return fmt.Errorf("only %d usable records in %s — a model cannot be told apart from another "+
			"on fewer than two, and records whose event and context are identical are skipped "+
			"because retrieving them is free", len(pairs), path)
	}
	modelsDir, err := store.modelsDir()
	if err != nil {
		return err
	}
	fmt.Printf("%d records from %s", len(pairs), path)
	if skipped > 0 {
		fmt.Printf(" (%d skipped: no distinct event/context, or unreadable)", skipped)
	}
	fmt.Printf("\n\n%-34s %6s %9s %9s %9s %10s\n",
		"model", "dims", "recall@1", "recall@3", "margin", "per doc")

	for _, name := range models {
		r, err := evalModel(name, *downloadURL, modelsDir, pairs)
		if err != nil {
			fmt.Printf("%-34s  %v\n", name, err)
			continue
		}
		fmt.Printf("%-34s %6d %8.0f%% %8.0f%% %+9.4f %9.1fms\n",
			name, r.dims,
			100*float64(r.top1)/float64(len(pairs)),
			100*float64(r.top3)/float64(len(pairs)),
			r.margin, r.msPerDoc)
	}
	return nil
}

// evalPair is one record's (query, document) pairing.
type evalPair struct{ query, doc string }

// evalPairs reads usable records out of the store.
//
// Records whose event and context are IDENTICAL are skipped: querying
// a document with itself is a free hit that every model scores, and
// including them inflates every result equally while hiding the
// differences the command exists to show.
func evalPairs(st *memory.Store, limit int) ([]evalPair, int, error) {
	var pairs []evalPair
	var skipped int
	unreadable, err := st.ForEachDecryptable(memory.BucketEpisodicRecords, func(_ string, raw []byte) error {
		if len(pairs) >= limit {
			return nil
		}
		var rec lobslawv1.EpisodicRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			skipped++
			return nil
		}
		if rec.Event == "" || rec.Context == "" || rec.Event == rec.Context {
			skipped++
			return nil
		}
		pairs = append(pairs, evalPair{query: rec.Event, doc: rec.Context})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	return pairs, skipped + unreadable, nil
}

type evalResult struct {
	dims       int
	top1, top3 int
	margin     float64
	msPerDoc   float64
}

func evalModel(name, downloadURL, dataDir string, pairs []evalPair) (evalResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	dir, err := embedder.Ensure(ctx, egress.For("embedding-model").HTTPClient(), dataDir, name, downloadURL)
	if err != nil {
		return evalResult{}, err
	}
	enc, err := embedder.Open(dir)
	if err != nil {
		return evalResult{}, err
	}
	defer func() { _ = enc.Close() }()

	start := time.Now()
	docs := make([][]float32, len(pairs))
	for i, p := range pairs {
		docs[i] = enc.Encode(p.doc)
	}
	msPerDoc := float64(time.Since(start).Milliseconds()) / float64(len(pairs))

	r := evalResult{dims: enc.Dim(), msPerDoc: msPerDoc}
	var marginSum float64
	for i, p := range pairs {
		q := enc.Encode(p.query)
		scores := make([]float64, len(docs))
		for j := range docs {
			scores[j] = cosineSim(q, docs[j])
		}
		order := make([]int, len(scores))
		for j := range order {
			order[j] = j
		}
		sort.SliceStable(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })

		if order[0] == i {
			r.top1++
		}
		for k := 0; k < 3 && k < len(order); k++ {
			if order[k] == i {
				r.top3++
				break
			}
		}
		// Distance from the right answer to the best WRONG one:
		// positive means it won, and by how much.
		best := scores[order[0]]
		if order[0] == i && len(order) > 1 {
			best = scores[order[1]]
		}
		marginSum += scores[i] - best
	}
	r.margin = marginSum / float64(len(pairs))
	return r, nil
}

func cosineSim(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}
