package compute

import (
	"context"
	"testing"
	"testing/synctest"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

func TestSkipEpisodicIngestStoresNothing(t *testing.T) {
	t.Parallel()
	for _, skip := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			ingester := newCapturingIngester()
			a := &Agent{cfg: AgentConfig{EpisodicIngester: ingester}}
			a.maybeIngestTurn(context.Background(), ProcessMessageRequest{Message: "hi", SkipEpisodicIngest: skip}, "reply", nil)
			synctest.Wait()
			select {
			case <-ingester.got:
				if skip {
					t.Fatal("a skipped turn was ingested")
				}
			default:
				if !skip {
					t.Fatal("an ordinary turn was not ingested")
				}
			}
		})
	}
}

func TestSkipEpisodicIngestSurvivesTheRemoteRunner(t *testing.T) {
	t.Parallel()
	if !requestFromProto(requestToProto(turn.Request{SkipEpisodicIngest: true})).SkipEpisodicIngest {
		t.Fatal("skip_episodic_ingest lost on the wire")
	}
	req, err := requestFromTurn(turn.Request{SkipEpisodicIngest: true})
	if err != nil || !req.SkipEpisodicIngest {
		t.Fatalf("skip lost adapting to the agent: %v", err)
	}
}
