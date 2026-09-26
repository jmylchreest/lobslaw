package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

type consoleReviewFake struct {
	mu        sync.Mutex
	decisions int
}

const consoleReviewRevision uint64 = 9007199254740993

func (*consoleReviewFake) Get(_ context.Context, claims *types.Claims, id string) (LearnedReview, error) {
	if claims.UserID != "alice" || id != "skill:worker-guide" {
		return LearnedReview{}, ErrLearnedReviewNotFound
	}
	return LearnedReview{ID: id, Name: "worker-guide", Revision: consoleReviewRevision, Digest: "review-digest", Body: "current", Files: map[string]string{"removed.txt": "old file"}, Active: true, Pending: &LearnedChange{Body: "proposed", Files: map[string]string{"new.txt": "new file"}, TurnID: "source-turn"}}, nil
}
func (s *consoleReviewFake) List(ctx context.Context, claims *types.Claims) ([]LearnedReview, error) {
	r, err := s.Get(ctx, claims, "skill:worker-guide")
	if err != nil {
		return nil, ErrLearnedReviewForbidden
	}
	return []LearnedReview{r}, nil
}
func (s *consoleReviewFake) Decide(ctx context.Context, claims *types.Claims, id string, revision uint64, digest string, _ bool) (string, error) {
	if _, err := s.Get(ctx, claims, id); err != nil {
		return "", err
	}
	if revision != consoleReviewRevision || digest != "review-digest" {
		return "", ErrLearnedReviewConflict
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisions++
	return "Approval recorded. Skill activation is pending on a compute node.", nil
}

func TestConsoleLearnedHTTPAndTypedRemoteReview(t *testing.T) {
	t.Parallel()
	for _, remote := range []bool{false, true} {
		name := "local"
		if remote {
			name = "remote"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			api := new(consoleReviewFake)
			backend := startWebREST(t, nil, func(c *RESTConfig) { c.Learned = api })
			front := backend
			if remote {
				client := testConsoleClient(t, backend)
				front = startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = client })
			}
			base := webBaseURL(front)
			login := doJSON(t, http.MethodPost, base+"/v1/session", "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
			cookie := loginCookie(login)
			if cookie == nil {
				t.Fatal("login missing")
			}
			read := doJSON(t, http.MethodGet, base+"/v1/learned-reviews/skill%3Aworker-guide", "", cookieHeader(cookie))
			var body struct {
				Revision string            `json:"revision"`
				Files    map[string]string `json:"files"`
				Pending  struct {
					Files  map[string]string `json:"files"`
					TurnID string            `json:"turnId"`
				} `json:"pending"`
			}
			if err := json.NewDecoder(read.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if read.StatusCode != http.StatusOK || body.Revision != "9007199254740993" || body.Files["removed.txt"] != "old file" || body.Pending.Files["new.txt"] != "new file" || body.Pending.TurnID != "source-turn" {
				t.Fatalf("incomplete generated JSON: %+v", body)
			}
			path := base + "/v1/learned-reviews/skill%3Aworker-guide/decide"
			decision := `{"revision":"9007199254740993","digest":"review-digest","approve":true}`
			response := doJSON(t, http.MethodPost, path, decision, cookieHeader(cookie))
			if response.StatusCode != http.StatusForbidden {
				t.Fatal("missing CSRF accepted")
			}
			for _, invalid := range []string{`{"revision":"9007199254740993","digest":"review-digest","approve":true,"owner":"user:bob"}`, `{"revision":"9007199254740993","digest":"review-digest"}`} {
				response = doJSON(t, http.MethodPost, path, invalid, cookieHeader(cookie, originFor(base)))
				if response.StatusCode != http.StatusBadRequest {
					t.Fatalf("unsafe decision accepted: %d", response.StatusCode)
				}
			}
			response = doJSON(t, http.MethodPost, path, strings.ReplaceAll(decision, "review-digest", "stale"), cookieHeader(cookie, originFor(base)))
			if response.StatusCode != http.StatusConflict {
				t.Fatalf("conflict = %d", response.StatusCode)
			}
			response = doJSON(t, http.MethodGet, base+"/v1/learned-reviews/skill%3Aworker-guide", "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "bob", nil)}})
			if response.StatusCode != http.StatusNotFound {
				t.Fatal("cross-owner read allowed")
			}
			response = doJSON(t, http.MethodPost, path, decision, cookieHeader(cookie, originFor(base)))
			raw, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != http.StatusOK || !strings.Contains(string(raw), "activation is pending") {
				t.Fatalf("activation receipt: %s err=%v", raw, err)
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.decisions != 1 {
				t.Fatalf("decisions = %d", api.decisions)
			}
		})
	}
}
