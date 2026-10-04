package calendar

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

func change() Mutation {
	return Mutation{Connection: "connection", Calendar: "cal", Title: "Dentist", Start: EventTime{DateTime: "2026-10-03T10:00:00Z"}, End: EventTime{DateTime: "2026-10-03T11:00:00Z"}}
}
func TestOperationPolicyBeforeNetwork(t *testing.T) {
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) {
		t.Fatal("denied request reached provider")
		return nil, nil
	})
	s.cfg.Authorize = func(_ context.Context, action, _ string) error {
		if action == "calendar:write" {
			return errors.New("write denied")
		}
		return nil
	}
	if _, err := s.Prepare(ctx, "create", change()); err == nil {
		t.Fatal("policy write denial bypassed")
	}
}
func TestUpdateRejectsGuestSeriesAndChangedVersion(t *testing.T) {
	for _, bad := range []string{`"attendees":[{"email":"guest@example.test"}]`, `"recurrence":["RRULE:FREQ=DAILY"]`, `"status":"cancelled"`, `"eventType":"outOfOffice"`} {
		t.Run(bad, func(t *testing.T) {
			s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) {
				return response(`{"id":"e","etag":"v1",` + bad + `}`), nil
			})
			m := change()
			m.Event = "e"
			if _, err := s.Prepare(ctx, "update", m); err == nil {
				t.Fatal("unsupported update prepared")
			}
		})
	}
	gets := 0
	s, ctx := fixture(t, true, func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("stale write transmitted")
		}
		gets++
		version := "v1"
		if gets > 1 {
			version = "v2"
		}
		return response(`{"id":"e","etag":"` + version + `"}`), nil
	})
	m := change()
	m.Event = "e"
	p, err := s.Prepare(ctx, "update", m)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p.ID); err == nil {
		t.Fatal("stale version accepted")
	}
}
func TestUpdateUsesIfMatchAndSuppressesInvitations(t *testing.T) {
	writes := 0
	s, ctx := fixture(t, true, func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPatch {
			writes++
			if r.Header.Get("If-Match") != "v1" || r.URL.Query().Get("sendUpdates") != "none" {
				t.Fatal("missing write constraints")
			}
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if _, ok := body["attendees"]; ok {
				t.Fatal("guest write")
			}
		}
		return response(`{"id":"e","etag":"v1"}`), nil
	})
	m := change()
	m.Event = "e"
	p, err := s.Prepare(ctx, "update", m)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Apply(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	if writes != 1 {
		t.Fatal(writes)
	}
}
func TestConcurrentAndUncertainWritesNeverReplay(t *testing.T) {
	var writes atomic.Int32
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) { writes.Add(1); return nil, errors.New("lost connection") })
	p, err := s.Prepare(ctx, "create", change())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() { _, _ = s.Apply(ctx, p.ID) })
	}
	wg.Wait()
	if writes.Load() != 1 {
		t.Fatalf("writes=%d", writes.Load())
	}
	if err = s.Approve(ctx, p.ID); err == nil {
		t.Fatal("uncertain write approved again")
	}
}
func TestRevocationAndSharedConversations(t *testing.T) {
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) { t.Fatal("revoked call reached Google"); return nil, nil })
	p, err := s.Prepare(ctx, "create", change())
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Approve(ctx, p.ID); err != nil {
		t.Fatal(err)
	}
	s.cfg.Credentials.(*testVault).p.Generation = "replacement"
	if _, err = s.Apply(ctx, p.ID); err == nil {
		t.Fatal("replacement reused approval")
	}
	ctx = turn.WithIdentity(ctx, turn.Identity{Principal: "alice", TurnID: "turn", Shared: true})
	if _, err = s.Calendars(ctx, "connection"); err == nil {
		t.Fatal("shared private data")
	}
}
func TestRecreatedPreparationCannotBorrowExpiredApproval(t *testing.T) {
	s, ctx := fixture(t, true, func(*http.Request) (*http.Response, error) { t.Fatal("unexpected network"); return nil, nil })
	p, err := s.Prepare(ctx, "create", change())
	if err != nil {
		t.Fatal(err)
	}
	state := s.cfg.State.(*testState)
	delete(state.records, p.ID)
	next, err := s.Prepare(ctx, "create", change())
	if err != nil {
		t.Fatal(err)
	}
	if p.ID != next.ID || p.ApprovalKey == next.ApprovalKey {
		t.Fatal("recreated state inherited approval")
	}
}

func TestHTTPFailuresDoNotLeakSecretsOrFollowRedirects(t *testing.T) {
	calls := 0
	s, ctx := fixture(t, false, func(r *http.Request) (*http.Response, error) {
		calls++
		resp := response("access-secret refresh-secret")
		resp.StatusCode = http.StatusFound
		resp.Header.Set("Location", "https://evil.example.test/steal")
		return resp, nil
	})
	_, err := s.Event(ctx, Query{Connection: "connection", Calendar: "cal", Event: "e"})
	if err == nil || strings.Contains(err.Error(), "secret") || calls != 1 {
		t.Fatalf("redirect/error boundary: calls=%d err=%v", calls, err)
	}
	s.client.Transport = roundTripFunc(func(*http.Request) (*http.Response, error) {
		return response(strings.Repeat("x", responseLimit+1)), nil
	})
	if _, err = s.Event(ctx, Query{Connection: "connection", Calendar: "cal", Event: "e"}); err == nil {
		t.Fatal("unbounded response")
	}
}
