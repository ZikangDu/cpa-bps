package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxRejectionBodyBytes = 64 << 10

// Rejection contains only fixed diagnostic text, allowlisted schema paths and
// validated numeric metadata.
// It never includes an upstream message, request header, account identifier, or
// response body. HTTPStatus preserves an upstream error status.
type Rejection struct {
	HTTPStatus        int               `json:"http_status"`
	Code              string            `json:"code"`
	Message           string            `json:"message"`
	RetryAfterSeconds int               `json:"retry_after_seconds,omitempty"`
	BodyFormat        string            `json:"body_format,omitempty"`
	DiagnosticShape   map[string]string `json:"diagnostic_shape,omitempty"`
}

// DescribeRejection consumes at most 64 KiB from a rejected response body. The
// caller still owns and must close the response body. Unknown or malformed
// responses retain a generic diagnosis rather than inferring an account ban.
func DescribeRejection(response *http.Response) Rejection {
	return describeRejection(response, time.Now())
}

func describeRejection(response *http.Response, now time.Time) Rejection {
	result := Rejection{HTTPStatus: http.StatusBadGateway, Code: "bps_upstream_rejected",
		Message: "The upstream rejected the request without a recognized diagnostic code."}
	if response == nil {
		return result
	}
	if response.StatusCode >= 400 && response.StatusCode <= 599 {
		result.HTTPStatus = response.StatusCode
	}
	if response.StatusCode == http.StatusTooManyRequests || response.StatusCode == http.StatusServiceUnavailable {
		result.RetryAfterSeconds = rejectionRetryAfter(response.Header.Get("Retry-After"), now)
	}
	set := func(code, message string) Rejection {
		result.Code, result.Message = code, message
		return result
	}
	if response.StatusCode == http.StatusForbidden && strings.EqualFold(strings.TrimSpace(response.Header.Get("CF-Mitigated")), "challenge") {
		return set("bps_upstream_challenge", "The upstream requires browser verification. Complete the official sign-in flow or contact the service owner.")
	}
	var body map[string]json.RawMessage
	if response.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(response.Body, maxRejectionBodyBytes))
		// At the limit the document might be truncated. Never classify a prefix
		// as a complete upstream response, even if it happens to parse as JSON.
		switch {
		case err != nil:
			result.BodyFormat = "read_error"
		case len(raw) == maxRejectionBodyBytes:
			result.BodyFormat = "at_limit"
		case len(raw) == 0:
			result.BodyFormat = "empty"
		default:
			result.BodyFormat = "non_json"
			var parsed map[string]json.RawMessage
			if json.Unmarshal(raw, &parsed) == nil {
				body = parsed
				result.BodyFormat = "json"
				result.DiagnosticShape = rejectionShape(body)
			}
		}
	}
	var cloudflare bool
	_ = json.Unmarshal(body["cloudflare_error"], &cloudflare)
	if response.StatusCode == http.StatusForbidden && cloudflare && rejectionCode(body["error_code"]) == "1010" {
		return set("bps_upstream_access_blocked", "The upstream access policy blocked this client (Cloudflare 1010). The service owner must resolve this access restriction.")
	}
	if response.StatusCode == http.StatusForbidden && isExactSecurityRejection(body) {
		return set("bps_upstream_security_blocked", "The upstream security systems blocked this request. Check access in the official service or contact its support team.")
	}
	if response.StatusCode == http.StatusForbidden && isExactUsagePolicyRejection(body) {
		return set("bps_usage_policy_blocked", "The upstream usage policy blocked this request. Check the official service or contact support; reset credits cannot remove this restriction.")
	}
	if response.StatusCode == http.StatusUnprocessableEntity {
		if isExactRequestBodyRejection(body) {
			return set("bps_request_validation_failed", "The upstream rejected the request body. Check that its structure matches the supported request format.")
		}
		if message, ok := requestValidationMessage(body["detail"]); ok {
			return set("bps_request_validation_failed", message)
		}
	}
	// Only exact machine-readable codes are accepted. Human text is neither
	// copied nor searched, since it may contain secrets or misleading content.
	for _, code := range rejectionMachineCodes(body) {
		switch code {
		case "invalid_token", "token_expired", "expired_token", "invalid_api_key", "invalid_grant", "token_revoked":
			return set("bps_reauthorization_required", "The upstream rejected this OAuth credential. Authorize this account again.")
		case "insufficient_scope", "missing_scope", "scope_missing":
			return set("bps_insufficient_scope", "This OAuth grant lacks the required permission. Reauthorize the account with the required scope.")
		case "account_id_mismatch", "workspace_mismatch", "invalid_account_id":
			return set("bps_account_context_mismatch", "The selected account or workspace does not match this OAuth grant.")
		case "user_blocked", "bps_user_blocked":
			return set("bps_user_blocked", "The upstream denied BPS access for this user. Check the official service or contact its support team.")
		case "account_deactivated", "account_suspended", "account_disabled":
			return set("bps_account_unavailable", "The upstream reports that this account is unavailable. Check the account in the official service.")
		case "account_not_found", "account_not_authorized", "workspace_access_denied", "permission_denied", "insufficient_permissions":
			return set("bps_account_access_denied", "The upstream denied this account or workspace permission to use this operation.")
		case "insufficient_quota", "quota_exceeded", "usage_limit_reached", "billing_hard_limit_reached", "credits_exhausted":
			return set("bps_quota_exhausted", "The upstream reports that this account has no quota available for this operation.")
		case "rate_limit_exceeded", "too_many_requests", "rate_limit_error":
			return set("bps_rate_limited", "The upstream rate limit was reached. Wait for the reported recovery time before trying again.")
		case "authentication_error":
			return set("bps_credential_rejected", "The upstream did not accept this credential. Check the account authorization status.")
		case "model_not_found", "model_not_supported":
			return set("bps_model_unavailable", "The upstream reports that this model is unavailable for the selected account.")
		}
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return set("bps_rate_limited", "The upstream rate limit was reached. Wait for the reported recovery time before trying again.")
	}
	if response.StatusCode == http.StatusUnauthorized {
		return set("bps_credential_rejected", "The upstream did not accept this credential. Check the account authorization status.")
	}
	return result
}

// This uncoded wrapper was observed for a BPS request-schema rejection. Match
// the complete signature, not fragments of potentially private upstream text;
// it establishes no particular field, quota or credential diagnosis.
func isExactRequestBodyRejection(body map[string]json.RawMessage) bool {
	var detail map[string]json.RawMessage
	if len(body) != 1 || json.Unmarshal(body["error"], &detail) != nil || len(detail) != 4 {
		return false
	}
	var message, kind string
	return json.Unmarshal(detail["message"], &message) == nil && message == "422: Invalid request body." &&
		json.Unmarshal(detail["type"], &kind) == nil && kind == "server_error" &&
		strings.TrimSpace(string(detail["code"])) == "null" && strings.TrimSpace(string(detail["param"])) == "null"
}

// FastAPI's validation message, input and ctx may include arbitrary request
// content. Only exact validation codes and bounded, known schema locations
// qualify; a partly recognized or malformed array must stay generic.
func requestValidationMessage(raw json.RawMessage) (string, bool) {
	const maxDetails, maxShown = 128, 4
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil || len(entries) == 0 || len(entries) > maxDetails {
		return "", false
	}
	fields := make([]string, 0, maxShown)
	seen := make(map[string]struct{})
	more := false
	for _, entry := range entries {
		var detail struct {
			Type string            `json:"type"`
			Loc  []json.RawMessage `json:"loc"`
		}
		if json.Unmarshal(entry, &detail) != nil || !knownValidationType(detail.Type) {
			return "", false
		}
		path, ok := validationFieldPath(detail.Loc)
		if !ok {
			return "", false
		}
		if _, exists := seen[path]; exists {
			continue
		}
		seen[path] = struct{}{}
		if len(fields) < maxShown {
			fields = append(fields, path+" ("+detail.Type+")")
		} else {
			more = true
		}
	}
	message := "The upstream rejected the request schema. Invalid fields: " + strings.Join(fields, "; ") + "."
	if more {
		message += " Additional validation fields were omitted."
	}
	return message, true
}

func knownValidationType(value string) bool {
	switch value {
	case "missing", "extra_forbidden", "literal_error", "enum", "value_error", "assertion_error",
		"string_type", "string_too_short", "string_too_long", "string_pattern_mismatch",
		"int_type", "int_parsing", "int_from_float", "float_type", "float_parsing", "finite_number",
		"bool_type", "bool_parsing", "none_required", "list_type", "tuple_type", "dict_type",
		"model_type", "model_attributes_type", "union_tag_invalid", "union_tag_not_found",
		"greater_than", "greater_than_equal", "less_than", "less_than_equal", "multiple_of",
		"too_short", "too_long", "json_invalid", "json_type", "url_type", "url_parsing", "url_scheme":
		return true
	default:
		return false
	}
}

func validationFieldPath(parts []json.RawMessage) (string, bool) {
	if len(parts) == 0 || len(parts) > 16 {
		return "", false
	}
	var root string
	if json.Unmarshal(parts[0], &root) != nil || root != "body" {
		return "", false
	}
	var path strings.Builder
	path.WriteString("body")
	for _, part := range parts[1:] {
		var name string
		if json.Unmarshal(part, &name) == nil {
			if !knownValidationField(name) {
				return "", false
			}
			path.WriteByte('.')
			path.WriteString(name)
		} else {
			var index int
			if json.Unmarshal(part, &index) != nil || index < 0 || index > 65535 {
				return "", false
			}
			path.WriteByte('[')
			path.WriteString(strconv.Itoa(index))
			path.WriteByte(']')
		}
		if path.Len() > 128 {
			return "", false
		}
	}
	return path.String(), true
}

func knownValidationField(value string) bool {
	switch value {
	case "model", "model_selection", "input", "instructions", "stream", "store", "tools", "tool_choice",
		"parallel_tool_calls", "reasoning", "reasoning_effort", "effort", "summary", "content", "type",
		"role", "text", "id", "status", "phase", "call_id", "name", "namespace", "arguments", "output",
		"encrypted_content", "encrypted_function_args", "image_url", "file_id", "detail", "format", "schema",
		"strict", "description", "parameters", "required", "properties", "additionalProperties", "items",
		"context_management", "compact_threshold", "prompt_cache_key", "prompt_cache_retention", "metadata",
		"client_metadata", "bps_tools_version_id", "language", "grammar", "syntax", "source", "mime_type",
		"url", "data", "include", "max_output_tokens", "temperature", "top_p", "stop", "service_tier",
		"message", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output",
		"compaction", "agent_message", "input_text", "output_text", "input_image", "input_file", "refusal",
		"summary_text", "reasoning_text":
		return true
	default:
		return false
	}
}

// Only these complete, known security messages qualify for this 403 diagnosis.
// Do not use substring, case-folded, or whitespace-normalized matching: quoted
// or attacker-supplied surrounding text must not manufacture this diagnosis.
func isExactSecurityRejection(body map[string]json.RawMessage) bool {
	var detail map[string]json.RawMessage
	if json.Unmarshal(body["error"], &detail) != nil || detail == nil || rejectionCode(detail["type"]) != "invalid_request_error" {
		return false
	}
	if raw, exists := detail["code"]; exists {
		var code *string
		if json.Unmarshal(raw, &code) != nil || code != nil && *code != "" {
			return false
		}
	}
	var message string
	if json.Unmarshal(detail["message"], &message) != nil {
		return false
	}
	return message == "Your request was blocked by our security systems. Please try again later." ||
		message == "This request was blocked by our security systems. Please try again later."
}

// This complete message was observed in an uncoded BPS 403 response. It is not
// an OAuth or quota diagnosis, and unknown error.type values do not change it.
func isExactUsagePolicyRejection(body map[string]json.RawMessage) bool {
	var detail map[string]json.RawMessage
	if json.Unmarshal(body["error"], &detail) != nil || detail == nil {
		return false
	}
	if raw, exists := detail["code"]; exists {
		var code *string
		if json.Unmarshal(raw, &code) != nil || code != nil && *code != "" {
			return false
		}
	}
	var message string
	return json.Unmarshal(detail["message"], &message) == nil &&
		message == "403: This request was blocked by our usage policy."
}

// BPS may wrap machine-readable errors in a FastAPI detail object. Inspect
// only error/detail objects and code/type/error strings, with bounded depth.
func rejectionMachineCodes(body map[string]json.RawMessage) []string {
	codes := []string{}
	var inspect func(map[string]json.RawMessage, int)
	inspect = func(current map[string]json.RawMessage, depth int) {
		for _, key := range []string{"error", "detail"} {
			raw := current[key]
			var nested map[string]json.RawMessage
			if depth < 2 && json.Unmarshal(raw, &nested) == nil && nested != nil {
				inspect(nested, depth+1)
			} else if key == "error" {
				if value := rejectionCode(raw); value != "" {
					codes = append(codes, value)
				}
			}
		}
		for _, key := range []string{"code", "type"} {
			if value := rejectionCode(current[key]); value != "" {
				codes = append(codes, value)
			}
		}
	}
	inspect(body, 0)
	return codes
}

// Diagnostic paths themselves are allowlisted, not copied from arbitrary JSON
// keys. Values are type labels only, so unknown errors remain inspectable
// without emitting messages, identities, tokens, or attacker-selected strings.
func rejectionShape(body map[string]json.RawMessage) map[string]string {
	shape := make(map[string]string)
	var inspect func(map[string]json.RawMessage, string, int)
	inspect = func(current map[string]json.RawMessage, prefix string, depth int) {
		for _, key := range []string{"error", "code", "type", "message", "detail", "title", "status", "error_code", "error_name", "error_category", "cloudflare_error", "retryable", "owner_action_required"} {
			raw, exists := current[key]
			if !exists || len(raw) == 0 {
				continue
			}
			path := prefix + key
			trimmed := strings.TrimSpace(string(raw))
			switch trimmed[0] {
			case '{':
				shape[path] = "object"
				if depth < 2 && (key == "error" || key == "detail") {
					var nested map[string]json.RawMessage
					if json.Unmarshal(raw, &nested) == nil {
						inspect(nested, path+".", depth+1)
					}
				}
			case '[':
				shape[path] = "array"
			case '"':
				shape[path] = "string"
			case 't', 'f':
				shape[path] = "boolean"
			case 'n':
				shape[path] = "null"
			default:
				shape[path] = "number"
			}
		}
	}
	inspect(body, "", 0)
	return shape
}

func rejectionCode(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		return value
	}
	if string(raw) == "1010" {
		return "1010"
	}
	return ""
}

func rejectionRetryAfter(value string, now time.Time) int {
	value = strings.TrimSpace(value)
	if seconds, err := strconv.ParseUint(value, 10, 31); err == nil {
		return int(seconds)
	}
	next, err := http.ParseTime(value)
	if err != nil || !next.After(now) {
		return 0
	}
	delay := next.Sub(now)
	seconds := delay / time.Second
	if delay%time.Second != 0 {
		seconds++
	}
	return int(min(seconds, time.Duration(1<<31-1)))
}
