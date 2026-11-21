// Package antigravity provides OAuth2 authentication functionality for the Antigravity provider.
package antigravity

import (
	"errors"
	"os"
	"strings"
)

// OAuth callback configuration
const (
	CallbackPort = 51121
)

// ErrOAuthClientNotConfigured indicates missing external OAuth registration.
var ErrOAuthClientNotConfigured = errors.New("antigravity OAuth client is not configured: set CPA_ANTIGRAVITY_OAUTH_CLIENT_ID and CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET")

// OAuthClientCredentials reads the operator's external OAuth registration.
// Neither a missing configuration error nor a partial result exposes its values.
func OAuthClientCredentials() (clientID, clientSecret string, err error) {
	clientID = strings.TrimSpace(os.Getenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID"))
	clientSecret = strings.TrimSpace(os.Getenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET"))
	if clientID == "" || clientSecret == "" {
		return "", "", ErrOAuthClientNotConfigured
	}
	return clientID, clientSecret, nil
}

// Scopes defines the OAuth scopes required for Antigravity authentication
var Scopes = []string{
	"https://www.googleapis.com/auth/cloud-platform",
	"https://www.googleapis.com/auth/userinfo.email",
	"https://www.googleapis.com/auth/userinfo.profile",
	"https://www.googleapis.com/auth/aicode",
	"https://www.googleapis.com/auth/cclog",
	"https://www.googleapis.com/auth/experimentsandconfigs",
}

// OAuth2 endpoints for Google authentication
const (
	TokenEndpoint    = "https://oauth2.googleapis.com/token"
	AuthEndpoint     = "https://accounts.google.com/o/oauth2/v2/auth"
	UserInfoEndpoint = "https://www.googleapis.com/oauth2/v2/userinfo?alt=json"
)

// Antigravity API configuration
const (
	APIEndpoint      = "https://cloudcode-pa.googleapis.com"
	DailyAPIEndpoint = "https://daily-cloudcode-pa.googleapis.com"
	APIVersion       = "v1internal"
)
