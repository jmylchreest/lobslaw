package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestLoginCodeRequiresHumanEnrollmentProof(t *testing.T) {
	t.Parallel()
	operator := []string{identity.RoleOperator}
	for _, tc := range []struct {
		name      string
		subject   string
		requested string
		turn      *turn.Identity
		want      int
	}{
		{name: "missing credential", want: http.StatusUnauthorized},
		{name: "unknown operator", subject: "mallory", want: http.StatusForbidden},
		{name: "mismatched account", subject: "alice@idp", requested: "bob", want: http.StatusForbidden},
		{name: "bot subject", subject: "bot:alice", want: http.StatusForbidden},
		{name: "specialist with inherited operator", subject: "alice@idp", turn: &turn.Identity{UserID: "alice", Principal: identity.Bot("worker"), BotOwner: identity.User("alice"), BotID: "worker", Specialist: true, Roles: operator, OriginalClaims: &types.Claims{UserID: "alice", Roles: operator}}, want: http.StatusForbidden},
		{name: "coordinator with inherited operator", subject: "alice@idp", turn: &turn.Identity{UserID: "alice", Principal: identity.User("alice"), BotID: "chief", Roles: operator}, want: http.StatusForbidden},
		{name: "queued human authority", subject: "alice@idp", turn: &turn.Identity{UserID: "alice", Principal: identity.User("alice"), Roles: operator}, want: http.StatusForbidden},
		{name: "turn without identity", subject: "alice@idp", turn: &turn.Identity{}, want: http.StatusForbidden},
		{name: "turn without role", subject: "alice@idp", turn: &turn.Identity{UserID: "alice", Principal: identity.User("alice")}, want: http.StatusForbidden},
		{name: "direct enrolled human", subject: "alice@idp", requested: "alice", want: http.StatusOK},
		{name: "direct enrolled human without operator role", subject: "alice@idp", requested: "alice", want: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := NewServer(RESTConfig{JWTValidator: webAuthValidator(t), Users: enrolledAlice()}, &captureRunner{})
			ctx := context.Background()
			if tc.turn != nil {
				ctx = turn.WithIdentity(ctx, *tc.turn)
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/session/code", strings.NewReader(`{"user":"`+tc.requested+`"}`))
			if tc.subject != "" {
				extra := jwt.MapClaims{"roles": operator}
				if tc.name == "direct enrolled human without operator role" {
					extra = nil
				}
				req.Header.Set("Authorization", "Bearer "+mintJWTWith(t, tc.subject, extra))
			}
			out := httptest.NewRecorder()
			srv.handleSessionCode(out, req)
			if out.Code != tc.want {
				t.Fatalf("mint status = %d, want %d: %s", out.Code, tc.want, out.Body.String())
			}
			if tc.want != http.StatusOK {
				if len(srv.logins.codes) != 0 {
					t.Fatal("refused request minted a credential")
				}
				return
			}
			var result struct {
				Code   string `json:"code"`
				UserID string `json:"user_id"`
			}
			if err := json.Unmarshal(out.Body.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			user, _, _, ok := srv.logins.consumeCode(result.Code)
			if !ok || user != "alice" || result.UserID != user {
				t.Fatalf("code was not bound to enrolled human: %+v", result)
			}
		})
	}
}
