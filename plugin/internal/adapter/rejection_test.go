package adapter

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRejectedExecutionPreservesSafeDiagnosticAndReleasesSlot(t *testing.T) {
	for _, tt := range []struct {
		status int
		body   string
		code   string
		retry  int
	}{
		{403, `{"cloudflare_error":true,"error_code":1010,"detail":"secret-account@example.invalid"}`, "bps_upstream_access_blocked", 0},
		{403, `{"error":{"code":null,"message":"403: This request was blocked by our usage policy.","type":"unspecified_upstream_type"}}`, "bps_usage_policy_blocked", 0},
		{403, `{"error":{"code":"insufficient_scope","message":"secret-account@example.invalid"}}`, "bps_insufficient_scope", 0},
		{403, `{"message":"secret-account@example.invalid"}`, "bps_upstream_rejected", 0},
		{429, `{"error":{"code":"rate_limit_exceeded"}}`, "bps_rate_limited", 7},
		{503, `{"message":"secret-account@example.invalid"}`, "bps_upstream_rejected", 7},
	} {
		t.Run(tt.code, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				calls.Add(1)
				if req.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("synthetic credential missing")
				}
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Retry-After", "7")
				w.WriteHeader(tt.status)
				_, _ = io.WriteString(w, tt.body)
			}))
			defer server.Close()
			t.Setenv("CPA_BPS_REJECTION_FIXTURE", "fixture-token")
			storage, _ := json.Marshal(credential{Type: provider, TokenEnv: "CPA_BPS_REJECTION_FIXTURE", UpstreamOrigin: server.URL})
			runtime := New(nil)
			defer runtime.Close()
			cfg := config{ResponsesURL: server.URL + "/responses", ConcurrencyLimit: 1,
				ConcurrencyPolicyPath: filepath.Join(t.TempDir(), "concurrency.json")}
			response, _, finish, err := runtime.open(cfg, executionRequest{AuthID: "synthetic-account", StorageJSON: storage}, []byte(`{"model":"fixture","input":"fixture"}`), false)
			if response != nil || finish != nil {
				t.Fatal("rejected execution returned an open result")
			}
			var problem *rpcError
			if !errors.As(err, &problem) || problem.StatusCode() != tt.status || problem.Code() != tt.code || problem.RetryAfterSeconds() != tt.retry {
				t.Fatalf("wrong public diagnostic: %v", err)
			}
			if strings.Contains(problem.Error(), "secret-account") || strings.Contains(problem.Error(), "fixture-token") {
				t.Fatal("private text leaked through CPA error response")
			}
			if calls.Load() != 1 {
				t.Fatal("rejected upstream request was automatically replayed")
			}
			snapshot := runtime.queue.Snapshot("synthetic-account", 1)
			if snapshot.Active != 0 || snapshot.Waiting != 0 {
				t.Fatal("rejection kept the account concurrency slot occupied")
			}
		})
	}
}
