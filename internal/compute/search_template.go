package compute

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// The declarative search driver.
//
// docs/dev/PROVIDERS.md argued for this shape and deferred it: a
// driver whose whole behaviour is "endpoint, auth style, and
// request/response field paths in TOML, interpreted by one compiled
// driver", covering the long tail with no code execution at all —
// nothing to sign, sandbox, spawn or get wrong.
//
// Search is where it earns its keep first. Brave, Tavily, Serper,
// Google CSE and every corporate search proxy differ only in which
// parameter holds the query and which JSON path holds the results.
// Writing a Go file per engine would be a rebuild and a release for
// what is, honestly, a field mapping.
//
// The line this does not cross: it describes ONE request and ONE
// response. A backend needing a second call, a signature, or a
// pagination loop is real behaviour and gets a real driver. Growing
// this into a scripting language would recreate the problem it exists
// to avoid.

// templateResponseFields are the result fields a mapping may name,
// with the defaults applied when it does not. The defaults describe
// the most common shape (a top-level `results` array of
// {title,url,content}), so a well-behaved API needs almost no mapping.
var templateResponseFields = map[string]string{
	"results":      "results",
	"title":        "title",
	"url":          "url",
	"snippet":      "content",
	"published_at": "publishedDate",
	"score":        "score",
}

var templateOptionKeys = []string{
	"method", "query_param", "count_param", "depth_param",
	"auth_style", "auth_name", "auth_prefix",
}

// TemplateSearchFactory builds a search driver entirely from config.
func TemplateSearchFactory(cfg SearchDriverConfig) (SearchDriver, error) {
	if bad := unknownOptions(cfg.Options, templateOptionKeys...); len(bad) > 0 {
		return nil, fmt.Errorf("template search: unknown option(s) %v; supported: %s",
			bad, strings.Join(templateOptionKeys, ", "))
	}
	if bad := unknownOptions(cfg.Response, slices.Sorted(maps.Keys(templateResponseFields))...); len(bad) > 0 {
		return nil, fmt.Errorf("template search: unknown response field(s) %v; supported: %s",
			bad, strings.Join(slices.Sorted(maps.Keys(templateResponseFields)), ", "))
	}
	if strings.TrimSpace(cfg.Endpoint) == "" {
		return nil, errors.New("template search: endpoint required")
	}
	if _, err := url.Parse(cfg.Endpoint); err != nil {
		return nil, fmt.Errorf("template search: endpoint %q: %w", cfg.Endpoint, err)
	}

	method := strings.ToUpper(option(cfg.Options, "method"))
	switch method {
	case "":
		method = http.MethodGet
	case http.MethodGet, http.MethodPost:
	default:
		return nil, fmt.Errorf("template search: method %q not supported; use GET or POST", method)
	}

	// The credential is built here rather than by the wiring layer,
	// because auth_style is the operator's description of THIS API and
	// the wiring layer has no way to know it.
	cred, err := templateCredential(cfg)
	if err != nil {
		return nil, err
	}

	queryParam := option(cfg.Options, "query_param")
	if queryParam == "" {
		queryParam = DefaultSearchQueryParam
	}
	fields := make(map[string]string, len(templateResponseFields))
	for k, def := range templateResponseFields {
		if v := option(cfg.Response, k); v != "" {
			fields[k] = v
		} else {
			fields[k] = def
		}
	}

	countParam := option(cfg.Options, "count_param")
	depthParam := option(cfg.Options, "depth_param")
	// extra_params are merged into the same map as the query, so a key
	// that collides silently overwrites what lobslaw put there —
	// extra_params = { q = "..." } against query_param = "q" would
	// replace the user's search with a constant and return plausible
	// results for the wrong question. Refused at boot instead.
	for _, reserved := range []struct{ name, key string }{
		{"query_param", queryParam},
		{"count_param", countParam},
		{"depth_param", depthParam},
	} {
		if reserved.key == "" {
			continue
		}
		if _, clash := cfg.ExtraParams[reserved.key]; clash {
			return nil, fmt.Errorf(
				"template search: extra_params sets %q, which is also %s; it would overwrite the value lobslaw supplies",
				reserved.key, reserved.name)
		}
	}

	return &templateSearchDriver{
		endpoint:    cfg.Endpoint,
		method:      method,
		cred:        cred,
		queryParam:  queryParam,
		countParam:  countParam,
		depthParam:  depthParam,
		extraParams: cfg.ExtraParams,
		fields:      fields,
		client:      searchHTTPClient(cfg.HTTPClient),
	}, nil
}

// templateCredential turns auth_style into one of the credentials that
// already exist. No new auth code: header/bearer are StaticCredential,
// query is QueryCredential, none is nil.
func templateCredential(cfg SearchDriverConfig) (Credential, error) {
	style := strings.ToLower(option(cfg.Options, "auth_style"))
	name := option(cfg.Options, "auth_name")
	// auth_prefix carries a scheme the built-in styles do not name —
	// Kagi's `Authorization: Bot <token>`, say.
	//
	// The separator is inferred rather than typed. An auth-scheme is
	// followed by a space (RFC 7235: "Bot ", "Token "), while a
	// parameter-style prefix is not ("key="), and the two are told
	// apart by whether the prefix ends in an alphanumeric. Requiring
	// the operator to write the space instead would hang correctness on
	// trailing whitespace surviving TOML, an editor and option()'s own
	// TrimSpace — and its loss shows up as "Bottok" in a header nobody
	// prints.
	prefix := authPrefixWithSeparator(option(cfg.Options, "auth_prefix"))
	if style == "" {
		// An operator who supplied a key wants it sent; bearer is the
		// overwhelmingly common shape. One who supplied none means none.
		if cfg.Credential == nil {
			style = "none"
		} else {
			style = "bearer"
		}
	}
	if style != "none" && cfg.Credential == nil {
		return nil, fmt.Errorf("template search: auth_style %q needs api_key_ref", style)
	}
	// The wiring layer hands over a bearer credential holding the
	// resolved secret; every style re-wraps the same value.
	secret, ok := credentialSecret(cfg.Credential)
	if style != "none" && !ok {
		// Only reachable if a caller supplies a credential shape this
		// cannot unwrap (a SigV4 signer, say). Failing at boot rather
		// than re-wrapping an empty secret, which would authenticate
		// with nothing and surface as a 401 from the search API.
		return nil, fmt.Errorf("template search: credential of type %T cannot be re-wrapped as auth_style %q", cfg.Credential, style)
	}

	switch style {
	case "none":
		if prefix != "" {
			return nil, errors.New(`template search: auth_prefix is set but auth_style = "none" sends no credential`)
		}
		return nil, nil
	case "bearer":
		if prefix != "" {
			return nil, errors.New(`template search: auth_style = "bearer" already sends "Bearer "; ` +
				`for a different scheme use auth_style = "header" with auth_name = "Authorization" and auth_prefix`)
		}
		return NewBearerCredential(secret), nil
	case "header":
		if name == "" {
			return nil, errors.New(`template search: auth_style = "header" needs auth_name (e.g. auth_name = "X-Subscription-Token")`)
		}
		return NewHeaderCredential(name, prefix+secret), nil
	case "query":
		if name == "" {
			return nil, errors.New(`template search: auth_style = "query" needs auth_name (e.g. auth_name = "key")`)
		}
		return NewQueryCredential(name, prefix+secret), nil
	default:
		return nil, fmt.Errorf(`template search: auth_style %q not supported; use header, bearer, query or none`, style)
	}
}

// credentialSecret unwraps the resolved secret. The bool distinguishes
// "no credential at all" (fine, auth_style none) from "a credential
// this cannot read" (a configuration error worth failing on).
func credentialSecret(c Credential) (string, bool) {
	if c == nil {
		return "", false
	}
	if sc, ok := c.(*StaticCredential); ok {
		return sc.Value, true
	}
	return "", false
}

type templateSearchDriver struct {
	endpoint    string
	method      string
	cred        Credential
	queryParam  string
	countParam  string
	depthParam  string
	extraParams map[string]string
	fields      map[string]string
	client      *http.Client
}

func (d *templateSearchDriver) Search(ctx context.Context, req SearchRequest) ([]SearchResult, error) {
	// any rather than string, because the result count has to survive
	// as a JSON number in a POST body. Tavily's max_results and its
	// peers are typed integers and reject "3" — and a driver that
	// silently sends the wrong type fails at the API, a layer where
	// the operator has no way to see which field did it.
	params := map[string]any{d.queryParam: req.Query}
	for k, v := range d.extraParams {
		params[k] = v
	}
	if d.countParam != "" && req.NumResults > 0 {
		params[d.countParam] = req.NumResults
	}
	if d.depthParam != "" && req.Depth != "" {
		params[d.depthParam] = req.Depth
	}

	httpReq, err := d.buildRequest(ctx, params)
	if err != nil {
		return nil, Permanent(err)
	}
	httpReq.Header.Set("Accept", "application/json")
	if d.cred != nil {
		if err := d.cred.Apply(ctx, httpReq); err != nil {
			return nil, CredentialRejected(err)
		}
	}

	resp, err := d.client.Do(httpReq)
	if err != nil {
		return nil, Transient(fmt.Errorf("template search: http: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, &DriverError{
			Class: ClassifyHTTPStatus(resp.StatusCode, string(raw)),
			Err:   fmt.Errorf("template search: HTTP %d: %s", resp.StatusCode, TruncateBodyFor(raw, 512)),
		}
	}

	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return nil, Permanent(fmt.Errorf("template search: decode: %w", err))
	}
	items, ok := dotPath(decoded, d.fields["results"]).([]any)
	if !ok {
		// Naming the path is the whole point: the operator wrote it,
		// and "results is not an array at web.results" tells them
		// exactly which line of TOML to look at.
		return nil, Permanent(fmt.Errorf(
			"template search: no result array at response path %q; check the mapping against the API's JSON",
			d.fields["results"]))
	}
	out := make([]SearchResult, 0, len(items))
	for _, item := range items {
		out = append(out, SearchResult{
			Title:         dotString(item, d.fields["title"]),
			URL:           dotString(item, d.fields["url"]),
			Text:          dotString(item, d.fields["snippet"]),
			PublishedDate: dotString(item, d.fields["published_at"]),
			Score:         dotFloat(item, d.fields["score"]),
		})
	}
	return out, nil
}

func (d *templateSearchDriver) buildRequest(ctx context.Context, params map[string]any) (*http.Request, error) {
	if d.method == http.MethodPost {
		body, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, d.endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Content-Type", "application/json")
		return httpReq, nil
	}
	u, err := url.Parse(d.endpoint)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	// A query string has no types, so everything flattens back to text
	// here — the distinction only ever mattered for the JSON body.
	for k, v := range params {
		q.Set(k, paramString(v))
	}
	u.RawQuery = q.Encode()
	return http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
}

func paramString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case int:
		return strconv.Itoa(t)
	default:
		return fmt.Sprint(t)
	}
}

// dotPath walks a decoded JSON value by a dotted path ("web.results").
// An empty path returns the value itself; anything unresolvable
// returns nil, because a missing optional field is normal and every
// caller treats nil as "absent".
func dotPath(v any, path string) any {
	if path == "" {
		return v
	}
	for seg := range strings.SplitSeq(path, ".") {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v, ok = m[seg]
		if !ok {
			return nil
		}
	}
	return v
}

// dotString coerces rather than type-asserting. Real APIs put numbers
// where a string is expected (page_age, timestamps, integer scores),
// and refusing them would fail a whole search over a field the model
// barely reads.
func dotString(v any, path string) string {
	switch t := dotPath(v, path).(type) {
	case nil:
		return ""
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case bool:
		return strconv.FormatBool(t)
	default:
		return ""
	}
}

func dotFloat(v any, path string) float64 {
	switch t := dotPath(v, path).(type) {
	case float64:
		return t
	case string:
		f, err := strconv.ParseFloat(t, 64)
		if err != nil {
			return 0
		}
		return f
	default:
		return 0
	}
}

// authPrefixWithSeparator appends the space an auth-scheme takes,
// leaving a parameter-style prefix alone. See templateCredential.
func authPrefixWithSeparator(prefix string) string {
	if prefix == "" {
		return ""
	}
	last := rune(prefix[len(prefix)-1])
	alnum := (last >= 'a' && last <= 'z') || (last >= 'A' && last <= 'Z') || (last >= '0' && last <= '9')
	if alnum {
		return prefix + " "
	}
	return prefix
}
