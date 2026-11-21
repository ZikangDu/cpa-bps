package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshRotationAndErrorClassification(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" ||
			r.Form.Get("refresh_token") != "fixture.old" || r.Form.Get("client_id") != "registered-fixture" {
			t.Error("unexpected refresh request")
		}
		switch n {
		case 1:
			_, _ = fmt.Fprint(w, `{"access_token":"next.access","token_type":"bearer","expires_in":"42"}`)
		case 2:
			_, _ = fmt.Fprint(w, `{"access_token":"next.access","refresh_token":"new.refresh","token_type":"Bearer","expires_in":43}`)
		case 3:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprint(w, `{"error":"invalid_grant","error_description":"private fixture description"}`)
		case 4:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = fmt.Fprint(w, `{"error":{"code":"invalid_grant","message":"private fixture description"}}`)
		case 5:
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprint(w, `{"error":"invalid_grant"}`)
		default:
			_, _ = fmt.Fprint(w, `{"access_token":"unexpected token","token_type":"Bearer","expires_in":43}`)
		}
	}))
	defer server.Close()
	f := New()
	defer f.Close()
	cfg := fixtureConfig(server.URL)
	old := Tokens{AccessToken: "fixture.access", RefreshToken: "fixture.old"}
	if _, err := f.Refresh(context.Background(), cfg, Tokens{}); !errors.Is(err, ErrMissingRefreshToken) || calls.Load() != 0 {
		t.Fatalf("missing refresh token was not rejected before network call: %v", err)
	}
	for _, want := range []string{"fixture.old", "new.refresh"} {
		next, err := f.Refresh(context.Background(), cfg, old)
		if err != nil || next.RefreshToken != want || next.AccessToken != "next.access" || next.ExpiresAt.IsZero() {
			t.Fatalf("wrong refresh token preservation or rotation: %v", err)
		}
	}
	for _, want := range []error{ErrInvalidGrant, ErrInvalidGrant, ErrExchange, ErrInvalidResponse} {
		next, err := f.Refresh(context.Background(), cfg, old)
		if !errors.Is(err, want) || next.AccessToken != "" || next.RefreshToken != "" {
			t.Fatalf("wanted %v, received %v", want, err)
		}
	}
	if calls.Load() != 6 {
		t.Fatalf("expected one exchange attempt per refresh, got %d", calls.Load())
	}
}

func TestTokenMetadataPreservedAcrossCodeExchangeAndRefresh(t *testing.T) {
	var calls atomic.Int32
	earliest := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		switch calls.Add(1) {
		case 1:
			if r.Form.Get("grant_type") != "authorization_code" {
				t.Error("first request should exchange the authorization code")
			}
			_, _ = fmt.Fprintf(w, `{"access_token":"fixture.first","refresh_token":"fixture.refresh","id_token":"fixture.identity","scope":"openid offline_access","token_type":"Bearer","expires_in":3600,"earliest_refresh_at":%d}`, earliest.Unix())
		case 2:
			if r.Form.Get("refresh_token") != "fixture.refresh" {
				t.Error("refresh did not use the granted refresh token")
			}
			_, _ = fmt.Fprint(w, `{"access_token":"fixture.second","token_type":"Bearer","expires_in":3600}`)
		default:
			_, _ = fmt.Fprintf(w, `{"access_token":"fixture.third","refresh_token":"fixture.rotated","id_token":"fixture.newidentity","scope":"openid email","token_type":"Bearer","expires_in":3600,"earliest_refresh_at":%q}`, earliest.Add(time.Hour).Format(time.RFC3339))
		}
	}))
	defer server.Close()
	f := New()
	defer f.Close()
	cfg := fixtureConfig(server.URL)
	login, err := f.Begin(cfg)
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.Complete(context.Background(), cfg, login.State, callback(t, cfg, login.State, "code", "fixture-code"))
	if err != nil || first.IDToken != "fixture.identity" || first.Scope != "openid offline_access" || !first.EarliestRefreshAt.Equal(earliest) {
		t.Fatalf("token metadata did not survive the initial exchange: %v", err)
	}
	saved, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	var restored Tokens
	if err := json.Unmarshal(saved, &restored); err != nil || !restored.EarliestRefreshAt.Equal(earliest) ||
		restored.IDToken != first.IDToken || restored.Scope != first.Scope {
		t.Fatal("saved metadata did not round-trip")
	}
	second, err := f.Refresh(context.Background(), cfg, restored)
	if err != nil || second.AccessToken != "fixture.second" || second.RefreshToken != first.RefreshToken ||
		second.IDToken != first.IDToken || second.Scope != first.Scope || !second.EarliestRefreshAt.Equal(earliest) {
		t.Fatalf("refresh dropped metadata omitted by issuer: %v", err)
	}
	third, err := f.Refresh(context.Background(), cfg, second)
	if err != nil || third.RefreshToken != "fixture.rotated" || third.IDToken != "fixture.newidentity" ||
		third.Scope != "openid email" || !third.EarliestRefreshAt.Equal(earliest.Add(time.Hour)) {
		t.Fatalf("refresh did not apply replacement metadata: %v", err)
	}
}

func TestEarliestRefreshTimeValidation(t *testing.T) {
	earliest := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, raw := range []string{
		fmt.Sprintf("%d", earliest.Unix()),
		fmt.Sprintf("%q", fmt.Sprintf("%d", earliest.Unix())),
		`"2026-11-21T12:00:00Z"`,
		`"2026-11-21T20:00:00+08:00"`,
	} {
		stamp, ok := refreshTime(json.RawMessage(raw))
		if !ok || !stamp.Equal(earliest) {
			t.Fatalf("valid earliest refresh representation rejected: %s", raw)
		}
	}
	if stamp, ok := refreshTime(nil); !ok || !stamp.IsZero() {
		t.Fatal("omitted earliest refresh should remain unset")
	}
	for _, raw := range []string{`null`, `""`, `"tomorrow"`, "0", "-1", "true", "{}", "1.5", "1795262400000"} {
		if _, ok := refreshTime(json.RawMessage(raw)); ok {
			t.Fatalf("invalid or ambiguous earliest refresh representation accepted: %s", raw)
		}
	}
}

func TestMalformedEarliestRefreshRejectsSuccessfulTokenResponse(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_, _ = fmt.Fprint(w, `{"access_token":"fixture.next","token_type":"Bearer","expires_in":3600,"earliest_refresh_at":"unrecognized"}`)
	}))
	defer server.Close()
	f := New()
	defer f.Close()
	next, err := f.Refresh(context.Background(), fixtureConfig(server.URL), Tokens{RefreshToken: "fixture.refresh"})
	if !errors.Is(err, ErrInvalidResponse) || next.AccessToken != "" || calls.Load() != 1 {
		t.Fatalf("invalid refresh bound was silently ignored or exchange was replayed: %v", err)
	}
}

func TestTokenExchangeHonorsExplicitProxy(t *testing.T) {
	var directCalls, proxyCalls atomic.Int32
	issuer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		directCalls.Add(1)
		w.WriteHeader(http.StatusForbidden)
	}))
	defer issuer.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyCalls.Add(1)
		if r.URL.String() != issuer.URL+"/token" {
			t.Error("proxy request did not preserve the token endpoint")
		}
		_, _ = fmt.Fprint(w, `{"access_token":"fixture.proxy","token_type":"Bearer","expires_in":3600}`)
	}))
	cfg := fixtureConfig(issuer.URL)
	cfg.ProxyURL = proxy.URL
	f := New()
	defer f.Close()
	next, err := f.Refresh(context.Background(), cfg, Tokens{RefreshToken: "fixture.refresh"})
	if err != nil || next.AccessToken != "fixture.proxy" || proxyCalls.Load() != 1 || directCalls.Load() != 0 {
		t.Fatalf("token request did not use the explicit proxy: %v", err)
	}
	proxy.Close()
	next, err = f.Refresh(context.Background(), cfg, Tokens{RefreshToken: "fixture.refresh"})
	if !errors.Is(err, ErrExchange) || next.AccessToken != "" || directCalls.Load() != 0 {
		t.Fatal("unavailable explicit proxy fell back to a direct token request")
	}
}

func TestTokenEndpointNeverRedirects(t *testing.T) {
	var calls, redirects atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			redirects.Add(1)
			w.WriteHeader(http.StatusOK)
			return
		}
		calls.Add(1)
		http.Redirect(w, r, "/other", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	f := New()
	defer f.Close()
	if _, err := f.Refresh(context.Background(), fixtureConfig(server.URL), Tokens{RefreshToken: "fixture.old"}); !errors.Is(err, ErrExchange) {
		t.Fatalf("redirect did not fail exchange: %v", err)
	}
	if calls.Load() != 1 || redirects.Load() != 0 {
		t.Fatalf("exchange followed redirect: direct %d, redirected %d", calls.Load(), redirects.Load())
	}
}

func TestConfidentialClientUsesEncodedBasicAuthentication(t *testing.T) {
	t.Setenv("CPA_BPS_OAUTH_FIXTURE_SECRET", "test:plus + space")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if err := r.ParseForm(); err != nil || r.Form.Has("client_id") || r.Form.Has("client_secret") ||
			!ok || user != "test%3Aclient" || pass != "test%3Aplus+%2B+space" {
			t.Error("credentials were not encoded as client_secret_basic")
		}
		_, _ = fmt.Fprint(w, `{"access_token":"next.access","token_type":"Bearer","expires_in":60}`)
	}))
	defer server.Close()
	cfg := fixtureConfig(server.URL)
	cfg.ClientID = "test:client"
	cfg.ClientSecretEnv = "CPA_BPS_OAUTH_FIXTURE_SECRET"
	f := New()
	defer f.Close()
	if _, err := f.Refresh(context.Background(), cfg, Tokens{RefreshToken: "fixture.old"}); err != nil {
		t.Fatal(err)
	}
}
