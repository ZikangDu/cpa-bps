package bpsprotocol

import (
	"encoding/json"
	"testing"
)

func TestTextToolRelayValidatesAndReplays(t *testing.T) {
	raw := []byte(`{"model":"fixture","input":"look up abc","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}],"tool_choice":{"type":"function","name":"lookup"}}`)
	_, bridge, err := PrepareWithTextTools(raw, "account-scope")
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{
		messageItem("assistant", `{"client_tool":"lookup","arguments":{"value":"abc"}}`),
	}}
	got, err := bridge.Transform(jsonBytes(result))
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct{ Output []map[string]any }
	_ = json.Unmarshal(got, &decoded)
	call := decoded.Output[0]
	if call["type"] != "function_call" || call["name"] != "lookup" || call["arguments"] != `{"value":"abc"}` || call["call_id"] == "" {
		t.Fatal("invalid transformed tool call")
	}
	var source map[string]any
	_ = json.Unmarshal(raw, &source)
	source["tool_choice"] = "none"
	source["input"] = []any{call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "VALUE"}}
	prepared, _, err := PrepareWithTextTools(jsonBytes(source), "account-scope")
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Input []map[string]any }
	_ = json.Unmarshal(prepared, &body)
	for _, item := range body.Input {
		if item["type"] != "message" {
			t.Fatal("text tool history was not lowered to messages")
		}
	}
	result["output"] = []any{messageItem("assistant", `{"client_tool":"undeclared","arguments":{}}`)}
	if _, err := bridge.Transform(jsonBytes(result)); err == nil {
		t.Fatal("undeclared tool was accepted")
	}
}

func TestTextToolRelayAlsoValidatesStructuredAnswers(t *testing.T) {
	raw := []byte(`{"model":"fixture","input":"answer as JSON","tools":[{"type":"custom","name":"note"}],"tool_choice":"none","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"ok":{"const":true}},"required":["ok"],"additionalProperties":false}}}}`)
	_, bridge, err := PrepareWithTextTools(raw, "scope")
	if err != nil {
		t.Fatal(err)
	}
	makeResponse := func(text string) []byte {
		return jsonBytes(map[string]any{"status": "completed", "output": []any{messageItem("assistant", text)}})
	}
	if _, err := bridge.Transform(makeResponse(`{"ok":false}`)); err == nil {
		t.Fatal("tool context bypassed the structured response schema")
	}
	if _, err := bridge.Transform(makeResponse(`{"ok":true}`)); err != nil {
		t.Fatal(err)
	}
}
