package gateway

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestAccessLogRecordsCredentialPresenceOnly(t *testing.T) {
	t.Parallel()
	for _, bearer := range []bool{false, true} {
		for _, cookie := range []bool{false, true} {
			var logs bytes.Buffer
			srv := &Server{log: slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))}
			req := httptest.NewRequest(http.MethodGet, "/v1/config", nil)
			token := mintJWTWith(t, "alice@idp", nil)
			const cookieValue = "private-login-cookie-value"
			if bearer {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if cookie {
				req.AddCookie(&http.Cookie{Name: LoginCookieName, Value: cookieValue})
			}
			srv.withAccessLog(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })).ServeHTTP(httptest.NewRecorder(), req)
			if strings.Contains(logs.String(), token) || strings.Contains(logs.String(), cookieValue) {
				t.Fatalf("credential leaked: %s", &logs)
			}
			var entry map[string]any
			if err := json.Unmarshal(logs.Bytes(), &entry); err != nil {
				t.Fatal(err)
			}
			if entry["bearer"] != bearer || entry["cookie"] != cookie || entry["status"] != float64(http.StatusNoContent) {
				t.Fatalf("incorrect access log: %v", entry)
			}
		}
	}
}

func TestConfigDerivesLoginConfigured(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name                   string
		users, validator, want bool
	}{
		{"neither", false, false, false}, {"users", true, false, true},
		{"validator", false, true, true}, {"both", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			view := &ConfigView{Gateway: ConfigGatewayView{LoginConfigured: !tc.want}}
			cfg := RESTConfig{Config: view}
			if tc.users {
				cfg.Users = enrolledAlice()
			}
			if tc.validator {
				cfg.JWTValidator = webAuthValidator(t)
			}
			srv := NewServer(cfg, nil)
			w := httptest.NewRecorder()
			srv.handleConfig(w, httptest.NewRequest(http.MethodGet, "/v1/config", nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d: %s", w.Code, w.Body)
			}
			var got ConfigView
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if got.Gateway.LoginConfigured != tc.want {
				t.Fatalf("login_configured=%v, want %v", got.Gateway.LoginConfigured, tc.want)
			}
			if view.Gateway.LoginConfigured != !tc.want {
				t.Fatal("handler mutated source configuration")
			}
		})
	}
}

func TestGroupResponsesIncludeMembershipCounts(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, nil, func(c *RESTConfig) {
		c.Groups = &memGroups{recs: []*pb.GroupRecord{{Id: "team", Owner: "user:alice"}}}
		c.Bots = &memBots{recs: map[string]*pb.BotRecord{
			"lead":   {Id: "lead", GroupId: "team", Owner: "user:alice", IsCoordinator: true},
			"worker": {Id: "worker", GroupId: "team", Owner: "user:alice"},
			"other":  {Id: "other", GroupId: "other", Owner: "user:alice"},
			"orphan": {Id: "orphan", Owner: "user:alice"},
		}}
	})
	auth := http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}}
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodGet, "/v1/groups", ""}, {http.MethodGet, "/v1/groups/team", ""},
		{http.MethodPatch, "/v1/groups/team", `{"name":"Renamed"}`},
	} {
		response := doJSON(t, tc.method, webBaseURL(srv)+tc.path, tc.body, auth)
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s %s: %d", tc.method, tc.path, response.StatusCode)
		}
		var group groupJSON
		if tc.path == "/v1/groups" {
			var body struct {
				Groups []groupJSON `json:"groups"`
			}
			if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if len(body.Groups) != 1 {
				t.Fatalf("groups=%v", body.Groups)
			}
			group = body.Groups[0]
		} else if err := json.NewDecoder(response.Body).Decode(&group); err != nil {
			t.Fatal(err)
		}
		if group.Bots != 2 {
			t.Fatalf("%s %s: bots=%d, want 2", tc.method, tc.path, group.Bots)
		}
	}
}
