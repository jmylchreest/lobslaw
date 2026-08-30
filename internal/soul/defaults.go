package soul

import "time"

// Defaults applied to a sparse SOUL.md so an operator writing three
// lines gets a working persona rather than a validation error.
//
// Named because these are product decisions, not implementation
// details: what the agent does when nobody has said otherwise is the
// behaviour most deployments will actually run, and it should be
// readable without following applyDefaults into the loader.
const (
	// DefaultScope is the audience a soul applies to when it names
	// none — every user of the node.
	DefaultScope = "default"

	// DefaultLanguage is the reply language when the soul names none.
	DefaultLanguage = "en"

	// DefaultEmojiUsage starts conservative. Emoji are easy for a user
	// to ask for and awkward to ask a stranger to stop, so the
	// default is the one that is comfortable to change.
	DefaultEmojiUsage = "minimal"

	// DefaultFeedbackCoefficient scales how far one piece of feedback
	// moves a personality dimension. Deliberately small: the soul
	// should drift over a relationship rather than swing on a single
	// remark, and a coefficient that feels responsive in a test is one
	// that makes the agent a different person by Friday.
	DefaultFeedbackCoefficient = 0.15

	// DefaultCooldownPeriod is the minimum gap between two adjustments
	// to the same dimension. A day, so a single bad conversation
	// cannot compound — the second nudge has to survive somebody
	// sleeping on it.
	//
	// Was written as 24 * 3600 * 1_000_000_000 with a comment saying
	// "24h in ns", which is the arithmetic the type already does.
	DefaultCooldownPeriod = 24 * time.Hour

	// DefaultFeedbackClassifier reads user feedback with the model
	// rather than keyword matching. "that was a bit much" carries no
	// keyword worth matching, and the cases where tone matters are
	// exactly the ones phrased indirectly.
	DefaultFeedbackClassifier = "llm"
)
