package gateway

import (
	"context"
	"net/http"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type stubBots struct {
	rec *lobslawv1.BotRecord
	err error
}

func (s stubBots) List(context.Context) ([]*lobslawv1.BotRecord, error) { return nil, nil }
func (s stubBots) Get(context.Context, string) (*lobslawv1.BotRecord, error) {
	return s.rec, s.err
}
func (s stubBots) Put(context.Context, *lobslawv1.BotRecord, uint64) (*lobslawv1.BotRecord, error) {
	return s.rec, s.err
}
func (s stubBots) Delete(context.Context, string) error { return s.err }

type stubGroups struct {
	rec *lobslawv1.GroupRecord
	err error
}

func (s stubGroups) List(context.Context) ([]*lobslawv1.GroupRecord, error) { return nil, nil }
func (s stubGroups) Get(context.Context, string) (*lobslawv1.GroupRecord, error) {
	return s.rec, s.err
}
func (s stubGroups) Put(context.Context, *lobslawv1.GroupRecord, uint64) (*lobslawv1.GroupRecord, error) {
	return s.rec, s.err
}
func (s stubGroups) Delete(context.Context, string) error { return s.err }

func TestMayModifyBotFailsClosedOnGroupLookupMiss(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: RESTConfig{
		Bots:   stubBots{rec: &lobslawv1.BotRecord{Id: "eng", GroupId: "missing"}},
		Groups: stubGroups{err: errGroupMissing},
	}}
	req, _ := http.NewRequest(http.MethodDelete, "/v1/bots/eng", nil)
	if srv.mayModifyBot(req, "eng") {
		t.Fatal("group lookup miss must fail closed")
	}
}

func TestMayUseGroupFailsClosedOnMissingDestination(t *testing.T) {
	t.Parallel()
	srv := &Server{cfg: RESTConfig{
		Groups: stubGroups{err: errGroupMissing},
	}}
	req, _ := http.NewRequest(http.MethodPost, "/v1/bots", nil)
	if srv.mayUseGroup(req, "someone-elses-team") {
		t.Fatal("POST into an unreadable team must fail closed")
	}
	if srv.mayUseGroup(req, "") {
		t.Fatal("empty destination must not fall through to a shared default")
	}
}

// memGroups is a stateful stub so ensureOwnersTeam can be tested for
// the thing that matters: it creates the caller's own team once, and
// then reuses it rather than creating a second.
type memGroups struct{ recs []*lobslawv1.GroupRecord }

func (m *memGroups) List(context.Context) ([]*lobslawv1.GroupRecord, error) { return m.recs, nil }
func (m *memGroups) Get(_ context.Context, id string) (*lobslawv1.GroupRecord, error) {
	for _, r := range m.recs {
		if r.GetId() == id {
			return r, nil
		}
	}
	return nil, errGroupMissing
}
func (m *memGroups) Put(_ context.Context, rec *lobslawv1.GroupRecord, _ uint64) (*lobslawv1.GroupRecord, error) {
	rec.Revision = 1
	m.recs = append(m.recs, rec)
	return rec, nil
}
func (m *memGroups) Delete(context.Context, string) error { return nil }

func TestEnsureOwnersTeamCreatesOnceAndReuses(t *testing.T) {
	t.Parallel()
	groups := &memGroups{}
	srv := &Server{cfg: RESTConfig{Groups: groups}}

	first, err := srv.ensureOwnersTeam(context.Background(), "user:chief")
	if err != nil {
		t.Fatal(err)
	}
	if first != defaultGroupID {
		t.Fatalf("team id = %q, want %q", first, defaultGroupID)
	}
	if len(groups.recs) != 1 || groups.recs[0].GetOwner() != "user:chief" || !groups.recs[0].GetIsDefault() {
		t.Fatalf("created team is not owned by the caller: %+v", groups.recs)
	}

	again, err := srv.ensureOwnersTeam(context.Background(), "user:chief")
	if err != nil {
		t.Fatal(err)
	}
	if again != first || len(groups.recs) != 1 {
		t.Fatalf("second call made another team: id=%q recs=%d", again, len(groups.recs))
	}

	if _, err := srv.ensureOwnersTeam(context.Background(), ""); err == nil {
		t.Fatal("an unauthenticated caller must not get an owned team")
	}
}

type missingGroupError struct{}

func (missingGroupError) Error() string { return "groups: not found: missing" }

var errGroupMissing = missingGroupError{}
