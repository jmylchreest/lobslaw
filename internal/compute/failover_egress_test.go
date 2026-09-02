package compute

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// An egress denial must walk the chain.
//
// The ACL is per-host and the backup provider is a different host
// with its own entry, so a denial is provider-specific in exactly the
// way the chain exists to route around. Classified permanent, one
// unlisted host took the entire turn down while a configured and
// allowed backup sat idle — the failure an operator reads as "the
// assistant is down" when it is "one provider is not on the list".
func TestEgressDenialFailsOverToTheNextProvider(t *testing.T) {
	t.Parallel()
	for _, msg := range []string{
		`Post "https://api.example.com/v1/messages": Request rejected by proxy`,
		`Post "https://api.example.com/v1/messages": proxy returned 407`,
	} {
		if !IsRetryableProviderError(context.Background(), errors.New(msg)) {
			t.Errorf("not retryable, so the chain stops here:\n  %s", msg)
		}
	}
}

// A driver that classifies its failure still decides structurally —
// the text scan is the fallback for providers that do not wrap their
// errors, and must not override a permanent classification.
func TestClassifiedPermanentStillWinsOverTheTextScan(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("wrapped: %w", &DriverError{
		Class: FailurePermanent,
		Err:   errors.New("Request rejected by proxy"),
	})
	if IsRetryableProviderError(context.Background(), err) {
		t.Error("a driver said permanent and the text scan overruled it")
	}
}

// A cancelled context still stops the chain: the backup's quota
// should not be spent on a turn nobody is waiting for.
func TestEgressDenialDoesNotOverrideCancellation(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if IsRetryableProviderError(ctx, errors.New("Request rejected by proxy")) {
		t.Error("failed over inside a cancelled context")
	}
}
