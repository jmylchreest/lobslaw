package oauth

// Microsoft returns a default ProviderConfig for Microsoft Entra ID
// (formerly Azure AD) device-flow auth. The "common" tenant accepts
// any Microsoft account (work/school/personal); operators with a
// single-tenant app override DeviceAuthEndpoint + TokenEndpoint with
// their tenant ID. offline_access is part of DefaultScopes because
// without it Microsoft refuses to issue a refresh token, which
// would break credential rotation.
func Microsoft() ProviderConfig {
	return ProviderConfig{
		Name:               "microsoft",
		DeviceAuthEndpoint: "https://login.microsoftonline.com/common/oauth2/v2.0/devicecode",
		TokenEndpoint:      "https://login.microsoftonline.com/common/oauth2/v2.0/token",
		UserInfoEndpoint:   "https://graph.microsoft.com/oidc/userinfo",
		DefaultScopes: []string{
			"openid",
			"email",
			"profile",
			"offline_access",
			"User.Read",
		},
		SubjectClaim: "email",
	}
}
