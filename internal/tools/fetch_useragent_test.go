package tools

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// THE HEADER THAT WENT NOWHERE.
//
// fetch_url set its User-Agent like this:
//
//	req.Header.Set("User-compute.Agent", "lobslaw-fetch/1.0")
//
// "User-compute.Agent" is not a header anyone reads. A package rename
// swept `agent.` -> `compute.` through the tree and rewrote the inside
// of a string literal, so the header NAME was corrupted while the
// value stayed perfectly plausible. Nothing failed, nothing warned,
// and Go quietly sent its default "Go-http-client/2.0" on every fetch
// for as long as that line stood.
//
// The cost was invisible until it wasn't: idealhome.co.uk refuses
// "Go-http-client/2.0" with a 403 and serves the same page happily to
// anything else, so the agent reported a site as blocked when it was
// only ever the header.
//
// No test asserted what went out on the wire. Reading the line back
// would not have helped either — it looks right at a glance, which is
// exactly why the assertion has to be against a real request.

func fetchOnce(t *testing.T, cfg FetchConfig, srv *httptest.Server) {
	t.Helper()
	b := NewBuiltins()
	cfg.HTTPClient = srv.Client()
	if err := RegisterFetchBuiltin(b, cfg); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("fetch_url")
	if !ok {
		t.Fatal("fetch_url not registered")
	}
	if _, _, err := fn(context.Background(), map[string]string{"url": srv.URL}); err != nil {
		t.Fatalf("fetch: %v", err)
	}
}

func TestFetchSendsItsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
		_, _ = w.Write([]byte("<html><body>ok</body></html>"))
	}))
	defer srv.Close()

	fetchOnce(t, FetchConfig{}, srv)

	if got != DefaultFetchUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, DefaultFetchUserAgent)
	}
	// The specific regression: Go's default reaching a server means
	// our header never landed, whatever the code appears to set.
	if strings.HasPrefix(got, "Go-http-client") {
		t.Errorf("the stdlib default went out on the wire: %q", got)
	}
	// And the corrupted name must not come back, under any spelling.
	if strings.Contains(got, "compute.") {
		t.Errorf("User-Agent carries a package-qualified fragment: %q", got)
	}
}

func TestFetchUserAgentIsConfigurable(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.UserAgent()
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()

	const custom = "acme-crawler/2.1 (+https://example.invalid/bot)"
	fetchOnce(t, FetchConfig{UserAgent: custom}, srv)

	if got != custom {
		t.Errorf("User-Agent = %q, want the configured %q", got, custom)
	}
}

// The default identifies lobslaw and carries a contact URL rather than
// impersonating a browser. Measured: the site that prompted this
// accepts both an honest string and a full Chrome one, and refuses
// only Go's default — so there is nothing to buy by pretending.
func TestDefaultUserAgentIsHonest(t *testing.T) {
	if !strings.HasPrefix(DefaultFetchUserAgent, "lobslaw") {
		t.Errorf("the default should name lobslaw first: %q", DefaultFetchUserAgent)
	}
	if !strings.Contains(DefaultFetchUserAgent, "+http") {
		t.Errorf("the default should carry a contact URL: %q", DefaultFetchUserAgent)
	}
	for _, masq := range []string{"Mozilla/", "AppleWebKit", "Chrome/", "Safari/"} {
		if strings.Contains(DefaultFetchUserAgent, masq) {
			t.Errorf("the default impersonates a browser (%q): %q", masq, DefaultFetchUserAgent)
		}
	}
}
