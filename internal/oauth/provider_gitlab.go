package oauth

// GitLab returns a default ProviderConfig for gitlab.com device-flow
// auth. Self-hosted GitLab instances override DeviceAuthEndpoint +
// TokenEndpoint + UserInfoEndpoint with their domain. Device flow
// support landed in GitLab v15.
func GitLab() ProviderConfig {
	return ProviderConfig{
		Name:               "gitlab",
		DeviceAuthEndpoint: "https://gitlab.com/oauth/authorize_device",
		TokenEndpoint:      "https://gitlab.com/oauth/token",
		UserInfoEndpoint:   "https://gitlab.com/oauth/userinfo",
		DefaultScopes: []string{
			"openid",
			"email",
			"profile",
			"read_user",
		},
		SubjectClaim: "email",
	}
}
