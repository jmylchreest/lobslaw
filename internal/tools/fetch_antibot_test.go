package tools

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// THE SECOND ATTEMPT.
//
// Measured on the two sites that prompted this:
//
//	idealhome.co.uk   403 to Go's default UA, 200 to any other header
//	argos.co.uk       403 to every header tried, 200 through a browser
//
// Only the second needs a browser, and that distinction is the design.
// A solve is a page load and seconds of JavaScript, so it runs only
// after a direct fetch has actually been refused — never in front of
// one, and never for a status a browser cannot help with.

// antibotStub answers the solver protocol.
func antibotStub(t *testing.T, status int, body string) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req antibotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("solver got undecodable body: %v", err)
		}
		if req.Cmd != "request.get" {
			t.Errorf("solver got cmd %q", req.Cmd)
		}
		w.Header().Set("Content-Type", "application/json")
		resp := antibotResponse{Status: "ok", Message: "Challenge solved successfully"}
		resp.Solution.URL = req.URL
		resp.Solution.Status = status
		resp.Solution.Response = body
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(srv.Close)
	return srv, &calls
}

func fetchThrough(t *testing.T, cfg FetchConfig, target string) (string, int, error) {
	t.Helper()
	b := NewBuiltins()
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = http.DefaultClient
	}
	if err := RegisterFetchBuiltin(b, cfg); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("fetch_url")
	if !ok {
		t.Fatal("fetch_url not registered")
	}
	out, code, err := fn(context.Background(), map[string]string{"url": target})
	return string(out), code, err
}

func TestAntibotRescuesARefusedFetch(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("<html><body>Access denied</body></html>"))
	}))
	defer site.Close()
	solver, calls := antibotStub(t, 200, "<html><body>the actual article</body></html>")

	out, code, err := fetchThrough(t, FetchConfig{Antibot: AntibotConfig{URL: solver.URL}}, site.URL)
	if err != nil || code != 0 {
		t.Fatalf("the solved fetch should succeed: code=%d err=%v", code, err)
	}
	if !strings.Contains(out, "the actual article") {
		t.Errorf("the solved body did not reach the caller:\n%s", out)
	}
	if *calls != 1 {
		t.Errorf("solver called %d times, want exactly 1", *calls)
	}
}

// The expensive path must not run when the cheap one worked.
func TestAntibotIsNotUsedWhenTheDirectFetchSucceeds(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><body>fine</body></html>"))
	}))
	defer site.Close()
	solver, calls := antibotStub(t, 200, "<html>should not be used</html>")

	out, _, err := fetchThrough(t, FetchConfig{Antibot: AntibotConfig{URL: solver.URL}}, site.URL)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "fine") {
		t.Errorf("wrong body:\n%s", out)
	}
	if *calls != 0 {
		t.Errorf("a successful direct fetch still called the solver %d times", *calls)
	}
}

// 404 means the page is not there for anybody, and 429 means slow
// down. Retrying either through a browser is slow, futile, and in the
// second case rude.
func TestAntibotOnlyRetriesChallengeStatuses(t *testing.T) {
	for _, status := range []int{http.StatusNotFound, http.StatusTooManyRequests, http.StatusInternalServerError} {
		site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(status)
		}))
		solver, calls := antibotStub(t, 200, "<html>nope</html>")

		_, _, err := fetchThrough(t, FetchConfig{Antibot: AntibotConfig{URL: solver.URL}}, site.URL)
		if err == nil {
			t.Errorf("HTTP %d should still fail", status)
		}
		if *calls != 0 {
			t.Errorf("HTTP %d triggered a solve; only 403 and 503 should", status)
		}
		site.Close()
	}
}

// A solver that reports ok while the SITE refused has not solved
// anything. Returning its body would cache a challenge page as though
// it were the article.
func TestAntibotSolvedButSiteStillRefused(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer site.Close()
	solver, _ := antibotStub(t, 403, "<html>still the challenge</html>")

	out, code, err := fetchThrough(t, FetchConfig{Antibot: AntibotConfig{URL: solver.URL}}, site.URL)
	if err == nil || code == 0 {
		t.Fatalf("a solver reporting the site's own 403 must not succeed: out=%s", out)
	}
	// The direct status is the fact about the site; the retry is
	// context. Reporting only the solver's error would hide what the
	// site actually said.
	if !strings.Contains(err.Error(), "HTTP 403") {
		t.Errorf("the error should lead with the site's status: %v", err)
	}
	if !strings.Contains(err.Error(), "antibot retry failed") {
		t.Errorf("the error should say a retry was attempted: %v", err)
	}
}

// With no solver configured nothing changes, and no second request is
// made on any status.
func TestNoAntibotConfiguredIsUnchangedBehaviour(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte("denied"))
	}))
	defer site.Close()

	_, code, err := fetchThrough(t, FetchConfig{}, site.URL)
	if err == nil || code == 0 {
		t.Fatal("a 403 with no solver should still fail")
	}
	if strings.Contains(err.Error(), "antibot") {
		t.Errorf("an unconfigured solver was mentioned: %v", err)
	}
}

// A bad endpoint fails at REGISTRATION, not at the first 403 weeks
// later while somebody is looking at a different problem.
func TestAntibotBadURLIsRefusedAtRegistration(t *testing.T) {
	for _, bad := range []string{"ftp://solver/v1", "not a url at all", "://nohost"} {
		b := NewBuiltins()
		err := RegisterFetchBuiltin(b, FetchConfig{
			HTTPClient: http.DefaultClient,
			Antibot:    AntibotConfig{URL: bad},
		})
		if err == nil {
			t.Errorf("registration accepted a bad solver URL: %q", bad)
		}
	}
}

// THE SSRF BOUNDARY.
//
// The solver endpoint is exempt from the guard that refuses loopback
// and private addresses, because a solver is nearly always on the same
// machine. The exemption must apply to the OPERATOR'S endpoint and to
// nothing the model can influence: a turn's URL travels as JSON in the
// body, read by the solver, never dialled here.
func TestAntibotDialsOnlyTheConfiguredEndpoint(t *testing.T) {
	var dialled []string
	solver := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dialled = append(dialled, r.Host)
		var req antibotRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		resp := antibotResponse{Status: "ok"}
		resp.Solution.Status = 200
		resp.Solution.Response = "<html>ok</html>"
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer solver.Close()

	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer site.Close()

	// A target naming an internal address the guard would normally
	// refuse. It must reach the solver as DATA, never as a dial.
	_, _, _ = fetchThrough(t, FetchConfig{Antibot: AntibotConfig{URL: solver.URL}}, site.URL)

	if len(dialled) != 1 {
		t.Fatalf("expected exactly one connection, to the solver; got %v", dialled)
	}
	solverHost := strings.TrimPrefix(solver.URL, "http://")
	if dialled[0] != solverHost {
		t.Errorf("connected to %q, want the configured solver %q", dialled[0], solverHost)
	}
}

func TestAntibotTimeoutDefaults(t *testing.T) {
	c, err := newAntibotClient(AntibotConfig{URL: "http://127.0.0.1:8191/v1"})
	if err != nil {
		t.Fatal(err)
	}
	if c.timeout != DefaultAntibotTimeout {
		t.Errorf("timeout = %v, want %v", c.timeout, DefaultAntibotTimeout)
	}
	c, err = newAntibotClient(AntibotConfig{URL: "http://127.0.0.1:8191/v1", Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if c.timeout != 5*time.Second {
		t.Errorf("timeout = %v, want 5s", c.timeout)
	}
	// Absence, not a flag: no URL means no client at all.
	c, err = newAntibotClient(AntibotConfig{})
	if err != nil || c != nil {
		t.Errorf("an empty config should yield no client: c=%v err=%v", c, err)
	}
}
