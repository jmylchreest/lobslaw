package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/config"
)

func TestBrowserLoginSurvivesRestartAndLogoutStaysRevoked(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth", "sessions.json")
	configure := func(c *RESTConfig) { c.LoginSessionFile = path }
	var cookie *http.Cookie
	t.Run("login", func(t *testing.T) {
		s := startWebREST(t, nil, configure)
		response := doJSON(t, http.MethodPost, webBaseURL(s)+"/v1/session", "", http.Header{"Authorization": {"Bearer " + mintJWTWith(t, "alice@idp", nil)}})
		cookie = loginCookie(response)
		if response.StatusCode != http.StatusOK || cookie == nil {
			t.Fatal("login failed")
		}
		raw, err := os.ReadFile(path)
		if err != nil || strings.Contains(string(raw), cookie.Value) {
			t.Fatalf("session not safely persisted: %v", err)
		}
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != 0o600 {
			t.Fatalf("session file is not private: %v", err)
		}
	})
	if cookie == nil {
		t.Fatal("missing login cookie")
	}
	t.Run("restart-and-logout", func(t *testing.T) {
		s := startWebREST(t, nil, configure)
		base := webBaseURL(s)
		response := doJSON(t, http.MethodGet, base+"/v1/session", "", cookieHeader(cookie))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("login lost after restart: %d", response.StatusCode)
		}
		response = doJSON(t, http.MethodDelete, base+"/v1/session", "", cookieHeader(cookie, originFor(base)))
		if response.StatusCode != http.StatusOK {
			t.Fatalf("logout failed: %d", response.StatusCode)
		}
	})
	t.Run("restart-after-logout", func(t *testing.T) {
		s := startWebREST(t, nil, configure)
		response := doJSON(t, http.MethodGet, webBaseURL(s)+"/v1/session", "", cookieHeader(cookie))
		if response.StatusCode != http.StatusUnauthorized {
			t.Fatal("revoked cookie survived restart")
		}
	})
}

func TestPersistentLoginExpiryAndEnrollment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	store := newLoginStore()
	if err := store.load(path); err != nil {
		t.Fatal(err)
	}
	for _, session := range []*loginSession{
		{ID: "expired", UserID: "alice", ExpiresAt: time.Now().Add(-time.Hour)},
		{ID: "live", UserID: "alice", Roles: []string{"old-admin"}, ExpiresAt: time.Now().Add(time.Hour)},
	} {
		if err := store.put(session); err != nil {
			t.Fatal(err)
		}
	}
	s := NewServer(RESTConfig{RequireAuth: true, Users: []config.UserConfig{{ID: "alice", Roles: []string{"reader"}}}}, nil)
	if err := s.logins.load(path); err != nil {
		t.Fatal(err)
	}
	if s.logins.get("expired") != nil {
		t.Fatal("restored an expired session")
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/session", nil)
	r.AddCookie(&http.Cookie{Name: LoginCookieName, Value: "live"})
	authn, err := s.authenticateRequest(r)
	if err != nil || len(authn.Claims.Roles) != 1 || authn.Claims.Roles[0] != "reader" {
		t.Fatalf("persisted roles overrode current enrollment: %+v %v", authn, err)
	}
	s.cfg.Users = nil
	if _, err := s.authenticateRequest(r); err == nil {
		t.Fatal("removed user kept access")
	}
}

func TestPersistentLoginWriteFailuresDoNotReportSuccess(t *testing.T) {
	store := newLoginStore()
	if err := store.put(&loginSession{ID: "existing", UserID: "alice", ExpiresAt: time.Now().Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}
	// Renaming a file over a directory fails even when tests run as root.
	store.path = t.TempDir()
	if err := store.put(&loginSession{ID: "new", UserID: "alice", ExpiresAt: time.Now().Add(time.Hour)}); err == nil {
		t.Fatal("failed login write accepted")
	}
	if store.get("new") != nil {
		t.Fatal("failed login left a live session")
	}
	if err := store.revoke("existing"); err == nil {
		t.Fatal("failed logout write accepted")
	}
	if store.get("existing") == nil {
		t.Fatal("failed logout changed state")
	}
}

func TestCorruptLoginStorePreventsStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(`{"Version":1,"Sessions":`), 0o600); err != nil {
		t.Fatal(err)
	}
	s := NewServer(RESTConfig{Addr: "127.0.0.1:0", LoginSessionFile: path}, nil)
	if err := s.Start(context.Background()); err == nil {
		t.Fatal("corrupt store silently discarded")
	}
}
