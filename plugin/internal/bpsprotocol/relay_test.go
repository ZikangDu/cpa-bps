package bpsprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCurrentAndLegacyToolRelays(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"call","tools":[
		{"type":"function","name":"echo","parameters":{"type":"object","required":["text"],"properties":{"text":{"type":"string"}}}},
		{"type":"custom","name":"patch"}
	]}`)
	_, bridge, err := Prepare(source, "relay-scope")
	if err != nil {
		t.Fatal(err)
	}
	rawCustom := "  *** Begin Patch\r\n+text(\"hello\");\n*** End Patch\t\n"
	for _, tc := range []struct {
		name    string
		code    string
		summary string
		refs    []any
		kind    string
		want    string
	}{
		{"current function", `{"name":"echo","arguments":{"text":"hello"}}`, "Call echo", []any{}, "function_call", `{"text":"hello"}`},
		{"legacy function", `{"text":"hello"}`, "Call echo", []any{"echo"}, "function_call", `{"text":"hello"}`},
		{"current custom", rawCustom, "codex2api.custom/patch", []any{}, "custom_tool_call", rawCustom},
		{"legacy custom", rawCustom, "Call patch", []any{"patch"}, "custom_tool_call", rawCustom},
		{"legacy custom JSON", `{"name":"echo","arguments":{"text":"literal"}}`, "Call patch", []any{"patch"}, "custom_tool_call", `{"name":"echo","arguments":{"text":"literal"}}`},
		{"custom replay envelope", `{"name":"patch","input":"literal raw"}`, "Replay", []any{}, "custom_tool_call", "literal raw"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "call_" + tc.name,
				"arguments": string(jsonBytes(map[string]any{"code": tc.code, "summary": tc.summary, "references": tc.refs}))}
			raw := jsonBytes(map[string]any{"status": "completed", "output": []any{native}})
			result, err := bridge.Transform(raw)
			if err != nil {
				t.Fatal(err)
			}
			final, _ := parseRelayObject(string(result))
			call := final["output"].([]any)[0].(map[string]any)
			field := "arguments"
			if tc.kind == "custom_tool_call" {
				field = "input"
			}
			if call["type"] != tc.kind || call[field] != tc.want {
				t.Fatal("relay payload or kind changed")
			}
		})
	}
}

func TestRejectsCustomMarkerForFunctionTool(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"call","tools":[{"type":"function","name":"echo"}]}`)
	_, bridge, err := Prepare(source, "scope")
	if err != nil {
		t.Fatal(err)
	}
	native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "fixture",
		"arguments": string(jsonBytes(map[string]any{"code": `{}`, "summary": "codex2api.custom/echo", "references": []any{}}))}
	if _, err := bridge.Transform(jsonBytes(map[string]any{"output": []any{native}})); err == nil {
		t.Fatal("custom marker routed a function call")
	}
}

func TestCurrentToolPromptAndReplay(t *testing.T) {
	request := []byte(`{"model":"fixture","input":"call echo","tool_choice":"required","tools":[{"type":"function","name":"echo"}]}`)
	body, bridge, err := Prepare(request, "scope")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "references=[]") ||
		!strings.Contains(string(body), "This response must contain a call") ||
		strings.Contains(string(body), "references to an array containing exactly one") {
		t.Fatal("prompt did not use the current relay contract")
	}
	call := map[string]any{"type": "function_call", "name": "echo", "call_id": "direct", "arguments": `{"text":"hi"}`}
	if _, err := bridge.Transform(jsonBytes(map[string]any{"output": []any{call}})); err != nil {
		t.Fatal(err)
	}
	replay := translateInputItems([]any{call}, "scope")[0].(map[string]any)
	if replay["name"] != "run_officejs" {
		t.Fatal("direct client tool was not wrapped for native history")
	}
	outer, _ := parseRelayObject(replay["arguments"])
	var code map[string]json.RawMessage
	if json.Unmarshal([]byte(outer["code"].(string)), &code) != nil ||
		string(code["name"]) != `"echo"` || string(code["arguments"]) != `{"text":"hi"}` {
		t.Fatal("history relay did not use the current envelope")
	}
}
