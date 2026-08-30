package compute

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Kagi's search API.
//
// A compiled driver rather than a template mapping, for two reasons
// the declarative interpreter cannot express:
//
//   - `data` is a HETEROGENEOUS array. Every item carries a type
//     discriminator `t`, where 0 is a search result and 1 is a block of
//     related searches. A field mapping that walked `data` would hand
//     the model Kagi's "people also searched for" list as though those
//     were results, with no url and no snippet.
//   - Snippets contain HTML. Kagi marks query terms with <b> tags, and
//     a model reading raw markup in what is supposed to be prose will
//     reproduce it in its reply.
//
// Everything else about Kagi is ordinary — one GET, one JSON body —
// which is exactly why the discriminator is worth naming: it is the
// only thing standing between this and a template config.
//
// v0, not v1. Kagi's own docs present v1 as current and v0 as legacy,
// but a v0 key authenticates only against v0: v1 answers the same
// credential with 401 and an envelope of a different shape entirely
// (`errors` of {code,url,message} rather than `error` of {code,msg}).
// Verified against the live API. An operator on a v1 key sets
// `endpoint` and, today, would also need the v1 response shape — which
// is a second driver, not a config knob.

// DefaultKagiEndpoint is Kagi's v0 search API.
const DefaultKagiEndpoint = "https://kagi.com/api/v0/search"

// kagiResultType is the value of `t` that marks an actual search
// result. Other values are companion blocks (1 is related searches),
// which share the array and nothing else.
const kagiResultType = 0

// KagiEffectiveEndpoint resolves the URL Kagi will actually be called
// at: configured, else the default.
//
// Exported for the same reason Exa's is — the egress ACL builder must
// derive the SAME host the driver dials. A provider that configures no
// endpoint would otherwise contribute no host to the web_search role,
// and the first search would be refused by the proxy rather than by
// anything that could explain itself.
func KagiEffectiveEndpoint(configured string) string {
	if e := strings.TrimSpace(configured); e != "" {
		return e
	}
	return DefaultKagiEndpoint
}

// KagiSearchFactory builds the Kagi driver.
func KagiSearchFactory(cfg SearchDriverConfig) (SearchDriver, error) {
	if bad := unknownOptions(cfg.Options); len(bad) > 0 {
		return nil, fmt.Errorf("kagi search: unknown option(s) %v; the kagi driver takes none", bad)
	}
	if cfg.Credential == nil {
		return nil, errors.New("kagi search: api_key_ref required (Kagi has no anonymous tier)")
	}
	// Kagi authenticates with `Authorization: Bot <token>` — a scheme
	// token that is neither Bearer nor a bare header value, so the
	// credential is built here rather than taken as handed over.
	secret, ok := credentialSecret(cfg.Credential)
	if !ok {
		return nil, fmt.Errorf("kagi search: credential of type %T cannot be read", cfg.Credential)
	}
	return &kagiSearchDriver{
		endpoint: KagiEffectiveEndpoint(cfg.Endpoint),
		cred:     NewHeaderCredential("Authorization", "Bot "+secret),
		client:   searchHTTPClient(cfg.HTTPClient),
	}, nil
}

type kagiSearchDriver struct {
	endpoint string
	cred     Credential
	client   *http.Client
}

// kagiResponse is the envelope every Kagi endpoint returns. `error` is
// an ARRAY and is present alongside a 200 on some failures, so it is
// checked rather than inferred from the status code.
type kagiResponse struct {
	Meta struct {
		ID string `json:"id"`
		MS int    `json:"ms"`
	} `json:"meta"`
	Data []struct {
		T         int    `json:"t"`
		URL       string `json:"url"`
		Title     string `json:"title"`
		Snippet   string `json:"snippet"`
		Published string `json:"published"`
	} `json:"data"`
	Error []struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
	} `json:"error"`
}

func (d *kagiSearchDriver) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return nil, Permanent(fmt.Errorf("kagi search: endpoint %q: %w", d.endpoint, err))
	}
	q := u.Query()
	q.Set("q", req.Query)
	if req.NumResults > 0 {
		q.Set("limit", strconv.Itoa(req.NumResults))
	}
	u.RawQuery = q.Encode()

	// Depth is ignored, deliberately. Kagi has no fast/deep dimension,
	// and refusing an argument the tool schema advertises would make
	// the model's own tool list a source of errors.
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, Permanent(fmt.Errorf("kagi search: build request: %w", err))
	}
	httpReq.Header.Set("Accept", "application/json")
	if err := d.cred.Apply(ctx, httpReq); err != nil {
		return nil, Permanent(fmt.Errorf("kagi search: apply credential: %w", err))
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		if ctx.Err() != nil {
			return nil, Permanent(fmt.Errorf("kagi search: %w", ctx.Err()))
		}
		return nil, Transient(fmt.Errorf("kagi search: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	body, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, Transient(fmt.Errorf("kagi search: read: %w", readErr))
	}
	if resp.StatusCode >= 400 {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(body)),
			Err: fmt.Errorf("kagi search: HTTP %d: %s",
				resp.StatusCode, TruncateBodyFor(body, 512)),
		}
	}

	var out kagiResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, Permanent(fmt.Errorf("kagi search: malformed response: %w", err))
	}
	// An error array on a 2xx is still a failure. Carried through with
	// Kagi's own wording, which is the part that helps.
	if len(out.Error) > 0 {
		msgs := make([]string, 0, len(out.Error))
		for _, e := range out.Error {
			msgs = append(msgs, fmt.Sprintf("%d: %s", e.Code, e.Msg))
		}
		return nil, Permanent(fmt.Errorf("kagi search: %s", strings.Join(msgs, "; ")))
	}

	results := make([]SearchResult, 0, len(out.Data))
	for _, item := range out.Data {
		if item.T != kagiResultType {
			continue
		}
		// A result with no URL is not addressable and is no use to the
		// model, whatever else it carries.
		if strings.TrimSpace(item.URL) == "" {
			continue
		}
		results = append(results, SearchResult{
			Title:         stripKagiMarkup(item.Title),
			URL:           item.URL,
			Text:          stripKagiMarkup(item.Snippet),
			PublishedDate: item.Published,
		})
	}
	return results, nil
}

// stripKagiMarkup removes the <b> emphasis Kagi wraps matched query
// terms in.
//
// Only tags are removed; the text between them is kept, because that
// text is the match and dropping it would gut the snippet. Deliberately
// a scanner rather than a regexp or an HTML parser: the input is one
// short snippet on the hot path of an interactive search, and the
// vocabulary is a handful of emphasis tags.
func stripKagiMarkup(s string) string {
	if !strings.ContainsRune(s, '<') {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	depth := 0
	for _, r := range s {
		switch {
		case r == '<':
			depth++
		case r == '>' && depth > 0:
			depth--
		case depth == 0:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}
