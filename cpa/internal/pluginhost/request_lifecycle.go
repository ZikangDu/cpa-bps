package pluginhost

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/MoeForge/moego/json"
)

type requestLifecycleRequest struct {
	HostCallbackID string `json:"host_callback_id"`
	OperationID    string `json:"operation_id,omitempty"`
}

func (h *Host) requestLifecycleOwner(ctx context.Context, raw []byte) (requestLifecycleRequest, error) {
	var req requestLifecycleRequest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if len(raw) > 2048 || decoder.Decode(&req) != nil || decoder.Decode(new(any)) != io.EOF || req.HostCallbackID == "" {
		return req, fmt.Errorf("invalid request lifecycle scope")
	}
	parent, pluginID, instance, exists := h.lookupCallbackContext(req.HostCallbackID)
	if !exists || parent == nil || instance == nil || instance.closed.Load() ||
		pluginID == "" || pluginID != hostCallbackPluginIDFromContext(ctx) || instance != hostCallbackInstanceFromContext(ctx) {
		return req, fmt.Errorf("request lifecycle scope is unavailable")
	}
	return req, nil
}

// Reuse the existing instance-bound operation registry so scope closure,
// request cancellation and plugin unload all release a waiting observer.
func (h *Host) callHostRequestOpen(ctx context.Context, raw []byte) ([]byte, error) {
	req, err := h.requestLifecycleOwner(ctx, raw)
	if err != nil {
		return nil, err
	}
	if req.OperationID != "" {
		return nil, fmt.Errorf("request lifecycle operation must be allocated by the host")
	}
	id, err := h.openHostHTTPOperation(ctx, req.HostCallbackID)
	if err != nil {
		return nil, err
	}
	return marshalRPCResult(rpcHostHTTPOperationOpenResponse{OperationID: id})
}

func (h *Host) callHostRequestWait(ctx context.Context, raw []byte) ([]byte, error) {
	req, err := h.requestLifecycleOwner(ctx, raw)
	if err != nil {
		return nil, err
	}
	if req.OperationID == "" {
		return nil, fmt.Errorf("request lifecycle operation is required")
	}
	operation, err := h.acquireHostHTTPOperation(ctx, req.HostCallbackID, req.OperationID)
	if err != nil {
		return nil, err
	}
	defer operation.finish()
	<-operation.ctx.Done()
	return marshalRPCResult(struct {
		Canceled bool `json:"canceled"`
	}{true})
}
