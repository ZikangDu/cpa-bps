package bpsprotocol

import "testing"

func TestSafeProtocolDiagnosticsContainOnlyCategories(t *testing.T) {
	for _, test := range []struct {
		input, want string
	}{
		{"required_tool_choice_not_satisfied", "required_tool_choice_not_satisfied"},
		{"unsafe_text_tool_response", "unsafe_text_tool_response"},
		{"multiple_text_tool_envelopes", "multiple_text_tool_envelopes"},
		{"unexpected_native_tool", "unexpected_native_tool"},
		{"arguments_schema_mismatch private-schema-value", "arguments_schema_mismatch"},
		{"outer_arguments invalid_json byte_offset=123 private-data", "outer_arguments_invalid"},
		{"code invalid_json byte_offset=123 private-data", "function_arguments_invalid"},
		{"private-tool-name: private-arguments", "invalid_client_tool_relay"},
	} {
		err := relayError(test.input).(*Error)
		stage, reason := err.SafeDiagnostic()
		if stage != "response_tool_conversion" || reason != test.want || err.StatusCode() != 422 || err.Code() != "invalid_tool_call" {
			t.Fatalf("unexpected safe diagnostic: stage=%q reason=%q", stage, reason)
		}
	}
	for _, test := range []struct {
		code, stage string
	}{
		{"bps_invalid_schema", "request_validation"},
		{"invalid_tool_choice", "request_validation"},
		{"bps_invalid_structured_output", "response_validation"},
		{"bps_invalid_event", "response_validation"},
	} {
		err := fail(400, test.code, "private-message").(*Error)
		stage, reason := err.SafeDiagnostic()
		if stage != test.stage || reason != test.code {
			t.Fatalf("unexpected diagnostic: stage=%q reason=%q", stage, reason)
		}
	}
	for _, err := range []*Error{nil, {code: "private-code", message: "private-message"}, {code: "invalid_tool_call", message: "private-message"}} {
		if stage, reason := err.SafeDiagnostic(); stage != "" || reason != "" {
			t.Fatal("unclassified diagnostic exposed private text")
		}
	}
}
