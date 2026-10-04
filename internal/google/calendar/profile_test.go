package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestCalendarSettingsNeedExactApprovalAndRouteByNickname(t *testing.T) {
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider request"); return nil, nil })
	s.cfg.Settings = newTestSettings()
	list, err := s.Inventory(ctx)
	if err != nil || len(list.Calendars) != 1 {
		t.Fatalf("inventory %+v %v", list, err)
	}
	ref := list.Calendars[0].ID
	renamed, err := s.PrepareSettings(ctx, SettingsChange{Operation: "rename", Calendar: ref, Value: "Personal"})
	if err != nil {
		t.Fatal(err)
	}
	if renamed.Confirm {
		t.Fatal("rename should be reversible without confirmation")
	}
	result, err := s.ApplySettings(ctx, renamed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if result.Undo == "" {
		t.Fatal("missing undo")
	}
	p, err := s.PrepareSettings(ctx, SettingsChange{Operation: "default", Calendar: "Personal", Value: "personal"})
	if err != nil {
		t.Fatal(err)
	}
	if !p.Confirm {
		t.Fatal("default changed without review")
	}
	if _, err = s.ApplySettings(ctx, p.ID); err == nil {
		t.Fatal("unapproved default")
	}
	if err = s.ApproveSettings(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	target, err := s.Resolve(ctx, "", "", "personal", true)
	if err != nil || target.Calendar != "cal" {
		t.Fatalf("default target %+v %v", target, err)
	}
	if _, err = s.Resolve(ctx, "", "", "work", true); err == nil {
		t.Fatal("unknown purpose guessed a target")
	}
	other := turn.WithIdentity(context.Background(), turn.Identity{Principal: "bob", TurnID: "other"})
	if _, err = s.ApplySettings(other, p.ID); err == nil {
		t.Fatal("cross-owner approval")
	}
}

type testSettings struct{ state *testState }

func newTestSettings() *testSettings {
	return &testSettings{state: &testState{records: map[string]*lobslawv1.IntegrationStateRecord{}}}
}
func (s *testSettings) Get(ctx context.Context) (*lobslawv1.IntegrationStateRecord, error) {
	owner, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	r, err := s.state.Get(ctx, owner)
	if err != nil {
		return &lobslawv1.IntegrationStateRecord{Id: owner, Owner: owner}, nil
	}
	return r, nil
}
func (s *testSettings) Save(ctx context.Context, r *lobslawv1.IntegrationStateRecord) error {
	return s.state.Save(ctx, r)
}

func TestCalendarSettingsUndoAndStaleConfirmation(t *testing.T) {
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider request"); return nil, nil })
	s.cfg.Settings = newTestSettings()
	in, _ := s.Inventory(ctx)
	ref := in.Calendars[0].ID
	pending, err := s.PrepareSettings(ctx, SettingsChange{Operation: "default", Calendar: ref, Value: "general"})
	if err != nil {
		t.Fatal(err)
	}
	rename, err := s.PrepareSettings(ctx, SettingsChange{Operation: "rename", Calendar: ref, Value: "Home"})
	if err != nil {
		t.Fatal(err)
	}
	result, err := s.ApplySettings(ctx, rename.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveSettings(ctx, pending.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, pending.ID); err == nil {
		t.Fatal("stale confirmation applied")
	}
	undo, err := s.PrepareSettings(ctx, SettingsChange{Operation: "undo", Value: result.Undo})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, undo.ID); err != nil {
		t.Fatal(err)
	}
	in, _ = s.Inventory(ctx)
	if in.Calendars[0].Nickname == "Home" {
		t.Fatal("undo did not restore nickname")
	}
}
func TestCalendarAccessAndEventApprovalRemainIndependent(t *testing.T) {
	writes := 0
	s, ctx := fixture(t, true, func(r *http.Request) (*http.Response, error) {
		if r.Method != "GET" {
			writes++
		}
		return response(`{"items":[]}`), nil
	})
	s.cfg.Settings = newTestSettings()
	m := Mutation{Connection: "connection", Calendar: "cal", Title: "Test", Start: EventTime{DateTime: "2026-10-03T10:00:00Z"}, End: EventTime{DateTime: "2026-10-03T11:00:00Z"}}
	event, err := s.Prepare(ctx, "create", m)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, event.ID); err != nil {
		t.Fatal(err)
	}
	in, _ := s.Inventory(ctx)
	change, err := s.PrepareSettings(ctx, SettingsChange{Operation: "access", Calendar: in.Calendars[0].ID, Permission: &Permission{Read: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, change.ID); err == nil {
		t.Fatal("access changed without human confirmation")
	}
	if err = s.ApproveSettings(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, change.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, event.ID); err == nil {
		t.Fatal("revoked event write executed")
	}
	if writes != 0 {
		t.Fatal("unexpected write")
	}
	if _, err = s.Events(ctx, Query{Connection: "connection", Calendar: "cal", Start: "2026-10-03T00:00:00Z", End: "2026-10-04T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
}
func TestCalendarAgendaReportsPartialFailuresAndNicknameCollisions(t *testing.T) {
	s, ctx := fixture(t, true, func(r *http.Request) (*http.Response, error) {
		if strings.Contains(r.URL.Path, "other") {
			return nil, errors.New("offline")
		}
		return response(`{"items":[{"id":"a","summary":"Private appointment"}]}`), nil
	})
	s.cfg.Settings = newTestSettings()
	v := s.cfg.Credentials.(*testVault)
	var metadata Connection
	if err := json.Unmarshal(v.p.ConnectorData, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.Calendars["other"] = Permission{Read: true}
	v.p.ConnectorData, _ = json.Marshal(metadata)
	in, _ := s.Inventory(ctx)
	one, err := s.PrepareSettings(ctx, SettingsChange{Operation: "rename", Calendar: in.Calendars[0].ID, Value: "Home"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareSettings(ctx, SettingsChange{Operation: "rename", Calendar: in.Calendars[1].ID, Value: " home "}); err == nil {
		t.Fatal("duplicate nickname")
	}
	result, err := s.Agenda(ctx, "2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z", "agenda")
	if err != nil {
		t.Fatal(err)
	}
	if result.Complete || len(result.Calendars) != 2 {
		t.Fatalf("partial result %+v", result)
	}
	failed, events := 0, 0
	for _, c := range result.Calendars {
		if c.Error != "" {
			failed++
		}
		events += len(c.Events)
	}
	if failed != 1 || events != 1 {
		t.Fatalf("lost sources %+v", result)
	}
}

type multipleVault struct {
	*testVault
	rows []*memory.PlaintextCredential
}

func (v *multipleVault) List(ctx context.Context) ([]*memory.PlaintextCredential, error) {
	owner, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	var out []*memory.PlaintextCredential
	for _, p := range v.rows {
		if p.Owner == owner {
			out = append(out, p)
		}
	}
	return out, nil
}
func (v *multipleVault) Get(ctx context.Context, provider, id string) (*memory.PlaintextCredential, error) {
	rows, err := v.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, p := range rows {
		if p.Provider == provider && p.Subject == id {
			return p, nil
		}
	}
	return nil, errors.New("missing")
}
func (v *multipleVault) Issue(ctx context.Context, provider, id string, _ memory.TokenRefresher) (*memory.SkillIssue, error) {
	p, err := v.Get(ctx, provider, id)
	if err != nil {
		return nil, err
	}
	return &memory.SkillIssue{AccessToken: p.AccessToken, Scopes: p.Scopes, ExpiresAt: p.ExpiresAt}, nil
}
func TestCalendarSeveralAccountsResolveByPurposeWithoutCrossOwnerAccess(t *testing.T) {
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) { return response(`{"items":[]}`), nil })
	s.cfg.Settings = newTestSettings()
	original := s.cfg.Credentials.(*testVault)
	second := *original.p
	second.Subject = "second-account"
	second.ConnectorData = []byte(`{"email":"family@example.test","active":true,"calendars":{"shared":{"read":true,"write":true}}}`)
	bob := second
	bob.Owner = "bob"
	bob.Subject = "bob-account"
	s.cfg.Credentials = &multipleVault{testVault: original, rows: []*memory.PlaintextCredential{original.p, &second, &bob}}
	in, err := s.Inventory(ctx)
	if err != nil || len(in.Calendars) != 2 {
		t.Fatalf("owned inventory %+v %v", in, err)
	}
	ref := calendarRef(second.Subject, "shared")
	rename, err := s.PrepareSettings(ctx, SettingsChange{Operation: "rename", Calendar: ref, Value: "Family"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, rename.ID); err != nil {
		t.Fatal(err)
	}
	def, err := s.PrepareSettings(ctx, SettingsChange{Operation: "default", Calendar: "Family", Value: "school"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ApproveSettings(ctx, def.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ApplySettings(ctx, def.ID); err != nil {
		t.Fatal(err)
	}
	target, err := s.Resolve(ctx, "", "", "school", true)
	if err != nil || target.Connection != second.Subject || target.Calendar != "shared" {
		t.Fatalf("wrong default %+v %v", target, err)
	}
	if _, err = s.Resolve(ctx, bob.Subject, "shared", "", false); err == nil {
		t.Fatal("cross-user account selected")
	}
	result, err := s.Agenda(ctx, "2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z", "agenda")
	if err != nil || !result.Complete || len(result.Calendars) != 2 {
		t.Fatalf("combined accounts %+v %v", result, err)
	}
}

func TestAvailabilityOmitsTransparentCancelledAndDeclinedEvents(t *testing.T) {
	s, ctx := fixture(t, false, func(*http.Request) (*http.Response, error) {
		return response(`{"items":[{"id":"busy"},{"id":"free","transparency":"transparent"},{"id":"gone","status":"cancelled"},{"id":"declined","attendees":[{"self":true,"responseStatus":"declined"}]}]}`), nil
	})
	out, err := s.Agenda(ctx, "2026-10-03T00:00:00Z", "2026-10-04T00:00:00Z", "availability")
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Calendars) != 1 || len(out.Calendars[0].Events) != 1 || out.Calendars[0].Events[0].ID != "busy" {
		t.Fatalf("wrong busy events %+v", out)
	}
}
