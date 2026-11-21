package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/quality"
	"cpa-bps-reconstruction/internal/upstream"
)

func TestExhaustedBPSAccountSkipsAllQualityModelsWithoutChangingPlan(t *testing.T) {
	r, _ := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) {
		t.Error("original generation transport must not be reached")
		return nil, fmt.Errorf("unexpected generation")
	})
	var reads, posts atomic.Int32
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			posts.Add(1)
			return qualityResponseFixture(429, `{}`, false), nil
		}
		if req.URL.Path == "/backend-api/wham/usage" {
			reads.Add(1)
			return qualityResponseFixture(200, fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":100,"limit_window_seconds":604800,"reset_at":%d}}}`, time.Now().Add(time.Hour).Unix()), false), nil
		}
		return qualityResponseFixture(200, `{"available_count":0}`, false), nil
	}))
	before := r.quality.Snapshot("B")
	for _, manual := range []bool{false, true} {
		for _, model := range []string{"gpt-test-excel", "gpt-other-excel"} {
			got := r.runQualityCheck(context.Background(), quality.Check{AuthIndex: "B", AuthID: "account-B", Model: model,
				Config: qualityDefaults(), RunID: "quota-fixture", Manual: manual})
			if got.Status != "skipped" || got.ErrorCode != "bps_quota_exhausted" {
				t.Fatalf("quota was bypassed or falsely recorded as a generated answer: %+v", got)
			}
		}
	}
	after := r.quality.Snapshot("B")
	a, _ := json.Marshal(before.Config)
	b, _ := json.Marshal(after.Config)
	if string(a) != string(b) || !before.NextDue.Equal(after.NextDue) || before.Blocked != after.Blocked {
		t.Fatal("quota admission changed the user's schedule")
	}
	if reads.Load() != 1 || posts.Load() != 0 {
		t.Fatalf("quota wait retried network generation: reads=%d posts=%d", reads.Load(), posts.Load())
	}
	if s := r.queue.Snapshot("account-B", 20); s.Active != 0 || s.Waiting != 0 {
		t.Fatal("quota skip leaked admission slot")
	}
}

func TestBPSQuotaGatePreventsImagesAndResponsesBeforeAdmission(t *testing.T) {
	r, _, raw := quotaFixture(t)
	var reads, writes atomic.Int32
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			writes.Add(1)
			return qualityResponseFixture(500, `{}`, false), nil
		}
		if req.URL.Path == "/backend-api/wham/usage" {
			reads.Add(1)
			return qualityResponseFixture(200, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":%d}}}`, time.Now().Add(time.Hour).Unix()), false), nil
		}
		return qualityResponseFixture(200, `{"available_count":0}`, false), nil
	}))
	for _, model := range []string{"fixture", "another-model"} {
		response, _, finish, err := r.open(r.cfg, executionRequest{AuthID: "fixture-id", StorageJSON: raw, Model: model}, []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]}`), false)
		p, ok := err.(*rpcError)
		if !ok || !p.IsCredentialScoped() || p.PublicCode != "bps_quota_exhausted" || p.RetryAfter < 3500 || response != nil || finish != nil {
			t.Fatalf("known exhausted account was not rejected before uploads: %v", err)
		}
	}
	if reads.Load() != 1 || writes.Load() != 0 {
		t.Fatalf("unexpected calls reads=%d writes=%d", reads.Load(), writes.Load())
	}
}
