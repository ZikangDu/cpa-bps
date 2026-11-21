package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func namespacedCacheTools() []any {
	return []any{map[string]any{
		"type": "namespace", "name": "fixture", "tools": []any{
			map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{
				"type": "object", "properties": map[string]any{"query": map[string]any{"type": "string"}},
				"required": []any{"query"}, "additionalProperties": false,
			}},
			map[string]any{"type": "custom", "name": "note"},
		},
	}}
}

func relayResponse(text string) []byte {
	return jsonBytes(map[string]any{
		"id": "resp_cache_fixture", "status": "completed", "output": []any{messageItem("assistant", text)},
	})
}

func TestAutoTextToolsRetainCompleteInputPrefixAcrossThreeTurns(t *testing.T) {
	for _, choice := range []any{nil, "", "auto"} {
		t.Run("choice_"+stringValue(choice), func(t *testing.T) {
			source := map[string]any{
				"model": "fixture", "instructions": "Preserve this fixed handbook exactly.",
				"input": []any{messageItem("user", "Use the fixture tools, then answer.")},
				"tools": namespacedCacheTools(), "tool_choice": choice,
			}
			var previousInput []any
			var previousKey any
			for turn, text := range []string{
				`{"client_tool":"fixture.lookup","arguments":{"query":"fixture"}}`,
				`{"client_tool":"fixture.note","input":" exact note\n"}`,
				"Completed.",
			} {
				body, bridge := preparedDocument(t, source, "account-a/caller-a")
				input := body["input"].([]any)
				if turn > 0 {
					if len(input) <= len(previousInput) || !bytes.Equal(jsonBytes(previousInput), jsonBytes(input[:len(previousInput)])) {
						t.Fatalf("turn %d did not retain the complete previous input as an exact prefix", turn+1)
					}
					if body["prompt_cache_key"] != previousKey {
						t.Fatal("tool replay changed the conversation cache key")
					}
				}
				for _, raw := range input {
					item := objectValue(raw)
					if strings.HasPrefix(itemText(item["content"]), "Current-turn tool policy.") {
						t.Fatal("auto choice appended a redundant dynamic policy")
					}
				}
				for range 5 {
					replayed, _ := preparedDocument(t, source, "account-a/caller-a")
					if !bytes.Equal(jsonBytes(input), jsonBytes(replayed["input"])) {
						t.Fatal("repeated function/custom history changed its serialized input")
					}
				}
				transformed, err := bridge.Transform(relayResponse(text))
				if err != nil {
					t.Fatal(err)
				}
				var response map[string]any
				if err := json.Unmarshal(transformed, &response); err != nil {
					t.Fatal(err)
				}
				if turn < 2 {
					call := objectValue(response["output"].([]any)[0])
					if call["namespace"] != "fixture" || call["call_id"] == "" {
						t.Fatal("auto policy changed namespaced tool identity")
					}
					if turn == 1 && call["input"] != " exact note\n" {
						t.Fatal("custom tool replay changed raw input whitespace")
					}
					history := append(source["input"].([]any), response["output"].([]any)...)
					source["input"] = append(history,
						map[string]any{"type": call["type"].(string) + "_output", "call_id": call["call_id"], "output": "fixture-result"},
						messageItem("user", "Continue using the supplied results."),
					)
				}
				previousInput, previousKey = input, body["prompt_cache_key"]
			}
		})
	}
}

func TestRestrictedTextToolPoliciesRemainAtTailAndEnforced(t *testing.T) {
	selected := map[string]any{"type": "function", "namespace": "fixture", "name": "lookup"}
	for _, fixture := range []struct {
		name          string
		choice        any
		allowFunction bool
		allowCustom   bool
		requireCall   bool
	}{
		{"none", "none", false, false, false},
		{"required", "required", true, true, true},
		{"specific", selected, true, false, true},
		{"allowed_auto", map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{selected}}, true, false, false},
		{"allowed_required", map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{selected}}, true, false, true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			source := map[string]any{
				"model": "fixture", "input": []any{messageItem("user", "Follow the current tool policy.")},
				"tools": namespacedCacheTools(), "tool_choice": fixture.choice,
			}
			body, bridge := preparedDocument(t, source, "account-a/caller-a")
			input := body["input"].([]any)
			tail := objectValue(input[len(input)-1])
			if tail["role"] != "developer" || !strings.HasPrefix(itemText(tail["content"]), "Current-turn tool policy.") {
				t.Fatal("restricted tool choice lost its current-turn tail policy")
			}
			if fixture.allowFunction && !strings.Contains(itemText(tail["content"]), "fixture.lookup") {
				t.Fatal("namespaced allowed tool is absent from the policy")
			}
			for _, response := range []struct {
				text  string
				allow bool
			}{
				{`{"client_tool":"fixture.lookup","arguments":{"query":"fixture"}}`, fixture.allowFunction},
				{`{"client_tool":"fixture.note","input":"fixture"}`, fixture.allowCustom},
				{`{"client_tool":"undeclared","arguments":{}}`, false},
				{"Plain answer.", !fixture.requireCall},
			} {
				_, err := bridge.Transform(relayResponse(response.text))
				if (err == nil) != response.allow {
					t.Fatalf("tool policy accepted=%v, expected=%v: %s", err == nil, response.allow, response.text)
				}
			}
		})
	}
}
