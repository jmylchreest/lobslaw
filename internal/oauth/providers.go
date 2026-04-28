package oauth

import (
	"errors"
	"strings"
)

// ProviderConfig declares everything the device-code flow needs to
// authenticate against one IdP. Endpoints follow the OAuth 2.0
// Device Authorization Grant spec (RFC 8628).
//
// Built-in defaults for common IdPs live alongside this type as
// per-provider files (provider_google.go, provider_microsoft.go, ...).
// Custom providers are configured by populating ProviderConfig
// directly from operator TOML.
type ProviderConfig struct {
	// Name is the canonical provider identifier ("google", "github",
	// "microsoft", ...). Used as the credentials bucket prefix.
	Name string

	// DeviceAuthEndpoint accepts the initial device-code request.
	// Returns {device_code, user_code, verification_url, interval,
	// expires_in}.
	DeviceAuthEndpoint string

	// TokenEndpoint exchanges device_code for tokens.
	TokenEndpoint string

	// ClientID is the OAuth app identifier the operator registered
	// with the IdP. Public for device-flow apps (no client_secret
	// required) but still resolved via the secret reference pattern
	// so deployments can rotate without recompiling.
	ClientID string

	// ClientSecret is required by some IdPs even for device flow
	// (Google's "TVs and Limited Input Devices" client type wants
	// one; GitHub's device flow doesn't). Empty when the provider
	// follows the public-client model.
	ClientSecret string

	// Scopes is the default scope set requested when the caller
	// doesn't supply explicit scopes. Operators can override per
	// oauth_start invocation.
	DefaultScopes []string

	// SubjectClaim is the response field that identifies the
	// authenticated user — typically a sub-claim of the ID token
	// or a /userinfo lookup. Used as the "subject" in the
	// credential bucket key. Empty → caller supplies an explicit
	// subject (less common).
	SubjectClaim string

	// UserInfoEndpoint is the URL FetchSubject calls to resolve
	// the authenticated user's identifier post-authorization. The
	// response is parsed as a flat JSON object; SubjectClaim names
	// the field to read out of it. Empty → FetchSubject returns
	// an error and the caller decides whether to fall back to a
	// synthetic key.
	UserInfoEndpoint string
}

// Validate fails on missing required fields. Called at config
// load so a typo doesn't surface mid-flow.
func (p *ProviderConfig) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("oauth: provider name required")
	}
	if strings.TrimSpace(p.DeviceAuthEndpoint) == "" {
		return errors.New("oauth: device_auth_endpoint required")
	}
	if strings.TrimSpace(p.TokenEndpoint) == "" {
		return errors.New("oauth: token_endpoint required")
	}
	if strings.TrimSpace(p.ClientID) == "" {
		return errors.New("oauth: client_id required")
	}
	return nil
}
