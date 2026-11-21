package oauthflow

// Public BPS OAuth protocol settings from bps-local-public's browser/device
// flows. The user authenticates and authorizes access on the issuer's website.
// These settings contain no client secret or account credentials.
const (
	BPSClientID     = "app_fnr0pYvVwwFDocDumLG3H2Bp"
	BPSRedirectURI  = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"
	BPSResponsesURL = "https://bps.openai.com/basispoints/api/responses"
)

func BPSConfig() Config {
	return Config{
		AuthorizationURL: "https://auth.openai.com/oauth/authorize",
		TokenURL:         "https://auth.openai.com/oauth/token?unified=true",
		RedirectURI:      BPSRedirectURI,
		ClientID:         BPSClientID,
		Scopes:           []string{"openid", "offline_access", "email", "profile", "organization.read"},
	}
}
