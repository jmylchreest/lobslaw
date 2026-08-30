package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/pkg/textutil"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// WebSearchConfig wires one search backend into the web_search
// builtin. The vendor specifics used to live in this file; they now
// live behind Driver, resolved from the DriverSet at boot the same way
// vision and audio resolve theirs.
type WebSearchConfig struct {
	// Driver is the resolved backend. Required — the wiring layer
	// builds it, so an unknown driver name is a start-up error rather
	// than a surprise at the first search.
	Driver compute.SearchDriver

	// Label is the provider's config label, used as the health key so
	// a demotion is shared with anything else reaching the same
	// endpoint. Empty opts out of health tracking.
	Label string

	// TrustTier is the provider's declared tier, checked against the
	// soul's min_trust_tier before this backend is used.
	//
	// web_search never had this check, which was the wrong builtin to
	// omit it from: it is the one that hands the user's own words to a
	// third party. A self-hosted SearXNG can honestly declare `local`;
	// a hosted API cannot.
	TrustTier types.TrustTier
}

// RegisterWebSearchBuiltin installs the web_search builtin.
//
// Variadic: one config is a single backend, several are a failover
// chain tried in the order given — which is what makes "self-hosted
// SearXNG, falling back to a hosted API when it is down" a config
// decision rather than a code one.
func RegisterWebSearchBuiltin(b *Builtins, cfgs ...WebSearchConfig) error {
	if len(cfgs) == 0 {
		return errors.New("web_search: at least one provider config required")
	}
	handlers := make([]compute.FailoverHandler, 0, len(cfgs))
	for _, cfg := range cfgs {
		if cfg.Driver == nil {
			return errors.New("web_search: Driver required (resolve it from the DriverSet)")
		}
		handlers = append(handlers, compute.FailoverHandler{
			Label: cfg.Label,
			Tier:  cfg.TrustTier,
			Fn:    newWebSearchHandler(cfg.Driver, cfg.Label),
		})
	}
	return b.Register("web_search", compute.FailoverBuiltin("web_search", nil, b.Health(), b.TrustFloor(), handlers...))
}

// WebSearchToolDef is the ToolDef to register alongside the
// builtin. Separate so node.New can conditionally register both or
// neither based on config.
//
// providers are the configured backend labels in failover order. They
// go into the DESCRIPTION rather than only into the response, because
// the description is the one thing the model has BEFORE it decides to
// call anything. Reporting the backend only in the result answers
// "which backend served this search"; it does not answer "is SearXNG
// available to you", which is a question about configuration and the
// model was previously reduced to guessing at from the process table.
func WebSearchToolDef(providers ...string) *types.ToolDef {
	return &types.ToolDef{
		Name: "web_search",
		Path: compute.BuiltinScheme + "web_search",
		Description: "Search the web for up-to-date information. Returns a list of results (title, url, snippet). Call this when the user asks about current events, recent changes, or facts you're not certain about. Pass query as the search string; optionally set num_results (default 5, max 10) and type (\"auto\", \"fast\", \"deep\" — \"auto\" is usually right). When summarising results for the user, CITE sources with markdown link syntax like [title](url) so the user can click through." +
			searchBackendSentence(providers) +
			" Each response repeats the backend in \"provider\", and per result in \"engine\" where the backend reports one.",
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

// searchBackendSentence names the configured backends for the tool
// description.
//
// Unlabelled providers are skipped rather than rendered as empty
// quotes, and a deployment with nothing to say gets nothing added —
// silence beats "This deployment's search backend is: .".
func searchBackendSentence(providers []string) string {
	named := make([]string, 0, len(providers))
	for _, p := range providers {
		if p = strings.TrimSpace(p); p != "" {
			named = append(named, strconv.Quote(p))
		}
	}
	switch len(named) {
	case 0:
		return ""
	case 1:
		return " This deployment's search backend is " + named[0] +
			" — treat that as authoritative if asked which search backend is available, rather than inspecting the host."
	default:
		return " This deployment's search backends, in failover order, are " + strings.Join(named, ", ") +
			" — treat that as authoritative if asked which search backends are available, rather than inspecting the host."
	}
}

// searchSnippetCap trims each result's text. LLMs don't need the full
// page, just a citable snippet — and a ten-result search at full page
// length would eat the turn's context budget on its own.
//
// Runes, not bytes. The byte slice this replaced was one of the class
// textutil.Truncate exists to end: it gave a Japanese speaker a third
// of the snippet an English one got, and cut the last character in
// half on the way out.
const searchSnippetCap = 600

// newWebSearchHandler is the backend-agnostic half: argument parsing,
// clamping, snippet trimming, and the response envelope. Every driver
// produces the same SHAPE, so nothing downstream — the prompt's tool
// guidance, the research workers, existing transcripts — has to change
// when the backend does. The provider label rides along inside it so
// the agent can still say which one answered.
func newWebSearchHandler(driver compute.SearchDriver, provider string) compute.BuiltinFunc {
	return func(ctx context.Context, args map[string]string) ([]byte, int, error) {
		query := args["query"]
		if query == "" {
			return nil, 2, fmt.Errorf("web_search: query is required")
		}
		numResults := compute.DefaultSearchResults
		if raw, ok := args["num_results"]; ok && raw != "" {
			if n, err := strconv.Atoi(raw); err == nil && n > 0 && n <= compute.MaxSearchResults {
				numResults = n
			}
		}
		searchType := args["type"]
		if searchType == "" {
			searchType = "auto"
		}

		results, err := driver.Search(ctx, compute.SearchRequest{
			Query:      query,
			NumResults: numResults,
			Depth:      searchType,
		})
		if err != nil {
			return nil, 1, err
		}
		for i := range results {
			results[i].Text = textutil.Truncate(results[i].Text, "…", searchSnippetCap)
		}
		out, err := json.Marshal(map[string]any{
			"query": query,
			// Which backend answered. Additive, and it exists because
			// the agent could not otherwise tell: asked whether a search
			// had gone through the operator's self-hosted SearXNG, it
			// shelled out to pgrep, got nothing (the sandbox has no
			// /proc), and reported there was no SearXNG — while every
			// result in its hands had come from one. A tool that cannot
			// say who served it makes the agent guess, and the agent
			// guessed wrong in the confident direction.
			//
			// Empty when the provider is unlabelled, so it costs an
			// unconfigured deployment nothing.
			"provider": provider,
			"results":  results,
		})
		if err != nil {
			return nil, 1, err
		}
		return out, 0, nil
	}
}
