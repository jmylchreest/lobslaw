package tools

import (
	"context"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type authorizationBots struct {
	record *lobslawv1.BotRecord
	writes int
}

type messagingProfiles map[string]*compute.BotProfile

func (p messagingProfiles) ResolveBot(_ context.Context, id string) (*compute.BotProfile, error) {
	return p[id], nil
}

func TestMessagingEdgeDoesNotGrantAnotherOwnersAuthority(t *testing.T) {
	t.Parallel()
	for _, owner := range []string{"user:alice", "user:bob", ""} {
		profiles := messagingProfiles{
			"source": {ID: "source", Owner: "user:alice", MayMessage: []string{"target"}},
			"target": {ID: "target", Owner: owner},
		}
		err := checkMayMessage(context.Background(), profiles, "source", "target")
		if (err == nil) != (owner == "user:alice") {
			t.Fatalf("recipient owner %q: %v", owner, err)
		}
	}
}

func (r *authorizationBots) Get(context.Context, string) (*lobslawv1.BotRecord, error) {
	return r.record, nil
}
func (r *authorizationBots) List(context.Context) ([]*lobslawv1.BotRecord, error) {
	return []*lobslawv1.BotRecord{r.record}, nil
}
func (r *authorizationBots) Put(_ context.Context, rec *lobslawv1.BotRecord, _ uint64) (*lobslawv1.BotRecord, error) {
	r.writes++
	return rec, nil
}

func TestBotUpdateRejectsDifferentOwner(t *testing.T) {
	t.Parallel()
	reg := &authorizationBots{record: &lobslawv1.BotRecord{Id: "worker", Owner: "user:bob", Enabled: true}}
	ctx := turn.WithIdentity(context.Background(), turn.Identity{Principal: identity.User("alice"), UserID: "alice"})
	_, _, err := newBotUpdateHandler(reg)(ctx, map[string]string{"bot_id": "worker", "enabled": "false"})
	if err == nil || reg.writes != 0 || !reg.record.Enabled {
		t.Fatal("cross-owner update was not rejected before mutation")
	}
}
