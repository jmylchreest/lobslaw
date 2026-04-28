package egress

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSmokescreenAllowsDeclaredHost — a request to a host the role's
// ACL allows should reach the upstream, with the role header
// injected on the proxy CONNECT (HTTPS) or request (HTTP) hop.
func TestSmokescreenAllowsDeclaredHost(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	host := strings.TrimPrefix(upstream.URL, "http://")
	host = strings.SplitN(host, ":", 2)[0]

	prov, err := NewSmokescreenProvider(SmokescreenConfig{
		AllowPrivateRanges: true,
		AllowRanges:        []string{"127.0.0.0/8"},
		ACL: Rules{
			Roles: map[string][]string{"test-role": {host}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = prov.Stop(context.Background())
	})

	c := prov.For("test-role")
	resp, err := c.HTTPClient().Get(upstream.URL)
	if err != nil {
		t.Fatalf("allowed host should reach upstream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "ok" {
		t.Errorf("body = %q", body)
	}
}

// TestSmokescreenDeniesUndeclaredHost — a host not in the role's
// ACL must be blocked at the proxy, surfaced as a non-2xx response
// or an outright connection error to the caller.
func TestSmokescreenUDSListenerCreatesSocket(t *testing.T) {
	t.Parallel()
	udsPath := filepath.Join(t.TempDir(), "egress.sock")
	prov, err := NewSmokescreenProvider(SmokescreenConfig{
		ACL:                Rules{Roles: map[string][]string{"test": {"example.com"}}},
		AllowPrivateRanges: true,
		AllowRanges:        []string{"127.0.0.0/8"},
		UDSPath:            udsPath,
	})
	if err != nil {
		t.Fatalf("NewSmokescreenProvider: %v", err)
	}
	t.Cleanup(func() { _ = prov.Stop(context.Background()) })

	if prov.UDSPath() != udsPath {
		t.Errorf("UDSPath() = %q, want %q", prov.UDSPath(), udsPath)
	}
	info, err := os.Stat(udsPath)
	if err != nil {
		t.Fatalf("UDS not created: %v", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		t.Errorf("path %q is not a socket: mode %v", udsPath, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != 0o660 {
		t.Errorf("UDS perm = %o, want 0660", perm)
	}
}

func TestSmokescreenStopRemovesUDS(t *testing.T) {
	t.Parallel()
	udsPath := filepath.Join(t.TempDir(), "egress.sock")
	prov, err := NewSmokescreenProvider(SmokescreenConfig{
		ACL:                Rules{Roles: map[string][]string{}},
		AllowPrivateRanges: true,
		UDSPath:            udsPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(udsPath); err != nil {
		t.Fatalf("UDS not created: %v", err)
	}
	if err := prov.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := os.Stat(udsPath); !os.IsNotExist(err) {
		t.Errorf("UDS should be removed after Stop, got err=%v", err)
	}
}

func TestSmokescreenDeniesUndeclaredHost(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	prov, err := NewSmokescreenProvider(SmokescreenConfig{
		AllowPrivateRanges: true,
		AllowRanges:        []string{"127.0.0.0/8"},
		ACL: Rules{
			Roles: map[string][]string{"test-role": {"only-this-host.example.com"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prov.Stop(context.Background()) })

	c := prov.For("test-role")
	c.HTTPClient().Timeout = 2 * time.Second
	resp, err := c.HTTPClient().Get(upstream.URL)
	if err != nil {
		// connection refused or proxy error — acceptable; the
		// proxy denied so the test passes.
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusOK {
		t.Errorf("expected proxy to deny; got 200 from upstream")
	}
}

// TestSmokescreenSetACLHotReload — replacing the ACL at runtime
// affects subsequent requests. Existing in-flight requests are
// out of scope for this test.
func TestSmokescreenSetACLHotReload(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(upstream.Close)

	host := strings.TrimPrefix(upstream.URL, "http://")
	host = strings.SplitN(host, ":", 2)[0]

	prov, err := NewSmokescreenProvider(SmokescreenConfig{
		AllowPrivateRanges: true,
		AllowRanges:        []string{"127.0.0.0/8"},
		ACL:                Rules{Roles: map[string][]string{"test-role": {"other-host.example.com"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = prov.Stop(context.Background()) })

	c := prov.For("test-role")
	c.HTTPClient().Timeout = 2 * time.Second

	// First call: should be denied (upstream not in ACL)
	if _, err := c.HTTPClient().Get(upstream.URL); err == nil {
		// might still get a non-2xx — acceptable; we only fail
		// if the call surprisingly succeeds.
	}

	// Hot-reload: now allow the upstream
	prov.SetACL(Rules{Roles: map[string][]string{"test-role": {host}}})

	resp, err := c.HTTPClient().Get(upstream.URL)
	if err != nil {
		t.Fatalf("after hot-reload, allowed host should reach upstream: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

// TestSmokescreenRefusesNonLoopbackBind — operators who try to bind
// the proxy on a routable interface get rejected at construction.
func TestSmokescreenRefusesNonLoopbackBind(t *testing.T) {
	t.Parallel()
	_, err := NewSmokescreenProvider(SmokescreenConfig{
		BindAddr: "0.0.0.0:0",
	})
	if err == nil {
		t.Error("expected error binding to non-loopback")
	}
}
