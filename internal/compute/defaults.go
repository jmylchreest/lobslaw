package compute

// Compiled-in defaults applied when a caller leaves a field unset.
//
// Named rather than written at the point of use because several of
// these are applied in more than one place — DefaultAuthHeader was
// spelled "Authorization" in both the credential wrapper and the
// driver that sends it, which is two chances to change one of them.
const (
	// DefaultAuthHeader is the request header a credential is sent in
	// when the provider config names none. Overridable because a
	// handful of providers want a vendor-specific header instead, and
	// the credential has to be able to say so.
	DefaultAuthHeader = "Authorization"

	// DefaultMaxToolCallsPerTurn caps tool INVOCATIONS in one turn when
	// the operator has set no limit. High enough that ordinary
	// multi-step work never reaches it; low enough that a model stuck
	// re-calling a failing tool stops in the same minute rather than
	// the same hour.
	//
	// Not the same axis as [DefaultMaxToolLoops], and deliberately a
	// larger number. That one bounds round TRIPS; a single round trip
	// can carry several tool calls, because the model may request them
	// in parallel (see the range over chatResp.ToolCalls in the agent
	// loop). So neither cap subsumes the other: a wide parallel fan-out
	// reaches 30 invocations in a handful of trips, while a long
	// serial chain reaches 24 trips having made 24 calls.
	//
	// Reconciling the two to one number would remove a real bound.
	DefaultMaxToolCallsPerTurn = 30

	// DefaultArtifactBaseName names a generated artifact whose prompt
	// yielded nothing usable as a filename. Anything is better than an
	// empty base: a file called ".png" is one nothing downstream can
	// identify.
	DefaultArtifactBaseName = "artifact"

	// DefaultSearchQueryParam is the query-string key a templated
	// search provider uses when its config names none. "q" is what
	// nearly every search endpoint expects.
	DefaultSearchQueryParam = "q"
)
