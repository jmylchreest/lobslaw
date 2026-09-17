package compute

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BotProfile is everything a turn needs to know about which bot it is.
//
// Resolved once at the start of a turn and never re-read, so a bot
// re-instructed mid-turn finishes the turn it started under the brief
// it started with. That matches how the soul snapshot already behaves
// and for the same reason: an instruction that changes underneath a
// running turn produces a reply nobody can account for.
type BotProfile struct {
	ID          string
	DisplayName string
	// Instructions is the bot's standing brief, rendered as soul
	// guidance. Standing configuration, never the current task.
	Instructions  string
	IsCoordinator bool
	// Tools is the registry filter. Empty means the node's full set —
	// see FilterTools for why that is not "no tools".
	Tools []string

	// Denied is subtracted after Tools, whatever Tools says.
	//
	// It exists because "empty allowlist means everything" and "this
	// specific tool must be absent" cannot both be expressed by one
	// list: removing the only entry from an allowlist would empty it,
	// and an empty allowlist re-grants the tool being taken away. The
	// delegation guard depends on the subtraction being unconditional.
	Denied []string
	// MayMessage is the declared edge list for inter-bot messaging.
	MayMessage []string
	ModelRole  string
	Caps       BudgetCaps
}

// Principal is the identity this bot's turns run as. Everything that
// decides against a principal — memory ownership, policy subjects,
// scheduled-task owners — sees this.
func (p *BotProfile) Principal() identity.Principal {
	if p == nil {
		return ""
	}
	return identity.Bot(p.ID)
}

// MayMessageBot reports whether this bot has a declared edge to
// another. Absence of an edge is the answer, not an error: the graph
// is an allowlist, so a bot that was never granted a target cannot
// reach it.
func (p *BotProfile) MayMessageBot(target string) bool {
	if p == nil {
		return false
	}
	return slices.Contains(p.MayMessage, strings.TrimSpace(target))
}

// FilterTools narrows a tool list to what this bot may be shown.
//
// Structural, not advisory. A tool absent from the list the model
// receives is not merely denied — the model has no way to name it, so
// there is no refusal to argue with and no policy decision to get
// wrong. This is the same move as ArtefactStore being the review
// fork's entire write surface: make the claim a property of what
// exists rather than of what is checked.
//
// An empty allowlist means the node's full set, NOT the empty set. A
// bot created without anyone stating its tools should be as capable as
// the assistant was before it existed; silently muting one would
// present as a broken bot rather than as a decision somebody made.
// Refusing everything is expressed by a policy rule, which is the
// thing that says no.
func (p *BotProfile) FilterTools(all []Tool) []Tool {
	if p == nil {
		return all
	}
	out := all
	if len(p.Tools) > 0 {
		allowed := make(map[string]struct{}, len(p.Tools))
		for _, name := range p.Tools {
			allowed[strings.TrimSpace(name)] = struct{}{}
		}
		kept := make([]Tool, 0, len(all))
		for _, t := range all {
			if _, ok := allowed[t.Name]; ok {
				kept = append(kept, t)
			}
		}
		out = kept
	}
	if len(p.Denied) == 0 {
		return out
	}
	// Subtracted last and unconditionally, so it holds whether the bot
	// had an allowlist or not.
	kept := make([]Tool, 0, len(out))
	for _, t := range out {
		if !slices.Contains(p.Denied, t.Name) {
			kept = append(kept, t)
		}
	}
	return kept
}

// Without returns a copy of the profile that will never be shown the
// named tools.
//
// The delegation guard uses it: a bot reached through ask_bot is
// handed a profile Without("ask_bot"), so one hop is the depth limit
// because a second hop is unexpressible. A counter would have been a
// rule somebody has to keep enforcing; this is a fact about the
// registry the child was built with.
func (p *BotProfile) Without(names ...string) *BotProfile {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Denied = append(append([]string(nil), p.Denied...), names...)
	return &clone
}

// BotResolver answers "which bot is this". Implemented over the raft
// registry; an interface so compute does not depend on which store the
// registry lives in, and so tests can substitute a map.
type BotResolver interface {
	ResolveBot(ctx context.Context, botID string) (*BotProfile, error)
}

// ErrBotDisabled is returned for a bot that exists but is switched
// off. Distinct from "no such bot" because the fix is different and a
// caller that conflates them tells the user to create something that
// already exists.
var ErrBotDisabled = errors.New("turnrunner: bot is disabled")

// TurnRunner is the one place a headless agent turn is started.
//
// "Headless" means no human is waiting on a channel: a scheduled
// routine, a commitment firing, a research worker, an inbox item being
// worked. Channel handlers keep their own entry points because they
// have a session to write, a responder to drive and a confirmation
// path to route — genuinely a different shape.
//
// It exists because there were three copies of this. R33 found them
// and named the fix as belonging in the writing rather than the
// post-mortem: one turn-runner, N callers. Every later feature that
// wants to run a turn — the inbox drain, ask_bot — is a caller here
// and not a fourth copy.
type TurnRunner struct {
	agent    TurnLoop
	bots     BotResolver
	caps     BudgetCaps
	sessions SessionWriter
	log      *slog.Logger
}

// SessionWriter persists a finished headless turn.
//
// Headless turns wrote no transcript at all. The console carried a
// deep-link from every queue item to "what the bot actually did" and
// every one of them pointed at nothing, because the only code that
// persisted a conversation lived on the channel path — so a turn you
// started from Telegram was recorded and the identical turn started by
// the inbox was not.
//
// Declared here, on the one place every headless turn passes through,
// so a future caller cannot forget it the way each existing one did.
type SessionWriter interface {
	// AppendTurn stores msgs and returns the id of the session they
	// landed in, for the caller to record alongside the work.
	AppendTurn(ctx context.Context, channel, channelID, turnID string, msgs []Message) (string, error)

	// LoadTurns returns the conversation so far for the same address,
	// capped at n verbatim messages, plus the summary standing in for
	// anything older.
	//
	// Reading matters as much as writing: persisting a transcript
	// nothing loads gives you a bot that answers every message as
	// though it were the first. The console showed that as a
	// conversation that vanished on refresh, but the more serious half
	// was invisible — the MODEL had no history either, so a follow-up
	// question landed with no idea what it followed.
	LoadTurns(ctx context.Context, channel, channelID string, n int) (history []Message, summary string, err error)
}

// TurnLoop is the agent loop the runner drives. *Agent implements it.
//
// An interface so a caller can exercise the runner's own behaviour —
// budget derivation, profile resolution, tool filtering — without
// standing up a provider. That matters because those are the parts
// with invariants worth asserting, and a test that has to mock a whole
// LLM to reach them tends not to get written.
type TurnLoop interface {
	RunToolCallLoop(ctx context.Context, req ProcessMessageRequest) (*ProcessMessageResponse, error)
	// ResumeFromConfirmation continues a turn that stopped to ask, once
	// somebody has answered. Part of the loop rather than a separate
	// capability because a runner that can start a turn and not finish
	// one can only ever fail closed.
	ResumeFromConfirmation(ctx context.Context, req ProcessMessageRequest, prior []Message) (*ProcessMessageResponse, error)
}

// NewTurnRunner constructs the runner. A nil resolver is usable and
// means every turn runs as the node's default assistant, which is
// exactly the behaviour before bots existed.
func NewTurnRunner(agent TurnLoop, bots BotResolver, caps BudgetCaps, sessions SessionWriter, log *slog.Logger) (*TurnRunner, error) {
	if agent == nil {
		return nil, errors.New("turnrunner: agent required")
	}
	if log == nil {
		log = slog.Default()
	}
	return &TurnRunner{agent: agent, bots: bots, caps: caps, sessions: sessions, log: log}, nil
}

// defaultHistoryTurns bounds how much of a conversation a headless
// turn replays.
//
// Bounded because the context budget is: an unbounded replay makes the
// oldest message in a long thread silently evict the system prompt.
// The summary covers anything past it.
const defaultHistoryTurns = 40

// TurnRequest is one unit of headless work.
type TurnRequest struct {
	// BotID names which bot runs this. Empty runs as the node default,
	// preserving pre-bot behaviour for a caller that has no opinion.
	BotID string

	// Prompt is the instruction. Required — an empty one would spend a
	// provider call asking the model to answer its own system prompt.
	Prompt string

	// Origin labels the turn id and the logs: "task", "commitment",
	// "inbox", "worker". Required, because a turn whose origin nobody
	// recorded is a turn nobody can account for afterwards.
	Origin string

	// OriginID is the record that caused this turn — the task id, the
	// commitment id, the inbox item id.
	OriginID string

	// Claims override the ones derived from the bot. Callers that must
	// attribute work to the human who scheduled it set this.
	Claims *types.Claims

	// Channel / ChannelID route any proactive reply the turn decides to
	// send. Empty for work with no originating conversation.
	Channel   string
	ChannelID string

	// Reservation, when set, is the budget this turn draws from rather
	// than a fresh one. Delegation passes the parent's here so a tree
	// of turns is bounded by one number — see TurnBudget.Sub.
	Reservation *TurnBudget

	// RequestedBy is the person this work traces back to, carried
	// across a delegation hop. See turn.Identity.RequestedBy.
	RequestedBy string

	// OnDelta streams assistant text to whoever started this turn.
	// Nil for work nobody is watching, which is most of it.
	OnDelta func(string)

	// Confirm is how this turn asks a human for permission.
	//
	// Nil means fail closed, which is what every headless caller did
	// unconditionally: a turn that reached a guarded tool stopped and
	// reported that it could not ask. Supplying this lets a caller
	// that HAS somebody watching — the console, with the person who
	// started the turn still on the other end of the stream — put the
	// question to them and carry on.
	//
	// Returning false denies. Returning an error abandons the turn
	// rather than treating a broken prompt channel as consent.
	Confirm func(ctx context.Context, reason, action, resource string) (bool, error)

	// Caps tighten this one turn's budget beyond the bot's and the
	// node's. A fan-out sets it to one worker's share of the
	// reservation: the share stops one worker starving its siblings,
	// the reservation stops the run. Zero fields inherit.
	Caps BudgetCaps

	// Profile short-circuits resolution. Delegation uses it to hand a
	// child a profile it has already narrowed (see BotProfile.Without),
	// so the narrowing cannot be undone by a fresh lookup.
	Profile *BotProfile

	// SystemPrompt replaces the assembled one. Set by callers whose
	// turn is a transformation rather than a conversation — a research
	// worker answering one sub-question wants its worker prompt, not
	// the assistant's personality.
	SystemPrompt string

	// Tools overrides the advertised tool list. The bot's registry
	// filter still applies ON TOP: an override widens what a turn is
	// about, never what a bot may reach.
	Tools []Tool

	// TurnIDOverride pins the turn id instead of deriving one. For
	// callers whose ids already encode a position a log reader depends
	// on — a research worker's "<task>/worker/3".
	TurnIDOverride string
}

// Run executes one turn and returns the agent's response.
func (r *TurnRunner) Run(ctx context.Context, req TurnRequest) (*ProcessMessageResponse, error) {
	if r == nil || r.agent == nil {
		return nil, errors.New("turnrunner: not wired")
	}
	if strings.TrimSpace(req.Prompt) == "" {
		return nil, fmt.Errorf("turnrunner: %s %q has no prompt", req.Origin, req.OriginID)
	}
	if strings.TrimSpace(req.Origin) == "" {
		return nil, errors.New("turnrunner: origin required")
	}

	profile, err := r.profileFor(ctx, req)
	if err != nil {
		return nil, err
	}

	budget, err := r.budgetFor(profile, req.Caps, req.Reservation)
	if err != nil {
		return nil, err
	}

	claims := req.Claims
	if claims == nil {
		claims = botClaims(profile)
	}

	turnID := req.TurnIDOverride
	if turnID == "" {
		turnID = fmt.Sprintf("%s-%s-%d", req.Origin, req.OriginID, time.Now().UnixNano())
	}
	// The conversation so far, when this turn has an address to have
	// one at. An inbox item gets its own session per item, so there is
	// nothing prior and it stays a clean single-shot — which is what
	// an independent task should be.
	var history []Message
	var summary string
	if r.sessions != nil && req.Channel != "" && req.ChannelID != "" {
		h, sum, herr := r.sessions.LoadTurns(ctx, req.Channel, req.ChannelID, defaultHistoryTurns)
		if herr != nil {
			// A turn with no history is worse than one with it, and
			// far better than no turn at all.
			r.log.Warn("turnrunner: could not load the conversation; answering without it",
				"origin", req.Origin, "origin_id", req.OriginID, "err", herr)
		} else {
			history, summary = h, sum
		}
	}

	agentReq := ProcessMessageRequest{
		Message:             req.Prompt,
		ConversationHistory: history,
		ConversationSummary: summary,
		Claims:              claims,
		TurnID:              turnID,
		Budget:              budget,
		Channel:             req.Channel,
		ChannelID:           req.ChannelID,
		BotID:               profile.botID(),
		Bot:                 profile,
		SystemPrompt:        req.SystemPrompt,
		Tools:               req.Tools,
		OnDelta:             req.OnDelta,
		RequestedBy:         req.RequestedBy,
	}
	resp, err := r.agent.RunToolCallLoop(ctx, agentReq)
	if err != nil {
		return nil, fmt.Errorf("turnrunner: %s %q: %w", req.Origin, req.OriginID, err)
	}

	// Ask, if this caller has somebody to ask. A turn may stop more
	// than once, so this loops until the turn either finishes or is
	// refused.
	for resp.NeedsConfirmation && req.Confirm != nil {
		approved, cerr := req.Confirm(ctx, resp.ConfirmationReason,
			resp.ConfirmationAction, resp.ConfirmationResource)
		if cerr != nil {
			// A prompt channel that failed is not a "no" and certainly
			// not a "yes" — it is an unanswered question, and the turn
			// stops with the reason intact for the caller to report.
			r.log.Warn("turnrunner: could not put the confirmation to anyone",
				"origin", req.Origin, "origin_id", req.OriginID,
				"bot", profile.botID(), "err", cerr)
			break
		}
		if !approved {
			resp.Reply = "Not done — you declined: " + resp.ConfirmationReason
			resp.NeedsConfirmation = false
			resp.ConfirmationReason = ""
			break
		}

		// Approved: lift the caps for the operation that was just
		// authorised and re-enter where the turn stopped.
		agentReq.Budget.Relax()
		resumeCtx := WithTurnApproval(ctx, resp.ConfirmationAction, resp.ConfirmationResource)
		resumed, rerr := r.agent.ResumeFromConfirmation(resumeCtx, agentReq, resp.Messages)
		if rerr != nil {
			return nil, fmt.Errorf("turnrunner: %s %q: resume after approval: %w",
				req.Origin, req.OriginID, rerr)
		}
		// Carry the earlier tool calls forward: the receipt for a
		// resumed turn must cover the whole turn, not just the half
		// that ran after the question.
		resumed.ToolCalls = append(resp.ToolCalls, resumed.ToolCalls...)
		resp = resumed
	}

	// Persist before returning, so the id is on the response the caller
	// records. A turn that cannot be written is still a turn that
	// happened: log it and carry on rather than failing work that has
	// already been done and paid for.
	if r.sessions != nil && req.Channel != "" && req.ChannelID != "" {
		newTurn := resp.Messages
		if resp.TurnStartIndex > 0 && resp.TurnStartIndex <= len(resp.Messages) {
			newTurn = resp.Messages[resp.TurnStartIndex:]
		}
		if len(newTurn) > 0 {
			sessionID, serr := r.sessions.AppendTurn(ctx, req.Channel, req.ChannelID, turnID, newTurn)
			if serr != nil {
				r.log.Warn("turnrunner: could not persist the transcript",
					"origin", req.Origin, "origin_id", req.OriginID,
					"bot", profile.botID(), "err", serr)
			} else {
				resp.SessionID = sessionID
			}
		}
	}

	r.log.Info("turnrunner: turn completed",
		"origin", req.Origin,
		"origin_id", req.OriginID,
		"bot", profile.botID(),
		"turn_id", turnID,
		"tool_calls", len(resp.ToolCalls),
		"needs_confirm", resp.NeedsConfirmation,
	)
	return resp, nil
}

func (r *TurnRunner) profileFor(ctx context.Context, req TurnRequest) (*BotProfile, error) {
	if req.Profile != nil {
		return req.Profile, nil
	}
	if req.BotID == "" || r.bots == nil {
		return nil, nil
	}
	profile, err := r.bots.ResolveBot(ctx, req.BotID)
	if err != nil {
		return nil, fmt.Errorf("turnrunner: resolve bot %q: %w", req.BotID, err)
	}
	return profile, nil
}

// budgetFor produces this turn's budget.
//
// With a reservation it is a child of it, so a tree of delegated turns
// is bounded by the one number the root authorised however the tree is
// shaped. Without one it is a fresh budget from the bot's caps falling
// back to the node's.
func (r *TurnRunner) budgetFor(profile *BotProfile, turnCaps BudgetCaps, reservation *TurnBudget) (*TurnBudget, error) {
	caps := r.caps
	if profile != nil {
		caps = mergeCaps(caps, profile.Caps)
	}
	caps = mergeCaps(caps, turnCaps)
	if reservation != nil {
		return reservation.Sub(caps)
	}
	b, err := NewTurnBudget(caps)
	if err != nil {
		return nil, fmt.Errorf("turnrunner: budget: %w", err)
	}
	return b, nil
}

// mergeCaps lets a bot tighten the node's caps but not escape them.
//
// Zero on a bot's cap means "inherit", so a record written without
// thinking about budgets gets the operator's limits. A bot naming a
// LARGER cap than the node's is clamped rather than rejected: the
// operator's number is the one that was chosen deliberately, and a bot
// record the coordinator wrote is not the place to overrule it.
func mergeCaps(node, bot BudgetCaps) BudgetCaps {
	out := node
	if bot.MaxToolCalls > 0 && (node.MaxToolCalls == 0 || bot.MaxToolCalls < node.MaxToolCalls) {
		out.MaxToolCalls = bot.MaxToolCalls
	}
	if bot.MaxSpendUSD > 0 && (node.MaxSpendUSD == 0 || bot.MaxSpendUSD < node.MaxSpendUSD) {
		out.MaxSpendUSD = bot.MaxSpendUSD
	}
	if bot.MaxEgressBytes > 0 && (node.MaxEgressBytes == 0 || bot.MaxEgressBytes < node.MaxEgressBytes) {
		out.MaxEgressBytes = bot.MaxEgressBytes
	}
	return out
}

// botClaims builds the identity a bot's turn runs as. Policy rules are
// written against "bot:engineering" as a subject, so the bare id goes
// in UserID — the engine adds the kind itself, and passing a rendered
// principal would produce "user:bot:engineering" and match nothing.
func botClaims(profile *BotProfile) *types.Claims {
	if profile == nil {
		return &types.Claims{UserID: "scheduler", Scope: "default"}
	}
	return &types.Claims{
		UserID: identity.Bot(profile.ID).String(),
		Scope:  identity.Bot(profile.ID).String(),
		Roles:  []string{identity.KindBot},
	}
}

func (p *BotProfile) botID() string {
	if p == nil {
		return ""
	}
	return p.ID
}

// appendBotBrief renders a bot's standing brief after the node's soul
// body. Returns body unchanged when there is no bot or no brief, so
// the pre-bot prompt is byte-identical.
//
// The heading matters. Without it the brief reads as more of the
// operator's soul prose, and a bot told "you are the engineer" in the
// middle of a paragraph about tone follows it about as reliably as it
// follows the tone advice.
func appendBotBrief(body string, profile *BotProfile) string {
	if profile == nil {
		return body
	}
	brief := strings.TrimSpace(profile.Instructions)
	name := strings.TrimSpace(profile.DisplayName)
	// A profile with neither a brief nor a name is not a team member —
	// it is the node's own assistant wearing a bot record, which is
	// what an upgraded deployment has before anybody writes one. That
	// case leaves the operator's soul byte-identical, and the voice
	// below would be a change to how the assistant talks that nobody
	// asked for.
	if brief == "" && name == "" {
		return body
	}

	var b strings.Builder
	if body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	if brief != "" {
		b.WriteString("## Your role\n\n")
		if name != "" {
			fmt.Fprintf(&b, "You are %s.\n\n", name)
		}
		b.WriteString(brief)
		b.WriteString("\n\n")
	} else {
		fmt.Fprintf(&b, "## Your role\n\nYou are %s.\n\n", name)
	}
	b.WriteString(botVoice)
	return b.String()
}

// botVoice is how a bot addresses a person, appended to every bot turn
// whether or not the operator wrote a brief.
//
// Added because the machinery leaked. Asked for a status on Telegram,
// the coordinator replied with tool names, a ULID and the phrase "the
// durable route is open" — an accurate account of its own plumbing and
// no use at all to somebody holding a phone. Left to themselves models
// narrate the mechanism, because the mechanism is what just happened
// to them.
//
// The last paragraph earns its place separately: that same reply took
// an empty answer from another bot and reported it as "he didn't
// respond — possibly a sleeping agent, probably nothing to report".
// None of that was known. A silence is a thing to say plainly, not a
// thing to explain.
const botVoice = `## How to report

You are talking to a person, not writing a log. Say what happened and
what it means for them.

Do not name your tools, quote record ids, or describe the mechanism.
"I've asked engineering and I'll come back when they answer" — not
"ask_bot returned empty so I used inbox_post, id 01M2R5N60Z...". The
plumbing is yours; the outcome is theirs.

If you could not find something out, say so plainly and say what you
are doing about it. Do not invent a reason for a silence and report the
guess as a finding.`
