package bpsprotocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func structuredRequest() []byte {
	return []byte(`{
		"model":"fixture","input":"Return a value","stream":true,
		"text":{"format":{"type":"json_schema","name":"fixture","strict":true,
			"schema":{"type":"object","required":["count"],"properties":{"count":{"type":"integer","minimum":3}},"additionalProperties":false}}}
	}`)
}

func structuredTerminal(text string) []byte {
	return jsonBytes(map[string]any{"id": "resp_fixture", "status": "completed", "output": []any{
		map[string]any{"type": "message", "id": "msg_fixture", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": text}}},
	}})
}

func TestStructuredAnswerValidatedBeforeTextEmission(t *testing.T) {
	body, bridge, err := Prepare(structuredRequest(), "structured-scope")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"text":{"format"`) || !strings.Contains(string(body), "The client's output format") {
		t.Fatal("structured format was not translated to instructions")
	}
	var frames []string
	emit := bridge.WrapEmitter(func(frame []byte) error {
		frames = append(frames, string(frame))
		return nil
	})
	if err := emit([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"invalid provisional text\"}\n\n")); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 0 {
		t.Fatal("unvalidated structured text was emitted")
	}
	final := map[string]any{"type": "response.completed", "response": json.RawMessage(structuredTerminal(`{"count":4}`))}
	if err := emit(append(append([]byte("data: "), jsonBytes(final)...), '\n', '\n')); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "response.output_text.delta") ||
		!strings.Contains(joined, `{\"count\":4}`) ||
		strings.Contains(joined, "invalid provisional") {
		t.Fatal("validated structured text was not emitted consistently")
	}
	for _, invalid := range []string{`{"count":2}`, `{"count":3.5}`, `{"count":4,"extra":1}`, "not-json"} {
		if _, err := bridge.Transform(structuredTerminal(invalid)); err == nil {
			t.Fatalf("invalid structured output %s was accepted", invalid)
		}
	}
}

func TestStructuredSchemaRejectsExternalResources(t *testing.T) {
	_, _, err := Prepare([]byte(`{"model":"fixture","input":"x","text":{"format":{"type":"json_schema","name":"fixture","schema":{"$ref":"file:///etc/passwd"}}}}`), "scope")
	if err == nil {
		t.Fatal("external schema was accepted")
	}
}

func TestFunctionSchemaChecksBoundsAndDirectCalls(t *testing.T) {
	_, bridge, err := Prepare([]byte(`{"model":"fixture","input":"x","tools":[{"type":"function","name":"count","parameters":{"type":"object","required":["n"],"properties":{"n":{"type":"integer","minimum":2}}}}]}`), "scope")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args  string
		valid bool
	}{
		{`{"n":4}`, true},
		{`{"n":1}`, false},
		{`{"n":2.5}`, false},
	} {
		result := jsonBytes(map[string]any{"status": "completed", "output": []any{
			map[string]any{"type": "function_call", "name": "count", "call_id": "fixture", "arguments": tc.args},
		}})
		_, err := bridge.Transform(result)
		if (err == nil) != tc.valid {
			t.Fatalf("schema acceptance mismatch for %s", tc.args)
		}
	}
}

func TestStructuredFinalAnswerDoesNotRejectIntermediateNativeTool(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"lookup then return JSON","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"],"additionalProperties":false}}],"tool_choice":"required","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"],"additionalProperties":false}}}}`)
	_, bridge, err := PrepareWithOptions(source, "fixture-scope", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	raw := []byte(`{"status":"completed","output":[{"type":"function_call","name":"run_officejs","id":"fc_fixture","call_id":"call_fixture","arguments":"{\"code\":\"{\\\"name\\\":\\\"lookup\\\",\\\"arguments\\\":{\\\"q\\\":\\\"fixture\\\"}}\",\"references\":[]}","status":"completed"}]}`)
	if _, err := bridge.Transform(raw); err != nil {
		t.Fatal("final answer schema was incorrectly applied to intermediate tool call", err)
	}
}
