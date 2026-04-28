package clawhub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

// SkillEntry is the catalog metadata for one publishable skill at
// one version. The clawhub server returns this from
// GET /v1/skills/<name>/<version>; lobslaw uses it to drive the
// download + verify steps before extracting into the skill mount.
type SkillEntry struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`

	// BundleURL points at the tarball containing manifest.yaml +
	// handler files. Hosted on the catalog itself or a CDN; the
	// hostname must be in the egress "clawhub" role's allowlist or
	// the download is rejected.
	BundleURL string `json:"bundle_url"`

	// BundleSHA256 is the hex-encoded digest of the bundle bytes.
	// Verified BEFORE the bundle is unpacked so a corrupted or
	// substituted tarball can't ship malicious content.
	BundleSHA256 string `json:"bundle_sha256"`

	// SignedBy + Signature are the ed25519 signature over the
	// canonical bytes of (Name|Version|BundleSHA256). SignedBy
	// names the publisher's public key; the operator's trusted
	// publishers list determines whether the signature is honoured.
	SignedBy  string `json:"signed_by,omitempty"`
	Signature string `json:"signature,omitempty"`
}

// Client talks to the clawhub.ai catalog. Construct via NewClient.
// Methods return wrapped errors with the request context preserved
// so the caller can surface diagnostics into the audit log.
type Client struct {
	baseURL    string
	httpClient *http.Client
}

// NewClient validates the base URL and constructs the HTTP client
// via egress.For("clawhub"). Empty baseURL returns an error —
// "no clawhub configured" is the operator's call (don't wire this
// package), not a runtime fallback.
func NewClient(baseURL string) (*Client, error) {
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, errors.New("clawhub: base URL required")
	}
	if _, err := url.Parse(baseURL); err != nil {
		return nil, fmt.Errorf("clawhub: invalid base URL %q: %w", baseURL, err)
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		httpClient: egress.For("clawhub").HTTPClient(),
	}, nil
}

// GetSkill fetches the catalog metadata for one (name, version).
// Returns an error when name or version contains characters that
// would break URL path interpolation — guards against an injected
// "../" trying to escape the /v1/skills/ namespace.
func (c *Client) GetSkill(ctx context.Context, name, version string) (*SkillEntry, error) {
	if err := validateSkillIdentifier(name); err != nil {
		return nil, fmt.Errorf("clawhub: %w", err)
	}
	if err := validateSkillIdentifier(version); err != nil {
		return nil, fmt.Errorf("clawhub: %w", err)
	}
	endpoint := c.baseURL + "/v1/skills/" + url.PathEscape(name) + "/" + url.PathEscape(version)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clawhub: GET %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("clawhub: read response: %w", err)
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("clawhub: skill %q version %q not in catalog", name, version)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("clawhub: GET %s HTTP %d: %s",
			endpoint, resp.StatusCode, truncate(body, 256))
	}
	var entry SkillEntry
	if err := json.Unmarshal(body, &entry); err != nil {
		return nil, fmt.Errorf("clawhub: parse response: %w", err)
	}
	if entry.Name == "" || entry.Version == "" || entry.BundleURL == "" || entry.BundleSHA256 == "" {
		return nil, errors.New("clawhub: catalog response missing required fields (name, version, bundle_url, bundle_sha256)")
	}
	return &entry, nil
}

// DownloadBundle fetches the bundle tarball at entry.BundleURL.
// Caller is responsible for verifying entry.BundleSHA256 against
// the streamed bytes (see install.go's hashing reader). Returns the
// response body so the caller can stream-decompress without
// buffering the full bundle in memory.
//
// HTTP routes through the same "clawhub" egress role — the role's
// allowlist must include the bundle host (default for github-hosted
// bundles is configured via [security.clawhub_binary_hosts]).
func (c *Client) DownloadBundle(ctx context.Context, entry *SkillEntry) (io.ReadCloser, error) {
	if entry == nil {
		return nil, errors.New("clawhub: nil entry")
	}
	if entry.BundleURL == "" {
		return nil, errors.New("clawhub: entry has no bundle_url")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, entry.BundleURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clawhub: GET bundle %s: %w", entry.BundleURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("clawhub: GET bundle %s HTTP %d", entry.BundleURL, resp.StatusCode)
	}
	return resp.Body, nil
}

// DownloadBundleBySlug fetches a clawhub.ai-style bundle directly
// from the catalogue API at <baseURL>/api/v1/download?slug=<slug>.
// Slug is the bare skill name (e.g. "gog") — clawhub.ai displays
// skills under <owner>/<name> URLs but the download API takes only
// the name. Owner prefixes are stripped here for operators who paste
// the full page URL slug ("steipete/gog") instead of the API slug.
//
// Caller (typically Installer.InstallBySlug) is responsible for
// ProcessBundle on the returned bytes.
func (c *Client) DownloadBundleBySlug(ctx context.Context, slug string) (io.ReadCloser, error) {
	apiSlug, err := normalizeSlug(slug)
	if err != nil {
		return nil, err
	}
	endpoint := c.baseURL + "/api/v1/download?slug=" + url.QueryEscape(apiSlug)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("clawhub: GET %s: %w", endpoint, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("clawhub: GET %s HTTP %d", endpoint, resp.StatusCode)
	}
	return resp.Body, nil
}

// normalizeSlug accepts either a bare skill name ("gog") or an
// "owner/name" pair as it appears in clawhub.ai URLs ("steipete/gog")
// and returns just the API-shaped slug (the name component).
func normalizeSlug(slug string) (string, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		return "", errors.New("clawhub: slug required")
	}
	if strings.Contains(slug, "/") {
		parts := strings.SplitN(slug, "/", 2)
		if len(parts) != 2 || parts[1] == "" {
			return "", fmt.Errorf("clawhub: slug %q has empty name component", slug)
		}
		slug = parts[1]
	}
	if err := validateSkillIdentifier(slug); err != nil {
		return "", fmt.Errorf("clawhub: slug %w", err)
	}
	return slug, nil
}

// validateSkillIdentifier guards against path-traversal-shaped
// inputs in (name, version). Catalogs use these in URL paths;
// rejecting "/" and ".." here means a malformed catalog entry
// can't make us fetch the wrong endpoint.
func validateSkillIdentifier(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return errors.New("identifier required")
	}
	if strings.ContainsAny(s, "/\\") || strings.Contains(s, "..") {
		return fmt.Errorf("identifier %q contains forbidden characters", s)
	}
	return nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
