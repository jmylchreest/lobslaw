package compute

import (
	"context"

	"github.com/jmylchreest/lobslaw/internal/commandrisk"
)

// Carrying a classification into a policy decision.
//
// These live in compute rather than in internal/commandrisk because
// they are about POLICY, not about reading a command. The classifier
// answers "what does this do"; a grant key and a request-scoped label
// set are how that answer reaches the engine and the session store.
// Keeping them apart is what lets the classifier be a package with no
// idea what an action or a grant is.

// RiskGrantResource is the key a grant covering ONE LABEL is recorded
// under.
//
// Per label rather than per command, because that is what makes the
// grant compose: a conversation that has approved "reads" and "writes"
// satisfies a command carrying both, without anybody having granted
// that exact pair. The gate subtracts what is already granted and asks
// about the remainder.
//
// A sentinel in the shape of the "(cwd=…)" and "(remote=…)" keys and
// of !unclassified: a real key always begins with a rendered command
// token, and NormaliseCommand single-quotes anything starting with "("
// — so no command can land in this namespace by accident, and an
// operator writing one has said what they meant.
func RiskGrantResource(label commandrisk.RiskLabel) string {
	if !label.Valid() || label == commandrisk.LabelUnreadable {
		// Unreadable is never grantable. "Allow everything I could not
		// read" is not a decision anybody can make.
		return ""
	}
	return "(risk=" + string(label) + ")"
}

// commandLabelsKey carries the classified labels from the approval gate
// to the policy condition evaluator.
//
// On the context rather than in the Evaluate signature, because the
// engine's question is (subject, action, resource) and widening it for
// one condition would put a shell concept into every policy check.
// ConditionEvaluator already takes a ctx for exactly this.
type commandLabelsKey struct{}

// WithCommandLabels records what this request was classified as.
//
// The labels come from the classifier over the parameters the executor
// is about to run, never from anything the model wrote as prose — the
// same reason the turn identity comes from the request context.
func WithCommandLabels(ctx context.Context, labels []commandrisk.RiskLabel) context.Context {
	kept := make([]commandrisk.RiskLabel, 0, len(labels))
	for _, l := range labels {
		if l.Valid() {
			kept = append(kept, l)
		}
	}
	if len(kept) == 0 {
		return ctx
	}
	return context.WithValue(ctx, commandLabelsKey{}, kept)
}

// CommandLabelsFrom reads them back. ok=false means this request was
// never classified — a memory write, say — and a rule conditioned on
// labels must not apply to it.
func CommandLabelsFrom(ctx context.Context) ([]commandrisk.RiskLabel, bool) {
	l, ok := ctx.Value(commandLabelsKey{}).([]commandrisk.RiskLabel)
	return l, ok
}
