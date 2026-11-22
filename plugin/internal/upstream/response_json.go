package upstream

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

var ErrInvalidResponse = errors.New("upstream returned invalid Responses JSON")

// ReadResponseJSON is only for generation replies. Other JSON endpoints such
// as files, model catalogs and quota retain the schema-neutral ReadJSON reader.
// An HTTP 200 body with an explicit error is not a successful response or a
// retryable tool-format failure. Preserve only its bounded safe diagnostic.
func ReadResponseJSON(body io.Reader) ([]byte, error) {
	data, err := ReadJSON(body)
	if err != nil {
		return nil, err
	}
	var response map[string]json.RawMessage
	if json.Unmarshal(data, &response) != nil || response == nil {
		return nil, ErrInvalidResponse
	}
	var kind string
	_ = json.Unmarshal(response["type"], &kind)
	rawError := bytes.TrimSpace(response["error"])
	if len(rawError) > 0 && !bytes.Equal(rawError, []byte("null")) || kind == "error" || kind == "response.error" || kind == "response.failed" {
		rejection := describeEventRejection(data, kind)
		if rejection == nil {
			rejection = describeEventRejection(data, "error")
		}
		rejection.Diagnostic.BodyFormat = "json"
		return nil, rejection
	}
	if raw, present := response["status"]; present {
		var status string
		if json.Unmarshal(raw, &status) != nil {
			return nil, ErrInvalidResponse
		}
		switch status {
		case "completed", "failed", "incomplete":
			// A terminal failed/incomplete response without an explicit error
			// retains its status. Tool validation still suppresses its calls.
		case "queued", "in_progress":
			return nil, ErrStreamIncomplete
		default:
			return nil, ErrInvalidResponse
		}
	}
	return data, nil
}
