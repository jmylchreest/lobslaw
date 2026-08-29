package compute

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func kagiDriver(t *testing.T, srv *httptest.Server) SearchDriver {
	t.Helper()
	d, err := KagiSearchFactory(SearchDriverConfig{
		Endpoint:   srv.URL + "/search",
		Credential: &StaticCredential{Value: "tok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// `data` is heterogeneous: t=0 is a result, t=1 is a block of related
// searches carrying no url and no snippet. Handing those to the model
// as results is the whole reason this is a compiled driver rather than
// a template mapping.
func TestKagiSkipsNonResultItems(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"meta":{"id":"x","ms":12},"data":[
			{"t":0,"rank":1,"url":"https://a.example","title":"A","snippet":"first"},
			{"t":1,"list":["related one","related two"]},
			{"t":0,"rank":2,"url":"https://b.example","title":"B","snippet":"second"}
		]}`))
	}))
	defer srv.Close()

	got, err := kagiDriver(t, srv).Search(context.Background(), SearchRequest{Query: "q", NumResults: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d results, want 2 (the t=1 block must be dropped): %+v", len(got), got)
	}
	if got[0].URL != "https://a.example" || got[1].URL != "https://b.example" {
		t.Errorf("wrong results: %+v", got)
	}
}

// Kagi marks matched query terms with <b>. Raw markup in what the
// prompt presents as prose gets reproduced by the model.
func TestKagiStripsSnippetMarkup(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"t":0,"url":"https://a.example",
			"title":"The <b>Grand</b> Canyon","snippet":"<b>Grand Canyon</b> National Park, in Arizona"}]}`))
	}))
	defer srv.Close()

	got, err := kagiDriver(t, srv).Search(context.Background(), SearchRequest{Query: "grand canyon"})
	if err != nil {
		t.Fatal(err)
	}
	if got[0].Title != "The Grand Canyon" {
		t.Errorf("title = %q, want the tags gone and the words kept", got[0].Title)
	}
	if got[0].Text != "Grand Canyon National Park, in Arizona" {
		t.Errorf("snippet = %q", got[0].Text)
	}
}

// An error array alongside a 200 is still a failure, and Kagi's own
// wording is the part that helps whoever reads the log.
func TestKagiErrorArrayOnSuccessStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"meta":{"id":"x"},"data":[],"error":[{"code":1,"msg":"out of credits"}]}`))
	}))
	defer srv.Close()

	_, err := kagiDriver(t, srv).Search(context.Background(), SearchRequest{Query: "q"})
	if err == nil {
		t.Fatal("want an error when the body carries one")
	}
	if !strings.Contains(err.Error(), "out of credits") {
		t.Errorf("err = %v, want the vendor's wording", err)
	}
}

// The scheme token is Kagi's own: neither Bearer nor a bare value.
func TestKagiSendsBotAuthorization(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, err := kagiDriver(t, srv).Search(context.Background(), SearchRequest{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if got != "Bot tok" {
		t.Errorf("Authorization = %q, want %q", got, "Bot tok")
	}
}

func TestKagiSendsQueryAndLimit(t *testing.T) {
	var q, limit string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q, limit = r.URL.Query().Get("q"), r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer srv.Close()

	if _, err := kagiDriver(t, srv).Search(context.Background(),
		SearchRequest{Query: "grand canyon", NumResults: 3}); err != nil {
		t.Fatal(err)
	}
	if q != "grand canyon" || limit != "3" {
		t.Errorf("q=%q limit=%q", q, limit)
	}
}

// The ACL builder and the driver must derive the same host, or the
// first search is a proxy denial that names neither.
func TestKagiEffectiveEndpoint(t *testing.T) {
	if got := KagiEffectiveEndpoint(""); got != DefaultKagiEndpoint {
		t.Errorf("empty = %q, want the default", got)
	}
	if got := KagiEffectiveEndpoint("  https://proxy.internal/search "); got != "https://proxy.internal/search" {
		t.Errorf("configured = %q", got)
	}
}

func TestKagiRequiresCredential(t *testing.T) {
	if _, err := KagiSearchFactory(SearchDriverConfig{}); err == nil {
		t.Error("want an error: Kagi has no anonymous tier")
	}
}

func TestKagiRejectsUnknownOptions(t *testing.T) {
	_, err := KagiSearchFactory(SearchDriverConfig{
		Credential: &StaticCredential{Value: "tok"},
		Options:    map[string]string{"query_param": "q"},
	})
	if err == nil || !strings.Contains(err.Error(), "query_param") {
		t.Errorf("want the offending key named, got %v", err)
	}
}
