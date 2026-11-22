package adapter

import (
	"context"
	"sync"
)

// Older hosts omit HostRequestLifecycle and keep the original lifecycle. New
// hosts bind this observer to the authenticated request's callback scope; no
// caller-provided request field can select another request's context.
func (r *Runtime) requestContext(req executionRequest) (context.Context, func(), *rpcError) {
	parent := r.ctx
	if req.parentContext != nil {
		parent = req.parentContext
	}
	if !req.HostRequestLifecycle {
		return parent, func() {}, nil
	}
	if r.host == nil || req.HostCallbackID == "" {
		return nil, nil, failure(499, "bps_request_canceled", "The request lifecycle is unavailable.")
	}
	var opened struct {
		OperationID string `json:"operation_id"`
	}
	if err := r.host("host.request.open", map[string]any{"host_callback_id": req.HostCallbackID}, &opened); err != nil || opened.OperationID == "" {
		return nil, nil, failure(499, "bps_request_canceled", "The request has ended or its lifecycle is unavailable.")
	}
	ctx, cancel := context.WithCancel(parent)
	request := map[string]any{"host_callback_id": req.HostCallbackID, "operation_id": opened.OperationID}
	done, canceled := make(chan struct{}), make(chan struct{})
	r.tasks.Add(1)
	go func() {
		<-ctx.Done()
		_ = r.host("host.http.cancel", request, nil)
		close(canceled)
	}()
	go func() {
		defer r.tasks.Done()
		defer close(done)
		_ = r.host("host.request.wait", request, nil)
		cancel()
		<-canceled
	}()
	stop := sync.OnceFunc(func() { cancel(); <-done })
	return ctx, stop, nil
}
