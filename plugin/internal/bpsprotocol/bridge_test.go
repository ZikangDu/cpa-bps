package bpsprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareNativeBodyAndScope(t *testing.T) {
	request := []byte(`{"model":"fixture-model","input":"hello","instructions":"Be concise","reasoning":{"effort":"high"},"service_tier":"priority","stream":false}`)
	raw, _, err := Prepare(request, "account-a/caller-one")
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	if body["model"] != "fixture-model" || body["model_selection"] != "explicit" ||
		body["stream"] != true || body["store"] != false || body["reasoning_effort"] != "high" ||
		body["service_tier"] != "priority" {
		t.Fatal("request options were not translated")
	}
	if body["reasoning"] != nil || body["instructions"] != nil {
		t.Fatal("client-only fields reached the native body")
	}
	if !strings.Contains(string(raw), "Be concise") || !strings.Contains(string(raw), "hello") {
		t.Fatal("input or instructions were dropped")
	}
	other, _, err := Prepare(request, "account-b/caller-one")
	if err != nil {
		t.Fatal(err)
	}
	var otherBody map[string]any
	_ = json.Unmarshal(other, &otherBody)
	if body["metadata"].(map[string]any)["task_id"] == otherBody["metadata"].(map[string]any)["task_id"] {
		t.Fatal("upstream task ID crossed account scopes")
	}
}

func TestToolTranslationAndScopedReplay(t *testing.T) {
	source := map[string]any{
		"model": "fixture", "input": "Call echo",
		"tools": []any{map[string]any{"type": "function", "name": "echo",
			"parameters": map[string]any{"type": "object", "required": []any{"message"},
				"properties": map[string]any{"message": map[string]any{"type": "string"}}}}},
	}
	_, bridge, err := Prepare(jsonBytes(source), "scope-a")
	if err != nil {
		t.Fatal(err)
	}
	native := map[string]any{
		"type": "function_call", "id": "fc_native", "call_id": "call_shared", "name": "run_officejs",
		"arguments": string(jsonBytes(map[string]any{
			"code": `{"message":"hello","integer":9007199254740993}`, "references": []any{"echo"},
			"destructive": false, "summary": "fixture-native-summary",
		})),
	}
	terminal, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
	if err != nil {
		t.Fatal(err)
	}
	parsed, _ := parseRelayObject(string(terminal))
	client := parsed["output"].([]any)[0].(map[string]any)
	if client["name"] != "echo" || client["type"] != "function_call" ||
		!strings.Contains(client["arguments"].(string), "9007199254740993") {
		t.Fatal("native tool was not translated exactly")
	}
	history := []any{client, map[string]any{"type": "function_call_output", "call_id": "call_shared", "output": "ok"}}
	sameScope := translateInputItems(history, "scope-a")
	if sameScope[0].(map[string]any)["arguments"] != native["arguments"] {
		t.Fatal("same-scope history lost native relay")
	}
	otherScope := translateInputItems(history, "scope-b")
	if strings.Contains(otherScope[0].(map[string]any)["arguments"].(string), "fixture-native-summary") {
		t.Fatal("native relay cache crossed scope")
	}
	modified := cloneObject(client)
	modified["arguments"] = `{"message":"changed"}`
	if rememberedNativeCall(modified, "scope-a") != nil {
		t.Fatal("modified historical call matched old native arguments")
	}
}

func TestWrapEmitterLeavesTextIncrementalAndTranslatesTerminal(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"Use the tool","tools":[{"type":"custom","name":"patch"}]}`)
	_, bridge, err := Prepare(source, "scope")
	if err != nil {
		t.Fatal(err)
	}
	var frames [][]byte
	emit := bridge.WrapEmitter(func(frame []byte) error {
		frames = append(frames, frame)
		return nil
	})
	delta := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"working\"}\n\n")
	if err := emit(delta); err != nil || len(frames) != 1 || string(frames[0]) != string(delta) {
		t.Fatal("text delta was delayed or changed")
	}
	native := map[string]any{
		"type": "function_call", "id": "fc_native", "call_id": "call_patch", "name": "run_officejs",
		"arguments": string(jsonBytes(map[string]any{"references": []any{"patch"}, "code": "raw\npatch"})),
	}
	event := map[string]any{"type": "response.completed", "response": map[string]any{
		"status": "completed", "output": []any{native},
	}}
	frame := append([]byte("data: "), jsonBytes(event)...)
	if err := emit(append(frame, '\n', '\n')); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 2 || !strings.Contains(string(frames[1]), `"custom_tool_call"`) ||
		strings.Contains(string(frames[1]), "run_officejs") {
		t.Fatal("terminal relay was not translated before delivery")
	}
}

func TestRejectsUndeclaredNativeTool(t *testing.T) {
	_, bridge, err := Prepare([]byte(`{"model":"fixture","input":"hello"}`), "scope")
	if err != nil {
		t.Fatal(err)
	}
	_, err = bridge.Transform([]byte(`{"status":"completed","output":[{"type":"function_call","name":"run_officejs","call_id":"x","arguments":"{}"}]}`))
	if err == nil {
		t.Fatal("undeclared tool must not reach the client")
	}
}
