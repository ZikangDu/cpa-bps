package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func TestCanceledSSEQuotaRefreshDoesNotRetainGenerationSlot(t *testing.T) {
	for _, mode := range []string{"text", "auto", "native"} {
		for _, format := range []string{"sse", "json"} {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/stream=%v", mode, format, stream), func(t *testing.T) {
					r, _, auth := quotaFixture(t)
					r.cfg.BPSToolTransport = mode
					r.cfg.NativeToolRepairAttempts = 2
					disconnected, canceled := make(chan struct{}), make(chan struct{})
					quotaStarted, releaseQuota, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
					release := sync.OnceFunc(func() { close(releaseQuota) })
					t.Cleanup(release)
					closeClient := sync.OnceFunc(func() { close(disconnected) })
					closeCanceled := sync.OnceFunc(func() { close(canceled) })
					closeFinished := sync.OnceFunc(func() { close(finished) })
					r.host = func(method string, request, result any) error {
						switch method {
						case "host.request.open":
							return json.Unmarshal([]byte(`{"operation_id":"fixture"}`), result)
						case "host.request.wait":
							<-disconnected
							return nil
						case "host.http.cancel":
							closeClient()
							closeCanceled()
						case "host.stream.close":
							closeFinished()
						}
						return nil
					}
					var reads, posts atomic.Int32
					r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
						if req.Method == http.MethodGet {
							if strings.HasSuffix(req.URL.Path, "/usage") {
								used := 1
								if reads.Add(1) > 1 {
									close(quotaStarted)
									<-releaseQuota
									used = 100
								}
								return qualityResponseFixture(200, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"reset_at":%d}}}`, used, time.Now().Add(time.Hour).Unix()), false), nil
							}
							return qualityResponseFixture(200, `{"available_count":0}`, false), nil
						}
						posts.Add(1)
						if format == "json" {
							return qualityResponseFixture(200, `{"error":{"code":"rate_limit_exceeded","message":"PRIVATE quota data"}}`, false), nil
						}
						return qualityResponseFixture(200, "data: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\"}\n\n", true), nil
					}))
					payload := []byte(`{"model":"fixture","input":"fixture"}`)
					if mode != "text" {
						payload = autoSource(true)
					}
					req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: payload, StreamID: "quota-stream", HostCallbackID: "fixture", HostRequestLifecycle: true}
					raw, _ := json.Marshal(req)
					executed := make(chan error, 1)
					go func() { _, err := r.execute(raw, stream); executed <- err }()
					select {
					case <-quotaStarted:
					case <-time.After(3 * time.Second):
						t.Fatal("post-rejection metadata read did not start")
					}
					closeClient()
					select {
					case <-canceled:
					case <-time.After(3 * time.Second):
						t.Fatal("request cancellation did not propagate")
					}
					if state := r.queue.Snapshot("fixture-id", 1); state.Active != 0 || state.Waiting != 0 {
						t.Errorf("canceled, terminal generation still occupies admission while quota metadata stalls: %+v", state)
					}
					release()
					select {
					case err := <-executed:
						if mode == "text" && stream && format == "sse" {
							if err != nil {
								t.Fatal(err)
							}
							select {
							case <-finished:
							case <-time.After(3 * time.Second):
								t.Fatal("stream did not finish")
							}
						} else if err == nil || strings.Contains(err.Error(), "PRIVATE") {
							t.Fatalf("rejection was lost or leaked upstream text: %v", err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("execution did not finish")
					}
					token, tokenErr := resolveToken(auth, r.cfg.ResponsesURL, r.cfg.OAuth)
					if tokenErr != nil {
						t.Fatal(tokenErr)
					}
					if rejected := r.beforeQuotaDispatch(context.Background(), r.cfg, req.AuthID, auth, token); rejected == nil || rejected.PublicCode != "bps_quota_exhausted" {
						t.Fatalf("subsequent dispatch escaped exhaustion: %v", rejected)
					}
					if posts.Load() != 1 || reads.Load() != 2 {
						t.Fatalf("generation replayed or quota refresh duplicated: posts=%d reads=%d", posts.Load(), reads.Load())
					}
				})
			}
		}
	}
}
