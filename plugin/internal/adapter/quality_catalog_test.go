package adapter

import (
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"

	"cpa-bps-reconstruction/internal/quality"
)

func TestQualityBlockedCatalogUsesOnlySelectedHostRegistry(t *testing.T) {
	var upstreamCalls atomic.Int32
	r, host := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) {
		upstreamCalls.Add(1)
		return nil, errors.New("no network expected")
	})
	originalHost := r.host
	r.host = func(method string, payload any, result any) error {
		if method != "host.auth.models" {
			return originalHost(method, payload, result)
		}
		if payload.(map[string]any)["auth_index"] != "A" {
			t.Fatal("catalog requested an unselected or global credential pool")
		}
		return json.Unmarshal([]byte(`{"credentials":[{"id":"account-A","auth_index":"A","provider":"cpa-bps","models":["gpt-second-excel","gpt-first-excel","gpt-first-excel","invalid model"]}]}`), result)
	}
	status, raw := qualityManagement(t, r, "GET", "models", "A", nil)
	var response struct {
		Models       []qualityModel `json:"models"`
		Source       string         `json:"source"`
		LiveVerified bool           `json:"live_verified"`
	}
	if status != 200 || json.Unmarshal(raw, &response) != nil || response.Source != "host_registered" || response.LiveVerified ||
		len(response.Models) != 2 || response.Models[0].ID != "gpt-first-excel" || response.Models[1].ID != "gpt-second-excel" {
		t.Fatalf("wrong registered catalog: %d %s", status, raw)
	}
	if upstreamCalls.Load() != 0 || len(host.gets) != 0 || len(r.quality.Snapshot("A").Results) != 0 {
		t.Fatal("read-only catalog fetched credentials/network or triggered a check")
	}
}

func TestQualityRegistryRejectsMismatchedAndMergedCredentials(t *testing.T) {
	for _, fixture := range []string{
		`{"credentials":[{"id":"account-B","auth_index":"B","provider":"cpa-bps","models":["private-other-model"]}]}`,
		`{"credentials":[{"id":"account-A","auth_index":"A","provider":"other","models":["private-other-model"]}]}`,
		`{"credentials":[{"id":"account-A","auth_index":"A","provider":"cpa-bps","models":["first"]},{"id":"account-B","auth_index":"B","provider":"cpa-bps","models":["second"]}]}`,
	} {
		t.Run(fixture, func(t *testing.T) {
			r := New(func(_ string, _ any, out any) error { return json.Unmarshal([]byte(fixture), out) })
			defer r.Close()
			models, ok := r.registeredQualityModels(config{}, qualityAuthEntry{ID: "account-A", AuthIndex: "A", Provider: provider}, "")
			if ok || len(models) != 0 {
				t.Fatal("catalog crossed credential/provider identity")
			}
		})
	}
}

func TestQualitySnapshotReportsCredentialPauseSeparately(t *testing.T) {
	r, host := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("no network") })
	host.mu.Lock()
	host.disabled = true
	host.mu.Unlock()
	for _, endpoint := range []string{"config", "results"} {
		status, raw := qualityManagement(t, r, "GET", endpoint, "B", nil)
		var view quality.Snapshot
		if status != 200 || json.Unmarshal(raw, &view) != nil || !view.CredentialDisabled || view.Blocked {
			t.Fatal("credential pause and observation block were conflated")
		}
	}
}
