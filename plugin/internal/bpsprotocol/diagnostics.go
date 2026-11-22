package bpsprotocol

import "strings"

// SafeDiagnostic exposes only fixed protocol categories. It never returns
// upstream text, tool names, schema paths, arguments, or a request payload.
func (e *Error) SafeDiagnostic() (stage, reason string) {
	if e == nil {
		return "", ""
	}
	if e.code == "invalid_tool_call" && e.diagnosticReason != "" {
		return "response_tool_conversion", e.diagnosticReason
	}
	switch e.code {
	case "bps_invalid_tool_transport", "bps_invalid_tools_version", "bps_invalid_request",
		"bps_invalid_schema", "bps_invalid_text", "unsupported_continuation", "unsupported_service_tier",
		"unsupported_model", "invalid_tool_choice", "bps_invalid_agent_message", "invalid_text_config",
		"invalid_text_format", "unsupported_text_format":
		return "request_validation", e.code
	case "invalid_upstream_response", "bps_invalid_event", "bps_invalid_response", "bps_invalid_structured_output":
		return "response_validation", e.code
	default:
		return "", ""
	}
}

func safeRelayReason(reason string) string {
	// Parser failures carry byte offsets internally. Publish the fixed parser
	// category only; never depend on an error's free-form suffix being safe.
	first, _, _ := strings.Cut(reason, " ")
	switch first {
	case "outer_arguments":
		return "outer_arguments_invalid"
	case "code":
		return "function_arguments_invalid"
	case "outer_not_transport", "code_invalid", "function_marker_invalid", "function_metadata_invalid",
		"function_metadata_duplicates_payload", "function_marker_references_invalid", "function_marker_destructive_invalid",
		"custom_marker_invalid", "references_must_select_one_tool", "invalid_client_tool_reference",
		"tool_not_allowed_by_tool_choice", "tool_not_in_catalog", "relay_tool_kind_mismatch",
		"function_transport_not_in_catalog", "missing_call_id", "conflicting_tool_reference", "arguments_schema_mismatch",
		"direct_tool_not_in_catalog", "direct_tool_kind_mismatch", "direct_arguments_invalid", "direct_custom_input_invalid",
		"direct_tool_kind_unavailable", "duplicate_call_id", "required_tool_choice_not_satisfied", "parallel_tool_calls_disabled",
		"unexpected_native_tool", "text_tool_not_in_catalog", "text_arguments_invalid", "text_custom_input_invalid", "multiple_text_tool_envelopes", "unsafe_text_tool_response":
		return first
	default:
		return "invalid_client_tool_relay"
	}
}
