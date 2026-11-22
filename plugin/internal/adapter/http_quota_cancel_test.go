package adapter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func TestObservedHTTPQuotaRejectionSurvivesCallerCancellation(t *testing.T) {
	r, _, auth := quotaFixture(t)
	r.cfg.BPSToolTransport = "text"
	clientClosed, metadataStarted, metadataRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	closeClient := sync.OnceFunc(func() { close(clientClosed) })
	release := sync.OnceFunc(func() { close(metadataRelease) })
	t.Cleanup(release)
	r.host = func(method string, request, result any) error {
		switch method {
		case "host.request.open":
			return json.Unmarshal([]byte(`{"operation_id":"fixture"}`), result)
		case "host.request.wait":
			<-clientClosed
			return nil
		case "host.http.cancel":
			closeClient()
		}
		return nil
	}
	var reads, posts atomic.Int32
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			if strings.HasSuffix(req.URL.Path, "/usage") {
				used := 1
				if reads.Add(1) > 1 {
					close(metadataStarted)
					select {
					case <-metadataRelease:
						used = 100
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				}
				return qualityResponseFixture(200, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"reset_at":%d}}}`, used, time.Now().Add(time.Hour).Unix()), false), nil
			}
			return qualityResponseFixture(200, `{"available_count":0}`, false), nil
		}
		posts.Add(1)
		return qualityResponseFixture(429, `{"error":{"code":"rate_limit_exceeded"}}`, false), nil
	}))
	req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: []byte(`{"model":"fixture","input":"fixture"}`), HostCallbackID: "fixture", HostRequestLifecycle: true}
	raw, _ := json.Marshal(req)
	done := make(chan error, 1)
	go func() { _, err := r.execute(raw, false); done <- err }()
	select {
	case <-metadataStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("post429 observation did not start")
	}
	closeClient()
	// The metadata reader is allowed to survive the caller, but must release
	// generation capacity while the fresh quota observation is pending.
	deadline := time.Now().Add(time.Second)
	for r.queue.Snapshot(req.AuthID, 1).Active != 0 && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if state := r.queue.Snapshot(req.AuthID, 1); state.Active != 0 {
		t.Errorf("terminal HTTP429 retained generation slot: %+v", state)
	}
	release()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("HTTP429 became success")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish")
	}
	token, tokenErr := resolveToken(auth, r.cfg.ResponsesURL, r.cfg.OAuth)
	if tokenErr != nil {
		t.Fatal(tokenErr)
	}
	if err := r.beforeQuotaDispatch(context.Background(), r.cfg, req.AuthID, auth, token); err == nil || err.PublicCode != "bps_quota_exhausted" {
		t.Fatalf("caller cancellation erased observed HTTP429: %v", err)
	}
	if posts.Load() != 1 || reads.Load() != 2 {
		t.Fatalf("unexpected request amplification: posts=%d reads=%d", posts.Load(), reads.Load())
	}
}
