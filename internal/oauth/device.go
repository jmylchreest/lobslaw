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
	"time"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

// DeviceAuthResponse is the IdP's reply to the initial device-code
// request. Field names match RFC 8628 + the Google/GitHub
// extensions (`verification_uri` vs `verification_url`).
type DeviceAuthResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"` // Google quirk
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// VerificationLink returns whichever of VerificationURI /
// VerificationURL the provider populated. RFC 8628 prescribes
// "verification_uri"; Google's older docs use "verification_url".
// Callers display this to the user.
func (r *DeviceAuthResponse) VerificationLink() string {
	if r.VerificationURI != "" {
		return r.VerificationURI
	}
	return r.VerificationURL
}

// TokenResponse is the IdP's reply once the user has authorized.
// Until that happens, the IdP returns an Error of "authorization_pending"
// (we keep polling) or "slow_down" (we increase the polling interval).
// "expired_token" or "access_denied" are terminal failures.
type TokenResponse struct {
	AccessToken      string `json:"access_token,omitempty"`
	RefreshToken     string `json:"refresh_token,omitempty"`
	Scope            string `json:"scope,omitempty"`
	ExpiresIn        int    `json:"expires_in,omitempty"`
	TokenType        string `json:"token_type,omitempty"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// Sentinel errors callers can branch on. Don't compare strings —
// errors.Is against these instead.
var (
	// ErrAuthorizationPending is the expected non-terminal state
	// while we're polling and the user hasn't approved yet.
	ErrAuthorizationPending = errors.New("oauth: authorization_pending")

	// ErrSlowDown asks us to increase the polling interval. RFC 8628
	// §3.5 — multiply the interval by 2 (or by 5, providers vary).
	ErrSlowDown = errors.New("oauth: slow_down")

	// ErrExpiredToken is terminal: the device_code has expired and
	// the user must restart the flow.
	ErrExpiredToken = errors.New("oauth: expired_token")

	// ErrAccessDenied is terminal: the user explicitly rejected
	// the authorization in their browser.
	ErrAccessDenied = errors.New("oauth: access_denied")
)

// StartDeviceAuth fires the initial request to the IdP's device-
// authorization endpoint. Returns the response containing the
// user_code the operator needs to enter at VerificationLink().
//
// HTTP routes through egress.For("oauth/<provider-name>") so the
// proxy ACL gates the call to just that provider's endpoint.
func StartDeviceAuth(ctx context.Context, p ProviderConfig, scopes []string) (*DeviceAuthResponse, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	if len(scopes) == 0 {
		scopes = p.DefaultScopes
	}
	form := url.Values{}
	form.Set("client_id", p.ClientID)
	if len(scopes) > 0 {
		form.Set("scope", strings.Join(scopes, " "))
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		p.DeviceAuthEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	client := egress.For("oauth/" + p.Name).HTTPClient()
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("oauth: device auth POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: read device auth body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: device auth HTTP %d: %s",
			resp.StatusCode, truncate(body, 256))
	}
	var out DeviceAuthResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("oauth: parse device auth response: %w", err)
	}
	if out.DeviceCode == "" || out.UserCode == "" {
		return nil, errors.New("oauth: device auth response missing required fields")
	}
	if out.Interval <= 0 {
		// RFC 8628 §3.2: SHOULD default to 5 seconds when absent.
		out.Interval = 5
	}
	return &out, nil
}

// PollToken sends one token-endpoint exchange attempt with the
// device_code. Returns ErrAuthorizationPending when the user
// hasn't approved yet — callers loop on this. Returns ErrSlowDown
// when the provider asks us to back off.
func PollToken(ctx context.Context, p ProviderConfig, deviceCode string) (*TokenResponse, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	form := url.Values{}
	form.Set("client_id", p.ClientID)
	if p.ClientSecret != "" {
		form.Set("client_secret", p.ClientSecret)
	}
	form.Set("device_code", deviceCode)
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

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
		return nil, fmt.Errorf("oauth: token POST: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("oauth: read token body: %w", err)
	}
	var out TokenResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("oauth: parse token response: %w", err)
	}
	switch out.Error {
	case "":
		// Success path — fall through.
	case "authorization_pending":
		return nil, ErrAuthorizationPending
	case "slow_down":
		return nil, ErrSlowDown
	case "expired_token":
		return nil, ErrExpiredToken
	case "access_denied":
		return nil, ErrAccessDenied
	default:
		return nil, fmt.Errorf("oauth: token error %q: %s", out.Error, out.ErrorDescription)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("oauth: token HTTP %d: %s",
			resp.StatusCode, truncate(body, 256))
	}
	if out.AccessToken == "" {
		return nil, errors.New("oauth: token response missing access_token")
	}
	return &out, nil
}

// PollUntilGrant runs the polling loop until the user authorises,
// the device_code expires, or ctx cancels. Implements the full
// RFC 8628 §3.5 backoff: starts at the interval the IdP returned,
// doubles on slow_down, surrenders on expired_token / access_denied.
//
// The loop respects ctx — callers cancel via the context to abort
// in-flight flows (e.g. operator types "cancel oauth" mid-poll).
func PollUntilGrant(ctx context.Context, p ProviderConfig, da *DeviceAuthResponse) (*TokenResponse, error) {
	interval := time.Duration(da.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(da.ExpiresIn) * time.Second)
	if da.ExpiresIn <= 0 {
		// Most providers cap device-code lifetime at 30 min; use
		// that as a fallback when the response omitted expires_in.
		deadline = time.Now().Add(30 * time.Minute)
	}

	for {
		if time.Now().After(deadline) {
			return nil, ErrExpiredToken
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(interval):
		}

		tok, err := PollToken(ctx, p, da.DeviceCode)
		if err == nil {
			return tok, nil
		}
		switch {
		case errors.Is(err, ErrAuthorizationPending):
			// Keep polling; interval unchanged.
		case errors.Is(err, ErrSlowDown):
			interval *= 2
		case errors.Is(err, ErrExpiredToken),
			errors.Is(err, ErrAccessDenied):
			return nil, err
		default:
			return nil, err
		}
	}
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
