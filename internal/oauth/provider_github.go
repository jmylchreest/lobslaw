package oauth

// GitHub returns a default ProviderConfig for GitHub device-flow
// auth. ClientID comes from the operator's GitHub OAuth App. No
// client_secret needed for the device flow.
func GitHub() ProviderConfig {
	return ProviderConfig{
		Name:               "github",
		DeviceAuthEndpoint: "https://github.com/login/device/code",
		TokenEndpoint:      "https://github.com/login/oauth/access_token",
		UserInfoEndpoint:   "https://api.github.com/user",
		DefaultScopes: []string{
			"read:user",
		},
		SubjectClaim: "login",
	}
}
