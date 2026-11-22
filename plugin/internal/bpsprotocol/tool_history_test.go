package bpsprotocol

import (
	"bytes"
	"strings"
	"testing"
)

func TestHistoricalObjectArgumentsArePreservedAcrossBothTransports(t *testing.T) {
	// Current declarations intentionally differ from the retired historical
	// tool; replay validates value shape without imposing today's schema.
	source := []byte(`{"model":"fixture","tools":[{"type":"function","name":"current","parameters":{"type":"object","properties":{"current_value":{"type":"integer"}},"required":["current_value"]}}],"tool_choice":"none","input":[{"role":"user","content":"Earlier fixture"},{"type":"function_call","name":"retired","namespace":"old.tools","call_id":"call_history","arguments":{"text":" exact old text ","integer":9007199254740993}},{"type":"function_call_output","call_id":"call_history","output":"old result"},{"role":"user","content":"Continue from history"}]}`)
	before := bytes.Clone(source)
	for _, mode := range []string{"native", "text"} {
		prepared, _, err := PrepareWithOptions(source, "history-object/"+mode, Options{ToolTransport: mode})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := parseRelayObject(string(prepared))
		found := false
		for _, raw := range body["input"].([]any) {
			item := objectValue(raw)
			var envelope map[string]any
			if mode == "native" && item["type"] == "function_call" {
				outer, _ := parseRelayObject(item["arguments"])
				envelope, _ = parseRelayObject(outer["code"])
				if envelope["name"] != "old.tools.retired" || item["call_id"] != "call_history" {
					t.Fatal("native history lost the retired tool identity")
				}
			} else if mode == "text" && item["role"] == "assistant" {
				envelope, _ = parseRelayObject(itemText(item["content"]))
				if envelope["client_tool"] != "old.tools.retired" {
					t.Fatal("text history lost the retired tool identity")
				}
			} else {
				continue
			}
			arguments := objectValue(envelope["arguments"])
			if arguments["text"] != " exact old text " || string(jsonBytes(arguments["integer"])) != "9007199254740993" {
				t.Fatal("object arguments were dropped, trimmed or rounded")
			}
			found = true
		}
		if !found || !strings.Contains(string(prepared), "old result") || !bytes.Equal(before, source) {
			t.Fatal("historical call/result was lost or caller input mutated")
		}
	}
}

func TestMalformedHistoricalArgumentsAreRejectedInsteadOfDiscarded(t *testing.T) {
	for _, mode := range []string{"native", "text"} {
		for _, args := range []any{nil, true, 7, []any{}, "", "{", "[]", "null", "{} {}"} {
			source := jsonBytes(map[string]any{"model": "fixture", "input": []any{map[string]any{"type": "function_call", "name": "old", "call_id": "call_old", "arguments": args}, map[string]any{"type": "function_call_output", "call_id": "call_old", "output": "result"}}})
			if _, _, err := PrepareWithOptions(source, "bad-history", Options{ToolTransport: mode}); err == nil || err.(*Error).Code() != "bps_invalid_tool_history" {
				t.Fatalf("malformed function history was dropped or altered: mode=%s args=%v err=%v", mode, args, err)
			}
		}
		for _, input := range []any{nil, true, 7, []any{}, map[string]any{}} {
			source := jsonBytes(map[string]any{"model": "fixture", "input": []any{map[string]any{"type": "custom_tool_call", "name": "old", "call_id": "call_old", "input": input}}})
			if _, _, err := PrepareWithOptions(source, "bad-custom-history", Options{ToolTransport: mode}); err == nil || err.(*Error).Code() != "bps_invalid_tool_history" {
				t.Fatal("non-string custom input was replaced with empty text")
			}
		}
		for _, input := range []string{"", " exact raw\n"} {
			source := jsonBytes(map[string]any{"model": "fixture", "input": []any{map[string]any{"type": "custom_tool_call", "name": "old", "call_id": "call_old", "input": input}}})
			if _, _, err := PrepareWithOptions(source, "valid-custom-history", Options{ToolTransport: mode}); err != nil {
				t.Fatal("valid raw custom history was rejected", err)
			}
		}
	}
}
