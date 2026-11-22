package bpsprotocol

import (
	"bytes"
	"strings"
	"testing"
)

// The real BPS run_officejs catalog captured on 2026-11-22 requires references
// and minItems:1. Descriptions are omitted here; these validation constraints
// match the captured tool schema, including its required outer fields.
const observedNativeOuterSchema = `{"type":"object","properties":{"summary":{"type":"string"},"extended_summary":{"type":"string"},"code":{"type":"string"},"destructive":{"type":"boolean"},"references":{"type":"array","items":{"type":"string"},"minItems":1}},"required":["summary","extended_summary","code","destructive","references"],"additionalProperties":false}`

func TestGeneratedRelaysSatisfyObservedNativeReferencesSchema(t *testing.T) {
	schemaObject, _ := parseRelayObject(observedNativeOuterSchema)
	schema, err := compileSchema(schemaObject)
	if err != nil {
		t.Fatal(err)
	}
	source := native054Source()
	source["tools"] = append(source["tools"].([]any), map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []any{"text"}, "additionalProperties": false}})
	for _, client := range []map[string]any{
		{"type": "function_call", "name": "echo", "call_id": "reference_regular", "arguments": `{"text":" exact fixture "}`},
		{"type": "function_call", "name": "python", "call_id": "reference_code", "arguments": `{"code":" \tprint(\"fixture\")\n","timeout":2}`},
		{"type": "function_call", "name": "exec_command", "namespace": "ops.shell", "call_id": "reference_cmd", "arguments": `{"cmd":" printf 'fixture'\n","workdir":"/tmp/fixture dir"}`},
		{"type": "custom_tool_call", "name": "apply_patch", "call_id": "reference_custom", "input": " *** Begin Patch\n*** End Patch\n"},
	} {
		t.Run(client["call_id"].(string), func(t *testing.T) {
			native := fallbackTransportCall(client)
			outer, reason := parseRelayObject(native["arguments"])
			if reason != "" || schema.Validate(outer) != nil {
				t.Fatal("generated history relay violates the current native schema")
			}
			if got := outer["references"].([]any); len(got) != 1 || got[0] != nativeRelayReference || validRelayName(got[0].(string)) {
				t.Fatal("native reference is not the stable non-tool label")
			}
			oldOuter := cloneObject(outer)
			oldOuter["references"] = []any{}
			if schema.Validate(oldOuter) == nil {
				t.Fatal("regression fixture did not detect the former empty-reference schema conflict")
			}
			for _, references := range []any{[]any{}, []any{nativeRelayReference}} {
				_, bridge, err := PrepareWithOptions(jsonBytes(source), "reference-schema", Options{ToolTransport: "native"})
				if err != nil {
					t.Fatal(err)
				}
				outer["references"] = references
				native["arguments"] = string(jsonBytes(outer))
				transformed, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
				if err != nil {
					t.Fatalf("old/new reference relay is incompatible: %v", err)
				}
				response, _ := parseRelayObject(string(transformed))
				call := objectValue(response["output"].([]any)[0])
				for _, field := range []string{"type", "name", "namespace", "call_id", "arguments", "input"} {
					if !bytes.Equal(jsonBytes(call[field]), jsonBytes(client[field])) {
						t.Fatalf("reference compatibility changed client field %s", field)
					}
				}
			}
		})
	}
}

func TestRawRelaysRejectUnrecognizedNonemptyReferences(t *testing.T) {
	for _, marker := range []struct{ summary, code, metadata string }{
		{functionCodeRelayPrefix + "python", "print(1)", `{"timeout":2}`},
		{functionCmdRelayPrefix + "ops.shell.exec_command", "printf fixture", `{}`},
	} {
		for _, refs := range []any{nil, "External client tool", []any{""}, []any{"python"}, []any{"External client tool "}, []any{"external client tool"}, []any{nativeRelayReference, nativeRelayReference}, []any{map[string]any{"reference": nativeRelayReference}}} {
			_, bridge, err := PrepareWithOptions(jsonBytes(native054Source()), "invalid-references", Options{ToolTransport: "native"})
			if err != nil {
				t.Fatal(err)
			}
			native := native054Call(marker.summary, marker.code, marker.metadata)
			outer, _ := parseRelayObject(native["arguments"])
			outer["references"] = refs
			native["arguments"] = string(jsonBytes(outer))
			if _, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}})); err == nil {
				t.Fatal("raw transport accepted an unrecognized reference")
			}
		}
	}
}

func TestNativePromptsExamplesAndRepairsRequireStableReference(t *testing.T) {
	source := native054Source()
	prepared, _, err := PrepareWithOptions(jsonBytes(source), "reference-prompts", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	for _, prompt := range []string{clientToolProtocolInstructions(source), clientToolProtocolReminder(source), nativeRepairInstruction} {
		if !strings.Contains(prompt, `references field to ["External client tool"]`) || strings.Contains(prompt, "references=[]") || strings.Contains(prompt, "Set references to an empty array") {
			t.Fatal("native instructions still conflict with the current references schema")
		}
	}
	schemaObject, _ := parseRelayObject(observedNativeOuterSchema)
	schema, _ := compileSchema(schemaObject)
	specs := clientToolSpecs(source)
	for _, key := range []string{"ops.shell.exec_command", "apply_patch"} {
		example := clientToolRelayExamples([]string{key}, specs)
		_, raw, ok := strings.Cut(example, "): ")
		if !ok {
			t.Fatal("expected a native example for the supported fixture")
		}
		outer, reason := parseRelayObject(strings.TrimSuffix(raw, "."))
		if reason != "" || schema.Validate(outer) != nil {
			t.Fatal("native catalog example violates current outer schema")
		}
	}
	repaired, ok := RepairNativePayload(prepared, invalidOfficeResponse("reference-repair"), relayError("references_must_select_one_tool"))
	if !ok {
		t.Fatal("known unexecuted relay could not be corrected")
	}
	body, _ := parseRelayObject(string(repaired))
	input := body["input"].([]any)
	if text := itemText(objectValue(input[len(input)-1])["content"]); !strings.Contains(text, nativeReferencesInstruction) {
		t.Fatal("repair did not repeat the current nonempty reference requirement")
	}
}
