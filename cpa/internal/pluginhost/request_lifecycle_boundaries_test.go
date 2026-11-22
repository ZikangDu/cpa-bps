package pluginhost

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/MoeForge/moego/json"
)

func TestRequestLifecycleRejectsMixedRequestScopeWithoutConsumingOwner(t *testing.T) {
	h := New()
	instance := &hostCallbackInstance{}
	caller := withHostCallbackIdentity(t.Context(), "cpa-bps", instance)
	first, closeFirst := h.openCallbackContextForPluginInstance(t.Context(), "cpa-bps", instance)
	defer closeFirst()
	second, closeSecond := h.openCallbackContextForPluginInstance(t.Context(), "cpa-bps", instance)
	defer closeSecond()
	raw, _ := json.Marshal(requestLifecycleRequest{HostCallbackID: first})
	opened, err := h.callFromPlugin(caller, "host.request.open", raw)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := decodeRPCEnvelope[rpcHostHTTPOperationOpenResponse](opened)
	if err != nil {
		t.Fatal(err)
	}
	wrong, _ := json.Marshal(requestLifecycleRequest{HostCallbackID: second, OperationID: operation.OperationID})
	if _, err := h.callFromPlugin(caller, "host.request.wait", wrong); err == nil {
		t.Fatal("another request claimed the owner's lifecycle operation")
	}
	entry, ok := h.httpOperations.claim("cpa-bps", instance, operation.OperationID, first)
	if !ok {
		t.Fatal("foreign request consumed the owner operation")
	}
	closeSecond()
	if entry.ctx.Err() != nil {
		t.Fatal("unrelated request scope canceled owner")
	}
	closeFirst()
	select {
	case <-entry.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("owner scope did not release its operation")
	}
}

func TestRequestLifecycleConcurrentInstanceUnloadPreservesReplacement(t *testing.T) {
	h := New()
	old, replacement := &hostCallbackInstance{}, &hostCallbackInstance{}
	const total = 64
	closes := make([]func(), total)
	results := make(chan error, total)
	for i := range total {
		scope, closeScope := h.openCallbackContextForPluginInstance(t.Context(), "cpa-bps", old)
		closes[i] = closeScope
		caller := withHostCallbackIdentity(context.Background(), "cpa-bps", old)
		raw, _ := json.Marshal(requestLifecycleRequest{HostCallbackID: scope})
		opened, err := h.callFromPlugin(caller, "host.request.open", raw)
		if err != nil {
			t.Fatal(err)
		}
		operation, err := decodeRPCEnvelope[rpcHostHTTPOperationOpenResponse](opened)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ = json.Marshal(requestLifecycleRequest{HostCallbackID: scope, OperationID: operation.OperationID})
		go func() { _, err := h.callFromPlugin(caller, "host.request.wait", raw); results <- err }()
	}
	replacementScope, closeReplacement := h.openCallbackContextForPluginInstance(t.Context(), "cpa-bps", replacement)
	defer closeReplacement()
	replacementID, replacementEntry, err := h.createHostHTTPOperation("cpa-bps", replacement, replacementScope, t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, closeScope := range closes {
		wg.Go(closeScope)
	}
	wg.Go(func() { h.closeHostHTTPCallbackInstance("cpa-bps", old) })
	wg.Wait()
	for range total {
		select {
		case <-results:
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent unload retained request observer")
		}
	}
	if replacementEntry.ctx.Err() != nil {
		t.Fatal("retired instance canceled its replacement")
	}
	if _, ok := h.httpOperations.claim("cpa-bps", replacement, replacementID, replacementScope); !ok {
		t.Fatal("replacement operation was removed")
	}
	h.closeHostHTTPCallbackInstance("cpa-bps", replacement)
	h.httpOperations.mu.Lock()
	remaining := len(h.httpOperations.operations)
	h.httpOperations.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("unload retained %d operations", remaining)
	}
	h.callbackContexts.mu.RLock()
	scopes := len(h.callbackContexts.contexts)
	h.callbackContexts.mu.RUnlock()
	if scopes != 1 {
		t.Fatalf("scope teardown retained %d scopes; expected live replacement only", scopes)
	}
}
