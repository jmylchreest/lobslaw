package compute

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

// A failover chain trying every provider on every turn is right the
// first time and wasteful the hundredth. These are about the second
// case, and about not over-correcting into a chain that has written
// off everything and replies to nobody.

func atFixed(h *ProviderHealth, t time.Time) { h.now = func() time.Time { return t } }

func TestUntriedProviderIsHealthy(t *testing.T) {
	t.Parallel()
	h := NewProviderHealth()
	if !h.Available("never-seen") {
		t.Error("a provider nobody has failed against was reported unhealthy")
	}
	// A nil tracker must not be pessimistic either — a caller with none
	// wired behaves exactly as before health tracking existed.
	var nilHealth *ProviderHealth
	if !nilHealth.Available("anything") {
		t.Error("a nil tracker reported a provider unhealthy")
	}
	nilHealth.RecordFailure("anything", FailureTransient) // must not panic
	nilHealth.RecordSuccess("anything")
}

func TestCredentialFailureGetsALongCooldown(t *testing.T) {
	t.Parallel()
	h := NewProviderHealth()
	start := time.Unix(1_000_000, 0)
	atFixed(h, start)

	h.RecordFailure("openai", FailureCredential)
	if h.Available("openai") {
		t.Fatal("a provider that rejected the credential is still first in the chain")
	}

	// Nothing about a wrong key improves in thirty seconds.
	atFixed(h, start.Add(time.Minute))
	if h.Available("openai") {
		t.Error("credential cooldown expired after a minute; the key has not changed")
	}
	atFixed(h, start.Add(cooldownCredential+time.Second))
	if !h.Available("openai") {
		t.Error("credential cooldown never expires; a fixed key would never be retried")
	}
}

// Transient failures back off, so a dead provider is not retried every
// thirty seconds forever — but never past the cap, or a chain that has
// written everything off replies to nobody.
func TestTransientFailuresBackOffButStayBounded(t *testing.T) {
	t.Parallel()
	h := NewProviderHealth()
	start := time.Unix(1_000_000, 0)
	atFixed(h, start)

	h.RecordFailure("flaky", FailureTransient)
	first := h.CooldownRemaining("flaky")
	if first != cooldownTransient {
		t.Errorf("first cooldown = %v, want %v", first, cooldownTransient)
	}

	h.RecordFailure("flaky", FailureTransient)
	if second := h.CooldownRemaining("flaky"); second <= first {
		t.Errorf("second cooldown %v did not grow past the first %v", second, first)
	}

	for range 20 {
		h.RecordFailure("flaky", FailureTransient)
	}
	if got := h.CooldownRemaining("flaky"); got > maxCooldown {
		t.Errorf("cooldown = %v, want no more than %v — a dead provider must still be retried sometimes",
			got, maxCooldown)
	}
}

// A 400 is a property of the request, not of the provider. Demoting on
// one would let a single malformed turn take a healthy provider out of
// the chain for everybody else.
func TestPermanentFailureDoesNotDemote(t *testing.T) {
	t.Parallel()
	h := NewProviderHealth()
	h.RecordFailure("openai", FailurePermanent)
	if !h.Available("openai") {
		t.Error("a bad request demoted the provider that correctly rejected it")
	}
}

// One success is enough. Requiring several keeps a recovered provider
// out of the chain for no reason, and being wrong costs one attempt.
func TestSuccessClearsTheDemotion(t *testing.T) {
	t.Parallel()
	h := NewProviderHealth()
	start := time.Unix(1_000_000, 0)
	atFixed(h, start)

	h.RecordFailure("openai", FailureCredential)
	h.RecordSuccess("openai")
	if !h.Available("openai") {
		t.Error("a recovered provider is still demoted")
	}

	// And the backoff counter resets with it: a provider that had a bad
	// minute last week must not start next week's first failure at a
	// five-minute cooldown.
	h.RecordFailure("openai", FailureTransient)
	if got := h.CooldownRemaining("openai"); got != cooldownTransient {
		t.Errorf("cooldown after recovery = %v, want the first-failure value %v", got, cooldownTransient)
	}
}

func TestDemotedListsWhatIsBeingSkipped(t *testing.T) {
	t.Parallel()
	h := NewProviderHealth()
	start := time.Unix(1_000_000, 0)
	atFixed(h, start)

	h.RecordFailure("openai", FailureCredential)
	h.RecordFailure("anthropic", FailureTransient)

	got := h.Demoted()
	if len(got) != 2 {
		t.Fatalf("Demoted() = %+v, want both providers", got)
	}
	if got["openai"].Class != FailureCredential {
		t.Errorf("openai class = %v, want credential — an operator needs to know which fault it was",
			got["openai"].Class)
	}

	// Past the longest cooldown, which is the credential one.
	atFixed(h, start.Add(cooldownCredential+time.Minute))
	if len(h.Demoted()) != 0 {
		t.Errorf("expired demotions are still listed: %+v", h.Demoted())
	}
}

// The classification change this all rests on. A 401 used to be
// permanent, which aborted the chain — one rotated key took the
// assistant down while two working providers sat idle.
func TestRejectedCredentialAdvancesTheChain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		class := ClassifyHTTPStatus(status, `{"error":"bad key"}`)
		if class != FailureCredential {
			t.Errorf("HTTP %d classified %s, want credential-rejected", status, class)
		}
		err := &DriverError{Class: class, Err: errors.New("auth failed")}
		if !IsRetryableProviderError(t.Context(), err) {
			t.Errorf("HTTP %d does not advance the chain; one stale key takes the assistant down", status)
		}
	}
}

// ...but a 400 still must not. Advancing on one would multiply a
// single clear error into one per provider and report the last.
func TestBadRequestStillAbortsTheChain(t *testing.T) {
	t.Parallel()
	if class := ClassifyHTTPStatus(http.StatusBadRequest, `{"error":"unknown model"}`); class != FailurePermanent {
		t.Fatalf("HTTP 400 classified %s, want permanent", class)
	}
	err := &DriverError{Class: FailurePermanent, Err: errors.New("unknown model")}
	if IsRetryableProviderError(t.Context(), err) {
		t.Error("a 400 walked the chain; every provider will reject it identically")
	}
}
