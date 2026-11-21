package adapter

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"cpa-bps-reconstruction/internal/quality"
)

func TestExplicitManualQualityUsesCurrentUpstreamDespiteHistoricalWarning(t *testing.T) {
	for _, status := range []int{200, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			r, host := qualityRuntimeFixture(t, func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.Header.Get("Authorization") != "Bearer synthetic-B-access-token" || req.Method != http.MethodPost {
					t.Error("manual check changed credential or request method")
				}
				if status == 403 {
					return qualityResponseFixture(status, `{"error":{"message":"403: This request was blocked by our usage policy.","code":null}}`, false), nil
				}
				return qualityResponseFixture(status, `data: {"type":"response.completed","response":{"status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}]}}`+"\n\n", true), nil
			})
			host.mu.Lock()
			host.status = "bps_usage_policy_blocked"
			host.mu.Unlock()
			check := quality.Check{AuthIndex: "B", AuthID: "account-B", Model: "gpt-test-excel", Config: qualityDefaults(), RunID: "manual-fixture", Manual: true}
			result := r.runQualityCheck(context.Background(), check)
			if calls.Load() != 1 || status == 200 && result.Status != "pass" || status == 403 && (result.Status != "request_error" || result.ErrorCode != "bps_usage_policy_blocked") {
				t.Fatalf("manual check did not preserve the real upstream outcome: %+v", result)
			}
			check.Manual = false
			automatic := r.runQualityCheck(context.Background(), check)
			if calls.Load() != 1 || automatic.ErrorCode != "bps_usage_policy_blocked" {
				t.Fatal("automatic observation ignored historical access warning")
			}
		})
	}
}
