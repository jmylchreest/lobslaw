package oauth

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

// ErrInvalidGrant is returned when the IdP rejects the refresh token
// (revoked, expired, or never issued for refresh). Terminal — the
// operator must re-run oauth_start to mint a new refresh token.
var ErrInvalidGrant = errors.New("oauth: invalid_grant")

// RefreshToken trades a refresh_token for a fresh access_token. RFC
// 6749 §6 — the response shape matches the device-flow token response,
// so we reuse TokenResponse. Some IdPs (Google) return a refresh_token
// in the response only when the original was rotation-eligible; most
// keep the existing one valid and omit it from the response (which
// callers must handle by preserving the prior refresh_token).
//
// HTTP routes through egress.For("oauth/<provider>") so the egress
// ACL gates the call to the same hosts as the device-auth + token
// endpoints.
func RefreshToken(ctx context.Context, p ProviderConfig, refreshToken string) (*TokenResponse, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(refreshToken) == "" {
		return nil, errors.New("oauth: refresh_token required")
	}
	form := url.Values{}
	form.Set("client_id", p.ClientID)
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := egress.For("oauth/" + p.Name).HTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: refresh POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: read refresh body: %w", err)
	}
	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("oauth: parse refresh response: %w", err)
	}
	switch out.Error {
	case "":
		// Success path — fall through.
	case "invalid_grant":
		return nil, ErrInvalidGrant
	default:
		return nil, fmt.Errorf("oauth: refresh error %q: %s", out.Error, out.ErrorDescription)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: refresh HTTP %d: %s",
			resp.StatusCode, truncate(body, 256))
	}
	if out.AccessToken == "" {
		return nil, errors.New("oauth: refresh response missing access_token")
	}
	return &out, nil
}
