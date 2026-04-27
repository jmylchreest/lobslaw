package oauth

import (
	"errors"
	"strings"
)

// ProviderConfig declares everything the device-code flow needs to
// authenticate against one IdP. Endpoints follow the OAuth 2.0
// Device Authorization Grant spec (RFC 8628).
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

// Google returns a default ProviderConfig for Google Workspace
// device-flow auth. Operators supply ClientID (and ClientSecret
// for the limited-input-device client type) via config; the
// endpoints are well-known.
func Google() ProviderConfig {
	return ProviderConfig{
		Name:               "google",
		DeviceAuthEndpoint: "https://oauth2.googleapis.com/device/code",
		TokenEndpoint:      "https://oauth2.googleapis.com/token",
		DefaultScopes: []string{
			"openid",
			"email",
			"profile",
		},
		SubjectClaim: "email",
	}
}

// GitHub returns a default ProviderConfig for GitHub device-flow
// auth. ClientID comes from the operator's GitHub OAuth App. No
// client_secret needed for the device flow.
func GitHub() ProviderConfig {
	return ProviderConfig{
		Name:               "github",
		DeviceAuthEndpoint: "https://github.com/login/device/code",
		TokenEndpoint:      "https://github.com/login/oauth/access_token",
		DefaultScopes: []string{
			"read:user",
		},
		SubjectClaim: "login",
	}
}
