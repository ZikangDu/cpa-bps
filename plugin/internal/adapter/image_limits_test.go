package adapter

import (
	"cpa-bps-reconstruction/internal/upstream"
	"errors"
	"strings"
	"testing"
)

func TestInlineImageLimitDiagnosticsIdentifyOnlySafeBudget(t *testing.T) {
	for kind, phrase := range map[string]string{"reference_count": "1025 inline image references", "image_bytes": "per-image limit", "unique_bytes": "Repeated image content is counted once", "encoded_bytes": "transfer-size budget", "encoded_image_bytes": "available encoded-size budget", "total_bytes": "decoded bytes"} {
		p := inlineImageLimitError(&upstream.ImageLimitError{Kind: kind, Limit: 1024, Actual: 1025})
		if p.StatusCode() != 413 || p.Code() != "bps_inline_image_limit" || !strings.Contains(p.Error(), phrase) {
			t.Fatalf("missing budget diagnosis for %s: %v", kind, p)
		}
	}
	for _, cause := range []error{errors.New("PRIVATE_IMAGE_DATA"), &upstream.ImageLimitError{Kind: "PRIVATE_IMAGE_DATA", Limit: 1, Actual: 2}} {
		if strings.Contains(inlineImageLimitError(cause).Error(), "PRIVATE_IMAGE_DATA") {
			t.Fatal("unknown error data escaped to caller")
		}
	}
}
