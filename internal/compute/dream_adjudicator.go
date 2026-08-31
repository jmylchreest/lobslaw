package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Deciding what near-identical memories mean.
//
// Clustering says two records are worded alike. This says whether
// that is the same fact twice, a fact that changed, or two facts that
// cannot both be true — and it is the difference between memory that
// tidies itself and memory that quietly loses things.
//
// Runs at dream time, once per cluster, and never again for the same
// cluster: the caller records the verdict against the cluster's id.
// That is what makes an LLM affordable here and would not make one
// affordable on the recall path.

// adjudicatePrompt asks for the verdict.
//
// The bias toward keep_distinct is deliberate and stated twice. Every
// other verdict has a consequence — merging deletes records,
// supersedes and conflict change what recall shows — and the cost of
// a wrong keep_distinct is a duplicate. The costs are not symmetric,
// so the default must not be either.
const adjudicatePrompt = `You are deciding what a group of near-identical memories from a personal assistant's store MEANS.

Reply with JSON only, no prose, in this exact shape:
{"verdict": "...", "reason": "...", "consolidated": "...", "current": "..."}

verdict is one of:
- "merge": the same fact recorded more than once, in different words. Nothing is lost by replacing them with one record. Put that single record's text in "consolidated", written as settled knowledge.
- "supersedes": the same subject at different times, where one is now current — a preference that changed, a plan that moved. Put the id of the CURRENT record in "current". The others are kept: what someone used to want is part of who they are.
- "conflict": they cannot all be true and nothing here says which is. Do not guess. Explain the disagreement in "reason" as a question a person could answer.
- "keep_distinct": similar wording, different meaning. Two people named John. The same word about two things.

Rules:
- Prefer "keep_distinct" whenever you are unsure. A duplicate left alone costs nothing; a wrong merge loses a memory.
- Only say "merge" when the records genuinely carry no information the others lack.
- "reason" is shown to the person whose memories these are. One sentence, plain, no jargon.`

// adjudicateMaxTokens bounds one verdict. Enough for a consolidated
// paragraph and a sentence of reasoning.
const adjudicateMaxTokens = 500

// DreamAdjudicator decides cluster verdicts through an LLM.
type DreamAdjudicator struct {
	provider LLMProvider
	model    string
	log      *slog.Logger
}

// NewDreamAdjudicator builds one. A nil provider returns nil, which
// Dream reads as "no adjudicator" and skips the phase entirely —
// clustering included, so a node without a provider pays nothing for
// a decision nobody is making.
func NewDreamAdjudicator(p LLMProvider, model string, log *slog.Logger) *DreamAdjudicator {
	if p == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	return &DreamAdjudicator{provider: p, model: model, log: log}
}

// adjudicationJSON is the model's reply.
type adjudicationJSON struct {
	Verdict      string `json:"verdict"`
	Reason       string `json:"reason"`
	Consolidated string `json:"consolidated"`
	Current      string `json:"current"`
}

// AdjudicateMerge implements memory.Adjudicator.
//
// An unparseable or unknown verdict becomes keep_distinct rather than
// an error: the cluster is still recorded as decided, so a model that
// cannot answer this question does not leave it to be asked again
// every night forever.
func (a *DreamAdjudicator) AdjudicateMerge(ctx context.Context, cluster *lobslawv1.Cluster) (*memory.Adjudication, error) {
	if a == nil || a.provider == nil {
		return nil, nil
	}
	members := cluster.GetRecords()
	if len(members) < 2 {
		return nil, nil
	}

	var b strings.Builder
	for _, v := range members {
		// Identified by the memory's id, not the vector's: the
		// verdict names memories, and "current" has to point at
		// something the caller can act on.
		id := ""
		if len(v.GetSourceIds()) > 0 {
			id = v.GetSourceIds()[0]
		}
		when := ""
		if v.GetCreatedAt() != nil {
			when = v.GetCreatedAt().AsTime().Format("2006-01-02")
		}
		fmt.Fprintf(&b, "id=%s date=%s\n%s\n\n", id, when, strings.TrimSpace(v.GetText()))
	}

	resp, err := a.provider.Chat(ctx, ChatRequest{
		Model:     a.model,
		MaxTokens: adjudicateMaxTokens,
		Messages: []Message{
			{Role: "system", Content: adjudicatePrompt},
			{Role: "user", Content: b.String()},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("adjudicate: %w", err)
	}

	var parsed adjudicationJSON
	if err := json.Unmarshal([]byte(extractJSON(resp.Content)), &parsed); err != nil {
		a.log.Warn("dream: unreadable adjudication; cluster kept distinct",
			"cluster", cluster.GetId(), "err", err)
		return &memory.Adjudication{
			Verdict: memory.VerdictKeepDistinct,
			Reason:  "the adjudicator's reply could not be read",
		}, nil
	}

	verdict := memory.MergeVerdict(strings.ToLower(strings.TrimSpace(parsed.Verdict)))
	switch verdict {
	case memory.VerdictMerge, memory.VerdictSupersedes,
		memory.VerdictConflict, memory.VerdictKeepDistinct:
	default:
		a.log.Warn("dream: unknown verdict; cluster kept distinct",
			"cluster", cluster.GetId(), "verdict", parsed.Verdict)
		verdict = memory.VerdictKeepDistinct
		parsed.Reason = "the adjudicator returned a verdict this node does not know"
	}

	return &memory.Adjudication{
		Verdict:      verdict,
		Reason:       strings.TrimSpace(parsed.Reason),
		Consolidated: strings.TrimSpace(parsed.Consolidated),
		Current:      strings.TrimSpace(parsed.Current),
	}, nil
}

// extractJSON pulls the object out of a reply that fenced it.
//
// Models wrap JSON in ```json blocks unasked, and refusing those
// would turn a formatting habit into a lost verdict.
func extractJSON(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return s
}
