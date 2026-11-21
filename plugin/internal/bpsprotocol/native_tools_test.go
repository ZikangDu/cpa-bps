package bpsprotocol

import (
	"encoding/json"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/compat"
)

func native054Source() map[string]any {
	return map[string]any{"model": "fixture", "input": "Use a declared tool", "tools": []any{
		map[string]any{"type": "function", "name": "python", "parameters": map[string]any{"type": "object", "required": []any{"code", "timeout"}, "additionalProperties": false, "properties": map[string]any{"code": map[string]any{"type": "string", "minLength": 1}, "timeout": map[string]any{"type": "integer", "minimum": 1}}}},
		map[string]any{"type": "namespace", "name": "ops", "tools": []any{map[string]any{"type": "namespace", "name": "shell", "tools": []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object", "required": []any{"cmd"}, "additionalProperties": false, "properties": map[string]any{"cmd": map[string]any{"type": "string"}, "workdir": map[string]any{"type": "string"}}}}}}}},
		map[string]any{"type": "function", "name": "other_shell", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}}},
		map[string]any{"type": "custom", "name": "apply_patch"},
	}}
}

func native054Call(summary, code, metadata string) map[string]any {
	return map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "native_call", "id": "fc_native", "status": "completed", "encrypted_function_args": []any{"opaque-native-only"}, "arguments": string(jsonBytes(map[string]any{"summary": summary, "code": code, "extended_summary": metadata, "destructive": false, "references": []any{}}))}
}

func TestNative054TransportOptions(t *testing.T) {
	source := jsonBytes(native054Source())
	_, text, err := PrepareWithOptions(source, "scope", Options{})
	if err != nil || !text.textTools {
		t.Fatalf("default must preserve text path: %v", err)
	}
	raw, native, err := PrepareWithOptions(source, "scope", Options{ToolTransport: "native", ToolsVersionID: "fixture-tools-v2"})
	if err != nil || native.textTools {
		t.Fatalf("native mode unavailable: %v", err)
	}
	body, _ := parseRelayObject(string(raw))
	if body["metadata"].(map[string]any)["bps_tools_version_id"] != "fixture-tools-v2" || !strings.Contains(string(raw), "codex2api.function_code/python") || !strings.Contains(string(raw), "codex2api.function_cmd/ops.shell.exec_command") {
		t.Fatal("native catalog/version did not reach request")
	}
	if _, _, err := PrepareWithOptions(source, "scope", Options{ToolTransport: "guess"}); err == nil {
		t.Fatal("unknown transport accepted")
	}
	if _, _, err := PrepareWithOptions(source, "scope", Options{ToolsVersionID: "version\nsecret"}); err == nil {
		t.Fatal("unsafe version accepted")
	}
}

func TestNative054RawFunctionAndNestedNamespaceRoundTrip(t *testing.T) {
	code := " \tprint(\"quoted\\path\")\r\n# 中文 🧪\n"
	command := "printf '%s\\n' \"a\\b\"\n"
	for _, tc := range []struct{ name, marker, payload, metadata, field, expectedName, namespace string }{
		{"code", functionCodeRelayPrefix + "python", code, `{"timeout":9007199254740993}`, "code", "python", ""},
		{"cmd", functionCmdRelayPrefix + "ops.shell.exec_command", command, `{"workdir":"/tmp/space dir"}`, "cmd", "exec_command", "ops.shell"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := native054Source()
			_, bridge, err := PrepareWithOptions(jsonBytes(source), "one-account", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			native := native054Call(tc.marker, tc.payload, tc.metadata)
			terminal, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
			if err != nil {
				t.Fatal(err)
			}
			response, _ := parseRelayObject(string(terminal))
			call := response["output"].([]any)[0].(map[string]any)
			args, why := parseRelayObject(call["arguments"])
			if why != "" || args[tc.field] != tc.payload || call["name"] != tc.expectedName || stringValue(call["namespace"]) != tc.namespace || call["status"] != "completed" || len(call["encrypted_function_args"].([]any)) != 0 {
				t.Fatalf("transport changed call: %+v", call)
			}
			if tc.field == "code" && !strings.Contains(call["arguments"].(string), "9007199254740993") {
				t.Fatal("large numeric argument rounded")
			}
			output := map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "fixture result"}
			source["input"] = []any{call, output}
			replay, _, err := PrepareWithOptions(jsonBytes(source), "one-account", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			prepared, _ := parseRelayObject(string(replay))
			var restored map[string]any
			for _, raw := range prepared["input"].([]any) {
				item := objectValue(raw)
				if item["type"] == "function_call" {
					restored = item
				}
			}
			if restored == nil || restored["arguments"] != native["arguments"] {
				t.Fatal("same-account replay lost native envelope")
			}
			replay, _, err = PrepareWithOptions(jsonBytes(source), "other-account", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			prepared, _ = parseRelayObject(string(replay))
			for _, raw := range prepared["input"].([]any) {
				item := objectValue(raw)
				if item["type"] == "function_call" {
					outer, _ := parseRelayObject(item["arguments"])
					if outer["summary"] != tc.marker || outer["code"] != tc.payload {
						t.Fatal("cross-account fallback lost raw transport")
					}
					if item["encrypted_function_args"] != nil {
						t.Fatal("native encrypted history crossed account")
					}
				}
			}
		})
	}
}

func TestNative054RejectsMalformedOrUnselectedRawRelays(t *testing.T) {
	source := native054Source()
	for _, tc := range []struct {
		name string
		call map[string]any
	}{
		{"duplicate payload", native054Call(functionCodeRelayPrefix+"python", "code", `{"code":"different","timeout":1}`)},
		{"metadata not object", native054Call(functionCodeRelayPrefix+"python", "code", `[]`)},
		{"trailing metadata", native054Call(functionCodeRelayPrefix+"python", "code", `{"timeout":1} {}`)},
		{"undeclared", native054Call(functionCodeRelayPrefix+"not_declared", "code", `{"timeout":1}`)},
		{"custom kind", native054Call(functionCodeRelayPrefix+"apply_patch", "code", `{}`)},
		{"cmd eligibility", native054Call(functionCmdRelayPrefix+"other_shell", "cmd", `{}`)},
		{"full schema minimum", native054Call(functionCodeRelayPrefix+"python", "code", `{"timeout":0}`)},
		{"full schema min length", native054Call(functionCodeRelayPrefix+"python", "", `{"timeout":1}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, bridge, err := PrepareWithOptions(jsonBytes(source), "scope", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{tc.call}})); err == nil {
				t.Fatal("invalid native call accepted")
			}
		})
	}
	source["tool_choice"] = "none"
	_, bridge, err := PrepareWithOptions(jsonBytes(source), "scope", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native054Call(functionCodeRelayPrefix+"python", "code", `{"timeout":1}`)}})); err == nil {
		t.Fatal("tool_choice none bypassed")
	}
}

func TestNative054StreamWithholdsRawToolUntilValidatedTerminal(t *testing.T) {
	source := native054Source()
	raw := jsonBytes(source)
	_, bridge, err := PrepareWithOptions(raw, "scope", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	catalog, err := compat.DeclaredTools(fields)
	if err != nil {
		t.Fatal(err)
	}
	var delivered [][]byte
	emit := bridge.WrapEmitter(compat.GuardStream(catalog, func(frame []byte) error { delivered = append(delivered, frame); return nil }))
	frame := func(value any) []byte { return append(append([]byte("data: "), jsonBytes(value)...), '\n', '\n') }
	native := native054Call(functionCodeRelayPrefix+"python", "print(1)\n", `{"timeout":2}`)
	if err := emit(frame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": native})); err != nil || len(delivered) != 0 {
		t.Fatal("native provisional call escaped validation")
	}
	if err := emit(frame(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": "untrusted raw"})); err != nil || len(delivered) != 0 {
		t.Fatal("native provisional arguments escaped validation")
	}
	if err := emit(frame(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "output": []any{native}}})); err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, f := range delivered {
		joined += string(f)
	}
	if len(delivered) != 5 || strings.Contains(joined, "run_officejs") || strings.Contains(joined, "untrusted raw") || !strings.Contains(joined, `"name":"python"`) {
		t.Fatalf("wrong validated stream: %s", joined)
	}
}

func TestNative054AgentMessageCompatibilityPreservesVisibleContext(t *testing.T) {
	input := []any{map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "task"}}},
		map[string]any{"type": "agent_message", "author": "worker", "recipient": "parent", "content": []any{map[string]any{"type": "text", "text": "visible result"}, map[string]any{"type": "encrypted_content", "encrypted_content": "secret-agent-cipher"}}},
		map[string]any{"type": "agent_message", "role": "assistant", "encrypted_content": "secret-top-cipher", "text": "metadata"},
		map[string]any{"type": "reasoning", "encrypted_content": "reasoning-kept"}, map[string]any{"type": "compaction", "encrypted_content": "compaction-kept"}}
	for _, mode := range []string{"text", "native"} {
		source := native054Source()
		source["input"] = input
		body, _, err := PrepareWithOptions(jsonBytes(source), "scope", Options{ToolTransport: mode})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), "secret-agent-cipher") || strings.Contains(string(body), "secret-top-cipher") || !strings.Contains(string(body), "visible result") || !strings.Contains(string(body), "worker") || !strings.Contains(string(body), agentEncryptedNotice) || !strings.Contains(string(body), "reasoning-kept") || !strings.Contains(string(body), "compaction-kept") {
			t.Fatalf("agent context compatibility incorrect: %s", body)
		}
	}
	for _, bad := range []any{"", 123, nil} {
		source := native054Source()
		source["input"] = []any{map[string]any{"type": "agent_message", "content": []any{map[string]any{"type": "encrypted_content", "encrypted_content": bad}}}}
		if _, _, err := PrepareWithOptions(jsonBytes(source), "scope", Options{ToolTransport: "native"}); err == nil {
			t.Fatal("malformed opaque state accepted")
		}
	}
}

func TestNative054ContextManagementDefaultAndExplicitDisable(t *testing.T) {
	for _, tc := range []struct{ name, field, want string }{
		{"missing", "", `[{"compact_threshold":200000,"type":"compaction"}]`},
		{"null", `,"context_management":null`, `[{"compact_threshold":200000,"type":"compaction"}]`},
		{"empty", `,"context_management":[]`, `[]`},
		{"custom", `,"context_management":[{"type":"compaction","compact_threshold":123456}]`, `[{"compact_threshold":123456,"type":"compaction"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := []byte(`{"model":"fixture","input":"hi"` + tc.field + `}`)
			prepared, _, err := PrepareWithOptions(raw, "scope", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := parseRelayObject(string(prepared))
			if string(jsonBytes(body["context_management"])) != tc.want {
				t.Fatalf("native context policy wrong: %s", prepared)
			}
			if tc.name != "custom" {
				prepared, _, err = PrepareWithTextTools(raw, "scope")
				if err != nil {
					t.Fatal(err)
				}
				body, _ = parseRelayObject(string(prepared))
				if _, exists := body["context_management"]; exists {
					t.Fatal("native default changed established text mode")
				}
			}
		})
	}
}

func TestNative054LegacyReferencesAndCurrentFunctionEnvelopeDisambiguation(t *testing.T) {
	sumSchema := map[string]any{"type": "object", "required": []any{"a", "b"}, "additionalProperties": false, "properties": map[string]any{"a": map[string]any{"type": "integer"}, "b": map[string]any{"type": "integer"}}}
	for _, tc := range []struct {
		name, kind, code string
		schema           map[string]any
		want             string
		reject           bool
	}{
		{"mixed function", "function", `{"name":"fixture_sum","arguments":{"a":2,"b":3}}`, sumSchema, `{"a":2,"b":3}`, false},
		{"raw legacy arguments", "function", `{"a":2,"b":3}`, sumSchema, `{"a":2,"b":3}`, false},
		{"custom JSON remains raw", "custom", `{"name":"fixture_sum","arguments":{"a":2,"b":3}}`, nil, `{"name":"fixture_sum","arguments":{"a":2,"b":3}}`, false},
		{"conflicting reference", "function", `{"name":"other_function","arguments":{"a":2,"b":3}}`, sumSchema, "", true},
		{"extra envelope fields", "function", `{"name":"fixture_sum","arguments":{"a":2,"b":3},"extra":true}`, sumSchema, "", true},
		{"wrong inner type", "function", `{"name":"fixture_sum","arguments":{"a":"2","b":3}}`, sumSchema, "", true},
		{"legitimate name arguments fields", "function", `{"name":"literal user name","arguments":{"a":2,"b":3}}`, map[string]any{"type": "object", "required": []any{"name", "arguments"}, "additionalProperties": false, "properties": map[string]any{"name": map[string]any{"type": "string"}, "arguments": map[string]any{"type": "object"}}}, `{"arguments":{"a":2,"b":3},"name":"literal user name"}`, false},
		{"full oneOf decides unwrap", "function", `{"name":"fixture_sum","arguments":{"a":2,"b":3}}`, map[string]any{"oneOf": []any{sumSchema, map[string]any{"type": "object", "required": []any{"text"}, "additionalProperties": false, "properties": map[string]any{"text": map[string]any{"type": "string"}}}}}, `{"a":2,"b":3}`, false},
		{"full oneOf preserves direct branch", "function", `{"name":"literal","arguments":{"a":2,"b":3}}`, map[string]any{"oneOf": []any{sumSchema, map[string]any{"type": "object", "required": []any{"name", "arguments"}, "additionalProperties": false, "properties": map[string]any{"name": map[string]any{"const": "literal"}, "arguments": map[string]any{"type": "object"}}}}}, `{"arguments":{"a":2,"b":3},"name":"literal"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tool := map[string]any{"type": tc.kind, "name": "fixture_sum"}
			if tc.schema != nil {
				tool["parameters"] = tc.schema
			}
			source := jsonBytes(map[string]any{"model": "fixture", "input": "test", "tools": []any{tool}})
			_, bridge, err := PrepareWithOptions(source, "scope", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			native := map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_mixed", "call_id": "call_mixed", "arguments": string(jsonBytes(map[string]any{"summary": "Call fixture_sum", "code": tc.code, "references": []any{"fixture_sum"}, "destructive": false, "extended_summary": "{}"}))}
			raw, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
			if tc.reject {
				if err == nil {
					t.Fatal("ambiguous/invalid mixed relay accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			result, _ := parseRelayObject(string(raw))
			call := objectValue(result["output"].([]any)[0])
			key := "arguments"
			if tc.kind == "custom" {
				key = "input"
			}
			if call[key] != tc.want {
				t.Fatalf("wrong argument interpretation: got=%v want=%s", call[key], tc.want)
			}
		})
	}
}
