package adapter

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/compat"
)

func automaticFixtureFrame(value map[string]any) []byte {
	raw, _ := json.Marshal(value)
	return append(append([]byte("data: "), raw...), '\n', '\n')
}
func automaticFixtureMessage(phase, text string) []map[string]any {
	return []map[string]any{
		{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "msg", "type": "message", "role": "assistant", "phase": phase, "content": []any{}}},
		{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": "msg", "part": map[string]any{"type": "output_text", "text": ""}},
		{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg", "delta": text},
	}
}
func automaticFixtureTerminal(text string, more ...any) []byte {
	items := []any{map[string]any{"id": "msg", "type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
	items = append(items, more...)
	raw, _ := json.Marshal(map[string]any{"id": "response", "status": "completed", "output": items})
	return raw
}

func TestAutomaticTextPrefixKeepsAmbiguousAndOversizedOutputBuffered(t *testing.T) {
	for _, tc := range []struct {
		name, phase, text    string
		toolFirst, oversized bool
	}{
		{name: "missing phase", text: "answer"},
		{name: "commentary", phase: "commentary", text: "answer"},
		{name: "native textual relay", phase: "final_answer", text: `{"client_tool":"lookup","arguments":{}}`},
		{name: "array", phase: "final_answer", text: `["answer"]`},
		{name: "fence", phase: "final_answer", text: "```json"},
		{name: "earlier tool", phase: "final_answer", text: "answer", toolFirst: true},
		{name: "bounded prefix", phase: "final_answer", text: "answer", oversized: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			emitted := 0
			state := newAutoTextStream(compat.ToolCatalog{}, func([]byte) error { emitted++; return nil })
			state.beginAttempt(true)
			close(state.resume)
			if tc.toolFirst {
				if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "function_call", "name": "run_officejs"}})); err != nil {
					t.Fatal(err)
				}
			}
			if tc.oversized {
				for range maxAutomaticPrefixFrames + 1 {
					if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.created", "response": map[string]any{"output": []any{}}})); err != nil {
						t.Fatal(err)
					}
				}
			}
			for _, event := range automaticFixtureMessage(tc.phase, tc.text) {
				if err := state.accept(automaticFixtureFrame(event)); err != nil {
					t.Fatal(err)
				}
			}
			if state.activated || emitted != 0 {
				t.Fatal("ambiguous prefix escaped complete validation")
			}
			if tc.oversized && (len(state.frames) != 0 || state.size != 0) {
				t.Fatal("disabled optimization retained an oversized prefix")
			}
		})
	}
}

func TestAutomaticTextPrefixNeverDeliversProvisionalToolsOrRepeatsText(t *testing.T) {
	var request map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"tools":[{"type":"function","name":"lookup"}]}`), &request)
	tools, err := compat.DeclaredTools(request)
	if err != nil {
		t.Fatal(err)
	}
	var frames []string
	state := newAutoTextStream(tools, func(frame []byte) error { frames = append(frames, string(frame)); return nil })
	state.beginAttempt(true)
	close(state.resume)
	for _, event := range automaticFixtureMessage("final_answer", "answer") {
		if err := state.accept(automaticFixtureFrame(event)); err != nil {
			t.Fatal(err)
		}
	}
	if !state.activated {
		t.Fatal("final answer did not commit")
	}
	provisional := map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "native", "id": "native", "arguments": "PRIVATE_PROVISIONAL"}
	if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": provisional})); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.Join(frames, ""), "PRIVATE_PROVISIONAL") {
		t.Fatal("provisional tool leaked")
	}
	finalCall := map[string]any{"type": "function_call", "name": "lookup", "call_id": "client", "id": "fc_client", "arguments": `{"ok":true}`, "status": "completed"}
	if err := state.prepareTerminal(automaticFixtureTerminal("answer", finalCall)); err != nil {
		t.Fatal(err)
	}
	if err := state.complete(); err != nil {
		t.Fatal(err)
	}
	emitted := strings.Join(frames, "")
	if strings.Count(emitted, `"type":"response.output_text.delta"`) != 1 || strings.Count(emitted, `"type":"response.completed"`) != 1 {
		t.Fatal("committed text or terminal was replayed")
	}
	if strings.Contains(emitted, "PRIVATE_PROVISIONAL") || !strings.Contains(emitted, `"name":"lookup"`) {
		t.Fatal("verified tool stream was incorrect")
	}
}

func TestAutomaticTextPrefixRejectsContradictoryTerminalAndUnknownActions(t *testing.T) {
	state := newAutoTextStream(compat.ToolCatalog{}, func([]byte) error { return nil })
	state.beginAttempt(true)
	close(state.resume)
	for _, event := range automaticFixtureMessage("final_answer", "original") {
		if err := state.accept(automaticFixtureFrame(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.prepareTerminal(automaticFixtureTerminal("replacement")); !errors.Is(err, compat.ErrInvalidToolResponse) {
		t.Fatal("contradictory final text was accepted")
	}
	if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "web_search_call"}})); !errors.Is(err, compat.ErrInvalidToolResponse) {
		t.Fatal("unvalidated hosted action escaped")
	}
}

func TestAutomaticTextPrefixTracksIdentityAndGuardHeldSuffix(t *testing.T) {
	for _, scenario := range []string{"moved slot", "held text changed", "held id changed", "missing item id", "invalid content index", "duplicate message slot", "done omits text"} {
		t.Run(scenario, func(t *testing.T) {
			var frames []string
			state := newAutoTextStream(compat.ToolCatalog{}, func(frame []byte) error { frames = append(frames, string(frame)); return nil })
			state.beginAttempt(true)
			close(state.resume)
			for _, event := range automaticFixtureMessage("final_answer", "answer") {
				if err := state.accept(automaticFixtureFrame(event)); err != nil {
					t.Fatal(err)
				}
			}
			before := len(frames)
			if scenario == "missing item id" || scenario == "invalid content index" {
				event := map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg", "delta": "bad"}
				if scenario == "missing item id" {
					delete(event, "item_id")
				} else {
					event["content_index"] = "0"
				}
				if err := state.accept(automaticFixtureFrame(event)); err == nil {
					t.Fatal("malformed text identity was emitted")
				}
				if len(frames) != before {
					t.Fatal("invalid text event reached the client")
				}
				return
			}
			if scenario == "done omits text" {
				if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": map[string]any{"id": "msg", "type": "message", "role": "assistant", "status": "completed", "content": []any{}}})); err == nil {
					t.Fatal("done snapshot omitted already delivered text")
				}
				return
			}
			if scenario == "duplicate message slot" {
				if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"id": "other", "type": "message", "role": "assistant", "content": []any{}}})); err == nil {
					t.Fatal("conflicting item identity reused a live slot")
				}
				return
			}
			var terminal map[string]any
			_ = json.Unmarshal(automaticFixtureTerminal("answer"), &terminal)
			output := terminal["output"].([]any)
			if scenario == "moved slot" {
				terminal["output"] = append([]any{map[string]any{"type": "message", "id": "inserted", "content": []any{}}}, output...)
			} else {
				if err := state.accept(automaticFixtureFrame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "function_call", "id": "native"}})); err != nil {
					t.Fatal(err)
				}
				events := automaticFixtureMessage("final_answer", "tail")
				for _, event := range events {
					event["output_index"] = 2
					if item, ok := event["item"].(map[string]any); ok {
						item["id"] = "tail"
					}
					if _, exists := event["item_id"]; exists {
						event["item_id"] = "tail"
					}
					if err := state.accept(automaticFixtureFrame(event)); err != nil {
						t.Fatal(err)
					}
				}
				if len(frames) != before {
					t.Fatal("Guard-held suffix was emitted before validation")
				}
				tail := map[string]any{"id": "tail", "type": "message", "content": []any{map[string]any{"type": "output_text", "text": "tail"}}}
				if scenario == "held id changed" {
					tail["id"] = "replacement"
				} else {
					tail["content"] = []any{map[string]any{"type": "output_text", "text": "replacement"}}
				}
				terminal["output"] = append(output, map[string]any{"type": "function_call", "name": "lookup", "id": "fc", "call_id": "c", "arguments": "{}"}, tail)
			}
			raw, _ := json.Marshal(terminal)
			if err := state.prepareTerminal(raw); err == nil {
				t.Fatal("terminal contradicted streamed or held message identity/text")
			}
			if len(frames) != before {
				t.Fatal("invalid suffix was flushed")
			}
		})
	}
}

func TestAutomaticTextPrefixRepairsOnlyUnfinishedTailAndBoundsAccumulation(t *testing.T) {
	var frames []string
	state := newAutoTextStream(compat.ToolCatalog{}, func(frame []byte) error { frames = append(frames, string(frame)); return nil })
	state.beginAttempt(true)
	close(state.resume)
	for _, event := range automaticFixtureMessage("final_answer", "Hello") {
		if err := state.accept(automaticFixtureFrame(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := state.prepareTerminal(automaticFixtureTerminal("Hello world")); err != nil {
		t.Fatal(err)
	}
	if err := state.complete(); err != nil {
		t.Fatal(err)
	}
	if all := strings.Join(frames, ""); strings.Count(all, `"delta":" world"`) != 1 || strings.Count(all, `"type":"response.completed"`) != 1 {
		t.Fatal("unfinished final tail was omitted or repeated")
	}
	next := newAutoTextStream(compat.ToolCatalog{}, func([]byte) error { return nil })
	next.beginAttempt(true)
	close(next.resume)
	for _, event := range automaticFixtureMessage("final_answer", "Hello") {
		if err := next.accept(automaticFixtureFrame(event)); err != nil {
			t.Fatal(err)
		}
	}
	if err := next.accept(automaticFixtureFrame(map[string]any{"type": "response.output_text.done", "output_index": 0, "content_index": 0, "item_id": "msg", "text": "Hello"})); err != nil {
		t.Fatal(err)
	}
	if err := next.prepareTerminal(automaticFixtureTerminal("Hello world")); err == nil {
		t.Fatal("final response changed an already completed text part")
	}
	bounded := newAutoTextStream(compat.ToolCatalog{}, func([]byte) error { return nil })
	bounded.beginAttempt(true)
	close(bounded.resume)
	for _, event := range automaticFixtureMessage("final_answer", "a") {
		if err := bounded.accept(automaticFixtureFrame(event)); err != nil {
			t.Fatal(err)
		}
	}
	chunk := strings.Repeat("x", 8192)
	var limitErr error
	for i := 0; i < maxAutomaticTextBytes/len(chunk)+1; i++ {
		limitErr = bounded.accept(automaticFixtureFrame(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg", "delta": chunk}))
		if limitErr != nil {
			break
		}
	}
	if limitErr == nil || bounded.textBytes > maxAutomaticTextBytes {
		t.Fatal("small incremental deltas escaped the aggregate text bound")
	}
}

func TestAutomaticTextStreamAcceptsEquivalentIntegerRepresentations(t *testing.T) {
	var frames []string
	state := newAutoTextStream(compat.ToolCatalog{}, func(frame []byte) error { frames = append(frames, string(frame)); return nil })
	state.beginAttempt(true)
	close(state.resume)
	events := automaticFixtureMessage("final_answer", "Hello")
	for index, event := range events {
		event["output_index"] = json.Number("0.0")
		event["sequence_number"] = json.Number([]string{"1e0", "2.0", "30e-1"}[index])
		if _, exists := event["content_index"]; exists {
			event["content_index"] = json.Number("0e0")
		}
		if err := state.accept(automaticFixtureFrame(event)); err != nil {
			t.Fatal(err)
		}
	}
	if !state.activated {
		t.Fatal("equivalent integer notation prevented first text")
	}
	if err := state.prepareTerminal(automaticFixtureTerminal("Hello world")); err != nil {
		t.Fatal(err)
	}
	if err := state.complete(); err != nil {
		t.Fatal(err)
	}
	previous := int64(-1)
	for _, frame := range frames {
		event, ok := automaticStreamEvent([]byte(frame))
		if !ok {
			t.Fatal("invalid frame")
		}
		if raw, exists := event["sequence_number"]; exists {
			current, ok := automaticInteger(raw)
			if !ok || current <= previous {
				t.Fatalf("sequence regressed: %d after %d", current, previous)
			}
			previous = current
		}
	}
	for _, raw := range []string{"0.1", "1e-1000", "-1.0", "4096.0", "40960e-1", "9e1000"} {
		if _, ok := automaticEventIndex(json.Number(raw)); ok {
			t.Fatalf("noninteger/out-of-range index accepted: %s", raw)
		}
	}
	for _, raw := range []string{"9223372036854775808.0", "1e1000", "1e-1000"} {
		if _, ok := automaticInteger(json.Number(raw)); ok {
			t.Fatalf("overflow/fraction became an integer: %s", raw)
		}
	}
}
