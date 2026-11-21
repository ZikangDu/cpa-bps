package bpsprotocol

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/upstream"
)

func invalidOfficeResponse(id string) json.RawMessage {
	call := native054Call("Read workbook", "await Excel.run(async context => context.sync());", `{}`)
	call["id"], call["call_id"] = "fc_"+id, "call_"+id
	return jsonBytes(map[string]any{"id": "resp_" + id, "status": "completed", "output": []any{call}})
}

func TestNativeRepairCorrectsOnlyUnexecutedTransportAndFinalStream(t *testing.T) {
	source := native054Source()
	source["tool_choice"] = "required"
	prepared, bridge, err := PrepareWithOptions(jsonBytes(source), "isolated-account", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	first := invalidOfficeResponse("first")
	if _, err = bridge.Transform(first); err == nil {
		t.Fatal("Office JS was mistaken for a client relay")
	}
	repaired, ok := RepairNativePayload(prepared, first, err)
	if !ok {
		t.Fatal("eligible correction unavailable")
	}
	before, _ := parseRelayObject(string(prepared))
	after, _ := parseRelayObject(string(repaired))
	beforeMeta, afterMeta := objectValue(before["metadata"]), objectValue(after["metadata"])
	if beforeMeta["agent_iteration"] != "1" || afterMeta["agent_iteration"] != "2" || beforeMeta["task_id"] != afterMeta["task_id"] || beforeMeta["turn_id"] != afterMeta["turn_id"] || before["prompt_cache_key"] != after["prompt_cache_key"] || before["model"] != after["model"] {
		t.Fatal("correction changed conversation or account policy")
	}
	original := before["input"].([]any)
	input := after["input"].([]any)
	if len(input) != len(original)+3 || !bytes.Equal(jsonBytes(input[:len(original)]), jsonBytes(original)) {
		t.Fatal("correction rewrote prior instructions/history")
	}
	call := input[len(original)].(map[string]any)
	result := input[len(original)+1].(map[string]any)
	failure, _ := parseRelayObject(result["output"])
	if call["name"] != "run_officejs" || result["call_id"] != call["call_id"] || failure["executed"] != false || objectValue(failure["error"])["code"] != "invalid_client_tool_transport" {
		t.Fatalf("missing explicit non-execution feedback: %+v", failure)
	}
	// A complete second response uses the same original declared contract.
	correct := native054Call(functionCodeRelayPrefix+"python", "print(1)\n", `{"timeout":2}`)
	correct["id"], correct["call_id"] = "fc_second", "call_second"
	final, err := bridge.Transform(jsonBytes(map[string]any{"id": "resp_second", "status": "completed", "output": []any{correct}}))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := ResponseStream(final)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := upstream.ReadEvents(context.Background(), bytes.NewReader(stream), nil)
	if err != nil || !bytes.Equal(parsed, final) || !bytes.Contains(stream, []byte(`"name":"python"`)) || bytes.Contains(stream, []byte("run_officejs")) {
		t.Fatalf("final corrected response did not round trip: %v", err)
	}
	if bytes.Count(stream, []byte("event: response.completed\n")) != 1 || bytes.Count(stream, []byte("data: [DONE]")) != 1 {
		t.Fatal("duplicate stream terminal")
	}
}

func TestNativeRepairRejectsUncertainUnsupportedAndDuplicateCalls(t *testing.T) {
	prepared, _, err := PrepareWithOptions(jsonBytes(native054Source()), "scope", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	base, _ := parseRelayObject(string(invalidOfficeResponse("first")))
	for _, tc := range []struct {
		name   string
		cause  error
		mutate func(map[string]any)
	}{
		{"transport error", errors.New("secret connection detail"), nil},
		{"permission", fail(403, "invalid_tool_call", "secret denied"), nil},
		{"other protocol", fail(422, "bps_invalid_request", "secret"), nil},
		{"incomplete", relayError("bad"), func(r map[string]any) { r["status"] = "incomplete" }},
		{"failed", relayError("bad"), func(r map[string]any) { r["status"] = "failed" }},
		{"no calls", relayError("bad"), func(r map[string]any) { r["output"] = []any{messageItem("assistant", "no tool")} }},
		{"direct tool", relayError("bad"), func(r map[string]any) { objectValue(r["output"].([]any)[0])["name"] = "python" }},
		{"custom tool", relayError("bad"), func(r map[string]any) { objectValue(r["output"].([]any)[0])["type"] = "custom_tool_call" }},
		{"missing call id", relayError("bad"), func(r map[string]any) { delete(objectValue(r["output"].([]any)[0]), "call_id") }},
		{"missing item id", relayError("bad"), func(r map[string]any) { delete(objectValue(r["output"].([]any)[0]), "id") }},
		{"invalid id", relayError("bad"), func(r map[string]any) { objectValue(r["output"].([]any)[0])["id"] = "bad\nvalue" }},
		{"duplicate", relayError("bad"), func(r map[string]any) { r["output"] = append(r["output"].([]any), r["output"].([]any)[0]) }},
		{"hosted tool", relayError("bad"), func(r map[string]any) {
			r["output"] = append(r["output"].([]any), map[string]any{"type": "web_search_call"})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := cloneObject(base)
			if tc.mutate != nil {
				tc.mutate(r)
			}
			if _, ok := RepairNativePayload(prepared, jsonBytes(r), tc.cause); ok {
				t.Fatal("unsafe correction accepted")
			}
		})
	}
	repaired, ok := RepairNativePayload(prepared, jsonBytes(base), relayError("secret-private-cause"))
	if !ok {
		t.Fatal("valid fixture failed")
	}
	if bytes.Contains(repaired, []byte("secret-private-cause")) {
		t.Fatal("internal cause reflected into upstream instructions")
	}
	if _, ok = RepairNativePayload(repaired, jsonBytes(base), relayError("duplicate")); ok {
		t.Fatal("repeated call ID collided with already corrected history")
	}
}

func TestNativeRepairBoundsAdditionsAndKeepsCompactionLast(t *testing.T) {
	source := native054Source()
	source["input"] = []any{messageItem("user", "task"), map[string]any{"type": "compaction_trigger"}}
	prepared, _, err := PrepareWithOptions(jsonBytes(source), "scope", Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	repaired, ok := RepairNativePayload(prepared, invalidOfficeResponse("first"), relayError("invalid"))
	if !ok {
		t.Fatal("safe correction rejected")
	}
	p, _ := parseRelayObject(string(repaired))
	items := p["input"].([]any)
	if objectValue(items[len(items)-1])["type"] != "compaction_trigger" {
		t.Fatal("repair moved compaction trigger")
	}
	if len(repaired)-len(prepared) > maxNativeRepairAddition {
		t.Fatal("addition limit exceeded")
	}
	huge := native054Call("invalid", strings.Repeat("x", maxNativeRepairAddition), `{}`)
	if _, ok := RepairNativePayload(prepared, jsonBytes(map[string]any{"status": "completed", "output": []any{huge}}), relayError("invalid")); ok {
		t.Fatal("oversized correction accepted")
	}
}

func TestResponseStreamRejectsNonterminalAndInvalidOutput(t *testing.T) {
	for _, raw := range []string{`{}`, `{"status":"failed","output":[]}`, `{"status":"in_progress","output":[]}`, `{"status":"completed","output":null}`, `{"status":"completed","output":[3]}`, `{"status":"completed","output":[]} {}`} {
		if _, err := ResponseStream([]byte(raw)); err == nil {
			t.Fatalf("invalid final response accepted: %s", raw)
		}
	}
	stream, err := ResponseStream([]byte(`{"status":"incomplete","output":[]}`))
	if err != nil || !bytes.Contains(stream, []byte("event: response.incomplete")) || bytes.Contains(stream, []byte("event: response.completed")) {
		t.Fatal("incomplete terminal promoted to completed")
	}
}
