package bpsprotocol

import (
	"testing"
)

// Fresh original-0.5.31 ABI captures accept JSON-encoded argument objects in
// the native name/arguments envelope, with or without legacy references.
func TestOriginalCompatibleEncodedFunctionArguments(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"Read fixture.","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}],"tool_choice":"required"}`)
	_, bridge, err := Prepare(source, "encoded-arguments")
	if err != nil {
		t.Fatal(err)
	}
	for _, legacy := range []bool{false, true} {
		for _, argument := range []any{map[string]any{"path": "fixture.txt"}, `{"path":"fixture.txt"}`} {
			outer := map[string]any{"code": string(jsonBytes(map[string]any{"name": "read_file", "arguments": argument})), "references": []any{}}
			if legacy {
				outer["references"] = []any{"read_file"}
			}
			call := map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_encoded", "call_id": "call_encoded", "arguments": string(jsonBytes(outer))}
			result, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{call}}))
			if err != nil {
				t.Fatalf("encoded arguments rejected, legacy=%v: %v", legacy, err)
			}
			response, _ := parseRelayObject(string(result))
			client := objectValue(response["output"].([]any)[0])
			if client["name"] != "read_file" || client["arguments"] != `{"path":"fixture.txt"}` || client["call_id"] != "call_encoded" {
				t.Fatal("decoded arguments changed the client call")
			}
		}
		for _, argument := range []any{`[]`, `null`, `{"path":"fixture.txt"} {}`, `{"path":42}`, `{"path":"fixture.txt","extra":"forbidden"}`} {
			outer := map[string]any{"code": string(jsonBytes(map[string]any{"name": "read_file", "arguments": argument})), "references": []any{}}
			if legacy {
				outer["references"] = []any{"read_file"}
			}
			call := map[string]any{"type": "function_call", "name": "run_officejs", "id": "fc_invalid", "call_id": "call_invalid", "arguments": string(jsonBytes(outer))}
			if _, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{call}})); err == nil {
				t.Fatalf("invalid encoded object or schema accepted, legacy=%v", legacy)
			}
		}
	}
}

func TestLegacyEncodedEnvelopeRetainsNamesAndDirectArgumentContracts(t *testing.T) {
	for _, test := range []struct {
		name, schema, envelopeName, arguments string
		want                                  string
	}{
		{"reference conflict", `{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`, "other_tool", `{"path":"fixture.txt"}`, ""},
		{"literal named fields", `{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"string"}},"required":["name","arguments"],"additionalProperties":false}`, "literal", `{"path":"fixture.txt"}`, `{"arguments":"{\"path\":\"fixture.txt\"}","name":"literal"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			schema, _ := parseRelayObject(test.schema)
			source := map[string]any{"model": "fixture", "input": "fixture", "tools": []any{map[string]any{"type": "function", "name": "read_file", "parameters": schema}}}
			_, bridge, err := Prepare(jsonBytes(source), "literal-named-fields")
			if err != nil {
				t.Fatal(err)
			}
			call := native054Call("Read fixture", string(jsonBytes(map[string]any{"name": test.envelopeName, "arguments": test.arguments})), `{}`)
			outer, _ := parseRelayObject(call["arguments"])
			outer["references"] = []any{"read_file"}
			call["arguments"] = string(jsonBytes(outer))
			transformed, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{call}}))
			if test.want == "" {
				if err == nil {
					t.Fatal("conflicting selectors were accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			response, _ := parseRelayObject(string(transformed))
			if objectValue(response["output"].([]any)[0])["arguments"] != test.want {
				t.Fatal("literal name/arguments function properties were unwrapped")
			}
		})
	}
}
