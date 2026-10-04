package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type testState struct {
	mu      sync.Mutex
	records map[string]*lobslawv1.IntegrationStateRecord
}

func (s *testState) Get(_ context.Context, id string) (*lobslawv1.IntegrationStateRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.records[id]
	if r == nil {
		return nil, errors.New("missing")
	}
	return proto.Clone(r).(*lobslawv1.IntegrationStateRecord), nil
}
func (s *testState) Save(_ context.Context, r *lobslawv1.IntegrationStateRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.records[r.Id]
	if (old == nil && r.Revision != 0) || (old != nil && old.Revision != r.Revision) {
		return memory.ErrClaimConflict
	}
	copy := proto.Clone(r).(*lobslawv1.IntegrationStateRecord)
	copy.Revision++
	s.records[r.Id] = copy
	return nil
}

type testVault struct{ p *memory.PlaintextCredential }

func (v *testVault) Get(ctx context.Context, _, id string) (*memory.PlaintextCredential, error) {
	who, _ := turn.IdentityFrom(ctx)
	if v.p == nil || id != v.p.Subject || string(who.Principal) != v.p.Owner {
		return nil, errors.New("missing")
	}
	c := *v.p
	return &c, nil
}
func (v *testVault) Put(ctx context.Context, p *memory.PlaintextCredential) error {
	who, _ := turn.IdentityFrom(ctx)
	c := *p
	c.Owner = string(who.Principal)
	v.p = &c
	return nil
}
func (v *testVault) List(ctx context.Context) ([]*memory.PlaintextCredential, error) {
	p, err := v.Get(ctx, "", "connection")
	if err != nil {
		return nil, nil
	}
	return []*memory.PlaintextCredential{p}, nil
}
func (v *testVault) Delete(ctx context.Context, p, id string) error {
	_, err := v.Get(ctx, p, id)
	if err == nil {
		v.p = nil
	}
	return err
}
func (v *testVault) Issue(ctx context.Context, p, id string, _ memory.TokenRefresher) (*memory.SkillIssue, error) {
	c, err := v.Get(ctx, p, id)
	if err != nil {
		return nil, err
	}
	return &memory.SkillIssue{AccessToken: c.AccessToken, Scopes: c.Scopes, ExpiresAt: c.ExpiresAt}, nil
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func fixture(t *testing.T, write bool, transport roundTripFunc) (*Service, context.Context) {
	t.Helper()
	data, _ := json.Marshal(Connection{Subject: "google-sub", Email: "alice@example.test", Active: true, Calendars: map[string]Permission{"cal": {Read: true, Write: write}}})
	v := &testVault{p: &memory.PlaintextCredential{Owner: "alice", Provider: "google", Subject: "connection", AccessToken: "test-access", Scopes: []string{scopeEventsWrite, scopeCalendarList}, ConnectorData: data, ExpiresAt: time.Now().Add(time.Hour)}}
	s, err := New(Config{ClientID: "client", ClientSecret: "secret", RedirectURL: "https://agent.example.test/integrations/google/callback", Client: &http.Client{Transport: transport}, Credentials: v, State: &testState{records: map[string]*lobslawv1.IntegrationStateRecord{}}, Authorize: func(context.Context, string, string) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return s, turn.WithIdentity(context.Background(), turn.Identity{Principal: "alice", TurnID: "turn-one"})
}
func response(body string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}

func TestReadGrantDoesNotAllowWrite(t *testing.T) {
	t.Parallel()
	calls := 0
	s, ctx := fixture(t, false, func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodGet {
			t.Fatal("sent write")
		}
		return response(`{"items":[]}`), nil
	})
	if _, err := s.Events(ctx, Query{Connection: "connection", Calendar: "cal", Start: "2026-10-03T00:00:00Z", End: "2026-10-04T00:00:00Z"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prepare(ctx, "create", Mutation{Connection: "connection", Calendar: "cal", Title: "Dentist", Start: EventTime{DateTime: "2026-10-03T10:00:00Z"}, End: EventTime{DateTime: "2026-10-03T11:00:00Z"}}); err == nil {
		t.Fatal("read grant permitted write")
	}
	if calls != 1 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestWriteRequiresExactApprovalAndIsConsumed(t *testing.T) {
	t.Parallel()
	writes := 0
	s, ctx := fixture(t, true, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			writes++
		}
		return response(`{"id":"created","summary":"Dentist"}`), nil
	})
	m := Mutation{Connection: "connection", Calendar: "cal", Title: "Dentist", Start: EventTime{DateTime: "2026-10-03T10:00:00Z"}, End: EventTime{DateTime: "2026-10-03T11:00:00Z"}}
	p, err := s.Prepare(ctx, "create", m)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p.ID); err == nil {
		t.Fatal("unapproved write")
	}
	if writes != 0 {
		t.Fatal("unapproved network write")
	}
	if err := s.Approve(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Apply(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatalf("duplicate writes=%d", writes)
	}
	m.Title = "Changed"
	other, err := s.Prepare(ctx, "create", m)
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == p.ID {
		t.Fatal("changed payload reused approval")
	}
	if _, err := s.Apply(ctx, other.ID); err == nil {
		t.Fatal("changed payload borrowed approval")
	}
}
