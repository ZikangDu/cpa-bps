package adapter

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
)

func TestBPSModelMetadataAdvertisesOnlySupportedEffortAliases(t *testing.T) {
	r := New(nil)
	t.Cleanup(r.Close)
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nauto_discover: true\nmodel_suffix: ''\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = cfg
	r.client.CloseIdleConnections()
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet || request.URL.Path != "/basispoints/api/responses/access" {
			t.Fatalf("unexpected catalog request: %s %s", request.Method, request.URL.Path)
		}
		return qualityResponseFixture(200, `{"allowed":true,"model_catalog":{"models":[
			{"id":"xhigh-model","efforts":[{"value":"none"},{"value":"low"},{"value":"high"},{"value":"xhigh"}]},
			{"id":"native-max-model","efforts":[{"value":"high"},{"value":"xhigh"},{"value":"max"}]},
			{"id":"high-only-model","efforts":[{"value":"low"},{"value":"high"}]},
			{"id":"opaque-model"}
		],"restricted_models":[]}}`, false), nil
	}))
	storage, err := json.Marshal(credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Grant: &managedGrant{
		ClientID: cfg.OAuth.ClientID, TokenURL: cfg.OAuth.TokenURL,
		Tokens: oauthflow.Tokens{AccessToken: "synthetic-catalog-token", ExpiresAt: time.Now().Add(time.Hour)},
	}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(map[string]any{"AuthProvider": provider, "StorageJSON": storage})
	if err != nil {
		t.Fatal(err)
	}
	response, err := r.models(raw)
	if err != nil {
		t.Fatal(err)
	}
	models := response.(map[string]any)["Models"].([]modelInfo)
	want := map[string][]string{
		"xhigh-model":      {"none", "low", "high", "xhigh", "max", "ultra", "x-high"},
		"native-max-model": {"high", "xhigh", "max", "ultra", "x-high"},
		"high-only-model":  {"low", "high"},
		"opaque-model":     nil,
	}
	if len(models) != len(want) {
		t.Fatalf("model count = %d, want %d", len(models), len(want))
	}
	for _, model := range models {
		var got []string
		if model.Thinking != nil {
			got = model.Thinking.Levels
		}
		if levels, exists := want[model.ID]; !exists || !reflect.DeepEqual(got, levels) {
			t.Fatalf("model %q levels = %v, want %v", model.ID, got, levels)
		}
	}
}
