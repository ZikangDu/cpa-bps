package adapter

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func TestPlanObservationsPreferFreshServerAndFallbackToGrant(t *testing.T) {
	r, auth, _ := quotaFixture(t)
	snapshot := upstream.BPSQuotaResult{
		Quota:      json.RawMessage(`{"subscription":{"plan":"pro"}}`),
		ObservedAt: time.Now().UTC().Format(time.RFC3339),
		Source:     "active_usage",
	}
	r.rememberBPSPlan("fixture-id", auth.Grant.AccessToken, snapshot)
	read := func(expectedPlan, expectedSource string) int {
		t.Helper()
		result, err := r.manageBPSPlans(managementRequest{Method: http.MethodGet, HostCallbackID: "fixture-context"})
		if err != nil {
			t.Fatal(err)
		}
		reply := result.(map[string]any)
		var data struct {
			Accounts []struct {
				Plan   string `json:"plan_type"`
				Source string `json:"source"`
			}
		}
		if err := json.Unmarshal(reply["Body"].([]byte), &data); err != nil {
			t.Fatal(err)
		}
		for _, entry := range data.Accounts {
			if entry.Plan != expectedPlan || entry.Source != expectedSource {
				t.Fatal("invalid observation")
			}
		}
		return len(data.Accounts)
	}
	if read("pro", "active_usage") != 1 {
		t.Fatal("fresh server plan was omitted")
	}
	old := r.planObservations["fixture-id"]
	old.observed = time.Now().Add(-6 * time.Minute)
	r.planObservations["fixture-id"] = old
	if read("team", "token_claims") != 1 {
		t.Fatal("expired server plan did not fall back to the current grant")
	}
	old.observed = time.Now()
	old.token = sha256.Sum256([]byte("different-token"))
	r.planObservations["fixture-id"] = old
	if read("team", "token_claims") != 1 {
		t.Fatal("another credential revision overrode current grant")
	}
}
