package memory

import (
	"context"
	"math"
	"sort"
	"strings"
	"unicode"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Exact-name matching only catches a proposer that happens to reuse
// the same string. "tidy-notes" and "tidying-notes" are two artefacts
// that do one job, and nothing downstream will ever reconcile them —
// the curator sees two things it has no basis to merge, and the model
// sees two entries in its index that contradict each other about how
// to do the same task.
//
// So a near-duplicate search runs before anything new is accepted, and
// the proposer has to say which it is.
//
// NOT auto-adjudicated, deliberately. Deciding
// merge/conflict/supersedes automatically is a plausible thing to
// want here and would be wrong: a memory is data the model may weigh,
// a skill is an instruction it follows. Silently merging two
// instructions produces a third that nobody wrote.

// SimilarityThreshold is the score above which a candidate is close
// enough that accepting a new artefact without a declared intent is
// refused.
//
// Lower than the 0.88 used to cluster memories for consolidation,
// because the costs are asymmetric. A false positive here costs one
// extra field on a call — the proposer says "distinct" and moves on. A
// false negative creates a duplicate instruction that persists and
// that nothing downstream can reconcile.
const SimilarityThreshold = 0.72

// SimilarArtefact is one near-duplicate and how close it is.
type SimilarArtefact struct {
	Record     *lobslawv1.SelfTaughtRecord
	Similarity float64
	// Why names which signal matched, so a proposer told "this looks
	// like an existing skill" can see whether that was the name, the
	// description, or the meaning.
	Why string
}

// Similar ranks existing artefacts against a candidate.
//
// Two signals. Lexical always runs: it needs no dependency, cannot
// fail, and catches the common case of a near-identical name.
// Semantic runs when an embedder is wired, and catches the case
// lexical cannot — "tidy-notes" against "organise-my-scratchpad".
//
// The higher of the two wins rather than an average. A pair that is
// lexically identical and semantically distant is still a collision
// worth stopping on, and averaging would hide it.
func (s *SelfTaughtStore) Similar(ctx context.Context, candidate *lobslawv1.SelfTaughtRecord, limit int) ([]SimilarArtefact, error) {
	if candidate == nil {
		return nil, nil
	}
	existing, err := s.List(SelfTaughtQuery{Kind: candidate.Kind})
	if err != nil {
		return nil, err
	}

	var candidateVec []float32
	if s.embedder != nil {
		// A configured embedder that errors is a hard failure, not a
		// silent downgrade to lexical. The check exists to stop
		// duplicates, and an operator who wired an embedder should not
		// discover it has been quietly skipped by finding the
		// duplicates it was meant to prevent.
		candidateVec, err = s.embedder.Embed(ctx, similarityText(candidate))
		if err != nil {
			return nil, err
		}
	}

	out := make([]SimilarArtefact, 0, len(existing))
	for _, rec := range existing {
		if rec.Id == candidate.Id {
			// The same artefact is not its own near-duplicate; an
			// explicit refinement addresses it by id.
			continue
		}
		lex := lexicalSimilarity(candidate, rec)
		best, why := lex, "name and description overlap"
		if len(candidateVec) > 0 && len(rec.Embedding) > 0 {
			if sem := cosine(candidateVec, rec.Embedding); sem > best {
				best, why = sem, "similar meaning"
			}
		}
		if best <= 0 {
			continue
		}
		out = append(out, SimilarArtefact{Record: rec, Similarity: best, Why: why})
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Similarity > out[j].Similarity })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// similarityText is what gets embedded: the short discriminating
// fields first, then the body. Name and description carry most of the
// signal for a skill, and putting them first keeps them inside the
// window of an embedder that truncates.
func similarityText(rec *lobslawv1.SelfTaughtRecord) string {
	return strings.TrimSpace(rec.Name + "\n" + rec.Description + "\n" + rec.Body)
}

// lexicalSimilarity scores the name and description.
//
// The body is excluded on purpose. Two skills for different jobs share
// long stretches of boilerplate — "read the file, parse it, write it
// back" — and including that drags every pair toward the threshold,
// which is how a check meant to catch duplicates starts refusing
// everything.
func lexicalSimilarity(a, b *lobslawv1.SelfTaughtRecord) float64 {
	nameScore := jaccard(tokenise(a.Name), tokenise(b.Name))
	descScore := jaccard(tokenise(a.Description), tokenise(b.Description))

	// Weighted toward the name, which is the field a proposer varies
	// accidentally. Two skills with the same name are almost certainly
	// the same job whatever their descriptions say.
	return 0.7*nameScore + 0.3*descScore
}

// tokenise splits on anything that is not a letter or digit, so
// "tidy-notes", "tidy_notes" and "Tidy Notes" produce one token set.
func tokenise(s string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, f := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		out[stem(f)] = struct{}{}
	}
	return out
}

// stem folds the endings that make one job look like two:
// "tidy-notes" against "tidying-note".
//
// Crude rather than a real stemmer — the vocabulary is skill names,
// and over-stemming costs a false positive that the proposer clears
// with one extra field. What matters far more than linguistic
// accuracy is CONSISTENCY: both sides go through this function, so a
// rule that folds "notes" to something other than what it folds
// "note" to makes the whole check useless. Stripping "es" did exactly
// that — "notes" became "not" while "note" stayed "note", and the two
// never matched.
func stem(w string) string {
	switch {
	case len(w) > 4 && strings.HasSuffix(w, "ies"):
		// "tidies" -> "tidy", so a name and a description agree.
		return strings.TrimSuffix(w, "ies") + "y"
	case len(w) > 5 && strings.HasSuffix(w, "ing"):
		return strings.TrimSuffix(w, "ing")
	case len(w) > 3 && strings.HasSuffix(w, "s"):
		return strings.TrimSuffix(w, "s")
	default:
		return w
	}
}

// jaccard is intersection over union. Two empty sets score zero rather
// than one: an artefact with no description is not "identical" to
// every other artefact with no description.
func jaccard(a, b map[string]struct{}) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	var shared int
	for k := range a {
		if _, ok := b[k]; ok {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	if union == 0 {
		return 0
	}
	return float64(shared) / float64(union)
}

// cosine is the usual similarity, clamped at zero. A negative cosine
// means "actively opposite", which for this purpose is the same as
// unrelated.
func cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	sim := dot / (math.Sqrt(na) * math.Sqrt(nb))
	if sim < 0 {
		return 0
	}
	return sim
}
