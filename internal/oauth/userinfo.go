package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

// FetchSubject calls the provider's UserInfoEndpoint with the just-
// granted access token and pulls the SubjectClaim field out of the
// flat JSON response. Result is the stable identifier used as the
// credentials-bucket subject key (e.g. "user@example.com" for Google,
// "octocat" for GitHub).
//
// HTTP routes through egress.For("oauth/<provider>") so the
// already-installed ACL gates this call to the same hosts as the
// device-auth + token endpoints.
//
// Failures here are non-fatal at the policy layer: the caller can
// fall back to a synthetic subject if the operator hasn't configured
// a UserInfoEndpoint or the IdP rejects the token. The credential
// still persists, just with a less-stable key.
func FetchSubject(ctx context.Context, p ProviderConfig, tok *TokenResponse) (string, error) {
	if p.UserInfoEndpoint == "" {
		return "", errors.New("oauth: UserInfoEndpoint not configured for provider " + p.Name)
	}
	if p.SubjectClaim == "" {
		return "", errors.New("oauth: SubjectClaim not configured for provider " + p.Name)
	}
	if tok == nil || tok.AccessToken == "" {
		return "", errors.New("oauth: cannot fetch subject without an access token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.UserInfoEndpoint, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", bearerHeader(tok))
	req.Header.Set("Accept", "application/json")

	client := egress.For("oauth/" + p.Name).HTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("oauth: userinfo GET: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("oauth: read userinfo body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("oauth: userinfo HTTP %d: %s",
			resp.StatusCode, truncate(body, 256))
	}
	var fields map[string]any
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", fmt.Errorf("oauth: parse userinfo: %w", err)
	}
	raw, ok := fields[p.SubjectClaim]
	if !ok {
		return "", fmt.Errorf("oauth: userinfo missing %q field", p.SubjectClaim)
	}
	switch v := raw.(type) {
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return "", fmt.Errorf("oauth: userinfo %q is empty", p.SubjectClaim)
		}
		return s, nil
	case float64:
		// GitHub user IDs come back as JSON numbers when the
		// SubjectClaim is "id". Fold to the canonical string form.
		return fmt.Sprintf("%d", int64(v)), nil
	default:
		return "", fmt.Errorf("oauth: userinfo %q has unsupported type %T", p.SubjectClaim, raw)
	}
}

// bearerHeader builds the Authorization value using the token's
// declared TokenType when present, falling back to the standard
// "Bearer" prefix. GitHub returns "bearer" (lowercase) which works
// fine; some custom IdPs declare exotic types.
func bearerHeader(tok *TokenResponse) string {
	prefix := strings.TrimSpace(tok.TokenType)
	if prefix == "" {
		prefix = "Bearer"
	}
	return prefix + " " + tok.AccessToken
}
