package adapter

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
)

func TestRotateGrantDeduplicatesAndRejectsStaleRevisions(t *testing.T) {
	var calls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls.Add(1)
		if err := req.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		switch req.Form.Get("refresh_token") {
		case "fixture-refresh-a":
			_, _ = w.Write([]byte(`{"access_token":"fixture-b","refresh_token":"fixture-refresh-b","expires_in":3600,"token_type":"Bearer"}`))
		case "fixture-refresh-b":
			_, _ = w.Write([]byte(`{"access_token":"fixture-c","refresh_token":"fixture-refresh-c","expires_in":3600,"token_type":"Bearer"}`))
		default:
			w.WriteHeader(http.StatusBadRequest)
		}
	}))
	defer issuer.Close()
	cfg := fixtureOAuthConfig(issuer.URL)
	runtime := New(nil)
	defer runtime.Close()
	first := oauthflow.Tokens{AccessToken: "fixture-a", RefreshToken: "fixture-refresh-a", ExpiresAt: time.Now().Add(time.Hour)}
	var wait sync.WaitGroup
	for range 8 {
		wait.Go(func() {
			got, err := runtime.rotateGrant("fixture", cfg, first)
			if err != nil || got.AccessToken != "fixture-b" {
				t.Error("concurrent callers did not share the rotation result")
			}
		})
	}
	wait.Wait()
	second, err := runtime.rotateGrant("fixture", cfg, first)
	if err != nil || calls.Load() != 1 {
		t.Fatal("the original refresh token was sent more than once")
	}
	third, err := runtime.rotateGrant("fixture", cfg, second)
	if err != nil || third.AccessToken != "fixture-c" {
		t.Fatal("the current grant revision could not rotate")
	}
	_, err = runtime.rotateGrant("fixture", cfg, first)
	if err == nil || err.Code() != "bps_oauth_stale_grant" || calls.Load() != 2 {
		t.Fatal("a stale grant revision reached the issuer")
	}
}

func TestRotateGrantDoesNotReplayUncertainExchange(t *testing.T) {
	var calls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer issuer.Close()
	cfg := fixtureOAuthConfig(issuer.URL)
	runtime := New(nil)
	defer runtime.Close()
	old := oauthflow.Tokens{AccessToken: "fixture-a", RefreshToken: "fixture-refresh-a", ExpiresAt: time.Now().Add(time.Hour)}
	for range 2 {
		_, err := runtime.rotateGrant("fixture", cfg, old)
		if err == nil || err.Code() != "bps_oauth_refresh_uncertain" {
			t.Fatal("the uncertain exchange must be surfaced")
		}
	}
	if calls.Load() != 1 {
		t.Fatal("the uncertain exchange was repeated")
	}
}

func TestManagedGrantRequiresConfiguredClientAndOrigin(t *testing.T) {
	cfg := fixtureOAuthConfig("https://issuer.example")
	record := credential{Type: provider, UpstreamOrigin: "https://responses.example", Grant: &managedGrant{
		ClientID: cfg.ClientID, TokenURL: cfg.TokenURL,
		Tokens: oauthflow.Tokens{AccessToken: "fixture", ExpiresAt: time.Now().Add(time.Hour)},
	}}
	raw, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if token, problem := resolveToken(raw, "https://responses.example/v1/responses", &cfg); problem != nil || token != "fixture" {
		t.Fatal("the matching client could not use its authorized grant")
	}
	cfg.ClientID = "other-client"
	if token, problem := resolveToken(raw, "https://responses.example/v1/responses", &cfg); problem == nil || token != "" {
		t.Fatal("a different OAuth client was permitted to use the grant")
	}
}

func fixtureOAuthConfig(origin string) oauthflow.Config {
	return oauthflow.Config{
		AuthorizationURL: origin + "/authorize", TokenURL: origin + "/token",
		RedirectURI: "http://127.0.0.1:12345/callback", ClientID: "fixture-client",
	}
}
