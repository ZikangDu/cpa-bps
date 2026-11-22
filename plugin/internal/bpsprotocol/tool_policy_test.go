package bpsprotocol

import (
	"bytes"
	"testing"
)

func TestMalformedToolPoliciesAreRejectedBeforeGeneration(t *testing.T) {
	for _, mode := range []string{"native", "text"} {
		for _, policy := range []any{
			"surprise", true, 1, []any{}, map[string]any{}, map[string]any{"type": "surprise"},
			map[string]any{"type": "function"}, map[string]any{"type": "function", "name": "unknown"},
			map[string]any{"type": "function", "name": "lookup", "namespace": true},
			map[string]any{"type": "allowed_tools", "mode": "surprise", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}},
			map[string]any{"type": "allowed_tools", "mode": true, "tools": []any{}},
			map[string]any{"type": "allowed_tools", "tools": true},
			map[string]any{"type": "allowed_tools", "tools": nil},
			map[string]any{"type": "allowed_tools", "tools": []any{nil}},
			map[string]any{"type": "allowed_tools", "tools": []any{map[string]any{"type": "function", "name": "unknown"}}},
		} {
			source := jsonBytes(map[string]any{"model": "fixture", "input": "fixture", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}, "tool_choice": policy})
			before := bytes.Clone(source)
			payload, bridge, err := PrepareWithOptions(source, "invalid-policy", Options{ToolTransport: mode})
			problem, ok := err.(*Error)
			if !ok || problem.StatusCode() != 400 || problem.Code() != "invalid_tool_choice" || payload != nil || bridge != nil {
				t.Fatalf("malformed policy reached payload preparation: mode=%s policy=%v error=%v", mode, policy, err)
			}
			if !bytes.Equal(source, before) {
				t.Fatal("policy rejection mutated source")
			}
		}
		for _, parallel := range []any{"false", 0, []any{}, map[string]any{}} {
			source := jsonBytes(map[string]any{"model": "fixture", "input": "fixture", "parallel_tool_calls": parallel})
			if _, _, err := PrepareWithOptions(source, "invalid-parallel", Options{ToolTransport: mode}); err == nil || err.(*Error).Code() != "bps_invalid_parallel_tool_calls" {
				t.Fatal("malformed parallel policy was silently ignored")
			}
		}
	}
}

func TestValidToolPoliciesKeepDynamicNestedSelection(t *testing.T) {
	selection := map[string]any{"type": "function", "namespace": "ops.sheet", "name": "lookup"}
	tools := []any{map[string]any{"type": "namespace", "name": "ops", "tools": []any{map[string]any{"type": "namespace", "name": "sheet", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}}}}}
	for _, mode := range []string{"native", "text"} {
		for _, choice := range []any{nil, "", "auto", "none", "required", selection,
			map[string]any{"type": "allowed_tools", "tools": []any{selection}},
			map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{selection}},
			map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{selection}},
			map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{}},
		} {
			source := jsonBytes(map[string]any{"model": "fixture", "input": []any{messageItem("user", "fixture"), map[string]any{"type": "additional_tools", "tools": tools}}, "tool_choice": choice, "parallel_tool_calls": nil})
			if _, _, err := PrepareWithOptions(source, "valid-policy", Options{ToolTransport: mode}); err != nil {
				t.Fatalf("valid dynamic nested policy rejected: mode=%s choice=%v error=%v", mode, choice, err)
			}
		}
	}
}
