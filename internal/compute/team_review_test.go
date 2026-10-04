package compute

import (
	"testing"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestDelegatedReviewLearnsProcedureWithoutUserMemory(t *testing.T) {
	t.Parallel()
	f, store, provider := newFork(t, ReviewConfig{SkillToolIterations: 1, MemoryTurnInterval: 1})
	req := ProcessMessageRequest{BotID: "worker", Principal: identity.Bot("worker"), Claims: &types.Claims{UserID: "alice"}}
	axes := f.shouldReview(req, 1)
	if !axes.skills || axes.memory {
		t.Fatalf("delegated review axes = %+v", axes)
	}
	if got := ownerOf(req); got != "bot:worker" {
		t.Fatalf("learned artefact owner = %q", got)
	}
	provider.reply = decisionJSON(t, reviewDecision{Action: "new", Name: "procedure", Body: "steps", Distinct: true})
	if err := f.run(t.Context(), req, nil, axes); err != nil {
		t.Fatal(err)
	}
	if got := store.calls(); len(got) != 1 || got[0].Owner != "bot:worker" {
		t.Fatalf("delegated proposal ownership: %+v", got)
	}
}
