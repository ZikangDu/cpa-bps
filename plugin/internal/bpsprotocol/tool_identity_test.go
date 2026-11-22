package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"testing"
)

func identitySource() map[string]any {
	return map[string]any{"model": "fixture", "input": "Use the fixture", "tools": []any{map[string]any{"type": "function", "name": "echo"}}, "tool_choice": "required"}
}

func identityNative(id, callID string) map[string]any {
	call := native054Call("Echo fixture", `{"name":"echo","arguments":{"text":"fixture"}}`, `{}`)
	call["id"], call["call_id"] = id, callID
	return call
}

func TestDuplicateOutputItemIDsCannotReachClientsOrReplayCache(t *testing.T) {
	for _, output := range [][]any{
		{identityNative("fc_shared", "call_first"), identityNative("fc_shared", "call_second")},
		{map[string]any{"type": "message", "id": "fc_shared", "role": "assistant", "content": []any{}}, identityNative("fc_shared", "call_first")},
	} {
		source := identitySource()
		_, bridge, err := Prepare(jsonBytes(source), "duplicate-item-audit")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := bridge.Transform(jsonBytes(map[string]any{"status": "completed", "output": output})); err == nil || err.(*Error).diagnosticReason != "duplicate_output_item_id" {
			t.Fatal("duplicate output item ID was delivered")
		}
		client, err := extractNativeClientToolCall(identityNative("fc_shared", "call_first"), clientToolSpecs(source))
		if err != nil || rememberedNativeCall(client, "duplicate-item-audit") != nil {
			t.Fatal("rejected duplicate IDs polluted replay history")
		}
	}
}

func TestNativeHistoryReuseIsRejectedBeforeUpdatingReplayCache(t *testing.T) {
	for _, reuse := range []string{"call_id", "id"} {
		scope := "native-history-audit/" + reuse
		source := identitySource()
		_, first, err := Prepare(jsonBytes(source), scope)
		if err != nil {
			t.Fatal(err)
		}
		native := identityNative("fc_history", "call_history")
		firstRaw, err := first.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{native}}))
		if err != nil {
			t.Fatal(err)
		}
		response, _ := parseRelayObject(string(firstRaw))
		client := objectValue(response["output"].([]any)[0])
		cached := jsonBytes(rememberedNativeCall(client, scope))
		// A call and its result legitimately share call_id in source history.
		source["input"] = []any{messageItem("user", "Earlier fixture"), client, map[string]any{"type": "function_call_output", "call_id": client["call_id"], "output": "completed"}}
		prepared, next, err := Prepare(jsonBytes(source), scope)
		if err != nil {
			t.Fatal("valid call/result pair was rejected", err)
		}
		bad := identityNative("fc_new", "call_new")
		bad[reuse] = client[reuse]
		outer, _ := parseRelayObject(bad["arguments"])
		outer["extended_summary"] = "must not overwrite original replay"
		bad["arguments"] = string(jsonBytes(outer))
		terminal := jsonBytes(map[string]any{"status": "completed", "output": []any{bad}})
		_, err = next.Transform(terminal)
		if err == nil || err.(*Error).Code() != "bps_duplicate_tool_call" {
			t.Fatal("native response reused a history identifier")
		}
		if !bytes.Equal(cached, jsonBytes(rememberedNativeCall(client, scope))) {
			t.Fatal("reused call overwrote valid cached native history")
		}
		if CanFallbackToolResponse(terminal, err) {
			t.Fatal("history reuse enabled another generation")
		}
		if _, ok := RepairNativePayload(prepared, terminal, err); ok {
			t.Fatal("history reuse enabled native repair")
		}
		if _, err := next.Transform(jsonBytes(map[string]any{"status": "completed", "output": []any{identityNative("fc_fresh", "call_fresh")}})); err != nil {
			t.Fatal("new tool identifier was rejected after valid history", err)
		}
	}
}

func TestTextGeneratedIdentifiersCannotReuseHistory(t *testing.T) {
	source := identitySource()
	_, first, err := PrepareWithTextTools(jsonBytes(source), "text-history-audit")
	if err != nil {
		t.Fatal(err)
	}
	terminal := relayResponse(`{"client_tool":"echo","arguments":{"text":"fixture"}}`)
	transformed, err := first.Transform(terminal)
	if err != nil {
		t.Fatal(err)
	}
	response, _ := parseRelayObject(string(transformed))
	call := objectValue(response["output"].([]any)[0])
	source["input"] = []any{messageItem("user", "Earlier fixture"), call, map[string]any{"type": "function_call_output", "call_id": call["call_id"], "output": "completed"}}
	_, next, err := PrepareWithTextTools(jsonBytes(source), "text-history-audit")
	if err != nil {
		t.Fatal(err)
	}
	emitted := 0
	emit := next.WrapEmitter(func([]byte) error { emitted++; return nil })
	frame := append([]byte("data: "), jsonBytes(map[string]any{"type": "response.completed", "response": json.RawMessage(terminal)})...)
	err = emit(append(frame, '\n', '\n'))
	if err == nil || err.(*Error).Code() != "bps_duplicate_tool_call" || emitted != 0 || CanFallbackToolResponse(terminal, err) {
		t.Fatal("text-generated identifier reuse was emitted or regenerated")
	}
}
