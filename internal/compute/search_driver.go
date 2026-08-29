package compute

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Web search, as a driver rather than one vendor inlined in a builtin.
//
// `web_search` was Exa end to end — Exa's request body, Exa's
// x-api-key header, Exa's response struct — while every other
// pluggable backend in the tree resolved through DriverSet. Config
// even documented `provider = "tavily"`, a key that did not exist.
//
// Search is the shallowest modality lobslaw talks to: a query goes
// out, a list of {title, url, snippet} comes back. That shallowness is
// why it gets two tiers rather than one. A compiled driver is for the
// engines with real behaviour behind them — Exa's typed search, a
// SearXNG instance's engine and category knobs. Everything else is a
// field mapping, and a field mapping should not need a Go file, which
// is what DriverTemplate is for.

// SearchRequest is one query.
type SearchRequest struct {
	// Query is the search string, already validated non-empty by the
	// builtin.
	Query string

	// NumResults is the requested result count, clamped by the builtin
	// to 1..MaxSearchResults before it reaches a driver.
	NumResults int

	// Depth carries the tool's `type` argument ("auto", "fast",
	// "deep"). It is advisory: Exa acts on it, and a driver whose
	// engine has no such concept ignores it rather than failing. The
	// alternative — rejecting an argument the tool schema advertises —
	// would make the model's own tool list a source of errors.
	Depth string
}

// SearchResult is one hit, in the shape the builtin serialises back to
// the model. The JSON tags matter: they are the wire contract the
// agent's prompt and every existing transcript were written against,
// so they keep Exa's original names.
type SearchResult struct {
	Title         string  `json:"title"`
	URL           string  `json:"url"`
	PublishedDate string  `json:"publishedDate,omitempty"`
	Text          string  `json:"text,omitempty"`
	Score         float64 `json:"score,omitempty"`

	// Engine is which upstream engine produced the hit. A metasearch
	// front-end like SearXNG knows this and it is genuinely useful to
	// the model ("three independent engines agree"); everything else
	// leaves it empty and the field disappears from the JSON.
	Engine string `json:"engine,omitempty"`
}

// SearchDriver runs one query against one backend.
//
// Errors should be classified (DriverError / Transient /
// CredentialRejected, or the shared ClassifyHTTPStatus mapping) so the
// failover chain can tell "try the next provider" from "this fails
// everywhere". An unclassified error is treated as permanent and stops
// the chain.
type SearchDriver interface {
	Search(ctx context.Context, req SearchRequest) ([]SearchResult, error)
}

// SearchDriverConfig is what every search driver is built from.
//
// The three maps are deliberate. Search backends differ almost
// entirely in their parameter names and response paths, so typing each
// vendor's knobs into this struct would mean a config-struct change —
// and a rebuild — for every new engine, which is the exact cost this
// design exists to remove. A factory validates its own keys at boot,
// so a typo is a start-up error naming the offending key rather than a
// surprise at the first search.
type SearchDriverConfig struct {
	// Endpoint is the full search URL. Every driver has a sensible
	// default except the self-hosted ones, which cannot have one.
	Endpoint string

	// Credential is nil for backends that need no auth — a private
	// SearXNG being the motivating case, and the reason the builtin no
	// longer gates registration on a non-empty API key.
	Credential Credential

	HTTPClient *http.Client
	Logger     *slog.Logger

	// Options are driver-specific scalars from `options = {...}`.
	Options map[string]string

	// ExtraParams are static query parameters merged into every
	// request, used by DriverTemplate for the fixed knobs an API wants
	// but lobslaw has no opinion about (safesearch, market, freshness).
	ExtraParams map[string]string

	// Response maps result fields to dot-paths in the backend's JSON.
	// DriverTemplate only; compiled drivers know their own shape.
	Response map[string]string
}

// SearchDriverFactory builds one configured search driver.
type SearchDriverFactory func(SearchDriverConfig) (SearchDriver, error)

// Search driver names. Config uses these strings.
const (
	// DriverExa is the Exa search API, and the default when config
	// names no driver — that is what every pre-existing
	// [compute.web_search] block meant.
	DriverExa = "exa"

	// DriverKagi is Kagi's search API. Compiled rather than templated
	// because its result array is heterogeneous — see search_kagi.go.
	DriverKagi = "kagi"

	// DriverSearxng is a self-hosted SearXNG instance's JSON API.
	DriverSearxng = "searxng"

	// DriverTemplate is the declarative interpreter: a provider
	// described entirely by TOML, with no Go and no rebuild. See
	// search_template.go.
	DriverTemplate = "template"
)

// MaxSearchResults caps what any driver is asked for. Matches the
// tool schema's documented maximum.
const MaxSearchResults = 10

// DefaultSearchResults is what the tool returns when the model does
// not ask for a count.
const DefaultSearchResults = 5

// DefaultSearchTimeout bounds one query. Searches are interactive —
// the user is waiting — so this is far tighter than the 60s the
// modality drivers allow for a model round-trip.
const DefaultSearchTimeout = 15 * time.Second

// RegisterSearch adds a driver under name.
func (s *DriverSet) RegisterSearch(name string, f SearchDriverFactory) {
	if s.search == nil {
		s.search = make(map[string]SearchDriverFactory)
	}
	s.search[normaliseDriverName(name)] = f
}

// Search builds the named driver. An empty name picks Exa, so a config
// that predates driver selection resolves to what it always meant.
func (s *DriverSet) Search(name string, cfg SearchDriverConfig) (SearchDriver, error) {
	key := normaliseDriverName(name)
	if key == "" {
		key = DriverExa
	}
	f, ok := s.search[key]
	if !ok {
		return nil, fmt.Errorf("unknown search driver %q; available: %s",
			name, strings.Join(s.SearchNames(), ", "))
	}
	return f(cfg)
}

// SearchNames lists the registered search drivers, sorted.
func (s *DriverSet) SearchNames() []string { return sortedKeys(s.search) }

// searchHTTPClient is the per-driver client default. Separate from
// HTTPClientOr because 60s is the wrong answer for an interactive
// lookup.
func searchHTTPClient(c *http.Client) *http.Client {
	if c != nil {
		return c
	}
	return &http.Client{Timeout: DefaultSearchTimeout}
}

// option reads a driver option, trimmed. Missing and blank are the
// same answer — an operator who wrote `language = ""` meant "no
// preference", not "send an empty language".
func option(opts map[string]string, key string) string {
	return strings.TrimSpace(opts[key])
}

// unknownOptions reports any key not in allowed, so a factory can
// reject a typo at boot. Returned sorted for a stable message.
//
// Matched EXACTLY, because option() looks keys up exactly. An earlier
// version lowercased here and not there, which meant `Method = "POST"`
// passed validation and was then silently ignored — a setting that
// parses, validates, and does nothing, which is the failure
// TestEverySettingIsReadBySomething exists to keep out of this tree.
// Case-sensitive is also simply what TOML keys are.
func unknownOptions(opts map[string]string, allowed ...string) []string {
	if len(opts) == 0 {
		return nil
	}
	known := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		known[a] = struct{}{}
	}
	var bad []string
	for k := range opts {
		if _, ok := known[k]; !ok {
			bad = append(bad, k)
		}
	}
	sort.Strings(bad)
	return bad
}
