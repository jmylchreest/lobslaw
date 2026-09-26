package gateway

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestRosterReadDoesNotAdoptLegacyBots(t *testing.T) {
	t.Parallel()
	legacy := &lobslawv1.BotRecord{Id: "legacy", Enabled: true}
	chief := &lobslawv1.BotRecord{Id: "chief", Enabled: true}
	bots := &memBots{recs: map[string]*lobslawv1.BotRecord{"legacy": proto.Clone(legacy).(*lobslawv1.BotRecord), "chief": proto.Clone(chief).(*lobslawv1.BotRecord)}}
	srv := startWebREST(t, &captureRunner{}, func(c *RESTConfig) { c.Bots = bots; c.Groups = &memGroups{} })
	resp := doJSON(t, http.MethodGet, webBaseURL(srv)+"/v1/bots", "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("roster status %d", resp.StatusCode)
	}
	if !proto.Equal(legacy, bots.recs["legacy"]) || !proto.Equal(chief, bots.recs["chief"]) {
		t.Fatal("roster read assigned or edited an unowned bot")
	}
}

func TestSecondOwnersDefaultTeamDoesNotConflict(t *testing.T) {
	t.Parallel()
	groups, bots := &memGroups{}, &memBots{}
	srv := &Server{cfg: RESTConfig{Groups: groups, Bots: bots}}
	ctx := context.Background()
	alice, err := srv.ensureOwnersTeam(ctx, "user:alice")
	if err != nil {
		t.Fatal(err)
	}
	bob, err := srv.ensureOwnersTeam(ctx, "user:bob")
	if err != nil {
		t.Fatal(err)
	}
	if alice == bob || len(groups.recs) != 2 {
		t.Fatalf("teams collided: %s %s", alice, bob)
	}
	for _, owner := range []string{"user:alice", "user:bob"} {
		id, err := srv.ensureOwnersTeam(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		team, err := groups.Get(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		lead, err := bots.Get(ctx, team.CoordinatorBotId)
		if err != nil {
			t.Fatal(err)
		}
		if team.Owner != owner || lead.Owner != owner || len(lead.Id) > 63 || strings.Contains(lead.Id, ":") {
			t.Fatalf("wrong team/coordinator ownership: %v %v", team, lead)
		}
	}
}
