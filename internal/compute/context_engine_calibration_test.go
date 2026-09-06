package compute

import (
	"context"
	"fmt"
	"math"
	"os"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/embedder"
)

// WHERE THE DEFAULT RECALL FLOOR COMES FROM.
//
// DefaultMinSemanticScore is a number that decides, on every turn,
// whether a memory is allowed to influence the reply. Picking it by
// intuition would be picking it by how it reads, and a threshold that
// reads sensible can still admit every unrelated record in the corpus
// — which is exactly the state this test was written to end.
//
// So it is measured. The corpus below is labelled by topic; a pair is
// RELATED when a query and a memory share one, and unrelated
// otherwise. Embedding every pair with the real checkpoint gives two
// score distributions, and the floor lives in the gap between them.
//
// Two properties matter, and only one of them is about accuracy:
//
//   - Related pairs must survive. A memory wrongly withheld is
//     invisible — the turn behaves as though it were never stored and
//     nothing logs a miss — so this side is weighted heavily.
//   - Contentless queries must recall NOTHING. "Hey you there?" has no
//     topic, so every pair it forms is unrelated by construction, and
//     the highest score it reaches anywhere in the corpus is the real
//     lower bound on the floor. This is the case that motivated the
//     feature: top-K search always returns its K nearest neighbours,
//     however far away they are, so on a small corpus a greeting
//     retrieves whatever happens to exist.
//
// Gated on LOBSLAW_EMBEDDER_MODEL like the parity gate, because it
// needs the checkpoint. TestRecallFloorFiltersByScore covers the
// mechanism with hand-built vectors and always runs.

type calibMemo struct {
	id    string
	topic string
	text  string
}

type calibQuery struct {
	// topic is empty for a contentless query — a greeting or an
	// acknowledgement that should match nothing at all.
	topic string
	text  string
	// targets are the memory ids this query is actually asking for.
	//
	// Labelled per MEMORY rather than per topic, because topic is too
	// coarse to be relevance: "when is the cat's vet appointment" and
	// "the cattery needs booking ahead of any trip" share a topic and
	// are not an answer to one another. Scoring topic-mates as related
	// mixes genuine hits and near-misses into one distribution, which
	// makes the two classes overlap and any threshold drawn from it
	// meaningless.
	targets []string
}

// calibMemos are written as episodic records actually look: a short
// prose account of a turn, in the domestic-assistant register this
// deployment runs in.
var calibMemos = []calibMemo{
	{"choc", "shopping", "Added Galaxy chocolate to the household shopping list at John's request."},
	{"couscous", "shopping", "Put couscous, chocolate rice cakes and tinned new potatoes on the shopping list."},
	{"oatbars", "shopping", "John asked me to remove oat bars from the shopping list; they had already been bought."},
	{"shared", "shopping", "The shopping list is shared with Claire, who ticks items off in the app directly."},
	{"dino", "shopping", "Dino strings were added to the list back in July and are still on it."},

	{"dentist", "scheduling", "Booked the dentist appointment for Thursday at 9:15am and set a reminder."},
	{"sync", "scheduling", "John has a standing team sync every Monday at 10am that should not be double-booked."},
	{"fricall", "scheduling", "Moved the Friday call to the following week because of the school run."},
	{"mot", "scheduling", "Reminder set for the car MOT which is due at the end of the month."},

	{"sched", "code", "Fixed a race in the scheduler where the leader woke its own loop every second."},
	{"gover", "code", "The build pins Go 1.27 and the lint toolchain is read from go.mod."},
	{"catalogue", "code", "Migrated the command risk catalogue from Go source into an embedded TOML file."},
	{"golden", "code", "Added a golden corpus test so classification changes have to be deliberate."},

	{"ferry", "travel", "Booked the ferry to Rotterdam for the last weekend in September."},
	{"aisle", "travel", "John prefers aisle seats on flights longer than two hours."},
	{"passport", "travel", "The passport expires in March so it needs renewing before any summer trip."},
	{"parking", "travel", "Found parking near the terminal that is cheaper booked in advance."},

	{"running", "health", "John has been trying to run three times a week and logs it in the morning."},
	{"prescription", "health", "The prescription is collected from the pharmacy on the high street monthly."},
	{"hayfever", "health", "Hay fever is worst in June so antihistamines get bought in bulk in May."},

	{"terse", "preferences", "John prefers terse replies without preamble or restating the question."},
	{"metric", "preferences", "Prefers metric units and the 24-hour clock in anything I write."},
	{"noconfirm", "preferences", "Does not want to be asked to confirm read-only operations."},

	{"boiler", "household", "The boiler service contract renews in October and is paid annually."},
	{"bins", "household", "Bin collection is Wednesday, recycling on alternate weeks."},
	{"sparekey", "household", "The spare key is with the neighbour at number 14."},
	{"broadband", "household", "Broadband contract is out of its minimum term and could be renegotiated."},

	{"vet", "pets", "The cat has a vet appointment for her booster in early October."},
	{"catfood", "pets", "She is fed twice a day and will not touch the fish-flavoured food."},
	{"cattery", "pets", "Cattery needs booking well ahead of any trip away."},

	{"spoilers", "media", "John is part way through a series and does not want it spoiled."},
	{"podcasts", "media", "Prefers podcasts at 1.5x speed on the commute."},
	{"gig", "media", "Bought tickets for the gig in November, two of them."},

	{"deck", "work", "The quarterly review deck is due to the director by the end of next week."},
	{"expenses", "work", "Expenses have to be filed within thirty days or they are refused."},
	{"oncall", "work", "The on-call rota puts John on the week after next."},
}

var calibQueries = []calibQuery{
	{"shopping", "what's on the shopping list at the moment?", []string{"couscous", "dino"}},
	{"shopping", "did you add the Galaxy chocolate?", []string{"choc"}},
	{"shopping", "are the tinned potatoes still on the list", []string{"couscous"}},
	{"shopping", "can you take the oat bars off the list", []string{"oatbars"}},
	{"shopping", "who else can edit the shopping list?", []string{"shared"}},

	{"scheduling", "when is my dentist appointment?", []string{"dentist"}},
	{"scheduling", "what have I got on Monday morning?", []string{"sync"}},
	{"scheduling", "did we move that Friday call?", []string{"fricall"}},
	{"scheduling", "when is the car MOT due?", []string{"mot"}},

	{"code", "what did we change in the scheduler?", []string{"sched"}},
	{"code", "which Go version does the build pin?", []string{"gover"}},
	{"code", "where does the command risk catalogue live now?", []string{"catalogue"}},
	{"code", "what does the golden corpus test cover?", []string{"golden"}},

	{"travel", "when is the ferry booked for?", []string{"ferry"}},
	{"travel", "do I need to renew my passport?", []string{"passport"}},
	{"travel", "which seat should I pick on the flight?", []string{"aisle"}},
	{"travel", "is there cheap parking at the terminal?", []string{"parking"}},

	{"health", "how often have I been running?", []string{"running"}},
	{"health", "when do I pick up my prescription?", []string{"prescription"}},
	{"health", "when should I buy the antihistamines?", []string{"hayfever"}},

	{"preferences", "how do you like me to write my replies?", []string{"terse"}},
	{"preferences", "which units should I use?", []string{"metric"}},
	{"preferences", "should I ask before read-only commands?", []string{"noconfirm"}},

	{"household", "when does the boiler contract renew?", []string{"boiler"}},
	{"household", "which day is the bin collection?", []string{"bins"}},
	{"household", "who has the spare key?", []string{"sparekey"}},
	{"household", "can I renegotiate the broadband?", []string{"broadband"}},

	{"pets", "when is the cat's vet appointment?", []string{"vet"}},
	{"pets", "what food will the cat not eat?", []string{"catfood"}},
	{"pets", "do I need to book the cattery?", []string{"cattery"}},

	{"media", "what gig tickets did I buy?", []string{"gig"}},
	{"media", "what speed do I listen to podcasts at?", []string{"podcasts"}},
	{"media", "can you avoid spoilers for my series?", []string{"spoilers"}},

	{"work", "when is the quarterly review deck due?", []string{"deck"}},
	{"work", "how long do I have to file expenses?", []string{"expenses"}},
	{"work", "when am I next on call?", []string{"oncall"}},

	// The bug case. None of these carries a topic or a target, and
	// none should retrieve anything — but every one is a real message
	// a user sends, and before the floor each returned the three
	// nearest records in the corpus whatever they were.
	{"", "Hey you there?", nil},
	{"", "hello", nil},
	{"", "morning", nil},
	{"", "you around?", nil},
	{"", "thanks", nil},
	{"", "ok", nil},
	{"", "nice one", nil},
	{"", "cheers", nil},
	{"", "yep that's right", nil},
	{"", "sorry, one sec", nil},
	{"", "hmm", nil},
	{"", "still there?", nil},
	{"", "how's it going?", nil},
	{"", "any update?", nil},
	{"", "what's new?", nil},
	{"", "just checking in", nil},
	{"", "are you awake", nil},
	{"", "hey", nil},
	{"", "good evening", nil},
	{"", "quick question", nil},
	{"", "one moment", nil},
	{"", "never mind", nil},
	{"", "that's great, thank you", nil},
	{"", "understood", nil},
	{"", "can you help me with something?", nil},
	{"", "I need a hand with a thing", nil},
	{"", "got a sec?", nil},
}

func cosineF32(a, b []float32) float64 {
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

func pct(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	i := int(p / 100 * float64(len(sorted)-1))
	return sorted[i]
}

func TestCalibrateRecallFloor(t *testing.T) {
	dir := os.Getenv("LOBSLAW_EMBEDDER_MODEL")
	if dir == "" {
		t.Skip("set LOBSLAW_EMBEDDER_MODEL to a HF snapshot directory to calibrate the recall floor")
	}
	enc, err := embedder.Open(dir)
	if err != nil {
		t.Fatalf("open checkpoint: %v", err)
	}
	defer func() { _ = enc.Close() }()

	// The production type, not the raw encoder, so any asymmetric
	// prefix configured for the checkpoint applies here exactly as it
	// does on a live turn.
	emb := NewBuiltinEmbedder(enc, "calibration")
	ctx := context.Background()

	memVecs := make([][]float32, len(calibMemos))
	for i, m := range calibMemos {
		v, err := emb.Embed(ctx, m.text)
		if err != nil {
			t.Fatalf("embed memory %d: %v", i, err)
		}
		memVecs[i] = v
	}

	// Three buckets, not two. The middle one is the reason the first
	// attempt at this corpus produced a useless number: a topic-mate
	// that is not the answer is neither a hit to protect nor noise to
	// cut, and folding it into either class makes the two overlap.
	var targeted, sameTopic, unrelated []float64
	// Per contentless query, the single best score it achieves. The
	// floor has to clear the worst of these for a greeting to recall
	// nothing.
	contentlessBest := map[string]float64{}

	for _, q := range calibQueries {
		qv, err := emb.EmbedQuery(ctx, q.text)
		if err != nil {
			t.Fatalf("embed query %q: %v", q.text, err)
		}
		for i, m := range calibMemos {
			s := cosineF32(qv, memVecs[i])
			switch {
			case q.topic == "":
				if s > contentlessBest[q.text] {
					contentlessBest[q.text] = s
				}
				unrelated = append(unrelated, s)
			case slices.Contains(q.targets, m.id):
				targeted = append(targeted, s)
			case q.topic == m.topic:
				sameTopic = append(sameTopic, s)
			default:
				unrelated = append(unrelated, s)
			}
		}
	}
	sort.Float64s(targeted)
	sort.Float64s(sameTopic)
	sort.Float64s(unrelated)

	worstContentless := 0.0
	worstContentlessQuery := ""
	for q, s := range contentlessBest {
		if s > worstContentless {
			worstContentless, worstContentlessQuery = s, q
		}
	}

	t.Logf("%d queries x %d memories = %d pairs: %d targeted, %d same-topic, %d unrelated",
		len(calibQueries), len(calibMemos), len(targeted)+len(sameTopic)+len(unrelated),
		len(targeted), len(sameTopic), len(unrelated))
	t.Logf("targeted    min=%.3f p05=%.3f p25=%.3f p50=%.3f max=%.3f",
		pct(targeted, 0), pct(targeted, 5), pct(targeted, 25), pct(targeted, 50), pct(targeted, 100))
	t.Logf("same-topic  p50=%.3f p95=%.3f max=%.3f",
		pct(sameTopic, 50), pct(sameTopic, 95), pct(sameTopic, 100))
	t.Logf("unrelated   p50=%.3f p95=%.3f p99=%.3f max=%.3f",
		pct(unrelated, 50), pct(unrelated, 95), pct(unrelated, 99), pct(unrelated, 100))
	t.Logf("worst contentless query: %q best match scores %.3f",
		worstContentlessQuery, worstContentless)

	// The sweep is the artefact worth reading when this corpus
	// changes: it says what each candidate threshold would cost and
	// buy, rather than asserting one answer.
	t.Logf("threshold  targeted_kept  unrelated_cut  greetings_silenced")
	for _, th := range []float64{0.10, 0.15, 0.20, 0.22, 0.24, 0.25, 0.26, 0.28, 0.32, 0.36, 0.40, 0.45, 0.50} {
		kept := 0
		for _, s := range targeted {
			if s >= th {
				kept++
			}
		}
		cut := 0
		for _, s := range unrelated {
			if s < th {
				cut++
			}
		}
		silenced := 0
		for _, s := range contentlessBest {
			if s < th {
				silenced++
			}
		}
		t.Logf("  %.2f        %5.1f%%         %5.1f%%          %d/%d",
			th,
			100*float64(kept)/float64(len(targeted)),
			100*float64(cut)/float64(len(unrelated)),
			silenced, len(contentlessBest))
	}

	// The two properties the shipped default has to hold. Asserted
	// rather than printed, so a checkpoint change or a corpus edit
	// that invalidates the number fails instead of scrolling past.
	//
	// Nothing is asserted about the same-topic band on purpose. Those
	// pairs are neither hits to protect nor noise to cut — where they
	// fall is a taste question about how much adjacent context is
	// welcome, and pinning it would turn a judgement into a gate.
	const minTargetedRetained = 0.95
	kept := 0
	for _, s := range targeted {
		if s >= float64(DefaultMinSemanticScore) {
			kept++
		}
	}
	if got := float64(kept) / float64(len(targeted)); got < minTargetedRetained {
		t.Errorf("DefaultMinSemanticScore=%.2f retains only %.1f%% of targeted pairs, want >=%.0f%%; "+
			"the floor is too high and memories that answer the question are being withheld",
			DefaultMinSemanticScore, 100*got, 100*minTargetedRetained)
	}
	if worstContentless >= float64(DefaultMinSemanticScore) {
		t.Errorf("DefaultMinSemanticScore=%.2f still admits a memory for the contentless query %q (%.3f); "+
			"the floor is too low and greetings will recall noise",
			DefaultMinSemanticScore, worstContentlessQuery, worstContentless)
	}

	// THE INCIDENT, as its own check.
	//
	// "Hey you there?" was answered with "Galaxy chocolate is still on
	// the list from the other day, nothing has changed since" — a
	// stale claim about a list someone else had edited, volunteered
	// unprompted because recall injected the chocolate memory into a
	// turn that had no topic. Named here so the specific pair is
	// checked by name and cannot regress quietly inside an aggregate.
	var chocVec []float32
	for i, m := range calibMemos {
		if m.id == "choc" {
			chocVec = memVecs[i]
		}
	}
	greeting, err := emb.EmbedQuery(ctx, "Hey you there?")
	if err != nil {
		t.Fatalf("embed greeting: %v", err)
	}
	if s := cosineF32(greeting, chocVec); s >= float64(DefaultMinSemanticScore) {
		t.Errorf("the original failure still recalls: %q against the chocolate memory scores %.3f, "+
			"floor is %.2f", "Hey you there?", s, DefaultMinSemanticScore)
	} else {
		t.Logf("original incident: %q vs the chocolate memory scores %.3f, floor %.2f — withheld",
			"Hey you there?", s, DefaultMinSemanticScore)
	}

	fmt.Fprintf(os.Stderr, "\ncalibration: DefaultMinSemanticScore=%.2f keeps %.1f%% of targeted pairs; "+
		"worst contentless query reaches %.3f\n",
		DefaultMinSemanticScore, 100*float64(kept)/float64(len(targeted)), worstContentless)
}

// THE LEXICAL FLOOR, and the limit of what a threshold can do for it.
//
// The fallback path scores a record as the fraction of query tokens
// found in its text, which shares the 0..1 range with cosine and
// nothing else. It needs its own number, measured the same way.
//
// It also has a failure the semantic path does not, and this test
// exists as much to document it as to pick a value. TokeniseQuery
// drops stopwords and tokens of two characters or fewer, so a short
// message can arrive at the scorer as a single token — and one token
// that matches gives a score of exactly 1.0, the top of the range,
// on the strength of one word appearing somewhere in a record. No
// threshold can separate that from a genuine full-query match,
// because they produce the identical score.
//
// Needs no checkpoint, so unlike the semantic calibration it always
// runs.
func TestCalibrateLexicalRecallFloor(t *testing.T) {
	// Mirrors lexicalEpisodicSearch exactly, word-start anchoring
	// included. A calibration that scored differently from production
	// would be measuring a scorer nobody runs.
	lexScore := func(query, text string) (float64, int) {
		toks := TokeniseQuery(query)
		if len(toks) == 0 {
			return 0, 0
		}
		hay := strings.ToLower(text)
		matches := 0
		for _, tok := range toks {
			if matchesAtWordStart(hay, tok) {
				matches++
			}
		}
		return float64(matches) / float64(len(toks)), len(toks)
	}

	var targeted, unrelated []float64
	contentlessBest := map[string]float64{}
	// Queries that survive tokenisation as a single term. Their scores
	// are quantised to {0, 1}, so they cannot be graded at all.
	singleToken := map[string]bool{}

	for _, q := range calibQueries {
		for _, m := range calibMemos {
			s, ntok := lexScore(q.text, m.text)
			if ntok == 1 {
				singleToken[q.text] = true
			}
			// Passive recall declines a query this short BEFORE it
			// scans, so these recall nothing whatever they would have
			// scored. Zeroed here so the sweep reports what the
			// pipeline does rather than what the scorer alone says.
			if ntok < minLexicalTerms {
				s = 0
			}
			switch {
			case q.topic == "":
				if s > contentlessBest[q.text] {
					contentlessBest[q.text] = s
				}
				unrelated = append(unrelated, s)
			case slices.Contains(q.targets, m.id):
				targeted = append(targeted, s)
			case q.topic == m.topic:
				// Same-topic pairs are excluded from both classes, as
				// in the semantic calibration.
			default:
				unrelated = append(unrelated, s)
			}
		}
	}
	sort.Float64s(targeted)
	sort.Float64s(unrelated)

	worstContentless, worstQuery := 0.0, ""
	for q, s := range contentlessBest {
		if s > worstContentless {
			worstContentless, worstQuery = s, q
		}
	}

	t.Logf("targeted   min=%.3f p05=%.3f p50=%.3f max=%.3f",
		pct(targeted, 0), pct(targeted, 5), pct(targeted, 50), pct(targeted, 100))
	t.Logf("unrelated  p50=%.3f p95=%.3f p99=%.3f max=%.3f",
		pct(unrelated, 50), pct(unrelated, 95), pct(unrelated, 99), pct(unrelated, 100))
	t.Logf("worst contentless query: %q reaches %.3f", worstQuery, worstContentless)
	t.Logf("queries reduced to a single token by TokeniseQuery: %d of %d",
		len(singleToken), len(calibQueries))
	// Named, not just counted. Each of these is a message with no
	// topic that can still pull a record, and the terms it survives
	// tokenisation with are the actionable part — a residual here is
	// usually a stopword the list is missing rather than anything
	// about the scorer.
	for q, sc := range contentlessBest {
		t.Logf("  contentless still scoring: %-32q %.3f  terms=%v", q, sc, TokeniseQuery(q))
	}

	t.Logf("threshold  targeted_kept  unrelated_cut  greetings_silenced")
	for _, th := range []float64{0.20, 0.25, 0.30, 0.34, 0.40, 0.50, 0.60} {
		kept, cut, silenced := 0, 0, 0
		for _, s := range targeted {
			if s >= th {
				kept++
			}
		}
		for _, s := range unrelated {
			if s < th {
				cut++
			}
		}
		for _, s := range contentlessBest {
			if s < th {
				silenced++
			}
		}
		t.Logf("  %.2f        %5.1f%%         %5.1f%%          %d/%d",
			th,
			100*float64(kept)/float64(len(targeted)),
			100*float64(cut)/float64(len(unrelated)),
			silenced, len(contentlessBest))
	}

	const minTargetedRetained = 0.90
	kept := 0
	for _, s := range targeted {
		if s >= float64(DefaultMinLexicalScore) {
			kept++
		}
	}
	if got := float64(kept) / float64(len(targeted)); got < minTargetedRetained {
		t.Errorf("DefaultMinLexicalScore=%.2f retains only %.1f%% of targeted pairs, want >=%.0f%%",
			DefaultMinLexicalScore, 100*got, 100*minTargetedRetained)
	}
}
