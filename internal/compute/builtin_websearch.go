package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// defaultExaEndpoint is Exa's search API. Overridable via
// EXA_API_URL — lets operators point at their own search proxy
// or a staging environment.
const defaultExaEndpoint = "https://api.exa.ai/search"

// WebSearchConfig wires the Exa-backed web_search builtin. Zero
// values disable the builtin (so tests without an EXA_API_KEY
// don't accidentally leak network traffic).
type WebSearchConfig struct {
	// APIKey is the Exa API key. When empty, the builtin refuses
	// to register — operators who haven't configured a key get a
	// clear "not configured" error instead of silent no-op.
	APIKey string

	// Endpoint overrides the Exa URL. Zero → defaultExaEndpoint.
	Endpoint string

	// HTTPClient lets callers inject a test double. Zero → a new
	// http.Client with a 15s timeout.
	HTTPClient *http.Client
}

// RegisterWebSearchBuiltin installs the web_search builtin when an
// API key is configured. Callers that don't want the builtin
// simply don't call this; the tool won't show up in the LLM's
// function list.
func RegisterWebSearchBuiltin(b *Builtins, cfg WebSearchConfig) error {
	if cfg.APIKey == "" {
		return fmt.Errorf("web_search: APIKey required to register builtin")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultExaEndpoint
	}
	if v := os.Getenv("EXA_API_URL"); v != "" {
		endpoint = v
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	fn := newExaWebSearchHandler(endpoint, cfg.APIKey, client)
	return b.Register("web_search", fn)
}

// WebSearchToolDef is the ToolDef to register alongside the
// builtin. Separate so node.New can conditionally register both or
// neither based on config.
func WebSearchToolDef() *types.ToolDef {
	return &types.ToolDef{
		Name:        "web_search",
		Path:        BuiltinScheme + "web_search",
		Description: "Search the web for up-to-date information. Returns a list of results (title, url, snippet). Call this when the user asks about current events, recent changes, or facts you're not certain about. Pass query as the search string; optionally set num_results (default 5, max 10) and type (\"auto\", \"fast\", \"deep\" — \"auto\" is usually right). When summarising results for the user, CITE sources with markdown link syntax like [title](url) so the user can click through.",
		ParametersSchema: []byte(`{
			"type": "object",
			"properties": {
				"query": {"type": "string", "description": "Search query text."},
				"num_results": {"type": "integer", "description": "Results to return (1-10). Default 5."},
				"type": {"type": "string", "enum": ["auto", "fast", "deep"], "description": "Search latency/depth tradeoff."}
			},
			"required": ["query"],
			"additionalProperties": false
		}`),
		RiskTier: types.RiskCommunicating,
	}
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

// newExaWebSearchHandler returns the BuiltinFunc that dispatches a
// web_search tool-call to Exa. Returns compact JSON the model can
// cite back to the user.
func newExaWebSearchHandler(endpoint, apiKey string, client *http.Client) BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		query := args["query"]
		if query == "" {
			return nil, 2, fmt.Errorf("web_search: query is required")
		}
		numResults := 5
		if raw, ok := args["num_results"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= 10 {
				numResults = n
			}
		}
		searchType := args["type"]
		if searchType == "" {
			searchType = "auto"
		}

		reqBody, _ := json.Marshal(exaSearchRequest{
			Query:      query,
			NumResults: numResults,
			Type:       searchType,
			Contents:   &exaContentsOpt{Text: true},
		})
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(reqBody))
		if err != nil {
			return nil, 1, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json")
		httpReq.Header.Set("x-api-key", apiKey)

		resp, err := client.Do(httpReq)
		if err != nil {
			return nil, 1, fmt.Errorf("web_search: http: %w", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return nil, 1, fmt.Errorf("web_search: HTTP %d: %s", resp.StatusCode, truncateBodyFor(raw, 512))
		}
		var decoded exaSearchResponse
		if err := json.Unmarshal(raw, &decoded); err != nil {
			return nil, 1, fmt.Errorf("web_search: decode: %w", err)
		}
		// Trim each result's Text to keep the tool output compact —
		// LLMs don't need the full page, just a citable snippet.
		const snippetCap = 600
		for i := range decoded.Results {
			if len(decoded.Results[i].Text) > snippetCap {
				decoded.Results[i].Text = decoded.Results[i].Text[:snippetCap] + "…"
			}
		}
		out, err := json.Marshal(map[string]any{
			"query":   query,
			"results": decoded.Results,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}

// truncateBodyFor is a local helper that doesn't conflict with the
// one in llmclient.go (they serve different truncation caps).
func truncateBodyFor(body []byte, max int) string {
	if len(body) <= max {
		return string(body)
	}
	return string(body[:max]) + "…[truncated]"
}
