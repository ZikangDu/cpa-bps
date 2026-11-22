package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

// EventRejection describes an error delivered after HTTP 200 established an
// SSE stream. HTTPStatus is the semantic status of an exact machine code, not
// a replacement for the already-sent HTTP status. No upstream text is retained.
type EventRejection struct {
	Diagnostic Rejection
}

func (e *EventRejection) Error() string { return e.Diagnostic.Message }
func (e *EventRejection) Unwrap() error { return ErrUpstreamEvent }

func describeEventRejection(raw []byte, kind string) *EventRejection {
	if kind != "error" && kind != "response.error" && kind != "response.failed" {
		return nil
	}
	var body map[string]json.RawMessage
	_ = json.Unmarshal(raw, &body)
	if kind == "response.failed" || kind == "response.error" && len(body["response"]) > 0 {
		var response map[string]json.RawMessage
		_ = json.Unmarshal(body["response"], &response)
		body = response
	}
	for _, code := range rejectionMachineCodes(body) {
		status := eventMachineStatus(code)
		if status == 0 {
			continue
		}
		// Reuse HTTP rejection's fixed allowlisted messages. The temporary body
		// contains only the recognized code, never the original event payload.
		encoded, _ := json.Marshal(map[string]string{"code": code})
		diagnostic := DescribeRejection(&http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(encoded)))})
		diagnostic.BodyFormat = "sse"
		diagnostic.DiagnosticShape = nil
		return &EventRejection{Diagnostic: diagnostic}
	}
	if kind == "response.failed" {
		// Preserve the existing terminal response contract for failures that
		// have no recognized machine code. Never infer quota from human text.
		return nil
	}
	return &EventRejection{Diagnostic: Rejection{
		HTTPStatus: http.StatusBadGateway, Code: "bps_upstream_rejected", BodyFormat: "sse",
		Message: "The upstream rejected the request without a recognized diagnostic code.",
	}}
}

func eventMachineStatus(code string) int {
	switch code {
	case "invalid_token", "token_expired", "expired_token", "invalid_api_key", "invalid_grant", "token_revoked", "authentication_error":
		return http.StatusUnauthorized
	case "insufficient_scope", "missing_scope", "scope_missing", "account_id_mismatch", "workspace_mismatch", "invalid_account_id",
		"user_blocked", "bps_user_blocked", "account_deactivated", "account_suspended", "account_disabled",
		"account_not_found", "account_not_authorized", "workspace_access_denied", "permission_denied", "insufficient_permissions":
		return http.StatusForbidden
	case "insufficient_quota", "quota_exceeded", "usage_limit_reached", "billing_hard_limit_reached", "credits_exhausted",
		"rate_limit_exceeded", "too_many_requests", "rate_limit_error":
		return http.StatusTooManyRequests
	case "model_not_found", "model_not_supported":
		return http.StatusNotFound
	default:
		return 0
	}
}

func sanitizeEventRejection(raw []byte, kind string, diagnostic Rejection) ([]byte, json.RawMessage) {
	var event map[string]json.RawMessage
	_ = json.Unmarshal(raw, &event)
	safe, _ := json.Marshal(map[string]any{"code": diagnostic.Code, "message": diagnostic.Message, "type": "server_error", "param": nil})
	if kind == "response.failed" || kind == "response.error" && len(event["response"]) > 0 {
		var response map[string]json.RawMessage
		_ = json.Unmarshal(event["response"], &response)
		if response == nil {
			response = map[string]json.RawMessage{}
		}
		response["error"] = safe
		result, _ := json.Marshal(response)
		event["response"] = result
		encoded, _ := json.Marshal(event)
		return encoded, result
	}
	// Error events may use either the flat Responses envelope or an error
	// object. Keep only protocol identity/ordering fields plus the safe error.
	out := map[string]json.RawMessage{}
	for _, key := range []string{"type", "id", "response_id", "sequence_number", "status"} {
		if value := event[key]; len(value) > 0 {
			out[key] = value
		}
	}
	if _, nested := event["error"]; nested {
		out["error"] = safe
	} else {
		out["code"], _ = json.Marshal(diagnostic.Code)
		out["message"], _ = json.Marshal(diagnostic.Message)
		out["param"] = json.RawMessage("null")
	}
	encoded, _ := json.Marshal(out)
	return encoded, nil
}
