package bpsprotocol

import "testing"

func TestNativeFileReferencesDoNotOverrideDeclaredEnvelope(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"Read fixture.txt once.","tool_choice":"auto","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}},{"type":"function","name":"write_to_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}},{"type":"function","name":"replace_in_file","parameters":{"type":"object","properties":{"path":{"type":"string"},"old_str":{"type":"string"},"new_str":{"type":"string"}},"required":["path","old_str","new_str"],"additionalProperties":false}}]}`)
	_, bridge, err := Prepare(source, "file-references")
	if err != nil {
		t.Fatal(err)
	}
	for _, references := range [][]any{{}, {"fixture.txt"}, {"fixture.txt", "other.txt"}, {"read_file"}} {
		for _, arguments := range []any{map[string]any{"path": "fixture.txt"}, `{"path":"fixture.txt"}`} {
			outer := map[string]any{"summary": "Read synthetic fixture once", "extended_summary": "Read only the synthetic fixture", "code": string(jsonBytes(map[string]any{"name": "read_file", "arguments": arguments})), "destructive": false, "references": references}
			native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "call_fixture", "id": "fc_fixture", "arguments": string(jsonBytes(outer))}
			terminal := jsonBytes(map[string]any{"status": "completed", "output": []any{native}})
			transformed, err := bridge.Transform(terminal)
			if err != nil {
				t.Fatalf("explicit tool envelope was overridden by references %v: %v", references, err)
			}
			response, _ := parseRelayObject(string(transformed))
			call := objectValue(response["output"].([]any)[0])
			if call["type"] != "function_call" || call["name"] != "read_file" || call["arguments"] != `{"path":"fixture.txt"}` {
				t.Fatal("filename references changed the selected tool or arguments")
			}
			alternate, mode, ok := DecodeAlternateToolResponse(source, terminal, "file-references", Options{ToolTransport: "text"})
			if !ok || mode != "native" || string(alternate) != string(transformed) {
				t.Fatal("same-response alternate decoding did not recover native references")
			}
		}
	}
	// A declared legacy tool selector is different from a filename. Preserve
	// the conflict check instead of silently choosing a different client tool.
	outer := map[string]any{"code": `{"name":"read_file","arguments":{"path":"fixture.txt"}}`, "references": []any{"write_to_file"}}
	native := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "conflict", "arguments": string(jsonBytes(outer))}
	if _, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}})); err == nil {
		t.Fatal("conflicting declared tool selectors were silently resolved")
	}
}

func TestBufferedBridgePreservesEveryErrorTerminal(t *testing.T) {
	for _, source := range []string{
		`{"model":"fixture","input":"fixture","tools":[{"type":"function","name":"read_file"}]}`,
		`{"model":"fixture","input":"fixture","text":{"format":{"type":"json_object"}}}`,
	} {
		_, bridge, err := PrepareWithTextTools([]byte(source), "error-terminals")
		if err != nil {
			t.Fatal(err)
		}
		for _, kind := range []string{"error", "response.error", "response.failed"} {
			frame := []byte("event: " + kind + "\ndata: " + string(jsonBytes(map[string]any{"type": kind, "error": map[string]any{"code": "fixture_failure"}})) + "\n\n")
			count := 0
			emit := bridge.WrapEmitter(func(got []byte) error {
				count++
				if string(got) != string(frame) {
					t.Fatal("error terminal changed before upstream validation")
				}
				return nil
			})
			if err := emit(frame); err != nil || count != 1 {
				t.Fatalf("error terminal %s was swallowed: %v", kind, err)
			}
		}
	}
}
