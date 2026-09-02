package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// The review fork: after the reply has gone out, replay the turn and
// ask whether anything about it is worth keeping.
//
// Everything here is behind the reply, on a bounded background
// context, with failures logged and swallowed — the same pattern
// maybeIngestTurn already establishes. A learning side effect must
// never cost somebody their answer.

// Trigger intervals, deliberately measured on different axes. This is
// the best single idea in hermes's design and it is worth stating why
// it works:
//
// A skill answers "was there enough WORK here to be worth encoding",
// which is a property of one turn — forty tool calls in a debugging
// session is a procedure waiting to be written down. Memory answers
// "have we learned who this person IS", which only accumulates across
// turns. Measuring both on the same axis gets one of them wrong: forty
// turns of chat produce no procedure, and one enormous turn teaches
// you nothing about the user.
const (
	defaultSkillToolIterations = 10
	defaultMemoryTurnInterval  = 10

	// reviewTimeout bounds the fork. Generous, because it replays a
	// conversation, and bounded because nothing downstream is waiting
	// for it — a fork still running when the next turn arrives is
	// spending tokens on a conversation that has moved on.
	reviewTimeout = 90 * time.Second
)

// ArtefactKind mirrors the self-taught kinds without importing the
// memory package.
const (
	ArtefactSkill     = "skill"
	ArtefactProcedure = "procedure"
)

// ProposedArtefact is what a review decided to keep.
type ProposedArtefact struct {
	Kind        string
	Name        string
	Description string
	Body        string
	TurnID      string
	SessionID   string
	Owner       string

	// Refines names an existing artefact this improves. Empty means a
	// new one — and then Distinct must be true if the store finds a
	// near-duplicate.
	Refines   string
	Rationale string
	Distinct  bool
}

// ArtefactSummary is one existing artefact, as the fork sees it when
// deciding whether it is about to duplicate something.
type ArtefactSummary struct {
	ID          string
	Name        string
	Description string
}

// ArtefactStore is the fork's only write target.
//
// Narrow on purpose. The fork's authority is "write on the self-taught
// namespace and nothing else", and an interface that can only do that
// makes the claim structural rather than a policy rule somebody has to
// keep enforcing.
type ArtefactStore interface {
	// Existing lists what is already stored, so the fork can name a
	// refinement target rather than inventing a near-duplicate.
	Existing(kind string) ([]ArtefactSummary, error)
	// Propose records the artefact.
	Propose(ctx context.Context, a ProposedArtefact) error
}

// ReviewConfig wires the fork.
type ReviewConfig struct {
	Roles  *RoleMap
	Store  ArtefactStore
	Logger *slog.Logger

	// SkillToolIterations and MemoryTurnInterval override the
	// defaults. Zero takes the default; negative disables that axis.
	SkillToolIterations int
	MemoryTurnInterval  int
}

// ReviewFork decides whether a turn taught anything.
type ReviewFork struct {
	cfg ReviewConfig
	log *slog.Logger

	// turns counts conversation turns per session, for the memory
	// axis. In-process: a miscount costs a review that fires a turn
	// early or late, which is not worth a raft round-trip per message.
	mu    sync.Mutex
	turns map[string]int
}

func NewReviewFork(cfg ReviewConfig) (*ReviewFork, error) {
	if cfg.Roles == nil {
		return nil, fmt.Errorf("review: a RoleMap is required")
	}
	if cfg.Store == nil {
		return nil, fmt.Errorf("review: a store is required; the fork must have somewhere to write")
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &ReviewFork{cfg: cfg, log: cfg.Logger, turns: map[string]int{}}, nil
}

// reviewAxes is what a turn qualified for.
type reviewAxes struct {
	skills bool
	memory bool
}

func (r reviewAxes) any() bool { return r.skills || r.memory }

// shouldReview decides whether this turn is worth a fork, and on which
// axis.
func (f *ReviewFork) shouldReview(req ProcessMessageRequest, toolCalls int) reviewAxes {
	var out reviewAxes
	if f == nil {
		return out
	}

	// A turn with no channel origin is the scheduler, a commitment
	// firing, or a research worker. No human in the loop means nothing
	// to learn about the user — and the fork is expensive enough that
	// spending it on a cron tick is the wrong trade twice over.
	if req.Channel == "" {
		return out
	}

	// A fork's own replay must not spawn a fork. Without this the
	// first review triggers the second, and a review of a review is
	// both meaningless and unbounded.
	if req.IsReviewFork {
		return out
	}

	if n := f.skillThreshold(); n > 0 && toolCalls >= n {
		out.skills = true
	}
	if n := f.memoryThreshold(); n > 0 {
		key := req.Channel + ":" + req.ChannelID
		f.mu.Lock()
		f.turns[key]++
		if f.turns[key] >= n {
			f.turns[key] = 0
			out.memory = true
		}
		f.mu.Unlock()
	}
	return out
}

func (f *ReviewFork) skillThreshold() int {
	switch {
	case f.cfg.SkillToolIterations < 0:
		return 0
	case f.cfg.SkillToolIterations == 0:
		return defaultSkillToolIterations
	default:
		return f.cfg.SkillToolIterations
	}
}

func (f *ReviewFork) memoryThreshold() int {
	switch {
	case f.cfg.MemoryTurnInterval < 0:
		return 0
	case f.cfg.MemoryTurnInterval == 0:
		return defaultMemoryTurnInterval
	default:
		return f.cfg.MemoryTurnInterval
	}
}

// Consider fires a review if the turn warrants one. Non-blocking: the
// reply has already gone out and nothing waits on this.
//
// Takes the turn's context even though it detaches from it. Both
// callers have one, and the line directly above each is
// maybeIngestTurn(ctx, ...) — the same shape, the same detached
// goroutine, the same reason. Consider was the one that did not, and a
// review is an LLM call: without the turn's values on it, a failure in
// the fork is unattributable to the turn that caused it.
func (f *ReviewFork) Consider(ctx context.Context, req ProcessMessageRequest, messages []Message, toolCalls int) {
	if f == nil {
		return
	}
	axes := f.shouldReview(req, toolCalls)
	if !axes.any() {
		return
	}
	// Copied before the goroutine: the caller reuses its slice for the
	// next turn, and a review reading a transcript that changed under
	// it would learn from a conversation that never happened.
	replay := make([]Message, len(messages))
	copy(replay, messages)

	go func() {
		// WithoutCancel: the turn's context is cancelled the moment the
		// reply goes out, and this deliberately outlives it. The values
		// stay true.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx),
			orDefault(f.cfg.Roles.TimeoutFor(RoleReview), reviewTimeout))
		defer cancel()
		if err := f.run(ctx, req, replay, axes); err != nil {
			f.log.Warn("review: fork failed", "turn_id", req.TurnID, "err", err)
		}
	}()
}

func (f *ReviewFork) run(ctx context.Context, req ProcessMessageRequest, messages []Message, axes reviewAxes) error {
	existing, err := f.cfg.Store.Existing(ArtefactSkill)
	if err != nil {
		return fmt.Errorf("read existing artefacts: %w", err)
	}

	provider := f.cfg.Roles.For(RoleReview)
	fullReplay := f.cfg.Roles.IsMain(RoleReview)

	prompt := reviewPrompt(axes, existing)
	body := f.replayBody(messages, fullReplay)

	resp, err := provider.Chat(ctx, ChatRequest{
		Messages: []Message{
			{Role: "system", Content: prompt},
			{Role: "user", Content: body},
		},
	})
	if err != nil {
		return fmt.Errorf("review call: %w", err)
	}

	decision, err := parseReviewDecision(resp.Content)
	if err != nil {
		// A malformed decision is treated as "nothing worth keeping".
		// The alternative — retrying, or guessing at the intent — puts
		// a badly-formed artefact into the store on the strength of a
		// response the model could not even shape correctly.
		f.log.Debug("review: unparseable decision; treating as nothing learned",
			"turn_id", req.TurnID, "err", err)
		return nil
	}
	if decision.Action == "none" || strings.TrimSpace(decision.Name) == "" {
		f.log.Debug("review: nothing worth keeping", "turn_id", req.TurnID)
		return nil
	}

	artefact := ProposedArtefact{
		Kind:        ArtefactSkill,
		Name:        decision.Name,
		Description: decision.Description,
		Body:        decision.Body,
		TurnID:      req.TurnID,
		Owner:       ownerOf(req),
		Refines:     decision.Refines,
		Rationale:   decision.Rationale,
		Distinct:    decision.Action == "new" && decision.Distinct,
	}
	if err := f.cfg.Store.Propose(ctx, artefact); err != nil {
		// Logged and dropped rather than retried. A near-duplicate
		// refusal means the fork proposed something it was given the
		// index to avoid, and asking the model again costs another
		// replay to maybe produce a marginal artefact — which propose
		// mode would then make somebody approve.
		f.log.Info("review: proposal refused",
			"turn_id", req.TurnID, "name", decision.Name, "err", err)
		return nil
	}
	f.log.Info("review: artefact proposed",
		"turn_id", req.TurnID, "name", decision.Name,
		"refines", decision.Refines, "full_replay", fullReplay)
	return nil
}

// replayBody renders the conversation for the fork.
//
// Full when the review runs on the main model, because it reads a warm
// prefix cache and the replay is mostly cache reads. A digest
// otherwise: a different model cannot reuse that cache, so a full
// replay would cold-write the entire transcript to learn, at most, one
// skill.
func (f *ReviewFork) replayBody(messages []Message, full bool) string {
	var b strings.Builder
	b.WriteString("Conversation to review:\n\n")
	for _, m := range messages {
		if m.Role == "system" {
			// The system prompt is the assistant's own instructions;
			// reviewing them would have the fork learning from its own
			// configuration rather than from the exchange.
			continue
		}
		text := m.Content
		if !full {
			text = truncateForDigest(text)
		}
		if text == "" && len(m.ToolCalls) > 0 {
			// A tool-call turn with no prose still matters to the
			// skill axis: the tools ARE the procedure.
			names := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Name)
			}
			text = "(called: " + strings.Join(names, ", ") + ")"
		}
		if text == "" {
			continue
		}
		fmt.Fprintf(&b, "%s: %s\n\n", m.Role, text)
	}
	return b.String()
}

// digestMessageChars bounds each message in digest mode. Long enough
// to keep the shape of what happened, short enough that a forty-call
// session does not cold-write a novel.
const digestMessageChars = 400

func truncateForDigest(s string) string {
	r := []rune(s)
	if len(r) <= digestMessageChars {
		return s
	}
	return string(r[:digestMessageChars]) + " …[truncated]"
}

func ownerOf(req ProcessMessageRequest) string {
	if req.Claims == nil {
		return ""
	}
	return "user:" + req.Claims.UserID
}

// reviewDecision is the fork's structured answer.
type reviewDecision struct {
	Action      string `json:"action"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Refines     string `json:"refines"`
	Rationale   string `json:"rationale"`
	Distinct    bool   `json:"distinct"`
}

// parseReviewDecision reads the model's answer, tolerating the fenced
// code block models habitually wrap JSON in.
func parseReviewDecision(content string) (*reviewDecision, error) {
	s := strings.TrimSpace(content)
	if i := strings.Index(s, "```"); i >= 0 {
		s = s[i+3:]
		if j := strings.Index(s, "\n"); j >= 0 {
			s = s[j+1:]
		}
		if j := strings.Index(s, "```"); j >= 0 {
			s = s[:j]
		}
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, fmt.Errorf("empty decision")
	}
	var out reviewDecision
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil, fmt.Errorf("decode decision: %w", err)
	}
	switch out.Action {
	case "none", "new", "refine":
	default:
		return nil, fmt.Errorf("unknown action %q", out.Action)
	}
	if out.Action == "refine" && strings.TrimSpace(out.Refines) == "" {
		return nil, fmt.Errorf("refine with no target")
	}
	return &out, nil
}
