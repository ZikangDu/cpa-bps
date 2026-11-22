package adapter

import (
	"context"
	"cpa-bps-reconstruction/internal/upstream"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRequestContextCancellationAndLegacyFallback(t *testing.T) {
	for _, mode := range []string{"client", "finish", "shutdown"} {
		t.Run(mode, func(t *testing.T) {
			closed := make(chan struct{})
			closeHost := sync.OnceFunc(func() { close(closed) })
			r := New(func(method string, request, result any) error {
				switch method {
				case "host.request.open":
					return json.Unmarshal([]byte(`{"operation_id":"fixture"}`), result)
				case "host.request.wait":
					<-closed
					return nil
				case "host.http.cancel":
					closeHost()
					return nil
				default:
					return fmt.Errorf("unexpected callback: %s", method)
				}
			})
			t.Cleanup(r.Close)
			ctx, stop, err := r.requestContext(executionRequest{HostCallbackID: "fixture", HostRequestLifecycle: true})
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "client":
				closeHost()
			case "finish":
				stop()
			case "shutdown":
				r.Close()
			}
			select {
			case <-ctx.Done():
			case <-time.After(time.Second):
				t.Fatal("host cancellation was not propagated")
			}
			stop()
			stop()
		})
	}
	r := New(func(string, any, any) error { t.Error("legacy host received lifecycle callback"); return nil })
	t.Cleanup(r.Close)
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	ctx, stop, err := r.requestContext(executionRequest{HostCallbackID: "old-host", parentContext: parent})
	if err != nil || ctx != parent {
		t.Fatal("legacy host lost its existing parent context")
	}
	stop()
	if ctx.Err() != nil {
		t.Fatal("legacy fallback canceled caller-owned context")
	}
}

func TestRequestContextReachesPlainAndBufferedUpstreamOperations(t *testing.T) {
	for _, mode := range []string{"text", "auto", "native"} {
		t.Run(mode, func(t *testing.T) {
			r, _, auth := quotaFixture(t)
			r.cfg.BPSToolTransport = mode
			r.cfg.NativeToolRepairAttempts = 2
			closed, started := make(chan struct{}), make(chan struct{})
			closeHost := sync.OnceFunc(func() { close(closed) })
			r.host = func(method string, request, result any) error {
				switch method {
				case "host.request.open":
					return json.Unmarshal([]byte(`{"operation_id":"fixture"}`), result)
				case "host.request.wait":
					<-closed
					return nil
				case "host.http.cancel":
					closeHost()
					return nil
				default:
					return fmt.Errorf("unexpected callback: %s", method)
				}
			}
			r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if strings.HasSuffix(req.URL.Path, "/usage") {
						return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
					}
					return qualityResponseFixture(200, `{"available_count":0}`, false), nil
				}
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			}))
			req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth,
				Payload: autoSource(mode != "text"), HostCallbackID: "fixture", HostRequestLifecycle: true}
			raw, _ := json.Marshal(req)
			done := make(chan error, 1)
			go func() { _, err := r.execute(raw, false); done <- err }()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("upstream request did not start")
			}
			closeHost()
			select {
			case err := <-done:
				problem, ok := err.(*rpcError)
				if !ok || problem.HTTPStatus != 499 {
					t.Fatalf("cancellation was lost: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("host cancellation did not stop upstream operation")
			}
			if state := r.queue.Snapshot("fixture-id", 1); state.Active != 0 || state.Waiting != 0 {
				t.Fatal("cancellation retained account capacity")
			}
		})
	}
}
