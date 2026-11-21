package oauthflow

import (
	"errors"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Config describes OAuth endpoints and the credentials of a registered client.
// An optional client secret is loaded via the named environment variable and
// sent using the client_secret_basic token endpoint authentication method.
type Config struct {
	AuthorizationURL string   `yaml:"authorization_url"`
	TokenURL         string   `yaml:"token_url"`
	RedirectURI      string   `yaml:"redirect_uri"`
	ClientID         string   `yaml:"client_id"`
	Scopes           []string `yaml:"scopes"`
	ClientSecretEnv  string   `yaml:"client_secret_env"`
	ProxyURL         string   `yaml:"proxy_url"`
	// ResourceOrigin binds pending sessions to the caller's resource configuration.
	ResourceOrigin string `yaml:"-"`
}

func (c Config) Validate() error {
	if c.ProxyURL != "" {
		proxy, err := url.Parse(c.ProxyURL)
		if err != nil || proxy.Hostname() == "" || proxy.Opaque != "" || proxy.RawQuery != "" ||
			proxy.ForceQuery || strings.ContainsAny(c.ProxyURL, "#\r\n") ||
			(proxy.Path != "" && proxy.Path != "/") ||
			(proxy.Scheme != "http" && proxy.Scheme != "https" && proxy.Scheme != "socks5" && proxy.Scheme != "socks5h") {
			return errors.New("oauth proxy_url is invalid")
		}
		if port := proxy.Port(); port != "" {
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return errors.New("oauth proxy port is invalid")
			}
		}
	}
	if strings.TrimSpace(c.ClientID) == "" || len(c.ClientID) > 1024 || !printable(c.ClientID) {
		return errors.New("oauth client_id is missing or invalid")
	}
	if c.ClientSecretEnv != "" && !validEnvironmentName(c.ClientSecretEnv) {
		return errors.New("oauth client_secret_env is not a valid environment variable name")
	}
	for _, scope := range c.Scopes {
		if scope == "" || !printable(scope) || strings.ContainsAny(scope, " \"\\") {
			return errors.New("oauth scopes must contain individual OAuth scope tokens")
		}
	}
	if !validEndpoint(c.AuthorizationURL, "client_id", "response_type", "redirect_uri", "scope", "state", "code_challenge", "code_challenge_method", "response_mode") {
		return errors.New("oauth authorization_url is missing or invalid")
	}
	if !validEndpoint(c.TokenURL, "grant_type", "client_id", "client_secret", "code", "code_verifier", "refresh_token", "redirect_uri") {
		return errors.New("oauth token_url is missing or invalid")
	}
	if !validEndpoint(c.RedirectURI, "code", "state", "error", "error_description", "error_uri") {
		return errors.New("oauth redirect_uri is missing or invalid")
	}
	return nil
}

func sameConfig(a, b Config) bool {
	return a.AuthorizationURL == b.AuthorizationURL && a.TokenURL == b.TokenURL &&
		a.RedirectURI == b.RedirectURI && a.ClientID == b.ClientID &&
		a.ClientSecretEnv == b.ClientSecretEnv && a.ProxyURL == b.ProxyURL &&
		a.ResourceOrigin == b.ResourceOrigin && slices.Equal(a.Scopes, b.Scopes)
}

func validEndpoint(raw string, forbidden ...string) bool {
	u, ok := parseURL(raw)
	if !ok {
		return false
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	for _, name := range forbidden {
		if q.Has(name) {
			return false
		}
	}
	return true
}

func parseURL(raw string) (*url.URL, bool) {
	if raw == "" || !printable(raw) || strings.ContainsAny(raw, " #\\") {
		return nil, false
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Opaque != "" || u.Hostname() == "" ||
		strings.HasSuffix(u.Host, ":") {
		return nil, false
	}
	if port := u.Port(); port != "" {
		num, err := strconv.Atoi(port)
		if err != nil || num < 1 || num > 65535 {
			return nil, false
		}
	}
	switch u.Scheme {
	case "https":
		return u, true
	case "http":
		host := u.Hostname()
		return u, host == "127.0.0.1" || host == "::1"
	default:
		return nil, false
	}
}

func printable(value string) bool {
	for _, r := range value {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return true
}

func validEnvironmentName(value string) bool {
	for index, char := range value {
		if char != '_' && (char < 'a' || char > 'z') &&
			(char < 'A' || char > 'Z') && (index == 0 || char < '0' || char > '9') {
			return false
		}
	}
	return value != ""
}
