package oauth

// Google returns a default ProviderConfig for Google Workspace
// device-flow auth. Operators supply ClientID (and ClientSecret
// for the limited-input-device client type) via config; the
// endpoints are well-known.
func Google() ProviderConfig {
	return ProviderConfig{
		Name:               "google",
		DeviceAuthEndpoint: "https://oauth2.googleapis.com/device/code",
		TokenEndpoint:      "https://oauth2.googleapis.com/token",
		UserInfoEndpoint:   "https://openidconnect.googleapis.com/v1/userinfo",
		DefaultScopes: []string{
			"openid",
			"email",
			"profile",
		},
		SubjectClaim: "email",
	}
}
