package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/compat"
)

func textHistorySource() map[string]any {
	return map[string]any{"model": "fixture", "tools": []any{map[string]any{"type": "function", "name": "echo"}}, "tool_choice": "none", "input": []any{
		messageItem("user", "Earlier fixture"),
		map[string]any{"type": "function_call", "name": "echo", "call_id": "call_history", "arguments": `{}`},
		map[string]any{"type": "function_call_output", "call_id": "call_history", "output": "fixture result"},
		messageItem("user", "Give the final answer"),
	}}
}

func TestBridgeIncrementalTextPolicySeparatesToolContracts(t *testing.T) {
	for _, test := range []struct {
		name, mode string
		choice     any
		structured bool
		plain      bool
		final      bool
	}{
		{"text history none", "text", "none", false, true, false},
		{"text empty allowed", "text", map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{}}, false, true, false},
		{"text relay auto", "text", "auto", false, false, false},
		{"text relay required", "text", "required", false, false, false},
		{"native optional", "native", "auto", false, false, true},
		{"native none", "native", "none", false, true, false},
		{"native required", "native", "required", false, false, false},
		{"native specific", "native", map[string]any{"type": "function", "name": "echo"}, false, false, false},
		{"native allowed required", "native", map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "function", "name": "echo"}}}, false, false, false},
		{"structured no tools", "text", "none", true, false, false},
		{"structured native optional", "native", "auto", true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := textHistorySource()
			source["tool_choice"] = test.choice
			if test.structured {
				source["text"] = map[string]any{"format": map[string]any{"type": "json_object"}}
			}
			_, bridge, err := PrepareWithOptions(jsonBytes(source), "stream-policy", Options{ToolTransport: test.mode})
			if err != nil {
				t.Fatal(err)
			}
			if bridge.CanStreamPlainText() != test.plain || bridge.CanStreamNativeFinalText() != test.final {
				t.Fatal("unsafe or unnecessarily buffered stream policy")
			}
		})
	}
	var absent *Bridge
	if absent.CanStreamPlainText() || absent.CanStreamNativeFinalText() {
		t.Fatal("missing protocol contract authorized early output")
	}
}

func streamFrame(kind string, sequence int, fields map[string]any) []byte {
	fields["type"], fields["sequence_number"] = kind, sequence
	return []byte("event: " + kind + "\ndata: " + string(jsonBytes(fields)) + "\n\n")
}

func historyStreamingEmitter(t *testing.T) (func([]byte) error, *[]map[string]any) {
	t.Helper()
	source := jsonBytes(textHistorySource())
	_, bridge, err := PrepareWithTextTools(source, "history-stream")
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(source, &raw) != nil {
		t.Fatal("invalid fixture")
	}
	catalog, err := compat.DeclaredTools(raw)
	if err != nil {
		t.Fatal(err)
	}
	events := []map[string]any{}
	emit := bridge.WrapEmitter(compat.GuardStream(catalog, func(frame []byte) error {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("data:")) {
				var event map[string]any
				if json.Unmarshal(bytes.TrimSpace(line[5:]), &event) != nil {
					t.Fatal("invalid emitted JSON")
				}
				events = append(events, event)
			}
		}
		return nil
	}))
	return emit, &events
}

func beginTextMessage(t *testing.T, emit func([]byte) error, index, sequence int, id string) {
	t.Helper()
	if err := emit(streamFrame("response.output_item.added", sequence, map[string]any{"output_index": index, "item": map[string]any{"type": "message", "role": "assistant", "id": id, "status": "in_progress", "content": []any{}}})); err != nil {
		t.Fatal(err)
	}
}

func textDelta(t *testing.T, emit func([]byte) error, index, sequence int, id, delta string) {
	t.Helper()
	if err := emit(streamFrame("response.output_text.delta", sequence, map[string]any{"output_index": index, "content_index": 0, "item_id": id, "delta": delta})); err != nil {
		t.Fatal(err)
	}
}

func terminalMessage(id, text string) map[string]any {
	message := messageItem("assistant", text)
	message["id"], message["status"] = id, "completed"
	return message
}

func TestNoCallableHistoryStreamsTextOnceWithOriginalIndexes(t *testing.T) {
	emit, events := historyStreamingEmitter(t)
	if err := emit(streamFrame("response.created", 0, map[string]any{"response": map[string]any{"id": "resp_stream", "status": "in_progress", "output": []any{}}})); err != nil {
		t.Fatal(err)
	}
	beginTextMessage(t, emit, 0, 1, "msg_first")
	if len(*events) != 0 {
		t.Fatal("empty metadata was presented as an early answer")
	}
	textDelta(t, emit, 0, 2, "msg_first", "Hello ")
	if len(*events) != 3 || (*events)[2]["delta"] != "Hello " {
		t.Fatal("ordinary text waited for the terminal response")
	}
	textDelta(t, emit, 0, 3, "msg_first", "world")
	terminal := map[string]any{"id": "resp_stream", "status": "completed", "output": []any{terminalMessage("msg_first", "Hello world")}}
	if err := emit(streamFrame("response.completed", 4, map[string]any{"response": terminal})); err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	added, completed := 0, 0
	for _, event := range *events {
		switch event["type"] {
		case "response.output_item.added":
			added++
			if event["output_index"] != float64(0) || objectValue(event["item"])["id"] != "msg_first" {
				t.Fatal("early message identity changed")
			}
		case "response.output_text.delta":
			text.WriteString(event["delta"].(string))
			if event["item_id"] != "msg_first" || event["output_index"] != float64(0) {
				t.Fatal("delta identity changed")
			}
		case "response.completed":
			completed++
		}
	}
	if text.String() != "Hello world" || added != 1 || completed != 1 {
		t.Fatal("terminal synthesis repeated the published message")
	}
}

func TestNoCallableHistoryHoldsToolJSONUntilRejected(t *testing.T) {
	for _, priorText := range []bool{false, true} {
		emit, events := historyStreamingEmitter(t)
		index := 0
		output := []any{}
		if priorText {
			beginTextMessage(t, emit, 0, 0, "msg_prior")
			textDelta(t, emit, 0, 1, "msg_prior", "Ordinary prefix.")
			output = append(output, terminalMessage("msg_prior", "Ordinary prefix."))
			index = 1
		}
		already := len(*events)
		beginTextMessage(t, emit, index, 2, "msg_json")
		textDelta(t, emit, index, 3, "msg_json", " \n")
		body := `{"client_tool":"echo","arguments":{}}`
		textDelta(t, emit, index, 4, "msg_json", body)
		if len(*events) != already {
			t.Fatal("forbidden text relay JSON escaped before validation")
		}
		output = append(output, terminalMessage("msg_json", " \n"+body))
		err := emit(streamFrame("response.completed", 5, map[string]any{"response": map[string]any{"status": "completed", "output": output}}))
		if err == nil || len(*events) != already {
			t.Fatal("forbidden relay was delivered or prior content repeated")
		}
	}
}

func TestNoCallableHistoryReleasesValidJSONSuffixAfterTerminal(t *testing.T) {
	emit, events := historyStreamingEmitter(t)
	beginTextMessage(t, emit, 0, 0, "msg_prior")
	textDelta(t, emit, 0, 1, "msg_prior", "Result follows.")
	prior := len(*events)
	beginTextMessage(t, emit, 1, 2, "msg_json")
	textDelta(t, emit, 1, 3, "msg_json", `{"answer":2}`)
	if len(*events) != prior {
		t.Fatal("JSON suffix was not held")
	}
	if err := emit(streamFrame("response.completed", 4, map[string]any{"response": map[string]any{"status": "completed", "output": []any{terminalMessage("msg_prior", "Result follows."), terminalMessage("msg_json", `{"answer":2}`)}}})); err != nil {
		t.Fatal(err)
	}
	if len(*events) != prior+3 || (*events)[prior]["output_index"] != float64(1) || (*events)[prior+1]["item_id"] != "msg_json" || (*events)[prior+1]["delta"] != `{"answer":2}` || (*events)[prior+2]["type"] != "response.completed" {
		t.Fatal("validated suffix was reordered or the original prefix was replayed")
	}
}

func TestNoCallableHistoryRejectsMovedOrChangedStreamText(t *testing.T) {
	for _, output := range [][]any{
		{terminalMessage("msg_other", "Inserted"), terminalMessage("msg_prior", "Published prefix")},
		{terminalMessage("msg_prior", "Replaced text")},
	} {
		emit, events := historyStreamingEmitter(t)
		beginTextMessage(t, emit, 0, 0, "msg_prior")
		textDelta(t, emit, 0, 1, "msg_prior", "Published prefix")
		published := len(*events)
		err := emit(streamFrame("response.completed", 2, map[string]any{"response": map[string]any{"status": "completed", "output": output}}))
		if err == nil || len(*events) != published {
			t.Fatal("terminal changed an already published text identity or value")
		}
	}
	for _, changed := range []map[string]any{terminalMessage("msg_changed", `{"answer":2}`), terminalMessage("msg_json", `{"answer":3}`)} {
		emit, events := historyStreamingEmitter(t)
		beginTextMessage(t, emit, 0, 0, "msg_prior")
		textDelta(t, emit, 0, 1, "msg_prior", "Published prefix")
		published := len(*events)
		beginTextMessage(t, emit, 1, 2, "msg_json")
		textDelta(t, emit, 1, 3, "msg_json", `{"answer":2}`)
		err := emit(streamFrame("response.completed", 4, map[string]any{"response": map[string]any{"status": "completed", "output": []any{terminalMessage("msg_prior", "Published prefix"), changed}}}))
		if err == nil || len(*events) != published {
			t.Fatal("contradictory held suffix escaped before terminal validation")
		}
	}
}

func TestNoCallableHistoryRejectsMalformedDeltaBeforePublication(t *testing.T) {
	emit, events := historyStreamingEmitter(t)
	beginTextMessage(t, emit, 0, 0, "msg_prior")
	textDelta(t, emit, 0, 1, "msg_prior", "Published prefix")
	published := len(*events)
	err := emit(streamFrame("response.output_text.delta", 2, map[string]any{"output_index": 0, "content_index": "0", "item_id": "msg_prior", "delta": "must not emit"}))
	if err == nil || len(*events) != published {
		t.Fatal("malformed text identity was emitted")
	}
}

func TestNoCallableHistoryKeepsJSONAndFencesBuffered(t *testing.T) {
	for _, text := range []string{`{"answer":2}`, `[1,2]`, "```json\n{}\n```"} {
		emit, events := historyStreamingEmitter(t)
		beginTextMessage(t, emit, 0, 0, "msg_json")
		textDelta(t, emit, 0, 1, "msg_json", text)
		if len(*events) != 0 {
			t.Fatal("JSON-like text was treated as a verified plain prefix")
		}
		if err := emit(streamFrame("response.completed", 2, map[string]any{"response": map[string]any{"status": "completed", "output": []any{terminalMessage("msg_json", text)}}})); err != nil {
			t.Fatal(err)
		}
		var emitted strings.Builder
		for _, event := range *events {
			if event["type"] == "response.output_text.delta" {
				emitted.WriteString(event["delta"].(string))
			}
		}
		if emitted.String() != text {
			t.Fatal("buffered JSON-like answer did not survive terminal validation")
		}
	}
}
