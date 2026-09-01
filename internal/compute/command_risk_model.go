package compute

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"
)

// Asking a model what a command does, without letting it decide.
//
// The static classifier is sound and incomplete: it refuses a loop, a
// substitution, a variable in the command slot. Those are exactly the
// shapes a probing agent produces, so the commands that generate the
// most confirmations are the ones static reading helps least with. A
// model can read them.
//
// What it must not do is talk its way past the gate. So the contract
// is deliberately narrow:
//
//   - It answers the SAME enum the rest of the system uses. Not prose,
//     not a summary, not a score. A verdict that needs interpreting is
//     one the policy engine cannot act on, and one nobody can test.
//   - Anything outside the enum, unparseable, late, or hedged is
//     DISCARDED. The static verdict stands.
//   - Under the default trust setting it may only RAISE a tier. A
//     command that argues itself down is the entire vulnerability;
//     one that argues itself up costs an extra confirmation.
//
// The command text reaching the model is attacker-influenced — that is
// the premise of the whole approval subsystem — which is why nothing
// but an enum value is ever read out of the reply.

// RiskTrust is how far a model verdict is allowed to move the tier.
type RiskTrust string

const (
	// RiskTrustAdvisory lets the model raise a tier and nothing else.
	// The safe default: a wrong answer costs a confirmation somebody
	// did not need to give.
	RiskTrustAdvisory RiskTrust = "advisory"

	// RiskTrustResolveUnknown additionally lets the model resolve
	// UNKNOWN down to a concrete tier — and only unknown. Where the
	// static classifier has an opinion it keeps it, so soundness is
	// given up exactly where there was none to begin with.
	//
	// This is the setting that makes a probe like
	// `for b in node bun; do command -v "$b"; done` stop asking.
	RiskTrustResolveUnknown RiskTrust = "resolve_unknown"
)

// Valid reports whether t is a recognised trust setting.
func (t RiskTrust) Valid() bool {
	return t == RiskTrustAdvisory || t == RiskTrustResolveUnknown
}

// ParseRiskTrust reads an operator's setting.
//
// An empty value takes advisory. Anything else unrecognised returns
// advisory AND an error: this setting decides whether a model may talk
// a command's tier down, so a typo must fail towards the safe reading
// and say so, never towards the permissive one.
func ParseRiskTrust(s string) (RiskTrust, error) {
	trimmed := RiskTrust(strings.ToLower(strings.TrimSpace(s)))
	if trimmed == "" {
		return RiskTrustAdvisory, nil
	}
	if !trimmed.Valid() {
		return RiskTrustAdvisory, fmt.Errorf("unknown verdict_trust %q (want %q or %q)",
			s, RiskTrustAdvisory, RiskTrustResolveUnknown)
	}
	return trimmed, nil
}

// riskJudgeSystemPrompt asks for the internal enum directly.
//
// The tier names, the scratch-versus-elsewhere distinction and the
// "most severe step wins" rule are all stated in the model's terms
// because they are the rules the static classifier follows. Two
// classifiers answering the same question differently would be worse
// than one.
const riskJudgeSystemPrompt = `You classify a shell command by what it DOES. Reply with JSON only, no prose, no code fences:

{"tier": "read"|"write"|"network"|"destructive"|"unknown", "confidence": "high"|"low"}

read: inspects state and changes nothing.
write: creates, copies, appends to or edits files in ordinary places.
network: contacts another host — fetching, uploading, or running something remotely.
destructive: deletes data, kills processes, changes machine or system state, or runs as root.
unknown: you cannot tell what it does.

Judge the WHOLE command line — every step of a pipeline, a ;-list or an &&-chain — and answer with the MOST SEVERE tier any step reaches.

Deleting under a throwaway directory is "write"; deleting anything else is "destructive". Writing into /etc, /usr, /bin, /boot or / is "destructive" whatever the program.

Answer "unknown" if any part is something you cannot read: a variable whose value you cannot see, a command substitution, or code fetched and piped into a shell.

confidence: "low" whenever you are guessing. A low-confidence answer is thrown away, so guessing gains you nothing.`

// riskJudgeMaxTokens bounds the reply. The answer is one small object.
const riskJudgeMaxTokens = 64

// riskJudgeTimeout bounds the call.
//
// A confirmation is being composed while this runs, so it is latency a
// person is waiting on. Failing to a static verdict is always
// available and always correct, which makes a short bound cheap.
const riskJudgeTimeout = 5 * time.Second

// riskCommandMax bounds what is sent. A command longer than this is
// not one a model reads usefully, and the static verdict for it is
// already unknown.
const riskCommandMax = 4000

// RiskJudge asks a model for a command's tier.
//
// A nil *RiskJudge is usable and declines, so callers do not branch on
// whether one was configured — the same shape as Judge.
type RiskJudge struct {
	provider LLMProvider
	model    string
	trust    RiskTrust
	log      *slog.Logger
}

// NewRiskJudge wires a judge. A nil provider gives a nil judge:
// absence rather than a disabled flag. An unrecognised trust setting
// falls back to advisory rather than to the permissive one.
func NewRiskJudge(provider LLMProvider, model string, trust RiskTrust, log *slog.Logger) *RiskJudge {
	if provider == nil {
		return nil
	}
	if !trust.Valid() {
		trust = RiskTrustAdvisory
	}
	if log == nil {
		log = slog.Default()
	}
	return &RiskJudge{provider: provider, model: model, trust: trust, log: log}
}

// Trust reports what this judge is permitted to move.
func (j *RiskJudge) Trust() RiskTrust {
	if j == nil {
		return RiskTrustAdvisory
	}
	return j.trust
}

// Classify asks the model. ok=false means "no usable answer", which is
// every failure mode collapsed into one: no judge, no provider, a
// timeout, unparseable JSON, a tier outside the enum, or an answer the
// model itself marked low-confidence.
func (j *RiskJudge) Classify(ctx context.Context, command string) (CommandRisk, bool) {
	cmd := strings.TrimSpace(command)
	if j == nil || j.provider == nil || cmd == "" || len(cmd) > riskCommandMax {
		return "", false
	}

	ctx, cancel := context.WithTimeout(ctx, riskJudgeTimeout)
	defer cancel()

	resp, err := j.provider.Chat(ctx, ChatRequest{
		Model:       j.model,
		MaxTokens:   riskJudgeMaxTokens,
		Temperature: 0,
		Messages: []Message{
			{Role: "system", Content: riskJudgeSystemPrompt},
			{Role: "user", Content: cmd},
		},
	})
	if err != nil || resp == nil {
		// DEBUG, not WARN. The static verdict still governs and the
		// user is still asked; a provider having a bad minute must not
		// fill an operator's log with warnings about prompts that all
		// worked.
		j.log.Debug("command risk: judge unavailable; the static verdict stands", "error", err)
		return "", false
	}
	return parseRiskVerdict(resp.Content, j.log)
}

// parseRiskVerdict reads the model's object, discarding anything it
// got wrong rather than propagating it.
func parseRiskVerdict(content string, log *slog.Logger) (CommandRisk, bool) {
	raw := extractObject(content)
	if raw == "" {
		log.Debug("command risk: no JSON object in the reply")
		return "", false
	}
	var parsed struct {
		Tier       string `json:"tier"`
		Confidence string `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		log.Debug("command risk: unparseable verdict", "error", err)
		return "", false
	}
	// A model that says it is guessing is taken at its word. The static
	// verdict is a real answer; a hedged one is not an improvement on
	// it.
	if strings.ToLower(strings.TrimSpace(parsed.Confidence)) != "high" {
		return "", false
	}
	tier := CommandRisk(strings.ToLower(strings.TrimSpace(parsed.Tier)))
	if !tier.Valid() {
		log.Debug("command risk: verdict outside the enum", "tier", parsed.Tier)
		return "", false
	}
	return tier, true
}

// activeRiskJudge is the judge in force. A package var for the reason
// activeCommandClasses is one: the summariser and the resolver are
// plain function values fixed by the gate's signature.
var activeRiskJudge atomic.Pointer[RiskJudge]

// SetRiskJudge installs the judge. A nil judge removes it, which is
// what an operator who clears the config gets.
func SetRiskJudge(j *RiskJudge) {
	if j == nil {
		activeRiskJudge.Store(nil)
		return
	}
	activeRiskJudge.Store(j)
}

// ActiveRiskJudge returns the judge in force, or nil.
func ActiveRiskJudge() *RiskJudge { return activeRiskJudge.Load() }

// VerdictFor classifies a call's parameters, consulting a configured
// model where one is wired.
//
// This is what the prompt and the gate both read, so there is exactly
// one place the two verdicts are combined.
func VerdictFor(ctx context.Context, params map[string]string) RiskVerdict {
	return adjudicate(ctx, ClassifyRisk(params["command"]), params["command"], ActiveRiskJudge())
}

// adjudicate combines the static verdict with a model's, under the
// trust setting.
//
// Split out from VerdictFor so the combination rules — the part that
// decides whether something runs unasked — are testable without a
// provider.
func adjudicate(ctx context.Context, static RiskVerdict, command string, judge *RiskJudge) RiskVerdict {
	if judge == nil {
		return static
	}
	// Nothing to gain: a read verdict cannot be lowered and asking
	// costs a model call on the path that is meant to be free.
	if static.Tier == RiskRead {
		return static
	}
	tier, ok := judge.Classify(ctx, command)
	if !ok {
		return static
	}

	switch {
	case tier.Rank() > static.Tier.Rank():
		// Raising is allowed under every trust setting. The cost of a
		// wrong raise is a confirmation somebody did not need to give.
		static.Tier = tier
		static.Reason = reasonFor[tier]
		static.FromModel = true
	case static.Tier == RiskUnknown && judge.Trust() == RiskTrustResolveUnknown && tier != RiskUnknown:
		// The one permitted de-escalation, and only into the gap where
		// the static classifier had no opinion at all.
		static.Tier = tier
		static.Reason = reasonFor[tier]
		static.FromModel = true
	}
	return static
}
