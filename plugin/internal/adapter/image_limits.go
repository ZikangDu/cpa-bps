package adapter

import (
	"errors"
	"fmt"
	"net/http"

	"cpa-bps-reconstruction/internal/upstream"
)

// Publish fixed budget names and counts only, never image data or history text.
func inlineImageLimitError(cause error) *rpcError {
	message := "Inline images exceed the supported limits."
	var detail *upstream.ImageLimitError
	if errors.As(cause, &detail) && detail.Limit > 0 && detail.Actual >= 0 {
		switch detail.Kind {
		case "reference_count":
			message = fmt.Sprintf("The request contains %d inline image references; the limit is %d. Full conversation history is included in this count.", detail.Actual, detail.Limit)
		case "image_bytes":
			message = fmt.Sprintf("An inline image exceeds the per-image limit of %d bytes (observed at least %d bytes).", detail.Limit, detail.Actual)
		case "unique_bytes":
			message = fmt.Sprintf("Unique inline images total %d decoded bytes; the per-request limit is %d bytes. Repeated image content is counted once.", detail.Actual, detail.Limit)
		case "encoded_bytes":
			message = fmt.Sprintf("Inline image data totals %d encoded bytes; the per-request limit is %d bytes. Repeated history occurrences are included in this transfer-size budget.", detail.Actual, detail.Limit)
		case "encoded_image_bytes":
			message = fmt.Sprintf("An inline image requires %d encoded bytes; its available encoded-size budget is %d bytes.", detail.Actual, detail.Limit)
		case "total_bytes":
			message = fmt.Sprintf("Inline images total %d decoded bytes; the per-request limit is %d bytes.", detail.Actual, detail.Limit)
		}
	}
	return failure(http.StatusRequestEntityTooLarge, "bps_inline_image_limit", message)
}
