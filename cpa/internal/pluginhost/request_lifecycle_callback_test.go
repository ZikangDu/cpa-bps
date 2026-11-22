package pluginhost

import (
	"context"
	"github.com/MoeForge/moego/json"
	"runtime"
	"testing"
	"time"
)

func TestRequestLifecycleCancellationAndOwnership(t *testing.T) {
	for _, mode := range []string{"client", "scope", "finish", "unload"} {
		t.Run(mode, func(t *testing.T) {
			host := New()
			instance := &hostCallbackInstance{}
			parent, cancel := context.WithCancel(t.Context())
			defer cancel()
			id, closeScope := host.openCallbackContextForPluginInstance(parent, "cpa-bps", instance)
			defer closeScope()
			caller := withHostCallbackIdentity(context.Background(), "cpa-bps", instance)
			raw, _ := json.Marshal(requestLifecycleRequest{HostCallbackID: id})
			for _, foreign := range []context.Context{context.Background(), withHostCallbackIdentity(context.Background(), "other", instance), withHostCallbackIdentity(context.Background(), "cpa-bps", &hostCallbackInstance{})} {
				if _, err := host.callFromPlugin(foreign, "host.request.open", raw); err == nil {
					t.Fatal("foreign request scope was accepted")
				}
			}
			opened, err := host.callFromPlugin(caller, "host.request.open", raw)
			if err != nil {
				t.Fatal(err)
			}
			operation, err := decodeRPCEnvelope[rpcHostHTTPOperationOpenResponse](opened)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ = json.Marshal(requestLifecycleRequest{HostCallbackID: id, OperationID: operation.OperationID})
			done := make(chan error, 1)
			go func() { _, err := host.callFromPlugin(caller, "host.request.wait", raw); done <- err }()
			deadline := time.Now().Add(3 * time.Second)
			for {
				host.httpOperations.mu.Lock()
				entry := host.httpOperations.operations[hostHTTPOperationKey{pluginID: "cpa-bps", operationID: operation.OperationID}]
				claimed := entry != nil && entry.started
				host.httpOperations.mu.Unlock()
				if claimed {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("observer did not start")
				}
				runtime.Gosched()
			}
			switch mode {
			case "client":
				cancel()
			case "scope":
				closeScope()
			case "finish":
				if _, err := host.callFromPlugin(caller, "host.http.cancel", raw); err != nil {
					t.Fatal(err)
				}
			case "unload":
				host.closeHostHTTPCallbackInstance("cpa-bps", instance)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("request cancellation did not release observer")
			}
			host.httpOperations.mu.Lock()
			remaining := len(host.httpOperations.operations)
			host.httpOperations.mu.Unlock()
			if remaining != 0 {
				t.Fatal("completed observer retained an operation")
			}
		})
	}
}
