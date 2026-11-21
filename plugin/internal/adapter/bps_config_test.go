package adapter

import (
	"net/url"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
)

func TestBPSPresetConfiguresIndependentAuthorization(t *testing.T) {
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nmodels: [gpt-6.2-sol]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.OAuth == nil || cfg.ResponsesURL != oauthflow.BPSResponsesURL ||
		cfg.OAuth.RedirectURI != oauthflow.BPSRedirectURI || cfg.OAuth.ClientID != oauthflow.BPSClientID ||
		cfg.OAuth.ClientSecretEnv != "" || cfg.OAuth.ResourceOrigin != "https://bps.openai.com" {
		t.Fatal("BPS preset did not configure the independent public client flow")
	}
	token, _ := url.Parse(cfg.OAuth.TokenURL)
	if token.Query().Get("unified") != "true" {
		t.Fatal("BPS token exchange lost its unified mode")
	}
	flow := oauthflow.New()
	defer flow.Close()
	login, err := flow.Begin(*cfg.OAuth)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := url.Parse(login.URL)
	if start.Host != "auth.openai.com" || start.Query().Get("state") != login.State ||
		start.Query().Get("code_challenge_method") != "S256" || !flow.Pending(*cfg.OAuth, login.State) {
		t.Fatal("BPS authorization could not be initialized")
	}
}

func TestBPSPresetAllowsAccountModelDiscovery(t *testing.T) {
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nmodels: []\nauto_discover: true\nmodel_suffix: ''\n"))
	if err != nil || !cfg.discoveryEnabled() {
		t.Fatal("BPS account catalog discovery was not enabled")
	}
	if cfg.upstreamModel("gpt-6.2-astra") != "gpt-6.2-astra" {
		t.Fatal("discovered model name was altered")
	}
}

func TestBPSPresetRejectsMixedDestinations(t *testing.T) {
	for _, extra := range []string{
		"delegated_gateway: true\n",
		"responses_url: https://gateway.example/v1/responses\n",
		"oauth: {}\n",
		"oauth_proxy_url: file:///tmp/proxy\n",
	} {
		_, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nmodels: [gpt-6.2-sol]\n" + extra))
		if err == nil {
			t.Errorf("BPS preset accepted conflicting configuration %q", extra)
		}
	}
}

func TestRefreshScheduleRespectsIssuerBound(t *testing.T) {
	tokens := oauthflow.Tokens{
		RefreshToken:      "fixture",
		ExpiresAt:         time.Now().Add(time.Hour),
		EarliestRefreshAt: time.Now().Add(2 * time.Hour),
	}
	scheduled, interval := refreshSchedule(tokens)
	if !scheduled.Equal(tokens.EarliestRefreshAt) || interval <= 0 {
		t.Fatal("scheduled refresh precedes the issuer's earliest refresh time")
	}
}
