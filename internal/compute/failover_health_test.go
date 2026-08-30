package compute

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The chain remembering a failure is only worth anything if it then
// acts on it. These are about the acting.

// countingHandler records how many times it ran, so a test can assert
// a provider was skipped rather than merely that the chain succeeded.
func countingHandler(err error, n *atomic.Int32) BuiltinFunc {
	return func(context.Context, map[string]string) ([]byte, int, error) {
		n.Add(1)
		if err != nil {
			return nil, 1, err
		}
		return []byte("ok"), 0, nil
	}
}

func TestChainSkipsADemotedProvider(t *testing.T) {
	t.Parallel()
	health := NewProviderHealth()
	var primary, backup atomic.Int32

	h := FailoverBuiltin("read_image", quietLog(), health, nil,
		FailoverHandler{Label: "openai", Fn: countingHandler(
			CredentialRejected(errors.New("401 bad key")), &primary)},
		FailoverHandler{Label: "anthropic", Fn: countingHandler(nil, &backup)},
	)

	// First call discovers the bad key the expensive way.
	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatalf("first call: %v", err)
	}
	if primary.Load() != 1 || backup.Load() != 1 {
		t.Fatalf("first call: primary=%d backup=%d, want 1 and 1", primary.Load(), backup.Load())
	}

	// Every call after it must not. This is the whole point: without
	// the tracker, a key revoked this morning costs a round-trip and a
	// timeout on every turn until somebody notices.
	for range 5 {
		if _, _, err := h(context.Background(), nil); err != nil {
			t.Fatalf("subsequent call: %v", err)
		}
	}
	if primary.Load() != 1 {
		t.Errorf("primary was called %d times; a demoted provider is still being retried", primary.Load())
	}
	if backup.Load() != 6 {
		t.Errorf("backup was called %d times, want 6", backup.Load())
	}
}

// A demotion must not outlive its cooldown, or one bad minute writes a
// provider off for the life of the process.
func TestChainRetriesAfterTheCooldown(t *testing.T) {
	t.Parallel()
	health := NewProviderHealth()
	start := time.Unix(1_000_000, 0)
	health.now = func() time.Time { return start }

	var primary, backup atomic.Int32
	primaryErr := Transient(errors.New("503"))
	h := FailoverBuiltin("read_image", quietLog(), health, nil,
		FailoverHandler{Label: "openai", Fn: countingHandler(primaryErr, &primary)},
		FailoverHandler{Label: "anthropic", Fn: countingHandler(nil, &backup)},
	)

	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if primary.Load() != 1 {
		t.Fatalf("primary called %d times during cooldown, want 1", primary.Load())
	}

	health.now = func() time.Time { return start.Add(cooldownTransient * 2) }
	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if primary.Load() != 2 {
		t.Errorf("primary called %d times after the cooldown lapsed, want 2 — it is never retried",
			primary.Load())
	}
}

// A chain whose every member is in cooldown must say so, not report
// "all providers failed" with no error to show — which reads as a bug
// in the chain rather than the chain protecting itself.
func TestFullyDemotedChainSaysWhy(t *testing.T) {
	t.Parallel()
	health := NewProviderHealth()
	health.RecordFailure("openai", FailureCredential)
	health.RecordFailure("anthropic", FailureCredential)

	var a, b atomic.Int32
	h := FailoverBuiltin("read_image", quietLog(), health, nil,
		FailoverHandler{Label: "openai", Fn: countingHandler(nil, &a)},
		FailoverHandler{Label: "anthropic", Fn: countingHandler(nil, &b)},
	)

	_, _, err := h(context.Background(), nil)
	if err == nil {
		t.Fatal("a fully demoted chain reported success")
	}
	if !strings.Contains(err.Error(), "cooldown") {
		t.Errorf("error = %q; it does not tell an operator the chain demoted itself", err)
	}
	if a.Load() != 0 || b.Load() != 0 {
		t.Errorf("demoted providers were called anyway: %d, %d", a.Load(), b.Load())
	}
}

// Success clears the demotion, so a provider that recovers returns to
// its normal position rather than staying skipped until the cooldown
// happens to lapse.
func TestRecoveryRestoresTheProvider(t *testing.T) {
	t.Parallel()
	health := NewProviderHealth()
	health.RecordFailure("openai", FailureTransient)
	if health.Available("openai") {
		t.Fatal("setup: provider should start demoted")
	}
	health.RecordSuccess("openai")

	var primary atomic.Int32
	h := FailoverBuiltin("read_image", quietLog(), health, nil,
		FailoverHandler{Label: "openai", Fn: countingHandler(nil, &primary)},
		FailoverHandler{Label: "anthropic", Fn: countingHandler(nil, new(atomic.Int32))},
	)
	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if primary.Load() != 1 {
		t.Error("a recovered provider is still being skipped")
	}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
