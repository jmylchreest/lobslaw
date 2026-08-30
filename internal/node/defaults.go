package node

// Compiled-in defaults for wiring decisions the operator has not made.
//
// These are the values a node falls back to while assembling its
// subsystems. Named rather than written inline because a schedule or a
// wire format buried in a wireX function is invisible to anyone
// reading the config reference and wondering what happens if they
// leave the key out.
const (
	// defaultProviderFormat is the wire protocol assumed for a
	// provider that declares none. OpenAI's chat-completions shape,
	// which is what most vendors emulate — so it is the assumption
	// most likely to work rather than merely the most common vendor.
	defaultProviderFormat = "openai"

	// defaultSessionSummarySchedule is how often session summarisation
	// runs when unscheduled. Hourly keeps the window small enough that
	// a summary still has the detail of the conversation in it.
	defaultSessionSummarySchedule = "@hourly"

	// defaultDreamSchedule is when memory consolidation runs when
	// unscheduled: 02:00 daily. Consolidation is expensive and rewrites
	// what recall reads, so it wants the quietest hour rather than a
	// fixed interval that will eventually land mid-conversation.
	defaultDreamSchedule = "0 2 * * *"

	// defaultSchedulerCreator attributes a turn the scheduler started
	// rather than a person. It reaches the audit log and the ownership
	// model, where "who asked for this" must never be blank.
	defaultSchedulerCreator = "scheduler"
)
