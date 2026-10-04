package gateway

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func stubConsole() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><div id="root"></div>`))
	})
}

func startRESTWithConsole(t *testing.T, h http.Handler, extra func(*RESTConfig)) *Server {
	t.Helper()
	cfg := RESTConfig{
		Addr:         "127.0.0.1:0",
		JWTValidator: webAuthValidator(t),
		RequireAuth:  true,
		Users:        enrolledAlice(),
		DefaultScope: "public",
	}
	if extra != nil {
		extra(&cfg)
	}
	srv := NewServer(cfg, &captureRunner{})
	srv.RegisterConsole(h)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = srv.Start(ctx)
	})
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		wg.Wait()
		t.Fatal("server didn't bind")
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return srv
}

func TestConsoleServesTheShellOnRoot(t *testing.T) {
	t.Parallel()
	srv := startRESTWithConsole(t, stubConsole(), nil)
	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `id="root"`) {
		t.Errorf("body %q is not the app shell", body)
	}
}

func TestConsoleNotMountedWhenUnregistered(t *testing.T) {
	t.Parallel()
	srv := startWebREST(t, &captureRunner{}, nil)
	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET / without console = %d, want 404", resp.StatusCode)
	}
}

func TestConsoleDoesNotSwallowAPIRoutes(t *testing.T) {
	t.Parallel()
	srv := startRESTWithConsole(t, stubConsole(), nil)
	token := mintJWTWith(t, "alice@idp", nil)
	resp := doJSON(t, http.MethodGet, "http://"+srv.Addr()+"/v1/capabilities", "", http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/capabilities = %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "json") {
		t.Errorf("Content-Type = %q, want JSON", ct)
	}
}

func TestCapabilitiesReportsUIWebWhenConsoleMounted(t *testing.T) {
	t.Parallel()
	srv := startRESTWithConsole(t, stubConsole(), nil)
	token := mintJWTWith(t, "alice@idp", nil)
	resp := doJSON(t, http.MethodGet, "http://"+srv.Addr()+"/v1/capabilities", "", http.Header{
		"Authorization": []string{"Bearer " + token},
	})
	var body capabilitiesResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.UIWeb.Enabled || !body.UIWeb.Available {
		t.Errorf("ui-web flags = %+v, want enabled and available", body.UIWeb)
	}
	if body.ComputeTeams.Enabled {
		t.Error("compute-teams must stay disabled in this story")
	}
}

func TestCheckConsoleBind(t *testing.T) {
	t.Parallel()
	cases := []struct {
		addr        string
		requireAuth bool
		wantErr     bool
	}{
		{addr: ":8443", requireAuth: false, wantErr: true},
		{addr: ":8443", requireAuth: true, wantErr: false},
		{addr: "0.0.0.0:8443", requireAuth: false, wantErr: true},
		{addr: "127.0.0.1:8443", requireAuth: false, wantErr: false},
		{addr: "localhost:8443", requireAuth: false, wantErr: false},
		{addr: "[::1]:8443", requireAuth: false, wantErr: false},
		{addr: "", requireAuth: false, wantErr: true},
	}
	for _, tc := range cases {
		err := checkConsoleBind(tc.addr, tc.requireAuth)
		if tc.wantErr && err == nil {
			t.Errorf("checkConsoleBind(%q, %v) = nil, want error", tc.addr, tc.requireAuth)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("checkConsoleBind(%q, %v) = %v, want nil", tc.addr, tc.requireAuth, err)
		}
	}
}

func TestConsoleRefusesNonLoopbackWithoutAuth(t *testing.T) {
	t.Parallel()
	srv := NewServer(RESTConfig{Addr: "0.0.0.0:0", RequireAuth: false}, &captureRunner{})
	srv.RegisterConsole(stubConsole())
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := srv.Start(ctx)
	if err == nil {
		t.Fatal("ui-web on a non-loopback bind without require_auth must refuse to start")
	}
}

func TestConsoleAllowsLoopbackWithoutAuth(t *testing.T) {
	t.Parallel()
	srv := startRESTWithConsole(t, stubConsole(), func(cfg *RESTConfig) {
		cfg.RequireAuth = false
		cfg.JWTValidator = nil
	})
	resp, err := http.Get("http://" + srv.Addr() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("loopback console = %d", resp.StatusCode)
	}
}
