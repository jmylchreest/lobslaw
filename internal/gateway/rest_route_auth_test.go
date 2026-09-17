package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// stubGroups is present so the handler reaches its auth check rather
// than short-circuiting on a nil registry — which would make this pass
// for the wrong reason.
type stubGroups struct{}

func (stubGroups) List(context.Context) ([]*lobslawv1.GroupRecord, error) { return nil, nil }
func (stubGroups) Get(context.Context, string) (*lobslawv1.GroupRecord, error) {
	return &lobslawv1.GroupRecord{Id: "default", Name: "Default"}, nil
}
func (stubGroups) Put(_ context.Context, rec *lobslawv1.GroupRecord, _ uint64) (*lobslawv1.GroupRecord, error) {
	return rec, nil
}
func (stubGroups) Delete(context.Context, string) error { return nil }

// Every console route that reads or changes state must gate itself.
//
// There is no auth middleware on this surface — routes bind straight
// to handlers in registerRoutes, so a handler that forgets to call
// authenticate is simply open. Two did: /v1/groups could be listed,
// created and patched by anyone, and /v1/prompts could APPROVE A
// GUARDED TOOL for anyone holding a prompt id — an id that travels in
// the SSE stream, the Slack callback and the node log.
//
// A table rather than a test each, so adding a route without gating it
// fails here rather than in review.
func TestConsoleRoutesRefuseUnauthenticatedCallers(t *testing.T) {
	t.Parallel()

	key, err := DeriveConsoleKey([]byte("0123456789abcdef0123456789abcdef"))
	if err != nil {
		t.Fatalf("DeriveConsoleKey: %v", err)
	}
	// RequireAuth, because that is the deployment the question is
	// about. Without it authenticate hands back anonymous claims by
	// design — the supported single-machine loopback setup — and every
	// case below would pass for a reason that has nothing to do with
	// the routes being gated.
	s := NewServer(RESTConfig{
		ConsoleKey:   key,
		ConsoleToken: "shared-secret",
		RequireAuth:  true,
		Groups:       &stubGroups{},
		DefaultScope: "owner",
	}, nil)

	cases := []struct {
		name    string
		method  string
		path    string
		body    string
		handler func(http.ResponseWriter, *http.Request)
	}{
		{"list teams", http.MethodGet, "/v1/groups", "", s.handleGroups},
		{"create a team", http.MethodPost, "/v1/groups", `{"id":"x","name":"X"}`, s.handleGroups},
		{"patch a team", http.MethodPatch, "/v1/groups/default", `{"name":"Taken"}`, s.handleGroups},
		{"delete a team", http.MethodDelete, "/v1/groups/default", "", s.handleGroups},
		{"read a prompt", http.MethodGet, "/v1/prompts/p1", "", s.handlePrompt},
		// The one that matters most: approving a guarded tool.
		{"approve a prompt", http.MethodPost, "/v1/prompts/p1/resolve", `{"approve":true}`, s.handlePrompt},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			tc.handler(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("%s %s = %d, want 401 — this route is open",
					tc.method, tc.path, rec.Code)
			}
		})
	}
}
