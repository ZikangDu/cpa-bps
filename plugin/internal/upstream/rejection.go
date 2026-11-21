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

// Rejection contains only fixed diagnostic text and validated numeric metadata.
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

// These two complete, known service messages are the only human-text exception.
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
