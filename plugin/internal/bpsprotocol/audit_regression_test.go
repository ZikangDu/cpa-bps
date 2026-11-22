package bpsprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/compat"
	"cpa-bps-reconstruction/internal/upstream"
)

func TestEmptyHistoryDoesNotPanicAndKeepsStableTurnIdentity(t *testing.T) {
	for _, mode := range []string{"text", "native"} {
		t.Run(mode, func(t *testing.T) {
			source := []byte(`{"model":"fixture","instructions":"Answer the instructions.","input":[]}`)
			first, _, err := PrepareWithOptions(source, "empty-history", Options{ToolTransport: mode})
			if err != nil {
				t.Fatal(err)
			}
			second, _, err := PrepareWithOptions(source, "empty-history", Options{ToolTransport: mode})
			if err != nil || !bytes.Equal(first, second) {
				t.Fatalf("empty history did not produce a stable request: %v", err)
			}
			body, _ := parseRelayObject(string(first))
			metadata := objectValue(body["metadata"])
			if metadata["agent_iteration"] != "1" || stringValue(metadata["turn_id"]) == "" {
				t.Fatal("empty history lost turn identity")
			}
		})
	}
}

func TestNativeMixedToolHistorySurvivesManyTurnsAndAccountSwitch(t *testing.T) {
	source := native054Source()
	source["input"] = []any{messageItem("user", "Remember marker AUDIT-4207 and use the tools.")}
	source["tool_choice"] = "required"
	source["prompt_cache_key"] = "many-turn-tool-audit"
	initialScope := "mixed-tools-account-a"
	var keyA any
	for turn := 0; turn < 24; turn++ {
		prepared, bridge, err := PrepareWithOptions(jsonBytes(source), initialScope, Options{ToolTransport: "native"})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := parseRelayObject(string(prepared))
		if keyA == nil {
			keyA = body["prompt_cache_key"]
		} else if body["prompt_cache_key"] != keyA {
			t.Fatal("tool turns changed the account's conversation cache key")
		}
		var native map[string]any
		switch turn % 4 {
		case 0:
			native = native054Call(functionCodeRelayPrefix+"python", "  print(\"AUDIT-4207\")\n", `{"timeout":2}`)
		case 1:
			native = native054Call(functionCmdRelayPrefix+"ops.shell.exec_command", "printf '%s\\n' 'AUDIT-4207'\n", `{"workdir":"/tmp/fixture dir"}`)
		case 2:
			native = native054Call(customRelayPrefix+"apply_patch", "*** Begin Patch\n*** End Patch\n", `{}`)
		case 3:
			native = map[string]any{"type": "function_call", "name": "other_shell", "arguments": `{"cmd":"fixture only"}`, "status": "completed"}
		}
		native["id"], native["call_id"] = fmt.Sprintf("fc_turn_%d", turn), fmt.Sprintf("call_turn_%d", turn)
		transformed, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
		if err != nil {
			t.Fatalf("tool turn %d failed: %v", turn, err)
		}
		var rawSource map[string]json.RawMessage
		if err := json.Unmarshal(jsonBytes(source), &rawSource); err != nil {
			t.Fatal(err)
		}
		catalog, err := compat.DeclaredTools(rawSource)
		if err != nil || catalog.ValidateResponse(transformed) != nil {
			t.Fatalf("tool turn %d did not satisfy the client contract", turn)
		}
		response, _ := parseRelayObject(string(transformed))
		call := objectValue(response["output"].([]any)[0])
		if turn%4 == 1 && (call["namespace"] != "ops.shell" || call["name"] != "exec_command") {
			t.Fatal("nested namespace was lost in a later tool turn")
		}
		history := source["input"].([]any)
		source["input"] = append(history, call, map[string]any{"type": call["type"].(string) + "_output", "call_id": call["call_id"], "output": fmt.Sprintf("result-%d AUDIT-4207", turn)})
	}
	source["tool_choice"] = "none"
	for _, scope := range []string{initialScope, "mixed-tools-account-b", initialScope} {
		prepared, bridge, err := PrepareWithOptions(jsonBytes(source), scope, Options{ToolTransport: "native"})
		if err != nil {
			t.Fatal(err)
		}
		body, _ := parseRelayObject(string(prepared))
		if (body["prompt_cache_key"] == keyA) != (scope == initialScope) {
			t.Fatal("account switching lost cache partitioning")
		}
		calls, results := 0, 0
		for _, raw := range body["input"].([]any) {
			item := objectValue(raw)
			switch item["type"] {
			case "function_call":
				calls++
				if item["name"] != transportName || scope != initialScope && item["encrypted_function_args"] != nil {
					t.Fatal("cross-account replay leaked ciphertext or lost its native relay")
				}
			case "function_call_output":
				results++
			}
		}
		if calls != 24 || results != 24 || !strings.Contains(string(prepared), "result-23 AUDIT-4207") {
			t.Fatal("account switch or current tool prohibition discarded historical calls/results")
		}
		plain := jsonBytes(map[string]any{"status": "completed", "output": []any{messageItem("assistant", "AUDIT-4207")}})
		if _, err := bridge.Transform(plain); err != nil {
			t.Fatalf("tool_choice none rejected final text: %v", err)
		}
		if _, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native054Call(customRelayPrefix+"apply_patch", "new patch", `{}`)}})); err == nil {
			t.Fatal("historical tool declarations bypassed current tool prohibition")
		}
	}
}

func TestTextToolEnvelopePreservesSeparateCommentaryAndRoundTripsStream(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"Read the fixture file.","tools":[{"type":"function","name":"read_file","parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}],"tool_choice":"required"}`)
	_, bridge, err := PrepareWithTextTools(source, "separate-commentary")
	if err != nil {
		t.Fatal(err)
	}
	commentary := messageItem("assistant", "I will read the requested fixture.")
	commentary["id"], commentary["phase"] = "msg_commentary", "commentary"
	reasoning := map[string]any{"type": "reasoning", "id": "rs_fixture", "summary": []any{}}
	envelope := messageItem("assistant", `{"client_tool":"read_file","arguments":{"path":"fixture.txt"}}`)
	terminal := jsonBytes(map[string]any{"id": "resp_separate", "status": "completed", "output": []any{reasoning, commentary, envelope}})
	transformed, err := bridge.Transform(terminal)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := parseRelayObject(string(transformed))
	output := response["output"].([]any)
	if len(output) != 3 || !bytes.Equal(jsonBytes(output[0]), jsonBytes(reasoning)) || !bytes.Equal(jsonBytes(output[1]), jsonBytes(commentary)) {
		t.Fatal("decoding the tool envelope discarded separate commentary or reasoning")
	}
	call := objectValue(output[2])
	if call["type"] != "function_call" || call["name"] != "read_file" || call["arguments"] != `{"path":"fixture.txt"}` {
		t.Fatal("separate final message did not become the declared client call")
	}
	stream, err := ResponseStream(transformed)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := upstream.ReadEvents(context.Background(), bytes.NewReader(stream), nil)
	if err != nil || !bytes.Equal(roundTrip, transformed) {
		t.Fatalf("commentary and tool SSE lost the terminal response: %v", err)
	}
	// The same complete response is decodable after a native transport misses
	// its tool contract, with no second upstream generation.
	alternate, mode, ok := DecodeAlternateToolResponse(source, terminal, "separate-commentary", Options{ToolTransport: "native"})
	if !ok || mode != "text" || !bytes.Equal(alternate, transformed) {
		t.Fatal("complete alternate text decoding did not recover the separate envelope")
	}

	var request map[string]any
	if json.Unmarshal(source, &request) != nil {
		t.Fatal("invalid fixture")
	}
	request["tool_choice"] = "none"
	_, forbidden, err := PrepareWithTextTools(jsonBytes(request), "separate-commentary")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := forbidden.Transform(terminal); err == nil {
		t.Fatal("separate commentary bypassed tool_choice none")
	}
}

func TestTextToolEnvelopeDoesNotGuessWithinProseOrSelectAmbiguousCalls(t *testing.T) {
	source := []byte(`{"model":"fixture","input":"Read the fixture file.","tools":[{"type":"function","name":"read_file"}],"tool_choice":"required"}`)
	_, bridge, err := PrepareWithTextTools(source, "ambiguous-text")
	if err != nil {
		t.Fatal(err)
	}
	envelope := `{"client_tool":"read_file","arguments":{"path":"fixture.txt"}}`
	for _, output := range [][]any{
		{messageItem("assistant", "Example JSON: "+envelope)},
		{messageItem("assistant", envelope), messageItem("assistant", envelope)},
	} {
		if _, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": output})); err == nil {
			t.Fatal("ambiguous text was converted into an executable call")
		}
	}
}

func TestTextToolEnvelopeCannotOverrideLaterAnswerOrRefusal(t *testing.T) {
	envelope := messageItem("assistant", `{"client_tool":"read_file","arguments":{"path":"fixture.txt"}}`)
	envelope["phase"] = "commentary"
	final := messageItem("assistant", "This was an example; do not execute it.")
	final["phase"] = "final_answer"
	for _, choice := range []string{"auto", "required"} {
		source := map[string]any{"model": "fixture", "input": "Read a fixture.", "tools": []any{map[string]any{"type": "function", "name": "read_file"}}, "tool_choice": choice}
		_, bridge, err := PrepareWithTextTools(jsonBytes(source), "retracted-envelope")
		if err != nil {
			t.Fatal(err)
		}
		terminal := jsonBytes(map[string]any{"status": "completed", "output": []any{envelope, final}})
		transformed, err := bridge.Transform(terminal)
		if choice == "required" {
			if err == nil {
				t.Fatal("retracted envelope satisfied required tool choice")
			}
		} else if err != nil || !bytes.Equal(transformed, terminal) {
			t.Fatal("retracted tool example changed the final answer")
		}
	}
	source := []byte(`{"model":"fixture","input":"Read a fixture.","tools":[{"type":"function","name":"read_file"}]}`)
	_, bridge, err := PrepareWithTextTools(source, "unsafe-envelope")
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(map[string]any){
		func(r map[string]any) { r["refusal"] = "declined" },
		func(r map[string]any) { r["error"] = map[string]any{"code": "fixture"} },
		func(r map[string]any) { r["incomplete_details"] = map[string]any{"reason": "max_output_tokens"} },
		func(r map[string]any) { objectValue(r["output"].([]any)[0])["status"] = "in_progress" },
		func(r map[string]any) {
			r["output"] = append([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": "declined"}}}}, r["output"].([]any)...)
		},
		func(r map[string]any) {
			r["output"] = append(r["output"].([]any), map[string]any{"type": "web_search_call"})
		},
	} {
		response := map[string]any{"status": "completed", "output": []any{cloneObject(envelope)}}
		mutate(response)
		if _, err := bridge.Transform(jsonBytes(response)); err == nil {
			t.Fatal("contradictory or partial response authorized a tool call")
		}
	}
	refusal := jsonBytes(map[string]any{"status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": "declined"}}}}})
	if transformed, err := bridge.Transform(refusal); err != nil || !bytes.Equal(transformed, refusal) {
		t.Fatal("ordinary refusal response did not pass through")
	}
}

func TestNativeCustomToolCatalogHasStableCacheablePrefix(t *testing.T) {
	source := map[string]any{"model": "fixture", "input": "Use the declared tools.", "tools": []any{
		map[string]any{"type": "custom", "name": "z_last"},
		map[string]any{"type": "custom", "name": "a_first"},
		map[string]any{"type": "namespace", "name": "nested", "tools": []any{
			map[string]any{"type": "custom", "name": "z_patch"},
			map[string]any{"type": "custom", "name": "a_note"},
		}},
	}}
	var first []byte
	for range 100 {
		prepared, _, err := PrepareWithOptions(jsonBytes(source), "catalog-order", Options{ToolTransport: "native"})
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = prepared
		} else if !bytes.Equal(first, prepared) {
			t.Fatal("identical native tool requests changed their cacheable input")
		}
	}
	// Reordering declarations must not rewrite an otherwise identical catalog.
	tools := source["tools"].([]any)
	tools[0], tools[1] = tools[1], tools[0]
	prepared, _, err := PrepareWithOptions(jsonBytes(source), "catalog-order", Options{ToolTransport: "native"})
	if err != nil || !bytes.Equal(first, prepared) {
		t.Fatalf("declaration order changed the native input prefix: %v", err)
	}
}
