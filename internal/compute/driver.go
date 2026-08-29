package compute

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// The provider layer grew one modality at a time and each reinvented
// the same ideas: the wire protocol lives in four enums under four
// names, and failover exists for chat only. See docs/dev/PROVIDERS.md.
//
// This file is the narrow waist. Three rounds of vendor research moved
// that design three times, and each time the thing that moved was one
// of four axes — interaction shape, billing unit, credential kind,
// artifact delivery. They will keep moving, so those are the only
// things a driver states, and everything above is blind to them.
//
// The corollary matters as much: retries, failover, budget, policy,
// tracing and credential refresh all live ABOVE this line. A driver
// does not know failover exists. That is what keeps adding one to a
// single file rather than a tour of the codebase.

// Modality is what a provider does. Chat and embedding are
// infrastructure, called directly by the agent loop and by memory;
// everything else is surfaced to the model as a tool, so any text-only
// model can use it.
type Modality string

const (
	ModalityChat       Modality = "chat"
	ModalityEmbedding  Modality = "embedding"
	ModalityVision     Modality = "vision"
	ModalityTranscribe Modality = "transcribe"
	ModalityDocument   Modality = "document"
	ModalitySpeak      Modality = "speak"
	ModalityImage      Modality = "image"
	ModalityVideo      Modality = "video"
)

// ChatDriver is one wire protocol for the chat modality.
//
// Deliberately identical in shape to the existing LLMProvider so the
// migration is mechanical. The difference is what surrounds it: a
// driver is now selected by (modality, driver) rather than being the
// only implementation there is.
type ChatDriver interface {
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
}

// --- failure classification ---------------------------------------

// FailureClass decides what the layer above does with an error. More
// than two, because the interesting failures are neither.
//
// A plan-billed provider that has exhausted its monthly quota is
// neither transient (retrying before the month rolls over fails
// identically) nor a request bug (the request is fine). Alibaba's
// Token Plan blocks calls outright at that point rather than charging
// overage.
//
// A rejected credential is neither either: retrying the same provider
// is pointless because the key is wrong, but the NEXT provider has its
// own key and is very likely fine. Classing it permanent — which is
// where an unclassified 401 lands — means one stale key takes down a
// turn that had two working providers behind it.
type FailureClass int

const (
	// FailurePermanent will fail the same way on a backup — a 400, a
	// bad model name, a malformed response. Falling through would turn
	// one clear error into several confusing ones.
	FailurePermanent FailureClass = iota

	// FailureTransient is worth trying elsewhere and worth retrying
	// later: 5xx, timeouts, rate limits, connection refusals.
	FailureTransient

	// FailureQuotaExhausted falls through to the backup and must be
	// surfaced loudly — an operator whose plan ran out on the 3rd and
	// who has been silently billed per call ever since will want to
	// have known. Not retried against this provider until reset.
	FailureQuotaExhausted

	// FailureCredential is a rejected or missing credential: 401, 403,
	// a revoked key, an expired token. Advances to the next provider —
	// its key is a different key — but never retries this one, and is
	// logged loudly because it is a configuration fault somebody has
	// to fix rather than weather that passes.
	FailureCredential
)

func (c FailureClass) String() string {
	switch c {
	case FailureTransient:
		return "transient"
	case FailureQuotaExhausted:
		return "quota-exhausted"
	case FailureCredential:
		return "credential-rejected"
	default:
		return "permanent"
	}
}

// DriverError lets a driver state a class explicitly. Drivers should
// return one for anything they can classify, because the fallback
// below is deliberately conservative.
type DriverError struct {
	Class FailureClass
	Err   error
}

func (e *DriverError) Error() string {
	if e.Err == nil {
		return "driver: " + e.Class.String()
	}
	return e.Err.Error()
}
func (e *DriverError) Unwrap() error { return e.Err }

// Transient wraps err as retryable.
func Transient(err error) error { return &DriverError{Class: FailureTransient, Err: err} }

// QuotaExhausted wraps err as plan-quota exhaustion.
func QuotaExhausted(err error) error {
	return &DriverError{Class: FailureQuotaExhausted, Err: err}
}

// Permanent wraps err as not worth trying elsewhere.
func Permanent(err error) error { return &DriverError{Class: FailurePermanent, Err: err} }

// CredentialRejected wraps err as a bad or missing credential.
func CredentialRejected(err error) error {
	return &DriverError{Class: FailureCredential, Err: err}
}

// ClassifyFailure reports what to do with err.
//
// An unclassified error is treated as PERMANENT, which is the
// conservative choice in the direction that matters: failing one call
// clearly beats walking a whole backup chain producing the same error
// at every hop and reporting the last one. Drivers are expected to
// classify, and the conformance suite checks that they do.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return FailurePermanent
	}
	var de *DriverError
	if errors.As(err, &de) {
		return de.Class
	}
	// Fall back to the sentinels the OpenAI client already defines, so
	// existing callers keep their behaviour through the migration.
	switch {
	case errors.Is(err, ErrLLMRateLimit):
		return FailureTransient
	case errors.Is(err, context.DeadlineExceeded):
		return FailureTransient
	default:
		return FailurePermanent
	}
}

// ClassifyHTTPStatus is the shared mapping every HTTP driver should
// start from, so "is 429 transient" is answered once rather than per
// driver. A driver may override where its vendor disagrees.
//
// 402 and 429-with-quota-wording are the interesting ones: a provider
// out of plan quota reports it differently everywhere, so drivers pass
// the body through and this looks for the shape.
func ClassifyHTTPStatus(status int, body string) FailureClass {
	switch {
	case status == http.StatusPaymentRequired:
		return FailureQuotaExhausted
	case status == http.StatusTooManyRequests:
		// Rate limiting and quota exhaustion share a status code at
		// several providers. They need different handling — one is
		// worth retrying shortly, the other not until the plan resets
		// — so the body is the only signal available.
		if looksLikeQuotaExhaustion(body) {
			return FailureQuotaExhausted
		}
		return FailureTransient
	case status == http.StatusUnauthorized, status == http.StatusForbidden:
		// Not permanent: the next provider authenticates with its own
		// key. Not transient either — retrying this one cannot help,
		// because nothing about the credential changes by waiting.
		return FailureCredential
	case status >= 500:
		return FailureTransient
	case status == http.StatusRequestTimeout:
		return FailureTransient
	default:
		return FailurePermanent
	}
}

// looksLikeQuotaExhaustion matches the wording providers use when a
// plan or credit balance is spent rather than a rate limit hit.
// Necessarily heuristic; it only ever upgrades a 429 from transient to
// quota-exhausted, and both fall through to the backup, so a false
// match costs a retry rather than a failed turn.
func looksLikeQuotaExhaustion(body string) bool {
	b := strings.ToLower(body)
	for _, marker := range []string{
		"insufficient_quota",
		"quota exceeded",
		"exceeded your current quota",
		"credit balance is too low",
		"billing_hard_limit_reached",
		"free allocated quota exceeded",
	} {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}

// --- usage ---------------------------------------------------------

// Unit is what a provider charges by. Not every provider bills in
// tokens: image and video generators bill per image, per megapixel,
// per second of output, per second of GPU, or in plan credits.
// Multiplying a zero token count by a per-token price yields zero, and
// a cost report claiming a video generation was free is worse than one
// that omits it — it is confidently wrong and nothing invites doubt.
type Unit string

const (
	UnitTokens       Unit = "tokens"
	UnitImages       Unit = "images"
	UnitMegapixels   Unit = "megapixels"
	UnitVideoSeconds Unit = "video_seconds"
	UnitGPUSeconds   Unit = "gpu_seconds"
	UnitCredits      Unit = "credits"
	// UnitAudioCharacters is billed per character of INPUT text, the
	// usual shape for text-to-speech — not per second of output,
	// which is not known until the audio exists.
	UnitAudioCharacters Unit = "audio_characters"
)

// Billing distinguishes drawing down a prepaid plan from spending
// against a balance.
type Billing string

const (
	// BillingBalance is pay-as-you-go: CostUSD is meaningful.
	BillingBalance Billing = "balance"
	// BillingPlan draws on a quota. Marginal CostUSD is genuinely
	// zero, so the meaningful number is Quantity against the plan —
	// and reporting £0 answers a question nobody asked.
	BillingPlan Billing = "plan"
)

// ModalUsage is what one call consumed. Named to avoid colliding with
// the existing token-only Usage, which it will eventually absorb.
type ModalUsage struct {
	Unit     Unit
	Quantity float64

	// Tokens is the detailed breakdown, non-nil only when Unit is
	// UnitTokens. Kept separate because prompt/completion/cached is
	// meaningless for a per-second-of-video charge.
	Tokens *Usage

	BilledTo Billing
	CostUSD  float64
}

// TokenUsage builds a ModalUsage for a token-billed call.
func TokenUsage(u Usage, costUSD float64) ModalUsage {
	return ModalUsage{
		Unit:     UnitTokens,
		Quantity: float64(u.TotalTokens),
		Tokens:   &u,
		BilledTo: BillingBalance,
		CostUSD:  costUSD,
	}
}

// --- credentials ----------------------------------------------------

// Credential attaches authentication to an outbound request.
//
// A request mutator rather than a string, because a string cannot
// express what real providers need. Vertex AI rejects API keys
// outright and requires a short-lived OAuth token refreshed roughly
// hourly; Bedrock signs each request with SigV4. Both hide behind this
// one method, and a driver never asks which kind it holds.
type Credential interface {
	Apply(ctx context.Context, req *http.Request) error
}

// QueryCredential puts a secret in a URL query parameter.
//
// Google's generateContent endpoints authenticate this way. Expressed
// as a Credential like every other provider's, so the one vendor whose
// auth is not a header does not have it living somewhere no other
// provider's does — an endpoint string, say.
//
// A credential that edits the URL is still a credential. Keeping it
// behind the same interface is what stops the next such provider
// growing another special case.
type QueryCredential struct {
	Param string
	Value string
}

// NewQueryCredential returns a credential that sets param=value on the
// request URL.
func NewQueryCredential(param, value string) *QueryCredential {
	return &QueryCredential{Param: param, Value: value}
}

// Apply sets the parameter, replacing any existing value.
func (c *QueryCredential) Apply(_ context.Context, req *http.Request) error {
	if c == nil || c.Param == "" || c.Value == "" {
		return nil
	}
	q := req.URL.Query()
	q.Set(c.Param, c.Value)
	req.URL.RawQuery = q.Encode()
	return nil
}

// StaticCredential sets a header from a resolved secret. Covers every
// provider lobslaw talks to today.
type StaticCredential struct {
	// Header defaults to "Authorization".
	Header string
	// Prefix defaults to "Bearer " for the Authorization header and is
	// empty otherwise — Anthropic uses a bare x-api-key.
	Prefix string
	Value  string
}

// NewBearerCredential is the common case.
func NewBearerCredential(token string) *StaticCredential {
	return &StaticCredential{Header: "Authorization", Prefix: "Bearer ", Value: token}
}

// NewHeaderCredential sets a bare header value, as Anthropic's
// x-api-key expects.
func NewHeaderCredential(header, value string) *StaticCredential {
	return &StaticCredential{Header: header, Value: value}
}

func (c *StaticCredential) Apply(_ context.Context, req *http.Request) error {
	if c == nil || c.Value == "" {
		return fmt.Errorf("credential: no value configured")
	}
	h := c.Header
	if h == "" {
		h = "Authorization"
	}
	req.Header.Set(h, c.Prefix+c.Value)
	return nil
}
