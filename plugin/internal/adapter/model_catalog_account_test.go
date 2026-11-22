package adapter

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
)

func TestBPSModelCatalogUsesSelectedIDTokenWorkspace(t *testing.T) {
	r := New(nil)
	t.Cleanup(r.Close)
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nauto_discover: true\nmodel_suffix: ''\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = cfg
	var observed []string
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(request *http.Request) (*http.Response, error) {
		observed = append(observed, request.Header.Get("ChatGPT-Account-ID"))
		return qualityResponseFixture(200, `{"allowed":true,"model_catalog":{"models":[],"restricted_models":[]}}`, false), nil
	}))
	for _, account := range []string{"workspace-A", "workspace-B"} {
		claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
		idToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
		storage, _ := json.Marshal(credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Grant: &managedGrant{
			ClientID: cfg.OAuth.ClientID, TokenURL: cfg.OAuth.TokenURL,
			Tokens: oauthflow.Tokens{AccessToken: "opaque-selected-access", IDToken: idToken, ExpiresAt: time.Now().Add(time.Hour)},
		}})
		raw, _ := json.Marshal(map[string]any{"AuthProvider": provider, "StorageJSON": storage})
		result, err := r.models(raw)
		if err != nil {
			t.Fatal(err)
		}
		if models := result.(map[string]any)["Models"].([]modelInfo); len(models) != 0 {
			t.Fatal("empty catalog invented a model")
		}
		if got := observed[len(observed)-1]; got != account {
			t.Errorf("selected workspace = %q, catalog header = %q", account, got)
		}
	}
}
