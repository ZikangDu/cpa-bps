package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestAutoFallbackOnlyCompleteUnexecutedProtocolFailures(t *testing.T) {
	protocolErr := relayError("arguments_schema_mismatch")
	message := map[string]any{"id": "msg_fixture", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": "malformed tool JSON"}}}
	call := map[string]any{"id": "fc_fixture", "type": "function_call", "name": "run_officejs",
		"call_id": "call_fixture", "arguments": "malformed JSON", "status": "completed"}
	reasoning := map[string]any{"id": "rs_fixture", "type": "reasoning", "summary": []any{}, "encrypted_content": "opaque"}
	for _, output := range [][]any{{message}, {call}, {reasoning, message, call}} {
		raw := jsonBytes(map[string]any{"status": "completed", "output": output})
		before := bytes.Clone(raw)
		if !CanFallbackToolResponse(raw, protocolErr) || !bytes.Equal(raw, before) {
			t.Fatal("known buffered unexecuted tool correction was unavailable or mutated input")
		}
	}
	base := map[string]any{"status": "completed", "output": []any{message, call}}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
		cause  error
	}{
		{"nil error", nil, nil},
		{"uncertain transport", nil, errors.New("invalid_tool_call")},
		{"wrong protocol code", nil, fail(422, "bps_invalid_schema", "schema")},
		{"wrong status", nil, fail(403, "invalid_tool_call", "denied")},
		{"failed", func(r map[string]any) { r["status"] = "failed" }, protocolErr},
		{"incomplete", func(r map[string]any) { r["status"] = "incomplete" }, protocolErr},
		{"no status", func(r map[string]any) { delete(r, "status") }, protocolErr},
		{"error", func(r map[string]any) { r["error"] = map[string]any{"code": "quota"} }, protocolErr},
		{"conflicting incomplete detail", func(r map[string]any) { r["incomplete_details"] = map[string]any{"reason": "max_output_tokens"} }, protocolErr},
		{"top refusal", func(r map[string]any) { r["refusal"] = "declined" }, protocolErr},
		{"no output", func(r map[string]any) { delete(r, "output") }, protocolErr},
		{"empty output", func(r map[string]any) { r["output"] = []any{} }, protocolErr},
		{"unknown hosted", func(r map[string]any) {
			r["output"] = append(r["output"].([]any), map[string]any{"type": "web_search_call"})
		}, protocolErr},
		{"tool output", func(r map[string]any) {
			r["output"] = append(r["output"].([]any), map[string]any{"type": "function_call_output"})
		}, protocolErr},
		{"direct function", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["name"] = "charge_card" }, protocolErr},
		{"custom function", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["type"] = "custom_tool_call" }, protocolErr},
		{"unknown native", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["name"] = "run_connector_action" }, protocolErr},
		{"native namespace spoof", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["namespace"] = "unknown" }, protocolErr},
		{"native execution evidence", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["executed"] = true }, protocolErr},
		{"provisional native", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["status"] = "in_progress" }, protocolErr},
		{"missing call id", func(r map[string]any) { delete(r["output"].([]any)[1].(map[string]any), "call_id") }, protocolErr},
		{"bad call id", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["call_id"] = "bad id" }, protocolErr},
		{"duplicate item id", func(r map[string]any) { r["output"].([]any)[1].(map[string]any)["id"] = "msg_fixture" }, protocolErr},
		{"duplicate call id", func(r map[string]any) {
			c := cloneObject(r["output"].([]any)[1].(map[string]any))
			c["id"] = "other"
			r["output"] = append(r["output"].([]any), c)
		}, protocolErr},
		{"message refusal", func(r map[string]any) {
			r["output"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "refusal", "refusal": "declined"}}
		}, protocolErr},
		{"nonassistant message", func(r map[string]any) { r["output"].([]any)[0].(map[string]any)["role"] = "tool" }, protocolErr},
		{"unrecognized content", func(r map[string]any) {
			r["output"].([]any)[0].(map[string]any)["content"] = []any{map[string]any{"type": "image", "data": "opaque"}}
		}, protocolErr},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := cloneObject(base)
			if test.mutate != nil {
				test.mutate(r)
			}
			if CanFallbackToolResponse(jsonBytes(r), test.cause) {
				t.Fatal("unsafe or unrelated failure allowed protocol replay")
			}
		})
	}
	for _, raw := range []string{`{"status":"completed",`, `null`, `[]`, `{} {}`} {
		if CanFallbackToolResponse([]byte(raw), protocolErr) {
			t.Fatal("malformed response allowed replay")
		}
	}
}

func TestMergeResponseUsageCountsAllAttemptsWithoutChangingOutput(t *testing.T) {
	first := []byte(`{"status":"completed","error":{"code":"private-code"},"output":[{"type":"message","content":"private-output"}],"usage":{"input_tokens":9007199254740993,"output_tokens":2,"total_tokens":9007199254740995,"input_tokens_details":{"cached_tokens":5,"cache_write_tokens":3,"cache_write_tokens_details":{"ttl_30m_tokens":3,"ttl_12h_tokens":0}},"output_tokens_details":{"reasoning_tokens":1}}}`)
	final := []byte(`{"id":"final","model":"fixture","status":"completed","metadata":{"client":"keep","cpa_bps_usage_complete":"stale"},"output":[{"type":"function_call","name":"safe","arguments":"{}","call_id":"final-call"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10,"input_tokens_details":{"cached_tokens":6,"cache_write_tokens":1,"cache_write_tokens_details":{"ttl_30m_tokens":1,"ttl_12h_tokens":0}},"output_tokens_details":{"reasoning_tokens":2},"note":"final-only"}}`)
	beforeFirst, beforeFinal := bytes.Clone(first), bytes.Clone(final)
	got, err := MergeResponseUsage(final, [][]byte{first})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := fallbackObject(got)
	original, _ := fallbackObject(final)
	usage := objectValue(result["usage"])
	for key, want := range map[string]string{"input_tokens": "9007199254741000", "output_tokens": "5", "total_tokens": "9007199254741005"} {
		if usage[key] != json.Number(want) {
			t.Fatalf("%s lost exact counters: %v", key, usage[key])
		}
	}
	details := objectValue(usage["input_tokens_details"])
	if details["cached_tokens"] != json.Number("11") || details["cache_write_tokens"] != json.Number("4") || objectValue(details["cache_write_tokens_details"])["ttl_12h_tokens"] != json.Number("0") {
		t.Fatal("cache read/write details were lost or added to input")
	}
	if objectValue(usage["output_tokens_details"])["reasoning_tokens"] != json.Number("3") {
		t.Fatal("reasoning usage omitted")
	}
	if !reflect.DeepEqual(result["output"], original["output"]) || result["model"] != original["model"] || result["id"] != original["id"] || bytes.Contains(got, []byte("private-")) {
		t.Fatal("earlier response leaked or final output identity changed")
	}
	meta := objectValue(result["metadata"])
	if meta["client"] != "keep" || meta["cpa_bps_usage_complete"] != "true" || meta["cpa_bps_attempts"] != "2" {
		t.Fatal("aggregation metadata incorrect")
	}
	if !bytes.Equal(first, beforeFirst) || !bytes.Equal(final, beforeFinal) {
		t.Fatal("usage merge mutated supplied bytes")
	}
}

func TestMergeResponseUsageMissingCountersRemainUnknown(t *testing.T) {
	first := []byte(`{"usage":{"input_tokens":3,"output_tokens":1,"input_tokens_details":{"cached_tokens":2}}}`)
	final := []byte(`{"status":"completed","output":[],"usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7,"input_tokens_details":{"cached_tokens":4,"cache_write_tokens":0},"unknown_numeric":12}}`)
	got, err := MergeResponseUsage(final, [][]byte{first})
	if err != nil {
		t.Fatal(err)
	}
	result, _ := fallbackObject(got)
	usage := objectValue(result["usage"])
	if usage["input_tokens"] != json.Number("8") || usage["output_tokens"] != json.Number("3") || usage["total_tokens"] != nil || usage["unknown_numeric"] != nil {
		t.Fatal("partial total was invented or used as a full total")
	}
	details := objectValue(usage["input_tokens_details"])
	if details["cached_tokens"] != json.Number("6") || details["cache_write_tokens"] != nil || objectValue(result["metadata"])["cpa_bps_usage_complete"] != "false" {
		t.Fatal("missing details were treated as zero")
	}
	for _, noUsage := range []string{`{"status":"completed","output":[]}`, `{"status":"completed","output":[],"usage":null}`} {
		got, err := MergeResponseUsage([]byte(noUsage), [][]byte{first})
		if err != nil {
			t.Fatal(err)
		}
		r, _ := fallbackObject(got)
		if r["usage"] != nil || objectValue(r["metadata"])["cpa_bps_usage_complete"] != "false" {
			t.Fatal("missing final usage was invented")
		}
	}
	got, err = MergeResponseUsage(final, [][]byte{[]byte(`{"status":"completed"}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := fallbackObject(got)
	if len(objectValue(r["usage"])) != 0 || objectValue(r["metadata"])["cpa_bps_usage_complete"] != "false" {
		t.Fatal("missing earlier usage retained final-only counters")
	}
	priorUnknown := []byte(`{"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3,"private_extension":{"value":10}}}`)
	finalKnown := []byte(`{"output":[],"usage":{"input_tokens":2,"output_tokens":3,"total_tokens":5}}`)
	got, err = MergeResponseUsage(finalKnown, [][]byte{priorUnknown})
	if err != nil {
		t.Fatal(err)
	}
	r, _ = fallbackObject(got)
	if objectValue(r["metadata"])["cpa_bps_usage_complete"] != "false" || bytes.Contains(got, []byte("private_extension")) {
		t.Fatal("unknown prior counter was hidden or copied")
	}
}

func TestMergeResponseUsageRejectsInvalidCountersAndOverflow(t *testing.T) {
	final := []byte(`{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":0,"total_tokens":1}}`)
	for _, prior := range []string{
		`{"usage":{"input_tokens":-1}}`, `{"usage":{"input_tokens":"2"}}`, `{"usage":{"input_tokens":1.5}}`,
		fmt.Sprintf(`{"usage":{"input_tokens":%d}}`, int64(math.MaxInt64)),
		`{"usage":{"input_tokens_details":{"cached_tokens":-1}}}`, `{"usage":[]}`, `null`, `{}` + `{}`,
	} {
		if _, err := MergeResponseUsage(final, [][]byte{[]byte(prior)}); !errors.Is(err, ErrInvalidResponseUsage) {
			t.Fatalf("invalid usage accepted: %s (%v)", prior, err)
		}
	}
	got, err := MergeResponseUsage(final, nil)
	if err != nil || !bytes.Equal(got, final) {
		t.Fatal("one-attempt response should remain byte-for-byte unchanged")
	}
}

func alternateSource() []byte {
	return []byte(`{"model":"fixture","input":[{"role":"user","content":"Call sum"}],"tools":[{"type":"function","name":"sum","parameters":{"type":"object","properties":{"a":{"type":"integer","minimum":2},"b":{"type":"integer"}},"required":["a","b"],"additionalProperties":false}},{"type":"custom","name":"patch"}],"tool_choice":"required"}`)
}

func alternateMessage(text string) map[string]any {
	return map[string]any{"id": "msg_alternate", "type": "message", "role": "assistant", "status": "completed",
		"content": []any{map[string]any{"type": "output_text", "text": text}}}
}

func alternateCall() map[string]any {
	return map[string]any{"type": "function_call", "name": "sum", "id": "fc_alternate", "call_id": "call_alternate", "status": "completed", "arguments": `{"a":2,"b":3}`}
}

func TestAlternateDecodeRescuesSameTerminalWithoutAnotherGeneration(t *testing.T) {
	for _, test := range []struct {
		name, primary, selected, kind string
		output                        []any
	}{
		{"text received direct function", "text", "native", "function_call", []any{alternateCall()}},
		{"native received text function", "native", "text", "function_call", []any{alternateMessage(`{"client_tool":"sum","arguments":{"a":2,"b":3}}`)}},
		{"native received text custom", "native", "text", "custom_tool_call", []any{alternateMessage(`{"client_tool":"patch","input":"  exact raw\n"}`)}},
		{"text received direct custom", "text", "native", "custom_tool_call", []any{map[string]any{"type": "custom_tool_call", "name": "patch", "id": "ctc_alternate", "call_id": "call_alternate", "status": "completed", "input": "  exact raw\n"}}},
		{"text received native relay", "text", "native", "function_call", []any{native054Call("Relay", `{"name":"sum","arguments":{"a":2,"b":3}}`, "")}},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := alternateSource()
			terminal := jsonBytes(map[string]any{"status": "completed", "id": "response_fixture", "model": "fixture", "output": test.output, "usage": map[string]any{"input_tokens": 11, "output_tokens": 4, "total_tokens": 15}})
			sourceBefore, terminalBefore := bytes.Clone(source), bytes.Clone(terminal)
			_, first, err := PrepareWithOptions(source, "alternate-success/"+test.name, Options{ToolTransport: test.primary})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := first.Transform(terminal); err == nil {
				t.Fatal("fixture must fail primary decoding")
			}
			out, mode, ok := DecodeAlternateToolResponse(source, terminal, "alternate-success/"+test.name, Options{ToolTransport: test.primary})
			if !ok || mode != test.selected {
				t.Fatal("valid same-response alternate was unavailable")
			}
			result, _ := fallbackObject(out)
			calls := result["output"].([]any)
			if len(calls) != 1 || objectValue(calls[0])["type"] != test.kind || result["model"] != "fixture" || objectValue(result["usage"])["total_tokens"] != json.Number("15") {
				t.Fatal("conversion changed final response identity or usage")
			}
			if !bytes.Equal(source, sourceBefore) || !bytes.Equal(terminal, terminalBefore) {
				t.Fatal("alternate decode mutated original source/history")
			}
		})
	}
}

func TestAlternateDecodeRejectsUntrustedOrIncompleteTools(t *testing.T) {
	for _, test := range []struct {
		name, primary  string
		changeSource   func(map[string]any)
		changeResponse func(map[string]any)
	}{
		{"undeclared direct", "text", nil, func(r map[string]any) { r["output"].([]any)[0].(map[string]any)["name"] = "charge_card" }},
		{"invalid full schema", "text", nil, func(r map[string]any) { r["output"].([]any)[0].(map[string]any)["arguments"] = `{"a":1,"b":3}` }},
		{"invalid type", "text", nil, func(r map[string]any) { r["output"].([]any)[0].(map[string]any)["arguments"] = `{"a":2.5,"b":3}` }},
		{"none tool policy", "text", func(s map[string]any) { s["tool_choice"] = "none" }, nil},
		{"already executed call", "text", func(s map[string]any) {
			s["input"] = append(s["input"].([]any), alternateCall(), map[string]any{"type": "function_call_output", "call_id": "call_alternate", "output": "done"})
		}, nil},
		{"different selected tool", "text", func(s map[string]any) { s["tool_choice"] = map[string]any{"type": "custom", "name": "patch"} }, nil},
		{"parallel disabled", "text", func(s map[string]any) { s["parallel_tool_calls"] = false }, func(r map[string]any) {
			c := alternateCall()
			c["id"], c["call_id"] = "fc_second", "call_second"
			r["output"] = append(r["output"].([]any), c)
		}},
		{"duplicate item id", "text", nil, func(r map[string]any) {
			c := alternateCall()
			c["call_id"] = "call_second"
			r["output"] = append(r["output"].([]any), c)
		}},
		{"duplicate call id", "text", nil, func(r map[string]any) {
			c := alternateCall()
			c["id"] = "fc_second"
			r["output"] = append(r["output"].([]any), c)
		}},
		{"failed terminal", "text", nil, func(r map[string]any) { r["status"] = "failed" }},
		{"incomplete terminal", "text", nil, func(r map[string]any) { r["status"] = "incomplete" }},
		{"partial item", "text", nil, func(r map[string]any) { r["output"].([]any)[0].(map[string]any)["status"] = "in_progress" }},
		{"mixed hosted action", "text", nil, func(r map[string]any) {
			r["output"] = append(r["output"].([]any), map[string]any{"type": "web_search_call"})
		}},
		{"mixed refusal", "text", nil, func(r map[string]any) {
			r["output"] = append(r["output"].([]any), map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "refusal", "refusal": "no"}}})
		}},
		{"top error", "text", nil, func(r map[string]any) { r["error"] = map[string]any{"code": "denied"} }},
		{"plain prose no tool", "native", nil, func(r map[string]any) { r["output"] = []any{alternateMessage("I did not call a tool")} }},
		{"text invalid schema", "native", nil, func(r map[string]any) {
			r["output"] = []any{alternateMessage(`{"client_tool":"sum","arguments":{"a":1,"b":3}}`)}
		}},
		{"text undeclared", "native", nil, func(r map[string]any) {
			r["output"] = []any{alternateMessage(`{"client_tool":"delete","arguments":{}}`)}
		}},
		{"raw office code", "text", nil, func(r map[string]any) {
			r["output"] = []any{native054Call("Read workbook", "Excel.run(async ctx => ctx.sync());", "")}
		}},
		{"unknown primary", "guess", nil, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			source, _ := fallbackObject(alternateSource())
			response := map[string]any{"status": "completed", "model": "fixture", "output": []any{alternateCall()}}
			if test.changeSource != nil {
				test.changeSource(source)
			}
			if test.changeResponse != nil {
				test.changeResponse(response)
			}
			nativeCallCache.Lock()
			before := len(nativeCallCache.items)
			nativeCallCache.Unlock()
			out, mode, ok := DecodeAlternateToolResponse(jsonBytes(source), jsonBytes(response), "alternate-reject/"+test.name, Options{ToolTransport: test.primary})
			if ok || out != nil || mode != "" {
				t.Fatal("untrusted response accepted as alternate tool")
			}
			nativeCallCache.Lock()
			after := len(nativeCallCache.items)
			nativeCallCache.Unlock()
			if before != after {
				t.Fatal("failed alternate decode polluted native history cache")
			}
		})
	}
	for _, source := range [][]byte{nil, []byte(`{"tools":`)} {
		if _, _, ok := DecodeAlternateToolResponse(source, jsonBytes(map[string]any{"status": "completed", "output": []any{alternateCall()}}), "fixture", Options{ToolTransport: "text"}); ok {
			t.Fatal("bad source accepted")
		}
	}
}

func TestAlternateDecodeCombinedStructuredAnswerAndIntermediateTool(t *testing.T) {
	source, _ := fallbackObject(alternateSource())
	source["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "final_answer", "schema": map[string]any{"type": "object", "required": []any{"answer"}, "properties": map[string]any{"answer": map[string]any{"type": "integer"}}, "additionalProperties": false}}}
	for _, test := range []struct {
		primary, mode string
		item          map[string]any
	}{
		{"text", "native", alternateCall()},
		{"native", "text", alternateMessage(`{"client_tool":"sum","arguments":{"a":2,"b":3}}`)},
	} {
		terminal := jsonBytes(map[string]any{"status": "completed", "model": "fixture", "output": []any{test.item}})
		out, mode, ok := DecodeAlternateToolResponse(jsonBytes(source), terminal, "combined-format/"+test.primary, Options{ToolTransport: test.primary})
		if !ok || mode != test.mode {
			t.Fatal("intermediate tool was required to satisfy final-answer schema")
		}
		result, _ := fallbackObject(out)
		if !responseHasClientTool(result) {
			t.Fatal("alternate conversion lost intermediate call")
		}
	}
}
