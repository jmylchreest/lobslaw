package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
)

// The routing signal.
//
// [[compute.chains]] triggers on min_complexity and domains, and
// nothing in the system produced either — so of the three trigger
// kinds only `always` could ever fire, and chains could not route.
// This is what produces them.
//
// A cheap model rather than a heuristic, because the thing being
// judged is how hard a question is, and message length is a poor
// proxy for that: "prove this terminates" is short and hard, a pasted
// stack trace is long and easy. The preflight role already existed for
// exactly this shape of work — a small, fast model doing classification
// ahead of the main turn — and it falls back to main when unset.

// Hint is the coarse routing vocabulary, mapped onto chains an
// operator can inspect and override. Sugar over chains, not a second
// mental model: a hint selects a chain, it does not select a model.
type Hint string

const (
	HintFast      Hint = "fast"
	HintBalanced  Hint = "balanced"
	HintDeep      Hint = "deep"
	HintReasoning Hint = "reasoning"
)

// Valid reports whether h is one of the four. Anything else from a
// model is discarded rather than passed on — an unrecognised hint that
// reached the resolver would match no chain and silently route to the
// default, which looks exactly like the preflight having no opinion.
func (h Hint) Valid() bool {
	switch h {
	case HintFast, HintBalanced, HintDeep, HintReasoning:
		return true
	}
	return false
}

// Complexity thresholds for deriving a hint when the model offered
// none. HintReasoning is deliberately absent: it is a request for a
// different KIND of model, not a point on a difficulty scale, so it is
// only ever honoured when something asked for it by name.
const (
	complexityBalanced = 30
	complexityDeep     = 70
)

// Judgment is what routing decides on.
type Judgment struct {
	// Complexity is 0-100. Chains with a MinComplexity trigger match
	// when Complexity >= trigger.
	Complexity int
	// Domains are the tags the judge picked out of the operator's
	// declared vocabulary. Never anything else: see domainSet.
	Domains []string
	// Hint selects a chain directly when set.
	Hint Hint
}

// NeutralJudgment is "no opinion": no chain triggers on it, so the
// default route applies.
//
// This is what a failed or nonsensical preflight returns. A turn must
// not die because the thing that decides how to route it was
// unavailable — the answer to "I don't know how hard this is" is to
// answer it the normal way.
func NeutralJudgment() Judgment {
	return Judgment{Hint: HintBalanced}
}

// judgeSystemPromptFormat asks for a compact object. Models are told
// the scale in concrete terms because "rate complexity 0-100" without
// anchors produces a cluster around 50 and no discrimination.
//
// The domains line is substituted because it names the operator's
// vocabulary, which is the only one that can route.
const judgeSystemPromptFormat = `You classify requests for routing. Reply with JSON only, no prose, no code fences:

{"complexity": <0-100>, "domains": [<tags>], "hint": "fast"|"balanced"|"deep"|"reasoning"}

complexity: 0-20 greeting, acknowledgement, or a fact you could answer in one line. 21-50 a normal question needing a paragraph. 51-80 multi-step work, unfamiliar material, or careful reasoning. 81-100 research, proof, architecture, or anything where being wrong is expensive.

Judge the DIFFICULTY, not the length. "Prove this loop terminates" is short and hard. A pasted stack trace is long and easy.

%s

hint: "reasoning" only when the task needs sustained deduction rather than knowledge. Otherwise pick from the complexity.`

// judgeNoDomainsWanted is the instruction when no chain declares a
// domain. "choose from:" with nothing after it would be worse than
// asking for nothing, and a tag nothing routes on is not worth the
// tokens spent producing it.
const judgeNoDomainsWanted = `domains: always []. Nothing in this deployment routes on them.`

// judgeSystemPrompt builds the instruction for a given vocabulary. It
// still says "at most 3": a closed list does not stop a model naming all
// of it, and maxDomains decides which tags survive.
func judgeSystemPrompt(vocabulary []string) string {
	domains := judgeNoDomainsWanted
	if len(vocabulary) > 0 {
		domains = fmt.Sprintf(
			"domains: at most %d of these exact tags, whichever apply: %s. Use no others, and omit rather than guess.",
			maxDomains, strings.Join(vocabulary, ", "))
	}
	return fmt.Sprintf(judgeSystemPromptFormat, domains)
}

// judgeMaxCompletionTokens bounds the reply. The answer is one small
// object; a model that pads still yields a parseable prefix.
const judgeMaxCompletionTokens = 128

// judgeTimeout bounds the preflight independently of the turn.
//
// The preflight exists to make the turn better, so it must not be able
// to make it slower than the turn would have been alone. A judge that
// hangs yields the neutral judgment and the turn proceeds.
const judgeTimeout = 8 * time.Second

// maxDomains bounds what a model can put in the routing key.
const maxDomains = 3

// Judge classifies a turn for routing. A nil *Judge is usable and
// returns the neutral judgment, so call sites do not branch on whether
// a preflight provider was configured.
type Judge struct {
	provider LLMProvider
	model    string
	// domains is the closed vocabulary; prompt is the instruction built
	// from it, once, because neither changes without a restart.
	domains         domainSet
	prompt          string
	outOfVocabulary sync.Once
	// timeout overrides judgeTimeout when the operator set one for
	// this role. Zero keeps the constant.
	timeout time.Duration
	log     *slog.Logger
}

// NewJudge wires a judge to the preflight provider. A nil provider
// gives a nil judge — absence, not a disabled flag.
//
// The vocabulary comes from the resolver rather than the config,
// because the resolver is what MATCHES the tags: one reading of the
// chains, so the list the model is offered cannot drift from the list a
// chain can route on. Once is enough, as [compute] takes a restart.
func NewJudge(provider LLMProvider, model string, routes *Resolver, timeout time.Duration, log *slog.Logger) *Judge {
	if provider == nil {
		return nil
	}
	if log == nil {
		log = slog.Default()
	}
	domains := newDomainSet(routes.DomainVocabulary())
	return &Judge{
		provider: provider,
		model:    model,
		domains:  domains,
		prompt:   judgeSystemPrompt(domains.sorted()),
		timeout:  timeout,
		log:      log,
	}
}

// Judge classifies text, honouring an explicit hint.
//
// An explicit hint SKIPS THE CALL. Somebody who said "deep" has
// already answered the only question the preflight was going to ask,
// and paying for a model to second-guess them would be both slower and
// worse — the hint is the strong prior, not a suggestion to weigh.
func (j *Judge) Judge(ctx context.Context, text string, explicit Hint) Judgment {
	if explicit.Valid() {
		return Judgment{Complexity: complexityOf(explicit), Hint: explicit}
	}
	if j == nil || strings.TrimSpace(text) == "" {
		return NeutralJudgment()
	}

	ctx, cancel := context.WithTimeout(ctx, orDefault(j.timeout, judgeTimeout))
	defer cancel()

	resp, err := j.provider.Chat(ctx, ChatRequest{
		Model:       j.model,
		MaxTokens:   judgeMaxCompletionTokens,
		Temperature: 0,
		Messages: []Message{
			{Role: "system", Content: j.prompt},
			{Role: "user", Content: text},
		},
	})
	if err != nil || resp == nil {
		// DEBUG, not WARN. A preflight that fails costs routing
		// precision, not correctness, and a provider having a bad
		// minute should not fill an operator's log with warnings about
		// turns that all completed.
		j.log.Debug("preflight: judge unavailable; routing on the default", "error", err)
		return NeutralJudgment()
	}
	judgment, outside := parseJudgment(resp.Content, j.domains, j.log)
	j.reportOutOfVocabulary(outside)
	return judgment
}

// reportOutOfVocabulary says, once, that the model is answering outside
// the list it was given.
//
// Once, because a model that ignores the list ignores it every turn, and
// a line per turn is the noise that trains everyone to stop reading. WARN
// because it is a standing misconfiguration: the chains declare subjects
// this preflight model will not produce, so subject routing is inert
// until one of the two changes.
func (j *Judge) reportOutOfVocabulary(tags []string) {
	if len(tags) == 0 {
		return
	}
	j.outOfVocabulary.Do(func() {
		j.log.Warn("preflight: the judge answered outside the configured domains; those turns route as though they had none",
			"discarded", tags, "configured", j.domains.sorted())
	})
}

// complexityOf gives an explicit hint a score, so a hint and a
// preflight judgment are the same shape downstream and a chain with a
// MinComplexity trigger still matches one.
func complexityOf(h Hint) int {
	switch h {
	case HintFast:
		return 10
	case HintDeep, HintReasoning:
		return 85
	default:
		return 50
	}
}

// parseJudgment reads the model's object, discarding anything it got
// wrong rather than propagating it. The second return is the domain tags
// it threw away: returned rather than logged, because whether those are
// worth an operator's attention is a question about the deployment and
// not about one reply.
func parseJudgment(content string, allowed domainSet, log *slog.Logger) (Judgment, []string) {
	raw := extractObject(content)
	if raw == "" {
		log.Debug("preflight: no JSON object in the reply; routing on the default")
		return NeutralJudgment(), nil
	}
	var parsed struct {
		Complexity int      `json:"complexity"`
		Domains    []string `json:"domains"`
		Hint       string   `json:"hint"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Debug("preflight: unparseable judgment; routing on the default", "error", err)
		return NeutralJudgment(), nil
	}

	out := Judgment{Complexity: clampComplexity(parsed.Complexity)}
	if h := Hint(strings.ToLower(strings.TrimSpace(parsed.Hint))); h.Valid() {
		out.Hint = h
	} else {
		out.Hint = hintFor(out.Complexity)
	}
	var outside []string
	out.Domains, outside = normaliseDomains(parsed.Domains, allowed)
	return out, outside
}

// extractObject finds the first balanced {...} run.
//
// Models wrap JSON in code fences and prose however firmly they are
// told not to, and a reply that is right apart from three backticks is
// a reply worth reading.
func extractObject(s string) string {
	start := strings.Index(s, "{")
	if start < 0 {
		return ""
	}
	depth := 0
	inString := false
	escaped := false
	for i := start; i < len(s); i++ {
		c := s[i]
		switch {
		case escaped:
			escaped = false
		case c == '\\' && inString:
			escaped = true
		case c == '"':
			inString = !inString
		case inString:
			// Braces inside a string are not structure.
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return s[start : i+1]
			}
		}
	}
	return ""
}

func clampComplexity(n int) int {
	if n < 0 {
		return 0
	}
	if n > 100 {
		return 100
	}
	return n
}

func hintFor(complexity int) Hint {
	switch {
	case complexity < complexityBalanced:
		return HintFast
	case complexity < complexityDeep:
		return HintBalanced
	default:
		return HintDeep
	}
}

// domainSet is the closed vocabulary of subject tags: the union of every
// chain trigger's domains, the only set that can select a chain. The
// judge is offered this list and held to it, rather than asked for "the
// subject area" and hoped to answer in the operator's words.
type domainSet map[string]struct{}

// newDomainSet normalises declared tags exactly as a judgment is
// normalised, so the two sides cannot disagree on spelling. Unbounded,
// unlike a reply: how many subjects to route on is the operator's call.
func newDomainSet(in []string) domainSet {
	if len(in) == 0 {
		return nil
	}
	out := make(domainSet, len(in))
	for _, d := range in {
		if tag := normaliseDomain(d); tag != "" {
			out[tag] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// sorted gives a stable order, so one config is one prompt.
func (d domainSet) sorted() []string {
	if len(d) == 0 {
		return nil
	}
	out := make([]string, 0, len(d))
	for tag := range d {
		out = append(out, tag)
	}
	slices.Sort(out)
	return out
}

func normaliseDomain(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// normaliseDomains lowercases, trims, drops blanks, duplicates and
// anything no chain could route on, and bounds the count. The second
// return is what it dropped as unroutable. Normalising because these
// become a routing key: "Code" one turn and "code" the next would route
// the same question two different ways.
//
// THE FILTER RUNS BEFORE THE CAP, or a reply of three invented tags and
// then "legal" loses the one tag the operator wrote a rule for.
//
// An empty vocabulary drops everything and reports nothing: no chain
// routes on subject, so the tags are dead weight, not a signal.
func normaliseDomains(in []string, allowed domainSet) (kept, outside []string) {
	if len(in) == 0 || len(allowed) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(in))
	kept = make([]string, 0, maxDomains)
	for _, d := range in {
		d = normaliseDomain(d)
		if d == "" || seen[d] {
			continue
		}
		seen[d] = true
		if _, ok := allowed[d]; !ok {
			// Bounded: a reply can name any number of subjects, and the
			// first few say enough about what it produces.
			if len(outside) < maxDomains {
				outside = append(outside, d)
			}
			continue
		}
		kept = append(kept, d)
		if len(kept) == maxDomains {
			break
		}
	}
	if len(kept) == 0 {
		return nil, outside
	}
	return kept, outside
}
