package compute

import (
	"context"
	"strings"
	"text/template"
)

// Multi-step chains: a chain's later steps run as a PIPELINE.
//
// Step N's output is rendered into step N+1's prompt_template, and the
// last step's output is the answer. A "reviewer" is then just a step
// whose template says "improve this" — the mechanism does not need to
// know what a reviewer is, which is why roles stay descriptive rather
// than becoming behaviour.
//
// ON THE TURN'S ANSWER, NOT ON EACH ROUND-TRIP. Step 0 is the whole
// tool-call loop, however many round-trips that takes; the later steps
// run once, on what it finally said. Running a reviewer against an
// intermediate tool call would be reviewing a decision to look
// something up.

// stepPrompt is what a step's template is rendered against.
type stepPrompt struct {
	// Previous is the preceding step's output — the thing this step
	// exists to work on.
	Previous string
	// Message is the user's original text. A reviewer that cannot see
	// what was asked can only check the answer against itself.
	Message string
}

// defaultStepTemplate is used by a step that declares none.
//
// A step with no template is still a step: the operator asked for a
// second provider to see this. Handing it the previous output plus the
// question is the least surprising reading of that, and it is what a
// bare `[[compute.chains.steps]]` with only a provider means.
const defaultStepTemplate = `The user asked:

{{.Message}}

A draft reply was produced:

{{.Previous}}

Reply with an improved version. Output the reply itself, with no preamble and no commentary about what you changed.`

// runChainSteps runs every step after the first, returning the final
// answer.
//
// A FAILING STEP RETURNS THE BEST ANSWER SO FAR rather than an error.
// By this point the user has a complete reply from step 0; losing it
// because a reviewer's provider was rate-limited would make the chain
// a liability, and the entire failover layer exists on the premise
// that one provider being unavailable is not the turn's problem.
func (a *Agent) runChainSteps(ctx context.Context, req ProcessMessageRequest, answer string) string {
	route := RouteFrom(ctx)
	if route == nil || len(route.Steps) < 2 {
		return answer
	}

	current := answer
	for i, step := range route.Steps[1:] {
		out, err := a.runChainStep(ctx, req, step, current)
		if err != nil {
			a.cfg.Logger.Warn("chain: step failed; keeping the previous answer",
				"chain", route.ChainLabel, "step", i+1, "role", step.Role,
				"provider", step.Provider.Label, "error", err)
			return current
		}
		if strings.TrimSpace(out) == "" {
			// An empty reply is a failure that did not announce
			// itself. Returning it would replace a good answer with
			// silence — the one outcome worse than not running the
			// step at all.
			a.cfg.Logger.Warn("chain: step returned nothing; keeping the previous answer",
				"chain", route.ChainLabel, "step", i+1, "provider", step.Provider.Label)
			return current
		}
		a.cfg.Logger.Debug("chain: step applied",
			"chain", route.ChainLabel, "step", i+1, "role", step.Role,
			"provider", step.Provider.Label)
		current = out
	}
	return current
}

// runChainStep performs one step.
//
// Routed through callLLM rather than dispatching directly, so a step
// is billed, hooked, budgeted, traced, floor-checked and failed over
// exactly as the main turn is. A second code path for provider calls
// is how one of them comes to be missing the trust floor.
func (a *Agent) runChainStep(
	ctx context.Context,
	req ProcessMessageRequest,
	step ResolveStep,
	previous string,
) (string, error) {
	prompt, err := renderStepPrompt(step.PromptTemplate, stepPrompt{
		Previous: previous,
		Message:  req.Message,
	})
	if err != nil {
		return "", err
	}

	stepReq := req
	stepReq.Model = step.Provider.Model
	// NO TOOLS. A step refines what the previous one said; handing it
	// tools would reopen the whole tool-call loop once per step, and a
	// chain of three would be three agents rather than one answer
	// passed along.
	stepReq.Tools = nil

	// The step's provider becomes the start of the backup walk, so a
	// step whose provider is rate-limited falls through to that
	// provider's backups instead of failing the step — the chain-step
	// fallthrough R8 asked for, using the walk that already existed.
	stepCtx := WithRoute(ctx, &Route{
		StartLabel: step.Provider.Label,
		ChainLabel: step.Role,
	})

	// One user message, not the conversation. The template is the
	// whole instruction — it already carries the question and the
	// draft — and replaying the tool transcript would spend the
	// context window on material the step was not asked about.
	resp, err := a.callLLM(stepCtx, stepReq, []Message{{Role: "user", Content: prompt}})
	if err != nil {
		return "", err
	}
	if req.Budget != nil {
		req.Budget.RecordCostUSD(resp.cost)
	}
	return StripReasoningTags(resp.Content), nil
}

// renderStepPrompt renders a step's template.
//
// A template that fails to parse or execute is an error rather than a
// silent fallback to the default: the operator wrote something
// specific, and quietly substituting different instructions would
// produce a reply they did not ask for and cannot account for. The
// caller turns the error into "keep the previous answer".
func renderStepPrompt(tmpl string, data stepPrompt) (string, error) {
	if strings.TrimSpace(tmpl) == "" {
		tmpl = defaultStepTemplate
	}
	t, err := template.New("step").Parse(tmpl)
	if err != nil {
		return "", err
	}
	var out strings.Builder
	if err := t.Execute(&out, data); err != nil {
		return "", err
	}
	return out.String(), nil
}
