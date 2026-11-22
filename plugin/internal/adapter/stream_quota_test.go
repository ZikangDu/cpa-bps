package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func TestStreamQuotaRejectionBlocksLaterDispatchWithoutReplay(t *testing.T) {
	for _, mode := range []string{"text", "auto", "native"} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/stream=%v", mode, stream), func(t *testing.T) {
				r, _, auth := quotaFixture(t)
				r.cfg.BPSToolTransport = mode
				var reads, posts atomic.Int32
				done := make(chan struct{}, 1)
				r.host = func(method string, _ any, _ any) error {
					if method == "host.stream.close" {
						done <- struct{}{}
					}
					return nil
				}
				r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet {
						if strings.HasSuffix(req.URL.Path, "/usage") {
							used := 1
							if reads.Add(1) > 1 {
								used = 100
							}
							return qualityResponseFixture(200, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"reset_at":%d}}}`, used, time.Now().Add(time.Hour).Unix()), false), nil
						}
						return qualityResponseFixture(200, `{"available_count":0}`, false), nil
					}
					posts.Add(1)
					return qualityResponseFixture(200, "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"status\":\"in_progress\"}}\n\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"PRIVATE upstream data\"}\n\n", true), nil
				}))
				source := []byte(`{"model":"fixture","input":"fixture"}`)
				if mode != "text" {
					source = autoSource(true)
				}
				req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: source, StreamID: "quota-stream"}
				raw, _ := json.Marshal(req)
				_, err := r.execute(raw, stream)
				if stream && mode == "text" {
					if err != nil {
						t.Fatal(err)
					}
					select {
					case <-done:
					case <-time.After(3 * time.Second):
						t.Fatal("stream did not close")
					}
				} else if err == nil {
					t.Fatal("SSE rejection became a success")
				}
				_, err = r.execute(raw, false)
				problem, ok := err.(*rpcError)
				if !ok || problem.PublicCode != "bps_quota_exhausted" {
					t.Fatalf("subsequent account dispatch was not blocked: %v", err)
				}
				if reads.Load() != 2 || posts.Load() != 1 {
					t.Fatalf("quota refresh or no-replay violated reads=%d posts=%d", reads.Load(), posts.Load())
				}
			})
		}
	}
}

func TestObservedStreamQuotaSurvivesCallerCancellation(t *testing.T) {
	r, _, raw := quotaFixture(t)
	var reads atomic.Int32
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodGet {
			t.Fatal("rejection observer issued generation")
		}
		if strings.HasSuffix(req.URL.Path, "/usage") {
			reads.Add(1)
			return qualityResponseFixture(200, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":100,"reset_at":%d}}}`, time.Now().Add(time.Hour).Unix()), false), nil
		}
		return qualityResponseFixture(200, `{"available_count":0}`, false), nil
	}))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := executionRequest{AuthID: "fixture-id", StorageJSON: raw}
	r.observeStreamQuotaRejection(ctx, r.cfg, req, &upstream.EventRejection{Diagnostic: upstream.Rejection{HTTPStatus: 429, Code: "bps_rate_limited", Message: "Rate limited."}}, nil)
	token, err := resolveToken(raw, r.cfg.ResponsesURL, r.cfg.OAuth)
	if err != nil {
		t.Fatal(err)
	}
	rejected := r.beforeQuotaDispatch(context.Background(), r.cfg, req.AuthID, raw, token)
	if rejected == nil || rejected.PublicCode != "bps_quota_exhausted" || reads.Load() != 1 {
		t.Fatalf("cancellation erased rejection: %v reads=%d", rejected, reads.Load())
	}
}
