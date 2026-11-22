package adapter

import (
	"context"
	"errors"
	"net/http"
	"sync"

	"cpa-bps-reconstruction/internal/upstream"
)

// An error inside HTTP 200 JSON or SSE can still exhaust an account. Refresh admission
// for subsequent requests without replaying or changing this stream's outcome.
func (r *Runtime) observeStreamQuotaRejection(ctx context.Context, cfg config, req executionRequest, err error, releaseGeneration func()) {
	var event *upstream.EventRejection
	if !errors.As(err, &event) || event.Diagnostic.HTTPStatus != http.StatusTooManyRequests {
		return
	}
	// Slot cleanup must also run when credential resolution no longer succeeds.
	if releaseGeneration != nil {
		releaseGeneration = sync.OnceFunc(releaseGeneration)
		defer releaseGeneration()
	}
	token, tokenErr := resolveToken(req.StorageJSON, cfg.ResponsesURL, cfg.OAuth)
	if tokenErr != nil {
		return
	}
	// The caller can disconnect immediately after receiving the error event.
	// Preserve the same bounded observation used for explicit HTTP 429 replies.
	r.afterGenerationRejection(ctx, cfg, req.AuthID, req.StorageJSON, token, event.Diagnostic, releaseGeneration)
}
