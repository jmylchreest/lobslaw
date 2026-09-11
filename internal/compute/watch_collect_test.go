package compute

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// A watch check runs forever on its own cadence and its reply carries
// nothing a person would want recalled. Ingesting it writes a memory
// per probe, and those memories are then recalled into the NEXT
// probe's prompt — a loop observed on a running node within two
// minutes of creating a 10-second watch.
func TestSkipEpisodicIngestStopsTheTurnBeingRemembered(t *testing.T) {
	t.Parallel()
	ingester := newCapturingIngester()
	a := &Agent{cfg: AgentConfig{
		EpisodicIngester: ingester,
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
	}}

	// An ordinary turn is still remembered.
	a.maybeIngestTurn(context.Background(), ProcessMessageRequest{
		Message: "a real conversation", TurnID: "t1",
	}, "a reply", nil)
	select {
	case got := <-ingester.got:
		if got.TurnID != "t1" {
			t.Fatalf("ingested the wrong turn: %q", got.TurnID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("an ordinary turn was not remembered")
	}

	// A watch probe is not.
	a.maybeIngestTurn(context.Background(), ProcessMessageRequest{
		Message: "a watch probe", TurnID: "t2", SkipEpisodicIngest: true,
	}, "a reply", nil)
	select {
	case got := <-ingester.got:
		t.Errorf("a turn marked SkipEpisodicIngest was remembered anyway: %q", got.TurnID)
	case <-time.After(300 * time.Millisecond):
		// Nothing arrived, which is the point.
	}
}
