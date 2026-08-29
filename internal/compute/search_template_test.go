package compute

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The template driver's whole claim is that a new search backend is a
// TOML block. This is that claim, exercised: Brave's real shape —
// GET, an X-Subscription-Token header, results nested under
// web.results, and a snippet field called "description" — described
// entirely by config, with no Brave-specific Go anywhere.
func TestTemplateDriverDescribesBraveWithoutCode(t *testing.T) {
	t.Parallel()
	var gotPath, gotToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.String()
		gotToken = r.Header.Get("X-Subscription-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[
			{"title":"Brave hit","url":"https://example.com/a","description":"a snippet","page_age":"2026-01-02"}
		]}}`))
	}))
	defer srv.Close()

	d, err := TemplateSearchFactory(SearchDriverConfig{
		Endpoint:   srv.URL,
		Credential: NewBearerCredential("brave-key"),
		Options: map[string]string{
			"method": "GET", "query_param": "q", "count_param": "count",
			"auth_style": "header", "auth_name": "X-Subscription-Token",
		},
		ExtraParams: map[string]string{"safesearch": "moderate"},
		Response: map[string]string{
			"results": "web.results", "title": "title", "url": "url",
			"snippet": "description", "published_at": "page_age",
		},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	results, err := d.Search(context.Background(), SearchRequest{Query: "hello world", NumResults: 4})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if gotToken != "brave-key" {
		t.Errorf("auth header = %q", gotToken)
	}
	for _, want := range []string{"q=hello+world", "count=4", "safesearch=moderate"} {
		if !strings.Contains(gotPath, want) {
			t.Errorf("request %q missing %q", gotPath, want)
		}
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Title != "Brave hit" || results[0].Text != "a snippet" || results[0].PublishedDate != "2026-01-02" {
		t.Errorf("mapping produced %+v", results[0])
	}
}

// Tavily's shape: POST with a JSON body, bearer auth, top-level
// results, and a numeric score. Same driver, different TOML.
func TestTemplateDriverPostsJSONBody(t *testing.T) {
	t.Parallel()
	var body map[string]any
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://x","content":"c","score":0.75}]}`))
	}))
	defer srv.Close()

	d, err := TemplateSearchFactory(SearchDriverConfig{
		Endpoint:   srv.URL,
		Credential: NewBearerCredential("tvly-key"),
		Options:    map[string]string{"method": "POST", "query_param": "query", "count_param": "max_results"},
		ExtraParams: map[string]string{
			"search_depth": "basic",
		},
	})
	if err != nil {
		t.Fatalf("factory: %v", err)
	}
	results, err := d.Search(context.Background(), SearchRequest{Query: "q", NumResults: 3})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if auth != "Bearer tvly-key" {
		t.Errorf("authorization = %q", auth)
	}
	if body["query"] != "q" || body["search_depth"] != "basic" {
		t.Errorf("post body = %+v", body)
	}
	// A JSON number, not "3". Tavily's max_results and its peers are
	// typed integers and reject the string form.
	if n, ok := body["max_results"].(float64); !ok || n != 3 {
		t.Errorf("max_results = %#v; want the JSON number 3", body["max_results"])
	}
	// The defaults cover this shape with no response mapping at all.
	if len(results) != 1 || results[0].Text != "c" || results[0].Score != 0.75 {
		t.Errorf("results = %+v", results)
	}
}

func TestTemplateAuthStyles(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		opts   map[string]string
		cred   Credential
		verify func(t *testing.T, r *http.Request)
	}{
		{"query", map[string]string{"auth_style": "query", "auth_name": "key"}, NewBearerCredential("s"),
			func(t *testing.T, r *http.Request) {
				if r.URL.Query().Get("key") != "s" {
					t.Errorf("key param = %q", r.URL.Query().Get("key"))
				}
			}},
		{"none", map[string]string{"auth_style": "none"}, nil,
			func(t *testing.T, r *http.Request) {
				if r.Header.Get("Authorization") != "" {
					t.Error("no credential should mean no header")
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				tc.verify(t, r)
				_, _ = w.Write([]byte(`{"results":[]}`))
			}))
			defer srv.Close()
			d, err := TemplateSearchFactory(SearchDriverConfig{Endpoint: srv.URL, Credential: tc.cred, Options: tc.opts})
			if err != nil {
				t.Fatalf("factory: %v", err)
			}
			if _, err := d.Search(context.Background(), SearchRequest{Query: "q"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// A mapping is written by hand against an API's docs, so the errors
// have to name the thing the operator typed.
func TestTemplateFactoryRejectsBadMappings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		cfg  SearchDriverConfig
		want string
	}{
		{"no endpoint", SearchDriverConfig{}, "endpoint required"},
		{"typo'd option", SearchDriverConfig{
			Endpoint: "https://x", Options: map[string]string{"quesry_param": "q"},
		}, "quesry_param"},
		// Rejected rather than silently ignored. option() reads keys
		// exactly, so a case-insensitive validator would have let this
		// through to do nothing.
		{"wrong-case option", SearchDriverConfig{
			Endpoint: "https://x", Options: map[string]string{"Method": "POST"},
		}, "Method"},
		{"typo'd response field", SearchDriverConfig{
			Endpoint: "https://x", Response: map[string]string{"snipppet": "content"},
		}, "snipppet"},
		{"unsupported method", SearchDriverConfig{
			Endpoint: "https://x", Options: map[string]string{"method": "PATCH"},
		}, "PATCH"},
		// Silently replacing the user's query with a constant would
		// return confident results for a question nobody asked.
		{"extra_params collides with the query", SearchDriverConfig{
			Endpoint: "https://x", Options: map[string]string{"auth_style": "none", "query_param": "q"},
			ExtraParams: map[string]string{"q": "static"},
		}, "query_param"},
		{"header auth without a name", SearchDriverConfig{
			Endpoint: "https://x", Credential: NewBearerCredential("k"),
			Options: map[string]string{"auth_style": "header"},
		}, "auth_name"},
		{"auth style without a key", SearchDriverConfig{
			Endpoint: "https://x", Options: map[string]string{"auth_style": "bearer"},
		}, "api_key_ref"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := TemplateSearchFactory(tc.cfg)
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should name %q", err, tc.want)
			}
		})
	}
}

// A mapping that points at nothing is the failure mode of a
// config-driven driver, so the error names the path rather than
// leaving the operator to guess which of six lines is wrong.
func TestTemplateNamesTheMissingResultPath(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"items":[]}}`))
	}))
	defer srv.Close()

	d, err := TemplateSearchFactory(SearchDriverConfig{
		Endpoint: srv.URL,
		Options:  map[string]string{"auth_style": "none"},
		Response: map[string]string{"results": "web.results"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, searchErr := d.Search(context.Background(), SearchRequest{Query: "q"})
	if searchErr == nil || !strings.Contains(searchErr.Error(), "web.results") {
		t.Errorf("error should name the configured path; got %v", searchErr)
	}
	if got := ClassifyFailure(searchErr); got != FailurePermanent {
		t.Errorf("a wrong mapping is wrong on every retry; class = %v", got)
	}
}

// Real APIs put numbers where a string is expected. Failing the whole
// search over a date field the model barely reads would be the wrong
// trade.
func TestTemplateCoercesScalarFields(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://x","content":"c","publishedDate":1767225600,"score":"0.5"}]}`))
	}))
	defer srv.Close()

	d, err := TemplateSearchFactory(SearchDriverConfig{Endpoint: srv.URL, Options: map[string]string{"auth_style": "none"}})
	if err != nil {
		t.Fatal(err)
	}
	results, err := d.Search(context.Background(), SearchRequest{Query: "q"})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %+v", results)
	}
	if results[0].PublishedDate != "1767225600" {
		t.Errorf("numeric date = %q", results[0].PublishedDate)
	}
	if results[0].Score != 0.5 {
		t.Errorf("string score = %v", results[0].Score)
	}
}

// The result count is a number in a JSON body and text in a query
// string, because a query string has no types. Both forms have to work
// off the same params map.
func TestTemplateCountIsTextInAQueryString(t *testing.T) {
	t.Parallel()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	d, err := TemplateSearchFactory(SearchDriverConfig{
		Endpoint: srv.URL,
		Options:  map[string]string{"auth_style": "none", "count_param": "count"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Search(context.Background(), SearchRequest{Query: "q", NumResults: 7}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(gotQuery, "count=7") {
		t.Errorf("query = %q; want count=7", gotQuery)
	}
}

// auth_prefix covers the schemes that are neither Bearer nor a bare
// header value — Kagi's `Authorization: Bot <token>` being the case
// that prompted it.
func TestTemplateAuthPrefix(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()

	d, err := TemplateSearchFactory(SearchDriverConfig{
		Endpoint:   srv.URL,
		Credential: &StaticCredential{Value: "tok"},
		Options: map[string]string{
			"auth_style":  "header",
			"auth_name":   "Authorization",
			"auth_prefix": "Bot ",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Search(context.Background(), SearchRequest{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if got != "Bot tok" {
		t.Errorf("Authorization = %q, want %q", got, "Bot tok")
	}
}

// A parameter-style prefix ends in punctuation and must NOT gain a
// separator, unlike an auth-scheme token.
func TestTemplateAuthPrefixParameterStyleGetsNoSpace(t *testing.T) {
	c, err := templateCredential(SearchDriverConfig{
		Credential: &StaticCredential{Value: "tok"},
		Options: map[string]string{
			"auth_style": "query", "auth_name": "auth", "auth_prefix": "key=",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	qc, ok := c.(*QueryCredential)
	if !ok {
		t.Fatalf("credential type %T", c)
	}
	if qc.Value != "key=tok" {
		t.Errorf("value = %q, want %q (no space inserted)", qc.Value, "key=tok")
	}
}

// A prefix that cannot be applied is a config error at boot, not a
// silently ignored key: bearer already carries its own scheme, and
// none sends nothing to prefix.
func TestTemplateAuthPrefixRejectedWhereMeaningless(t *testing.T) {
	for _, style := range []string{"bearer", "none"} {
		t.Run(style, func(t *testing.T) {
			_, err := templateCredential(SearchDriverConfig{
				Credential: &StaticCredential{Value: "tok"},
				Options:    map[string]string{"auth_style": style, "auth_prefix": "Bot "},
			})
			if err == nil {
				t.Errorf("auth_style %q with auth_prefix should fail at boot", style)
			}
		})
	}
}
