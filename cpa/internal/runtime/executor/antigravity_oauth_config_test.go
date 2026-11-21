package executor

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func useAntigravityOAuthTestClient(t *testing.T) {
	t.Helper()
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", "fixture-client-id")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", "fixture-client-secret")
}

func TestAntigravityRefreshRequiresExternalOAuthClient(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		t.Fatal("unconfigured refresh must not make an HTTP request")
		return nil, errors.New("unexpected HTTP request")
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	_, err := (&AntigravityExecutor{}).refreshTokenSingleFlight(ctx, &cliproxyauth.Auth{}, "fixture-refresh")
	if !errors.Is(err, antigravityauth.ErrOAuthClientNotConfigured) {
		t.Fatalf("refresh error = %v, want missing OAuth configuration", err)
	}
}

func TestAntigravityRefreshUsesExternalOAuthClient(t *testing.T) {
	useAntigravityOAuthTestClient(t)
	transport := roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		if err := req.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if req.Form.Get("client_id") != "fixture-client-id" || req.Form.Get("client_secret") != "fixture-client-secret" || req.Form.Get("refresh_token") != "fixture-refresh" {
			t.Fatal("refresh did not use configured OAuth client and selected credential")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"access_token":"fixture-access","expires_in":3600}`))}, nil
	})
	ctx := context.WithValue(context.Background(), "cliproxy.roundtripper", transport)
	result, err := (&AntigravityExecutor{}).refreshTokenSingleFlight(ctx, &cliproxyauth.Auth{}, "fixture-refresh")
	if err != nil || result == nil || result.AccessToken != "fixture-access" {
		t.Fatalf("refresh failed: %v", err)
	}
}
