package bpsprotocol

import (
	"encoding/json"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/compat"
)

func TestToolTransportsNormalizeReasoningAfterHistoryPreparation(t *testing.T) {
	for _, omit := range []bool{false, true} {
		for _, mode := range []string{"native", "text"} {
			t.Run(mode+map[bool]string{false: "/keep", true: "/omit"}[omit], func(t *testing.T) {
				source := map[string]any{
					"model": "fixture",
					"input": []any{
						map[string]any{"type": "additional_tools", "tools": []any{
							map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}},
							map[string]any{"type": "custom", "name": "patch"},
						}},
						messageItem("user", "earlier question"),
						map[string]any{"type": "reasoning", "content": []any{}, "summary": []any{}, "encrypted_content": "old-reasoning-cipher"},
						map[string]any{"type": "reasoning", "content": []any{}, "summary": []any{}},
						map[string]any{"type": "reasoning", "encrypted_content": nil, "summary": []any{}},
						map[string]any{"type": "function_call", "call_id": "call_function", "name": "echo", "arguments": `{"text":"function payload"}`},
						map[string]any{"type": "function_call_output", "call_id": "call_function", "output": "function result"},
						map[string]any{"type": "custom_tool_call", "call_id": "call_custom", "name": "patch", "input": "custom payload"},
						map[string]any{"type": "custom_tool_call_output", "call_id": "call_custom", "output": "custom result"},
						map[string]any{"type": "compaction", "encrypted_content": "compaction-cipher", "future_state": map[string]any{"keep": true}},
						map[string]any{"type": "agent_message", "author": "worker", "recipient": "parent", "content": []any{map[string]any{"type": "input_text", "text": "visible agent result"}}},
						messageItem("user", "current question"),
						map[string]any{"type": "reasoning", "content": []any{}, "summary": []any{}, "encrypted_content": "current-reasoning-cipher"},
					},
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(jsonBytes(source), &fields); err != nil {
					t.Fatal(err)
				}
				if err := compat.PrepareHistory(fields, omit); err != nil {
					t.Fatal(err)
				}
				raw, bridge, err := PrepareWithOptions(jsonBytes(fields), "history-scope", Options{ToolTransport: mode})
				if err != nil {
					t.Fatal(err)
				}
				if bridge.textTools != (mode == "text") {
					t.Fatal("the dynamic tool catalog did not select the requested transport")
				}
				body, _ := parseRelayObject(string(raw))
				seen := map[string]int{}
				for _, value := range body["input"].([]any) {
					item := objectValue(value)
					if item["type"] != "reasoning" {
						continue
					}
					cipher, ok := item["encrypted_content"].(string)
					if !ok || cipher == "" {
						t.Fatal("cipherless reasoning reached the upstream history")
					}
					seen[cipher]++
					want := map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": cipher}
					if string(jsonBytes(item)) != string(jsonBytes(want)) {
						t.Fatal("reasoning was not normalized to the upstream replay envelope")
					}
				}
				if seen["current-reasoning-cipher"] != 1 || seen["old-reasoning-cipher"] != map[bool]int{false: 1, true: 0}[omit] {
					t.Fatal("the selected history policy changed the wrong reasoning item")
				}
				for _, value := range []string{"function payload", "function result", "custom payload", "custom result", "compaction-cipher", "future_state", "visible agent result", "worker", "current question"} {
					if !strings.Contains(string(raw), value) {
						t.Fatalf("history lost %s", value)
					}
				}
				if omit && !strings.Contains(string(raw), "An earlier opaque reasoning item was omitted") {
					t.Fatal("the history omission notice was lost")
				}
			})
		}
	}
}

func TestTextToolHistoryPreservesOpaqueAndFutureFields(t *testing.T) {
	items := []any{
		map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "visible history"}}, "phase": "commentary", "future_metadata": map[string]any{"keep": true}, "internal_chat_message_metadata_passthrough": map[string]any{"client_only": true}},
		map[string]any{"type": "compaction", "encrypted_content": "opaque-compaction", "future_state": []any{"keep"}},
		map[string]any{"type": "agent_message", "author": "worker", "recipient": "parent", "encrypted_content": "opaque-agent", "content": []any{map[string]any{"type": "encrypted_content", "encrypted_content": "opaque-agent-part"}}},
		map[string]any{"type": "future_business_item", "custom_state": map[string]any{"opaque": "keep-exactly"}},
	}
	source := map[string]any{"input": items}
	before := string(jsonBytes(source))
	body := map[string]any{}
	prepareTextTools(source, body, "")
	got := body["input"].([]any)
	if len(got) != len(items)+1 {
		t.Fatal("ordinary or opaque history items were dropped")
	}
	for index, item := range items {
		want := stripClientMetadata(objectValue(item))
		if string(jsonBytes(got[index+1])) != string(jsonBytes(want)) {
			t.Fatalf("history item %d changed beyond client-only metadata", index)
		}
	}
	if string(jsonBytes(source)) != before {
		t.Fatal("history normalization mutated the original request")
	}
}
