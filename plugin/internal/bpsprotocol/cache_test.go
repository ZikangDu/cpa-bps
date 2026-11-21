package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func preparedDocument(t *testing.T, source map[string]any, scope string) (map[string]any, *Bridge) {
	t.Helper()
	raw, bridge, err := PrepareWithTextTools(jsonBytes(source), scope)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	return body, bridge
}

func TestCacheKeyFollowsConversationAndIsolatesAccountCallerModel(t *testing.T) {
	source := map[string]any{
		"model": "fixture", "input": []any{
			messageItem("developer", "Shared application policy."),
			messageItem("user", "Remember session A."),
		},
	}
	first, _ := preparedDocument(t, source, "account-a/caller-a")
	key, ok := first["prompt_cache_key"].(string)
	if !ok || len(key) != 64 || strings.Contains(key, "Remember") {
		t.Fatal("cache key must be a bounded digest")
	}
	source["input"] = append(source["input"].([]any),
		messageItem("assistant", "Ready."),
		messageItem("user", "Continue session A."),
	)
	continued, _ := preparedDocument(t, source, "account-a/caller-a")
	if continued["prompt_cache_key"] != key {
		t.Fatal("appending history changed the conversation cache key")
	}
	for _, scope := range []string{"account-b/caller-a", "account-a/caller-b"} {
		other, _ := preparedDocument(t, source, scope)
		if other["prompt_cache_key"] == key {
			t.Fatal("cache routing crossed account or caller scope")
		}
	}
	source["model"] = "other-model"
	otherModel, _ := preparedDocument(t, source, "account-a/caller-a")
	if otherModel["prompt_cache_key"] == key {
		t.Fatal("cache routing crossed model scope")
	}
	source["model"] = "fixture"
	source["input"] = []any{messageItem("developer", "Shared application policy."), messageItem("user", "Remember session B.")}
	otherConversation, _ := preparedDocument(t, source, "account-a/caller-a")
	if otherConversation["prompt_cache_key"] == key {
		t.Fatal("shared leading policy collapsed independent user conversations")
	}
	source["prompt_cache_key"] = "customer-selected-session"
	explicit, _ := preparedDocument(t, source, "account-a/caller-a")
	source["input"] = "new input in the selected session"
	explicitContinued, _ := preparedDocument(t, source, "account-a/caller-a")
	if explicit["prompt_cache_key"] != explicitContinued["prompt_cache_key"] ||
		strings.Contains(explicit["prompt_cache_key"].(string), "customer-selected-session") {
		t.Fatal("explicit session key lost continuity or was exposed verbatim")
	}
}

func TestToolCachePrefixIsDeterministicAcrossChoiceTransitions(t *testing.T) {
	tools := []any{
		map[string]any{"type": "function", "name": "z_lookup", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "custom", "name": "a_note"},
		map[string]any{"type": "custom", "name": "m_note"},
	}
	source := map[string]any{
		"model": "fixture", "instructions": " Keep exact instruction whitespace. ",
		"input": []any{messageItem("user", "Call a tool.")}, "tools": tools,
		"tool_choice": map[string]any{"type": "function", "name": "z_lookup"},
	}
	var first []byte
	for range 40 {
		raw, _, err := PrepareWithTextTools(jsonBytes(source), "scope")
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = raw
		} else if !bytes.Equal(first, raw) {
			t.Fatal("identical tool request produced a nondeterministic payload")
		}
	}
	before, _ := preparedDocument(t, source, "scope")
	source["tool_choice"] = "none"
	source["input"] = append(source["input"].([]any),
		map[string]any{"type": "function_call", "name": "z_lookup", "call_id": "fixture-call", "arguments": "{}"},
		map[string]any{"type": "function_call_output", "call_id": "fixture-call", "output": "result"},
	)
	after, bridge := preparedDocument(t, source, "scope")
	beforeInput := before["input"].([]any)
	afterInput := after["input"].([]any)
	// The original policy, instructions and user input are stable before the
	// dynamic tool-choice policy, which is placed at the tail of each request.
	if !bytes.Equal(jsonBytes(beforeInput[:len(beforeInput)-1]), jsonBytes(afterInput[:len(beforeInput)-1])) {
		t.Fatal("tool result and choice changed the earlier cacheable prefix")
	}
	if before["prompt_cache_key"] != after["prompt_cache_key"] {
		t.Fatal("tool result changed conversation cache routing")
	}
	if !strings.Contains(string(jsonBytes(afterInput[len(afterInput)-1])), "No client tool is callable") {
		t.Fatal("current tool prohibition is missing")
	}
	call := map[string]any{"client_tool": "z_lookup", "arguments": map[string]any{}}
	response := map[string]any{"status": "completed", "output": []any{messageItem("assistant", string(jsonBytes(call)))}}
	if _, err := bridge.Transform(jsonBytes(response)); err == nil {
		t.Fatal("stable declared catalog made a forbidden tool callable")
	}
}

func TestStructuredInstructionsPrecedeChangingHistory(t *testing.T) {
	source := map[string]any{
		"model": "fixture", "input": []any{messageItem("user", "Return the first result.")},
		"text": map[string]any{"format": map[string]any{"type": "json_object"}},
	}
	for _, withTools := range []bool{false, true} {
		if withTools {
			source["tools"] = []any{map[string]any{"type": "custom", "name": "note"}}
			source["tool_choice"] = "none"
		}
		body, _ := preparedDocument(t, source, "scope")
		input := body["input"].([]any)
		formatIndex, userIndex := -1, -1
		for index, item := range input {
			entry := objectValue(item)
			if entry["role"] == "user" && userIndex < 0 {
				userIndex = index
			}
			if strings.Contains(itemText(entry["content"]), "client's output format") {
				formatIndex = index
			}
		}
		if formatIndex < 0 || userIndex < 0 || formatIndex >= userIndex {
			t.Fatalf("structured output policy is not in the stable prefix, tools=%v", withTools)
		}
	}
}
