package tools

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// stubHandler records that it ran and returns a fixed outcome.
func stubHandler(label string, err error, calls *[]string) compute.BuiltinFunc {
	return func(context.Context, map[string]string) ([]byte, int, error) {
		*calls = append(*calls, label)
		if err != nil {
			return nil, 1, err
		}
		return []byte("ok from " + label), 0, nil
	}
}

// Modalities had no failover at all: the resolver picked the
// highest-priority capable provider and discarded the rest, so a
// vision provider having a bad minute meant the agent could not see,
// with three other capable providers configured and idle.
//
// The chain must make the same decision per class that chat does —
// one policy, not one per modality.
func TestModalityFailoverPerClass(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		err     error
		advance bool
		why     string
	}{
		{"transient", compute.Transient(errors.New("upstream down")), true,
			"another capable provider is configured and idle"},
		{"quota exhausted", compute.QuotaExhausted(errors.New("out of credit")), true,
			"this provider is spent; the next has its own budget"},
		{"permanent", compute.Permanent(errors.New("model does not accept images")), false,
			"it fails identically on the backup"},
		{"unclassified", errors.New("read_image: path outside allowed root"), false,
			"an argument error is wrong everywhere; retrying it against every provider is noise"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := &[]string{}
			h := compute.FailoverBuiltin("read_image", quietLog(), nil, nil,
				compute.FailoverHandler{Label: "primary", Fn: stubHandler("primary", tc.err, calls)},
				compute.FailoverHandler{Label: "backup", Fn: stubHandler("backup", nil, calls)},
			)

			out, _, err := h(context.Background(), map[string]string{"path": "/x"})

			walked := len(*calls) > 1
			if walked != tc.advance {
				t.Errorf("chain walked = %v, want %v — %s\n  calls: %v",
					walked, tc.advance, tc.why, *calls)
			}
			if tc.advance {
				if err != nil {
					t.Errorf("backup should have served it, got: %v", err)
				} else if string(out) != "ok from backup" {
					t.Errorf("output %q did not come from the backup", out)
				}
			} else if err == nil {
				t.Error("a non-retryable failure produced success")
			}
		})
	}
}

// A lone provider must surface its own error verbatim. Dressing it up
// as "all 1 providers in the chain failed" would send an operator
// looking for a chain they never configured.
func TestSingleProviderReportsItsOwnError(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	h := compute.FailoverBuiltin("read_pdf", quietLog(), nil, nil,
		compute.FailoverHandler{Label: "only", Fn: stubHandler("only", compute.Transient(errors.New("boom")), calls)})

	_, _, err := h(context.Background(), nil)
	if err == nil {
		t.Fatal("expected the single provider's error")
	}
	if strings.Contains(err.Error(), "in the chain failed") {
		t.Errorf("a lone provider reported a chain failure: %v", err)
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("the provider's own error was lost: %v", err)
	}
}

// When every provider fails retryably the caller gets the last error
// plus the fact that the whole chain was exhausted — an operator
// reading "all 3 providers failed" knows this is an outage, not one
// endpoint having a bad minute.
func TestChainExhaustedReportsAll(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	h := compute.FailoverBuiltin("read_audio", quietLog(), nil, nil,
		compute.FailoverHandler{Label: "a", Fn: stubHandler("a", compute.Transient(errors.New("first")), calls)},
		compute.FailoverHandler{Label: "b", Fn: stubHandler("b", compute.Transient(errors.New("second")), calls)},
		compute.FailoverHandler{Label: "c", Fn: stubHandler("c", compute.Transient(errors.New("third")), calls)},
	)
	_, _, err := h(context.Background(), nil)
	if err == nil {
		t.Fatal("an exhausted chain reported success")
	}
	if len(*calls) != 3 {
		t.Errorf("tried %v, want all three", *calls)
	}
	if !strings.Contains(err.Error(), "all 3 providers") || !strings.Contains(err.Error(), "third") {
		t.Errorf("error should name the chain length and the last failure, got: %v", err)
	}
}

// A cancelled turn must not spend the backup's quota — the user gave
// up or the hard timeout fired.
func TestModalityFailoverStopsOnCancellation(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	h := compute.FailoverBuiltin("read_image", quietLog(), nil, nil,
		compute.FailoverHandler{Label: "primary", Fn: stubHandler("primary", compute.Transient(errors.New("boom")), calls)},
		compute.FailoverHandler{Label: "backup", Fn: stubHandler("backup", nil, calls)},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := h(ctx, nil); err == nil {
		t.Fatal("a cancelled turn succeeded")
	}
	if len(*calls) > 1 {
		t.Errorf("walked the chain on a cancelled turn: %v", *calls)
	}
}

// The classification has to survive the real HTTP path, not just
// hand-constructed errors: a 503 from a vision endpoint must reach the
// chain as transient. Before this change every modality error was an
// unclassified fmt.Errorf, so the chain would have stopped dead on the
// first provider no matter what the status said.
func TestVisionClassifiesHTTPFailures(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   compute.FailureClass
	}{
		{"server error", 503, `{"error":"upstream"}`, compute.FailureTransient},
		{"quota", 402, `{"error":"credit balance is too low"}`, compute.FailureQuotaExhausted},
		{"bad request", 400, `{"error":"model cannot see"}`, compute.FailurePermanent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			t.Cleanup(srv.Close)

			dir := t.TempDir()
			img := filepath.Join(dir, "x.png")
			// Content is irrelevant: the fake endpoint fails before the
			// bytes matter. It only has to exist and sit inside the root.
			if err := os.WriteFile(img, []byte("not-really-a-png"), 0o600); err != nil {
				t.Fatal(err)
			}

			b := NewBuiltins()
			if err := RegisterVisionBuiltin(b, VisionConfig{
				Endpoint: srv.URL, Model: "m", APIKey: "k", AllowedRoot: dir,
				Driver: openAIVisionFor(t, srv.URL),
			}); err != nil {
				t.Fatal(err)
			}
			h, ok := b.Get("read_image")
			if !ok {
				t.Fatal("read_image not registered")
			}
			_, _, err := h(context.Background(), map[string]string{"path": img})
			if err == nil {
				t.Fatalf("HTTP %d produced no error", tc.status)
			}
			if got := compute.ClassifyFailure(err); got != tc.want {
				t.Errorf("HTTP %d classified %s, want %s: %v", tc.status, got, tc.want, err)
			}
		})
	}
}

// A SILENT failover hides degradation. "The chain worked" and "the
// chain worked on its third try because two providers are down" look
// identical to an operator unless the fallthrough says so — which is
// why R22 asks for the log line and not only the behaviour.
func TestASuccessfulFallthroughIsLogged(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	calls := &[]string{}
	h := compute.FailoverBuiltin("read_image", log, nil, nil,
		compute.FailoverHandler{Label: "primary", Fn: stubHandler("primary",
			compute.Transient(errors.New("HTTP 503")), calls)},
		compute.FailoverHandler{Label: "backup", Fn: stubHandler("backup", nil, calls)},
	)
	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatalf("the chain should have succeeded on the backup: %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("calls = %v; the chain did not advance", *calls)
	}

	out := logs.String()
	if !strings.Contains(out, "backup") {
		t.Errorf("nothing was logged about the fallthrough:\n%s", out)
	}
	// The label matters more than the message: an operator needs to
	// know WHICH provider is failing, not merely that one did.
	if !strings.Contains(out, "primary") {
		t.Errorf("the log does not name the provider that failed:\n%s", out)
	}
}

// A chain that succeeds first time must not log a fallthrough, or the
// signal is worthless — every turn would carry one.
func TestNoFallthroughIsLoggedWhenThePrimaryWorks(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))

	calls := &[]string{}
	h := compute.FailoverBuiltin("read_image", log, nil, nil,
		compute.FailoverHandler{Label: "primary", Fn: stubHandler("primary", nil, calls)},
		compute.FailoverHandler{Label: "backup", Fn: stubHandler("backup", nil, calls)},
	)
	if _, _, err := h(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls = %v; the backup was tried unnecessarily", *calls)
	}
	if strings.Contains(logs.String(), "backup succeeded") {
		t.Errorf("a fallthrough was logged for a chain that never fell through:\n%s", logs.String())
	}
}
