package adapter

import (
	"context"
	"errors"
	"net/http"

	"cpa-bps-reconstruction/internal/upstream"
)

// An error inside HTTP 200 SSE can still exhaust an account. Refresh admission
// for subsequent requests without replaying or changing this stream's outcome.
func (r *Runtime) observeStreamQuotaRejection(_ context.Context, cfg config, req executionRequest, err error) {
	var event *upstream.EventRejection
	if !errors.As(err, &event) || event.Diagnostic.HTTPStatus != http.StatusTooManyRequests {
		return
	}
	token, tokenErr := resolveToken(req.StorageJSON, cfg.ResponsesURL, cfg.OAuth)
	if tokenErr != nil {
		return
	}
	// The caller can disconnect immediately after receiving the error event.
	// That must not erase an already observed rejection. Only the short quota
	// metadata read survives cancellation; generation is never repeated.
	ctx, cancel := context.WithTimeout(r.ctx, bpsQuotaTimeout)
	defer cancel()
	r.afterQuotaRejection(ctx, cfg, req.AuthID, req.StorageJSON, token, event.Diagnostic)
}
