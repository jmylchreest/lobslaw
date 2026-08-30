package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

// DefaultExaEndpoint is Exa's search API. Overridable via EXA_API_URL —
// lets operators point at their own search proxy or a staging
// environment.
const DefaultExaEndpoint = "https://api.exa.ai/search"

// ExaSearchFactory builds the Exa driver.
//
// This is the code that used to BE the web_search builtin, moved
// behind the driver interface with its behaviour unchanged: the same
// endpoint default, the same EXA_API_URL override, the same
// x-api-key header, the same request and response shapes. The only
// addition is failure classification, which the builtin had no use for
// when it could not fall through to anything.
func ExaSearchFactory(cfg SearchDriverConfig) (SearchDriver, error) {
	if bad := unknownOptions(cfg.Options); len(bad) > 0 {
		return nil, fmt.Errorf("exa search: unknown option(s) %v; the exa driver takes none", bad)
	}
	if cfg.Credential == nil {
		return nil, errors.New("exa search: api_key_ref required (Exa has no anonymous tier)")
	}
	return &exaSearchDriver{
		endpoint: ExaEffectiveEndpoint(cfg.Endpoint),
		cred:     cfg.Credential,
		client:   searchHTTPClient(cfg.HTTPClient),
	}, nil
}

// ExaEffectiveEndpoint resolves the URL Exa will actually be called
// at: configured, else the default, with EXA_API_URL overriding both.
//
// Exported because the egress ACL builder has to agree with the
// driver about this. A role allowing api.exa.ai while the driver calls
// a staging host is a proxy denial at the first search, naming neither
// the override nor the role.
func ExaEffectiveEndpoint(configured string) string {
	endpoint := configured
	if endpoint == "" {
		endpoint = DefaultExaEndpoint
	}
	// Env beats config, as it did before the driver split. It is how
	// the deploy manifests point a container at a staging Exa without
	// rewriting config.toml.
	if v := os.Getenv("EXA_API_URL"); v != "" {
		endpoint = v
	}
	return endpoint
}

type exaSearchDriver struct {
	endpoint string
	cred     Credential
	client   *http.Client
}

func (d *exaSearchDriver) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	body, err := json.Marshal(exaSearchRequest{
		Query:      req.Query,
		NumResults: req.NumResults,
		Type:       req.Depth,
		Contents:   &exaContentsOpt{Text: true},
	})
	if err != nil {
		return nil, Permanent(err)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, Permanent(err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if err := d.cred.Apply(ctx, httpReq); err != nil {
		return nil, CredentialRejected(err)
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, Transient(fmt.Errorf("exa search: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("exa search: HTTP %d: %s", resp.StatusCode, TruncateBodyFor(raw, 512)),
		}
	}
	var decoded exaSearchResponse
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, Permanent(fmt.Errorf("exa search: decode: %w", err))
	}
	out := make([]SearchResult, 0, len(decoded.Results))
	for _, r := range decoded.Results {
		out = append(out, SearchResult{
			Title:         r.Title,
			URL:           r.URL,
			PublishedDate: r.PublishedDate,
			Text:          r.Text,
			Score:         r.Score,
		})
	}
	return out, nil
}

// ExaCredential is the auth shape Exa expects: a bare x-api-key
// header, not a bearer token.
func ExaCredential(apiKey string) Credential {
	return NewHeaderCredential("x-api-key", apiKey)
}

type exaSearchRequest struct {
	Query      string          `json:"query"`
	NumResults int             `json:"numResults,omitempty"`
	Type       string          `json:"type,omitempty"`
	Contents   *exaContentsOpt `json:"contents,omitempty"`
}

type exaContentsOpt struct {
	Text bool `json:"text"`
}

type exaSearchResponse struct {
	Results []exaResult `json:"results"`
}

type exaResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	PublishedDate string  `json:"publishedDate,omitempty"`
	Text          string  `json:"text,omitempty"`
	Score         float64 `json:"score,omitempty"`
}

// TruncateBodyFor caps an error body. Local to the search drivers
// because llmclient.go has its own with a different cap.
//
// Runes, via textutil, for the same reason the snippet cap is: this
// string reaches a prompt and a Telegram message, and a search engine's
// error page is exactly the sort of prose that is not ASCII.
func TruncateBodyFor(body []byte, max int) string {
	return textutil.Truncate(string(body), "…[truncated]", max)
}
