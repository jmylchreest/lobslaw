// Package oauth implements OAuth 2.0 Device Authorization Grant
// (RFC 8628) flows. The device flow is the right shape for lobslaw
// because it requires no callback URL — the operator authorizes in
// any browser by entering a short user_code, while the lobslaw
// process polls the provider's token endpoint for completion.
//
// Provider configs live in internal/oauth/providers.go: Google and
// GitHub today, more land as operators need them. Each config
// carries the device-authorization endpoint, the token endpoint,
// and the operator-supplied client_id.
//
// HTTP traffic routes through internal/egress.For("oauth/<provider>")
// so the egress ACL gates lookup at the IdP host. ACLs for these
// routes are configured automatically by the egress builder when
// the operator's [security.oauth.<provider>] config block is
// non-empty.
//
// The package itself is provider-agnostic and credential-agnostic —
// callers (the oauth_start builtin in Phase C.2b) wire it to the
// CredentialService that persists the resulting refresh token.
package oauth
