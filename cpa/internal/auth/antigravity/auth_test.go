package antigravity

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestBuildAuthURLIncludesAICodeScope(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", "fixture-client-id")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", "fixture-client-secret")
	authURL, errBuild := NewAntigravityAuth(nil, nil).BuildAuthURL("state", "http://localhost/callback")
	if errBuild != nil {
		t.Fatalf("build auth URL: %v", errBuild)
	}
	parsedURL, err := url.Parse(authURL)
	if err != nil {
		t.Fatalf("parse auth URL: %v", err)
	}

	for _, scope := range strings.Fields(parsedURL.Query().Get("scope")) {
		if scope == "https://www.googleapis.com/auth/aicode" {
			return
		}
	}
	t.Fatalf("authorization URL scope does not include %q", "https://www.googleapis.com/auth/aicode")
}

func TestOAuthClientMissingConfigurationFailsBeforeNetwork(t *testing.T) {
	for _, tc := range []struct {
		name   string
		id     string
		secret string
	}{
		{name: "both absent"},
		{name: "id absent", secret: "fixture-secret-never-log"},
		{name: "secret absent", id: "fixture-id-never-log"},
		{name: "whitespace only", id: " \t", secret: "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", tc.id)
			t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", tc.secret)
			id, secret, errConfig := OAuthClientCredentials()
			if !errors.Is(errConfig, ErrOAuthClientNotConfigured) || id != "" || secret != "" {
				t.Fatal("missing configuration must return its sentinel with no partial credentials")
			}
			for _, value := range []string{strings.TrimSpace(tc.id), strings.TrimSpace(tc.secret)} {
				if value != "" && strings.Contains(errConfig.Error(), value) {
					t.Fatal("configuration error exposed a credential value")
				}
			}
			if !strings.Contains(errConfig.Error(), "CPA_ANTIGRAVITY_OAUTH_CLIENT_ID") || !strings.Contains(errConfig.Error(), "CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET") {
				t.Fatal("configuration error must name the required environment variables")
			}
			called := false
			auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				return jsonResponse(`{}`), nil
			})})
			if authURL, errBuild := auth.BuildAuthURL("state", ""); !errors.Is(errBuild, ErrOAuthClientNotConfigured) || authURL != "" {
				t.Fatal("authorization URL must fail clearly when registration is missing")
			}
			if tokens, errExchange := auth.ExchangeCodeForTokens(context.Background(), "fixture-code", "http://localhost/callback"); !errors.Is(errExchange, ErrOAuthClientNotConfigured) || tokens != nil {
				t.Fatal("token exchange must fail clearly when registration is missing")
			}
			if called {
				t.Fatal("missing registration must not send an upstream request")
			}
		})
	}
}

func TestBuildAuthURLUsesExternalClientRegistration(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", " fixture-client-id \n")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", " fixture-client-secret \n")
	authURL, errBuild := NewAntigravityAuth(nil, nil).BuildAuthURL("fixture-state", "")
	if errBuild != nil {
		t.Fatalf("build auth URL: %v", errBuild)
	}
	parsedURL, errParse := url.Parse(authURL)
	if errParse != nil {
		t.Fatalf("parse auth URL: %v", errParse)
	}
	query := parsedURL.Query()
	if query.Get("client_id") != "fixture-client-id" || query.Get("state") != "fixture-state" || query.Get("redirect_uri") != "http://localhost:51121/oauth-callback" {
		t.Fatal("authorization URL did not use the trimmed external registration and default callback")
	}
	if query.Has("client_secret") || strings.Contains(authURL, "fixture-client-secret") {
		t.Fatal("authorization URL must not expose the client secret")
	}
}

func TestExchangeCodeUsesExternalClientRegistration(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", " fixture-client-id \t")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", " fixture-client-secret \n")
	called := false
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		called = true
		if req.URL.String() != TokenEndpoint || req.Method != http.MethodPost {
			t.Fatal("unexpected token exchange endpoint or method")
		}
		if errParse := req.ParseForm(); errParse != nil {
			t.Fatalf("parse exchange form: %v", errParse)
		}
		for key, want := range map[string]string{
			"client_id": "fixture-client-id", "client_secret": "fixture-client-secret",
			"code": "fixture-code", "redirect_uri": "http://localhost/callback", "grant_type": "authorization_code",
		} {
			if req.PostForm.Get(key) != want {
				t.Fatalf("token exchange form field %s is incorrect", key)
			}
		}
		return jsonResponse(`{"access_token":"fixture-access","refresh_token":"fixture-refresh","expires_in":3600,"token_type":"Bearer"}`), nil
	})})
	tokens, errExchange := auth.ExchangeCodeForTokens(context.Background(), "fixture-code", "http://localhost/callback")
	if errExchange != nil {
		t.Fatalf("exchange code: %v", errExchange)
	}
	if !called || tokens.AccessToken != "fixture-access" || tokens.RefreshToken != "fixture-refresh" || tokens.ExpiresIn != 3600 {
		t.Fatal("token exchange did not return the upstream token response")
	}
}

func TestFetchProjectIDFromLoadCodeAssist(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist" {
			t.Fatalf("unexpected request URL: %s", req.URL.String())
		}
		assertLoadCodeAssistHeaders(t, req)
		assertJSONContains(t, req, `"ideType":"ANTIGRAVITY"`)
		return jsonResponse(`{"cloudaicompanionProject":"cogent-snow-4mnnp"}`), nil
	})})

	projectID, err := auth.FetchProjectID(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("FetchProjectID error: %v", err)
	}
	if projectID != "cogent-snow-4mnnp" {
		t.Fatalf("projectID = %q", projectID)
	}
}

func TestFetchProjectIDFallsBackToDailyOnboardUser(t *testing.T) {
	var sawOnboard bool
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.String() {
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:loadCodeAssist":
			assertLoadCodeAssistHeaders(t, req)
			return jsonResponse(`{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`), nil
		case "https://daily-cloudcode-pa.googleapis.com/v1internal:onboardUser":
			sawOnboard = true
			assertOnboardUserHeaders(t, req)
			assertJSONContains(t, req, `"tier_id":"free-tier"`)
			assertJSONContains(t, req, `"ide_type":"ANTIGRAVITY"`)
			return jsonResponse(`{
				"done": true,
				"response": {
					"cloudaicompanionProject": {
						"id": "cogent-snow-4mnnp",
						"name": "cogent-snow-4mnnp",
						"projectNumber": "22597072101"
					}
				}
			}`), nil
		default:
			t.Fatalf("unexpected request URL: %s", req.URL.String())
			return nil, nil
		}
	})})

	projectID, err := auth.FetchProjectID(context.Background(), "access-token")
	if err != nil {
		t.Fatalf("FetchProjectID error: %v", err)
	}
	if !sawOnboard {
		t.Fatalf("expected onboardUser fallback")
	}
	if projectID != "cogent-snow-4mnnp" {
		t.Fatalf("projectID = %q", projectID)
	}
}

func TestFetchProjectIDUpstreamForbiddenReturnsStatus403(t *testing.T) {
	auth := NewAntigravityAuth(nil, &http.Client{Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"The caller does not have permission"}}`)),
		}, nil
	})})

	_, err := auth.FetchProjectID(context.Background(), "access-token")
	if err == nil {
		t.Fatalf("expected error from 403 response")
	}
	type statusCoder interface {
		StatusCode() int
	}
	sc, ok := err.(statusCoder)
	if !ok || sc.StatusCode() != http.StatusForbidden {
		t.Fatalf("expected status code %d, got %T (%v)", http.StatusForbidden, err, err)
	}
}

func assertLoadCodeAssistHeaders(t *testing.T, req *http.Request) {
	t.Helper()
	if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "" {
		t.Fatalf("Accept = %q, want omitted", got)
	}
	if got := req.Header.Get("X-Goog-Api-Client"); got != "" {
		t.Fatalf("X-Goog-Api-Client = %q, want empty", got)
	}
	userAgent := req.Header.Get("User-Agent")
	if !strings.HasPrefix(userAgent, "antigravity/hub/") {
		t.Fatalf("User-Agent = %q", userAgent)
	}
	if strings.Contains(userAgent, "google-api-nodejs-client/") {
		t.Fatalf("User-Agent = %q", userAgent)
	}
}

func assertOnboardUserHeaders(t *testing.T, req *http.Request) {
	t.Helper()
	if got := req.Header.Get("Authorization"); got != "Bearer access-token" {
		t.Fatalf("Authorization = %q", got)
	}
	if got := req.Header.Get("Accept"); got != "*/*" {
		t.Fatalf("Accept = %q", got)
	}
	if got := req.Header.Get("X-Goog-Api-Client"); got != "gl-node/22.21.1" {
		t.Fatalf("X-Goog-Api-Client = %q", got)
	}
	userAgent := req.Header.Get("User-Agent")
	if !strings.HasPrefix(userAgent, "antigravity/hub/") {
		t.Fatalf("User-Agent = %q", userAgent)
	}
	if !strings.Contains(userAgent, "google-api-nodejs-client/10.3.0") {
		t.Fatalf("User-Agent = %q", userAgent)
	}
}

func assertJSONContains(t *testing.T, req *http.Request, want string) {
	t.Helper()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	bodyText := string(body)
	req.Body = io.NopCloser(strings.NewReader(bodyText))
	if !strings.Contains(bodyText, want) {
		t.Fatalf("body missing %s: %s", want, bodyText)
	}
}

func jsonResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
