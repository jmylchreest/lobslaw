package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jmylchreest/lobslaw/internal/compute"
)

// stubSearchDriver answers from a fixed script, recording that it ran.
type stubSearchDriver struct {
	name    string
	results []compute.SearchResult
	err     error
	calls   *[]string
	lastReq compute.SearchRequest
}

func (d *stubSearchDriver) Search(_ context.Context, req compute.SearchRequest) ([]compute.SearchResult, error) {
	if d.calls != nil {
		*d.calls = append(*d.calls, d.name)
	}
	d.lastReq = req
	if d.err != nil {
		return nil, d.err
	}
	return d.results, nil
}

func TestRegisterWebSearchBuiltinRequiresProvider(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b); err == nil {
		t.Error("no provider config should fail register")
	}
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{Label: "x"}); err == nil {
		t.Error("a config with no driver should fail register")
	}
}

// The output envelope is a contract, not an implementation detail: the
// prompt tells the model to cite [title](url) from it, and every
// transcript ever written contains it. A driver swap must not be
// visible in this JSON.
func TestWebSearchEnvelopeIsBackendAgnostic(t *testing.T) {
	t.Parallel()
	driver := &stubSearchDriver{results: []compute.SearchResult{
		{Title: "Go Generics Explained", URL: "https://go.dev/x", Text: strings.Repeat("a", 1000)},
		{Title: "Generics FAQ", URL: "https://go.dev/faq", Text: "short"},
	}}
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{Label: "stub", Driver: driver}); err != nil {
		t.Fatal(err)
	}
	fn, ok := b.Get("web_search")
	if !ok {
		t.Fatal("web_search not registered")
	}
	stdout, exit, err := fn(context.Background(), map[string]string{
		"query":       "golang generics",
		"num_results": "3",
	})
	if err != nil || exit != 0 {
		t.Fatalf("search: exit=%d err=%v", exit, err)
	}
	if driver.lastReq.Query != "golang generics" || driver.lastReq.NumResults != 3 {
		t.Errorf("driver saw %+v", driver.lastReq)
	}
	if driver.lastReq.Depth != "auto" {
		t.Errorf("depth = %q; want the schema default", driver.lastReq.Depth)
	}

	var payload struct {
		Query   string                 `json:"query"`
		Results []compute.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(stdout, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Query != "golang generics" {
		t.Errorf("echoed query = %q", payload.Query)
	}
	if len(payload.Results) != 2 {
		t.Fatalf("results = %d; want 2", len(payload.Results))
	}
	if !strings.HasSuffix(payload.Results[0].Text, "…") {
		t.Errorf("long snippet should be truncated with …; got len=%d", len(payload.Results[0].Text))
	}
	// engine is omitempty, so a backend that doesn't report one adds
	// no field the model has to reason about.
	if strings.Contains(string(stdout), "engine") {
		t.Errorf("empty engine should not appear in the envelope: %s", stdout)
	}
}

// Snippets are cut in runes, not bytes: a byte cut lands inside a
// character and hands the model — and the transcript, and Telegram —
// invalid UTF-8.
func TestWebSearchTruncatesSnippetsOnRuneBoundaries(t *testing.T) {
	t.Parallel()
	driver := &stubSearchDriver{results: []compute.SearchResult{
		{Title: "T", URL: "https://x", Text: strings.Repeat("日", 900)},
	}}
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{Driver: driver}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("web_search")
	stdout, _, err := fn(context.Background(), map[string]string{"query": "q"})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Results []compute.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(stdout, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := payload.Results[0].Text
	if !utf8.ValidString(got) {
		t.Fatal("truncation produced invalid UTF-8")
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("want an ellipsis; got %q", got[len(got)-8:])
	}
	if n := utf8.RuneCountInString(strings.TrimSuffix(got, "…")); n != searchSnippetCap {
		t.Errorf("kept %d runes; want %d — a byte cap would have kept a third of that", n, searchSnippetCap)
	}
}

// The agent was asked whether a search had gone through the operator's
// self-hosted SearXNG. Nothing in the tool result said, so it shelled
// out to pgrep, got nothing back through a sandbox with no /proc, and
// answered that there was no SearXNG — while holding results that had
// come from one. The envelope names the backend so that question has an
// answer that does not require guessing.
func TestWebSearchEnvelopeNamesTheProvider(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{
		Label:  "searxng",
		Driver: &stubSearchDriver{results: []compute.SearchResult{{Title: "T", URL: "https://x", Engine: "google cse"}}},
	}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("web_search")
	stdout, _, err := fn(context.Background(), map[string]string{"query": "q"})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Provider string                 `json:"provider"`
		Results  []compute.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(stdout, &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Provider != "searxng" {
		t.Errorf("provider = %q; want the configured label", payload.Provider)
	}
	// The per-result engine rides along too: a metasearch front-end
	// knows which upstream produced each hit, and that is a second,
	// independent signal of which backend answered.
	if payload.Results[0].Engine != "google cse" {
		t.Errorf("engine = %q", payload.Results[0].Engine)
	}
}

func TestWebSearchClampsNumResults(t *testing.T) {
	t.Parallel()
	driver := &stubSearchDriver{}
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{Driver: driver}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("web_search")
	for _, raw := range []string{"0", "-1", "99", "banana"} {
		if _, _, err := fn(context.Background(), map[string]string{"query": "q", "num_results": raw}); err != nil {
			t.Fatalf("num_results=%q: %v", raw, err)
		}
		if driver.lastReq.NumResults != compute.DefaultSearchResults {
			t.Errorf("num_results=%q gave %d; want the default %d",
				raw, driver.lastReq.NumResults, compute.DefaultSearchResults)
		}
	}
}

func TestWebSearchBuiltinRejectsEmptyQuery(t *testing.T) {
	t.Parallel()
	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b, WebSearchConfig{Driver: &stubSearchDriver{}}); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("web_search")
	_, exit, err := fn(context.Background(), map[string]string{})
	if err == nil || exit == 0 {
		t.Error("empty query should fail")
	}
}

// The reason web_search became variadic: a self-hosted SearXNG having
// a bad minute should fall through to whatever else is configured,
// rather than leaving the agent with no way to look anything up.
func TestWebSearchFallsOverToTheNextBackend(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	primary := &stubSearchDriver{name: "searxng", err: compute.Transient(errors.New("connection refused")), calls: calls}
	backup := &stubSearchDriver{name: "exa", calls: calls,
		results: []compute.SearchResult{{Title: "T", URL: "https://example.com"}}}

	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b,
		WebSearchConfig{Label: "searxng", Driver: primary},
		WebSearchConfig{Label: "exa", Driver: backup},
	); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("web_search")
	stdout, exit, err := fn(context.Background(), map[string]string{"query": "q"})
	if err != nil || exit != 0 {
		t.Fatalf("chain should have recovered: exit=%d err=%v", exit, err)
	}
	if got := strings.Join(*calls, ","); got != "searxng,exa" {
		t.Errorf("call order = %q; want searxng,exa", got)
	}
	if !strings.Contains(string(stdout), "https://example.com") {
		t.Errorf("backup's results should be returned; got %s", stdout)
	}
}

// A permanent failure must NOT walk the chain: every backend would
// reject the same thing and the operator would read the last one's
// error instead of the first one's.
func TestWebSearchDoesNotFallOverOnPermanent(t *testing.T) {
	t.Parallel()
	calls := &[]string{}
	primary := &stubSearchDriver{name: "searxng", calls: calls,
		err: compute.Permanent(errors.New("searxng search: JSON API disabled"))}
	backup := &stubSearchDriver{name: "exa", calls: calls}

	b := NewBuiltins()
	if err := RegisterWebSearchBuiltin(b,
		WebSearchConfig{Label: "searxng", Driver: primary},
		WebSearchConfig{Label: "exa", Driver: backup},
	); err != nil {
		t.Fatal(err)
	}
	fn, _ := b.Get("web_search")
	if _, _, err := fn(context.Background(), map[string]string{"query": "q"}); err == nil {
		t.Fatal("permanent failure should surface")
	}
	if got := strings.Join(*calls, ","); got != "searxng" {
		t.Errorf("call order = %q; want searxng alone", got)
	}
}

// Naming the backend only in the RESULT answers "which backend served
// this search". It does not answer "is SearXNG available to you" —
// a question about configuration, asked before any search happens.
// That one sent the agent to pgrep. The description is what it has in
// hand before it decides anything, so the answer lives there too.
func TestWebSearchToolDefNamesConfiguredBackends(t *testing.T) {
	t.Parallel()
	single := WebSearchToolDef("searxng").Description
	if !strings.Contains(single, `"searxng"`) {
		t.Errorf("single-backend description does not name it: %s", single)
	}
	if !strings.Contains(single, "authoritative") {
		t.Error("description should tell the model to trust it over inspecting the host")
	}

	chain := WebSearchToolDef("searxng", "exa").Description
	if !strings.Contains(chain, "failover order") || !strings.Contains(chain, `"exa"`) {
		t.Errorf("chain description = %s", chain)
	}

	// A deployment with nothing to say says nothing, rather than
	// emitting "the search backend is: ." at every turn.
	bare := WebSearchToolDef().Description
	if strings.Contains(bare, "search backend is") {
		t.Errorf("unlabelled deployment should add no sentence: %s", bare)
	}
	if strings.Contains(WebSearchToolDef("", "  ").Description, "search backend") {
		t.Error("blank labels should be skipped, not quoted as empty")
	}
}
