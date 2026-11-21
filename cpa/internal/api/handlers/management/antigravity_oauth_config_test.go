package management

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	antigravityauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/antigravity"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestAntigravityManagementLoginReportsMissingOAuthClient(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	response := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(response)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	(&Handler{}).RequestAntigravityToken(ctx)
	if response.Code != http.StatusServiceUnavailable || !strings.Contains(response.Body.String(), "CPA_ANTIGRAVITY_OAUTH_CLIENT_ID") {
		t.Fatalf("login should explain missing OAuth setup, got status %d", response.Code)
	}
}

func TestAntigravityManagementRefreshUsesExternalOAuthClient(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", "fixture-client-id")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", "fixture-client-secret")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("client_id") != "fixture-client-id" || r.Form.Get("client_secret") != "fixture-client-secret" || r.Form.Get("refresh_token") != "fixture-refresh" {
			t.Error("management refresh did not use the configured OAuth client")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"fixture-access","expires_in":3600}`))
	}))
	defer server.Close()
	previous := antigravityOAuthTokenURL
	antigravityOAuthTokenURL = server.URL
	t.Cleanup(func() { antigravityOAuthTokenURL = previous })
	auth := &coreauth.Auth{Metadata: map[string]any{"refresh_token": "fixture-refresh"}}
	token, err := (&Handler{}).refreshAntigravityOAuthAccessToken(context.Background(), auth, "")
	if err != nil || token != "fixture-access" {
		t.Fatalf("management refresh failed: %v", err)
	}
}

func TestAntigravityManagementRefreshMissingClientKeepsValidTokensUsable(t *testing.T) {
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_ID", "")
	t.Setenv("CPA_ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	handler := &Handler{}
	auth := &coreauth.Auth{Metadata: map[string]any{"access_token": "fixture-valid", "refresh_token": "fixture-refresh", "expired": time.Now().Add(time.Hour).Format(time.RFC3339)}}
	token, err := handler.refreshAntigravityOAuthAccessToken(context.Background(), auth, "")
	if err != nil || token != "fixture-valid" {
		t.Fatalf("existing valid access token unexpectedly requires OAuth setup: %v", err)
	}
	delete(auth.Metadata, "access_token")
	_, err = handler.refreshAntigravityOAuthAccessToken(context.Background(), auth, "")
	if !errors.Is(err, antigravityauth.ErrOAuthClientNotConfigured) {
		t.Fatalf("refresh error = %v, want missing OAuth configuration", err)
	}
}
