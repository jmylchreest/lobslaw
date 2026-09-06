package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// THE SECOND ATTEMPT, FOR SITES THAT REFUSE THE FIRST.
//
// Some sites do not serve a plain HTTP client at all, whatever header
// it sends. They answer a challenge that only a real browser completes
// — running the JavaScript, holding the cookies, looking like Chrome
// because it IS Chrome. Measured on the two that prompted this:
//
//	idealhome.co.uk   403 to Go's default UA, 200 to any other
//	argos.co.uk       403 to every header tried, 200 through a browser
//
// The first needed a correct header and nothing more (see #290). Only
// the second needs this, and the distinction is the whole design: a
// browser is expensive — a process, a page load, seconds not
// milliseconds — so it runs ONLY after a direct fetch has been
// refused, never in front of one.
//
// The service is spoken to over the FlareSolverr protocol because that
// is what the available implementations answer, but nothing here
// assumes a particular one.

// AntibotConfig points fetch_url at a challenge-solving service.
//
// Disabled by default and by absence: an empty URL means the field is
// never consulted and no second attempt is ever made. Turning this on
// routes some traffic through a browser somebody else operates, which
// is a decision an operator should make rather than inherit.
type AntibotConfig struct {
	// URL is the solver endpoint, e.g. "http://127.0.0.1:8191/v1".
	URL string

	// Timeout bounds one solve. Zero takes DefaultAntibotTimeout.
	//
	// Generous compared to a direct fetch because it is not comparable
	// work: a challenge means loading a page, running its JavaScript
	// and waiting out a deliberate delay, and a solver cut off at a
	// normal HTTP timeout would fail every time while looking like a
	// network fault.
	Timeout time.Duration
}

// DefaultAntibotTimeout is how long one challenge may take.
const DefaultAntibotTimeout = 60 * time.Second

// antibotMaxBody caps what a solve may return, matching the direct
// path. A rendered page is larger than its source — the two that
// prompted this came back around 2 MB each — but not unboundedly so.
const antibotMaxBody = fetchMaxResponseBody

// shouldRetryViaAntibot reports whether a status code looks like a
// refusal a browser might get past.
//
// Deliberately narrow. 403 and 503 are what challenge pages answer;
// 429 is rate limiting, where retrying through a browser is both
// unlikely to work and rude, and 404 means the page is not there for
// anybody. A wider net would send ordinary failures through a browser
// and turn every genuine 404 into a multi-second one.
func shouldRetryViaAntibot(status int) bool {
	return status == http.StatusForbidden || status == http.StatusServiceUnavailable
}

// antibotRequest is the solve command.
type antibotRequest struct {
	Cmd        string `json:"cmd"`
	URL        string `json:"url"`
	MaxTimeout int    `json:"maxTimeout"`
}

// antibotResponse is the reply. Only the fields that decide the
// outcome are modelled; a solver carrying more is not an error.
type antibotResponse struct {
	Status   string `json:"status"`
	Message  string `json:"message"`
	Solution struct {
		URL      string `json:"url"`
		Status   int    `json:"status"`
		Response string `json:"response"`
	} `json:"solution"`
}

// antibotClient fetches through a challenge solver.
type antibotClient struct {
	endpoint string
	timeout  time.Duration
	http     *http.Client
}

// newAntibotClient builds the client, or returns nil when no endpoint
// is configured.
//
// A SEPARATE http.Client from the one fetch_url uses, deliberately.
// The fetch client carries the SSRF guard, which refuses loopback and
// private addresses — correctly, since it is what stops the model
// reaching the metadata service or the machine's own admin ports. A
// solver, though, is nearly always ON the machine or beside it, so
// sending this request through that guard would refuse the one address
// the operator actually configured.
//
// The exemption is therefore as narrow as it can be: it applies to
// exactly one operator-named endpoint, for requests this file
// constructs, and never to a URL the model chose. The model's URL
// travels as JSON in the body — it is data the solver reads, not a
// destination anything here dials — so no input from a turn can
// redirect where this connects.
func newAntibotClient(cfg AntibotConfig) (*antibotClient, error) {
	endpoint := strings.TrimSpace(cfg.URL)
	if endpoint == "" {
		return nil, nil
	}
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("antibot: url must be http or https: %q", cfg.URL)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("antibot: url has no host: %q", cfg.URL)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultAntibotTimeout
	}
	return &antibotClient{
		endpoint: endpoint,
		timeout:  timeout,
		http: &http.Client{
			Timeout: timeout + 5*time.Second,
			Transport: &http.Transport{
				// Pinned to the configured host. A redirect cannot
				// walk this connection somewhere else, and neither can
				// a DNS answer that changes between calls.
				DialContext: (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			},
		},
	}, nil
}

// fetch asks the solver for a page and returns its rendered body.
func (a *antibotClient) fetch(ctx context.Context, target string) (string, error) {
	payload, err := json.Marshal(antibotRequest{
		Cmd:        "request.get",
		URL:        target,
		MaxTimeout: int(a.timeout / time.Millisecond),
	})
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(ctx, a.timeout+5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := a.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("antibot: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, antibotMaxBody))
	if err != nil {
		return "", fmt.Errorf("antibot: read: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("antibot: solver returned HTTP %d", resp.StatusCode)
	}
	var out antibotResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("antibot: decode: %w", err)
	}
	if !strings.EqualFold(out.Status, "ok") {
		return "", fmt.Errorf("antibot: %s", firstLine(out.Message, "solve failed"))
	}
	// A solver reporting ok while the SITE refused is still a refusal.
	// Without this the caller would cache and return a challenge page
	// as though it were the article.
	if out.Solution.Status >= 400 {
		return "", fmt.Errorf("antibot: solved but the site answered HTTP %d", out.Solution.Status)
	}
	if strings.TrimSpace(out.Solution.Response) == "" {
		return "", fmt.Errorf("antibot: solved but returned no body")
	}
	return out.Solution.Response, nil
}

// firstLine keeps a solver's message to one line for an error string.
func firstLine(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	return s
}
