package oauthflow

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixtureConfig(issuer string) Config {
	return Config{
		AuthorizationURL: issuer + "/authorize?tenant=fixture",
		TokenURL:         issuer + "/token",
		RedirectURI:      issuer + "/callback?tenant=fixture",
		ClientID:         "registered-fixture",
		Scopes:           []string{"responses", "offline_access"},
	}
}

func callback(t *testing.T, cfg Config, state, name, value string) string {
	t.Helper()
	u, err := url.Parse(cfg.RedirectURI)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	q.Set("state", state)
	q.Set(name, value)
	u.RawQuery = q.Encode()
	return u.String()
}

func TestPKCECallbackIsBoundToConfigAndExchangeIsSingleUse(t *testing.T) {
	var calls atomic.Int32
	challenge := make(chan string, 2)
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
			http.Error(w, "", http.StatusBadRequest)
			return
		}
		verifier := r.Form.Get("code_verifier")
		hash := sha256.Sum256([]byte(verifier))
		challenge <- base64.RawURLEncoding.EncodeToString(hash[:])
		if r.Method != http.MethodPost || r.URL.Path != "/token" ||
			r.Form.Get("grant_type") != "authorization_code" ||
			r.Form.Get("code") != "fixture+code" || len(verifier) != nonceLength ||
			r.Form.Get("client_id") != "registered-fixture" ||
			!strings.HasSuffix(r.Form.Get("redirect_uri"), "/callback?tenant=fixture") {
			t.Error("unexpected token request")
		}
		_, _ = fmt.Fprint(w, `{"access_token":"test.access","refresh_token":"test.refresh","token_type":"Bearer","expires_in":3600}`)
	}))
	defer issuer.Close()

	f := New()
	defer f.Close()
	now := time.Date(2026, 3, 9, 12, 0, 0, 0, time.UTC)
	f.now = func() time.Time { return now }
	cfg := fixtureConfig(issuer.URL)
	cfg.ResourceOrigin = "https://resources.example"
	login, err := f.Begin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	showURL, err := url.Parse(login.URL)
	if err != nil {
		t.Fatal(err)
	}
	q := showURL.Query()
	if len(login.State) != nonceLength || !login.ExpiresAt.Equal(now.Add(loginLifetime)) ||
		q.Get("state") != login.State || q.Get("code_challenge_method") != "S256" ||
		q.Get("code_challenge") == "" || q.Get("code_verifier") != "" ||
		q.Get("response_type") != "code" || q.Get("scope") != "responses offline_access" ||
		q.Get("tenant") != "fixture" || q.Get("redirect_uri") != cfg.RedirectURI {
		t.Fatal("authorization URL did not match expected grant")
	}

	callbackURL := callback(t, cfg, login.State, "code", "fixture+code")
	for _, invalid := range []string{
		strings.Replace(callbackURL, "/callback?", "/other?", 1),
		strings.Replace(callbackURL, "tenant=fixture", "tenant=other", 1),
		strings.Replace(callbackURL, login.State, "different-state", 1),
		callbackURL + "&state=" + login.State,
		callbackURL + "&code=second-code",
		callbackURL + "&error=access_denied",
		callbackURL + "#fragment",
	} {
		if _, err := f.Complete(context.Background(), cfg, login.State, invalid); !errors.Is(err, ErrInvalidCallback) {
			t.Fatalf("invalid callback accepted or misclassified: %v", err)
		}
	}
	configuredAgain := cfg
	configuredAgain.ClientID += "-other"
	if _, err := f.Complete(context.Background(), configuredAgain, login.State, callbackURL); !errors.Is(err, ErrState) {
		t.Fatalf("changed client could complete pending exchange: %v", err)
	}
	configuredAgain = cfg
	configuredAgain.ResourceOrigin = "https://different.example"
	if _, err := f.Complete(context.Background(), configuredAgain, login.State, callbackURL); !errors.Is(err, ErrState) {
		t.Fatalf("changed resource could complete pending exchange: %v", err)
	}
	if !f.Pending(cfg, login.State) || calls.Load() != 0 {
		t.Fatal("invalid callback changed session or started token exchange")
	}

	type completion struct {
		tokens Tokens
		err    error
	}
	completions := make(chan completion, 2)
	for range 2 {
		go func() {
			tokens, err := f.Complete(context.Background(), cfg, login.State, callbackURL)
			completions <- completion{tokens, err}
		}()
	}
	var success, spent int
	for range 2 {
		result := <-completions
		switch {
		case result.err == nil:
			success++
			if result.tokens.AccessToken != "test.access" || result.tokens.RefreshToken != "test.refresh" ||
				!result.tokens.ExpiresAt.Equal(now.Add(time.Hour)) {
				t.Error("unexpected fixture tokens")
			}
		case errors.Is(result.err, ErrState):
			spent++
		default:
			t.Errorf("unexpected exchange error: %v", result.err)
		}
	}
	if success != 1 || spent != 1 || calls.Load() != 1 || f.Pending(cfg, login.State) {
		t.Fatalf("complete was not single-use: %d success, %d spent, %d requests", success, spent, calls.Load())
	}
	if got := <-challenge; got != q.Get("code_challenge") {
		t.Fatal("token request did not prove the advertised PKCE challenge")
	}
}

func TestDeniedExpiredAndClosedSessions(t *testing.T) {
	flow := New()
	defer flow.Close()
	now := time.Now()
	flow.now = func() time.Time { return now }
	cfg := fixtureConfig("http://127.0.0.1:1")
	login, err := flow.Begin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	denied := callback(t, cfg, login.State, "error", "private-provider-message")
	if _, err := flow.Complete(context.Background(), cfg, login.State, denied); !errors.Is(err, ErrAuthorizationDenied) ||
		strings.Contains(err.Error(), "private-provider-message") {
		t.Fatalf("denial not safely reported: %v", err)
	}
	if flow.Pending(cfg, login.State) {
		t.Fatal("denied login remained pending")
	}
	login, err = flow.Begin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(loginLifetime)
	if flow.Pending(cfg, login.State) {
		t.Fatal("expired login remained pending")
	}
	if _, err := flow.Complete(context.Background(), cfg, login.State, callback(t, cfg, login.State, "code", "fixture")); !errors.Is(err, ErrState) {
		t.Fatalf("expired login was not rejected: %v", err)
	}
	flow.Close()
	if _, err := flow.Begin(cfg); !errors.Is(err, ErrClosed) {
		t.Fatalf("begin on closed flow: %v", err)
	}
}

func TestConfiguredEndpoints(t *testing.T) {
	for _, value := range []string{
		"http://example.test/authorize",
		"http://localhost/authorize",
		"http://127.0.0.2/authorize",
		"https://example.test:0/authorize",
		"https://example.test:/authorize",
		"https://user@example.test/authorize",
		"https://example.test/authorize#fragment",
		"https://example.test/authorize?state=other",
		"https://example.test/authorize?tenant=%q",
	} {
		cfg := fixtureConfig("http://127.0.0.1:1")
		cfg.AuthorizationURL = value
		if err := cfg.Validate(); err == nil {
			t.Error("accepted invalid OAuth authorization URL")
		}
	}
	for _, issuer := range []string{"https://accounts.example.test", "http://127.0.0.1:1234", "http://[::1]:1234"} {
		if err := fixtureConfig(issuer).Validate(); err != nil {
			t.Fatalf("valid issuer rejected: %v", err)
		}
	}
}
