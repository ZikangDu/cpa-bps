package compat

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func object(t *testing.T, raw string) map[string]json.RawMessage {
	t.Helper()
	var body map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestHistoryPreservesOpaqueInput(t *testing.T) {
	body := object(t, `{"input":[{"type":"reasoning","encrypted_content":null,"summary":[]},{"type":"compaction","encrypted_content":"opaque"},{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"opaque-state"},{"type":"text","text":"visible"}]}]}`)
	original := string(body["input"])
	if err := PrepareHistory(body, false); err != nil || string(body["input"]) != original {
		t.Fatalf("opaque input changed: %v, %s", err, body["input"])
	}
	body = object(t, `{"input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":false}]}]}`)
	original = string(body["input"])
	if err := PrepareHistory(body, false); !errors.Is(err, ErrInvalidHistory) || string(body["input"]) != original {
		t.Fatalf("invalid opaque input was changed or accepted: %v", err)
	}
}

func TestHistoryUsesKnownConfigurationOnlyAsFallback(t *testing.T) {
	const input = `[{"type":"configuration_update","reasoning":{"effort":"low"}},{"type":"configuration_update","reasoning":{"effort":"extra-high","mode":"standard"}},{"type":"configuration_update","reasoning":{"effort":"high"},"future_extension":true}]`
	for _, tc := range []struct{ initial, want string }{
		{`{"summary":"auto"}`, `{"effort":"xhigh","summary":"auto"}`},
		{`{"effort":"medium","summary":"auto"}`, `{"effort":"medium","summary":"auto"}`},
	} {
		body := object(t, `{"reasoning":`+tc.initial+`,"input":`+input+`}`)
		if err := PrepareHistory(body, false); err != nil || string(body["reasoning"]) != tc.want {
			t.Fatalf("unexpected reasoning: %v, %s", err, body["reasoning"])
		}
		history := string(body["input"])
		if strings.Count(history, `"type":"configuration_update"`) != 1 || !strings.Contains(history, `"future_extension":true`) {
			t.Fatalf("known and unknown history processed incorrectly: %s", history)
		}
	}
}

func TestHistoryOmitsOnlyEarlierStandaloneReasoning(t *testing.T) {
	body := object(t, `{"input":[{"role":"user","content":"first"},{"type":"reasoning","encrypted_content":"older-state","summary":[{"type":"summary_text","text":"visible-summary"}]},{"type":"compaction","encrypted_content":"capsule"},{"role":"user","content":"current"},{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"agent-state"}]},{"type":"reasoning","encrypted_content":"current-state"}]}`)
	if err := PrepareHistory(body, true); err != nil {
		t.Fatal(err)
	}
	input := string(body["input"])
	if strings.Contains(input, "older-state") || !strings.Contains(input, omittedHistoryNotice) {
		t.Fatalf("older reasoning ciphertext was not omitted or noted: %s", input)
	}
	for _, value := range []string{"visible-summary", "capsule", "agent-state", "current-state"} {
		if !strings.Contains(input, value) {
			t.Fatalf("history lost %q: %s", value, input)
		}
	}
}

func TestDeclaredToolsValidateNamespaceKindAndArguments(t *testing.T) {
	body := object(t, `{"tools":[{"type":"namespace","name":"example","tools":[{"type":"function","name":"find"}]},{"type":"custom","name":"note"},{"type":"web_search"}]}`)
	catalog, err := DeclaredTools(body)
	if err != nil {
		t.Fatal(err)
	}
	for _, call := range []string{
		`{"type":"function_call","namespace":"example","name":"find","arguments":"{\"id\":1}"}`,
		`{"type":"function_call","name":"example.find","arguments":{}}`,
		`{"type":"custom_tool_call","name":"note","input":""}`,
		`{"type":"web_search_call","name":"vendor-hosted"}`,
	} {
		if err := catalog.ValidateResponse(json.RawMessage(`{"output":[` + call + `]}`)); err != nil {
			t.Fatalf("valid call rejected: %s %v", call, err)
		}
	}
	for _, call := range []string{
		`{"type":"function_call","name":"find","arguments":"{}"}`,
		`{"type":"function_call","namespace":"other","name":"find","arguments":"{}"}`,
		`{"type":"function_call","name":"example.find","arguments":"[]"}`,
		`{"type":"function_call","name":"example.find","arguments":"{broken}"}`,
		`{"type":"function_call","name":"note","arguments":"{}"}`,
		`{"type":"custom_tool_call","name":"note","input":false}`,
	} {
		if err := catalog.ValidateResponse(json.RawMessage(`{"output":[` + call + `]}`)); !errors.Is(err, ErrInvalidToolResponse) {
			t.Fatalf("invalid call accepted: %s %v", call, err)
		}
	}
	body["tool_choice"] = json.RawMessage(`"none"`)
	catalog, err = DeclaredTools(body)
	if err != nil || !errors.Is(catalog.ValidateResponse(json.RawMessage(`{"output":[{"type":"custom_tool_call","name":"note","input":"x"}]}`)), ErrInvalidToolResponse) {
		t.Fatalf("disabled tools accepted: %v", err)
	}
}

func TestDeclaredToolsRejectAmbiguousQualifiedName(t *testing.T) {
	body := object(t, `{"tools":[{"type":"function","name":"group.call"},{"type":"namespace","name":"group","tools":[{"type":"function","name":"call"}]}]}`)
	if _, err := DeclaredTools(body); !errors.Is(err, ErrInvalidToolDeclaration) {
		t.Fatalf("ambiguous declaration accepted: %v", err)
	}
}

func TestGuardStreamEmitsOnlyVerifiedToolsInSequence(t *testing.T) {
	catalog, err := DeclaredTools(object(t, `{"tools":[{"type":"function","name":"safe"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var frames []string
	guard := GuardStream(catalog, func(frame []byte) error {
		frames = append(frames, string(frame))
		return nil
	})
	for _, event := range []string{
		`{"type":"response.created","sequence_number":0,"response":{"id":"response_1","output":[{"type":"function_call","name":"unsafe-provisional"},{"type":"message","id":"later-snapshot-slot"}]}}`,
		`{"type":"response.output_item.added","sequence_number":1,"item":{"type":"function_call","name":"unsafe-provisional"}}`,
		`{"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"tool_1","delta":"unsafe-provisional"}`,
	} {
		if err := guard([]byte("data: " + event + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}
	if len(frames) != 1 || strings.Contains(frames[0], "later-snapshot-slot") {
		t.Fatalf("provisional tool frames were forwarded: %s", frames)
	}
	terminal := `{"type":"response.completed","sequence_number":4,"response":{"id":"response_1","output":[{"type":"function_call","id":"tool_1","call_id":"call_1","name":"safe","arguments":"{\"confirmed\":true}"}]}}`
	if err := guard([]byte("data: " + terminal + "\n\n")); err != nil {
		t.Fatal(err)
	}
	for i, frame := range frames[1:] {
		if !strings.Contains(frame, fmt.Sprintf(`"sequence_number":%d`, 4+i)) {
			t.Fatalf("invalid emitted event sequence: %s", frame)
		}
	}
	if len(frames) != 6 || strings.Contains(strings.Join(frames, ""), "unsafe-provisional") ||
		!strings.Contains(frames[2], `\"confirmed\":true`) || !strings.Contains(frames[5], `"response.completed"`) {
		t.Fatalf("unverified tool exposure or missing terminal: %s", frames)
	}
}

func TestGuardStreamInsertsVerifiedToolBeforeLaterOutputSlots(t *testing.T) {
	catalog, err := DeclaredTools(object(t, `{"tools":[{"type":"function","name":"safe"},{"type":"custom","name":"note"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var slots []int
	var frames, previousSequence int
	var visible strings.Builder
	guard := GuardStream(catalog, func(frame []byte) error {
		frames++
		visible.Write(frame)
		_, data, _ := strings.Cut(string(frame), "data: ")
		var event struct {
			Type     string
			Sequence int `json:"sequence_number"`
			Index    int `json:"output_index"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return err
		}
		if event.Sequence <= previousSequence {
			t.Fatalf("nonmonotonic sequence in %s", frame)
		}
		previousSequence = event.Sequence
		if event.Type == "response.output_item.added" {
			if event.Index != len(slots) {
				t.Fatalf("client saw out-of-order addition of output slot %d: %v", event.Index, slots)
			}
			slots = append(slots, event.Index)
		}
		return nil
	})
	for _, event := range []string{
		`{"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"message","id":"m0"}}`,
		`{"type":"response.output_text.delta","sequence_number":2,"output_index":0,"delta":"early text"}`,
	} {
		if err := guard([]byte("data: " + event + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}
	for _, event := range []string{
		`{"type":"response.output_item.added","sequence_number":3,"output_index":1,"item":{"type":"function_call","name":"unsafe-provisional"}}`,
		`{"type":"response.output_item.added","sequence_number":4,"output_index":2,"item":{"type":"message","id":"m2"}}`,
		`{"type":"response.output_item.added","sequence_number":5,"output_index":3,"item":{"type":"custom_tool_call","name":"unsafe-provisional"}}`,
		`{"type":"response.output_text.delta","sequence_number":6,"output_index":2,"delta":"midstream text"}`,
		`{"type":"response.output_item.added","sequence_number":7,"output_index":4,"item":{"type":"message","id":"m4"}}`,
		`{"type":"response.output_text.delta","sequence_number":8,"output_index":4,"delta":"late text"}`,
	} {
		if err := guard([]byte("data: " + event + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}
	if frames != 2 || !strings.Contains(visible.String(), "early text") {
		t.Fatalf("the ordinary prefix was not forwarded incrementally, or suffix escaped: %s", &visible)
	}
	terminal := `{"type":"response.completed","sequence_number":9,"response":{"output":[{"type":"message","id":"m0"},{"type":"function_call","name":"safe","id":"f1","arguments":"{}"},{"type":"message","id":"m2"},{"type":"custom_tool_call","name":"note","input":"verified"},{"type":"message","id":"m4"}]}}`
	if err := guard([]byte("data: " + terminal + "\n\n")); err != nil {
		t.Fatal(err)
	}
	if len(slots) != 5 || strings.Contains(visible.String(), "unsafe-provisional") ||
		!strings.Contains(visible.String(), "midstream text") || !strings.Contains(visible.String(), "late text") {
		t.Fatalf("unexpected completed stream: %s", &visible)
	}
}

func TestGuardStreamNeverExposesFailedOrUndeclaredCalls(t *testing.T) {
	for _, tt := range []struct {
		kind string
		want error
		emit bool
	}{
		{"response.incomplete", nil, true},
		{"response.completed", ErrInvalidToolResponse, false},
	} {
		var emitted string
		guard := GuardStream(ToolCatalog{}, func(frame []byte) error {
			emitted += string(frame)
			return nil
		})
		for _, pending := range []string{
			`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"undeclared"}}`,
			`{"type":"response.output_text.delta","output_index":1,"delta":"withheld"}`,
		} {
			if err := guard([]byte("data: " + pending + "\n\n")); err != nil {
				t.Fatal(err)
			}
		}
		event := fmt.Sprintf(`{"type":%q,"response":{"id":"r","output":[{"type":"function_call","name":"undeclared","arguments":"{}"},{"type":"message","id":"visible"}]}}`, tt.kind)
		err := guard([]byte("data: " + event + "\n\n"))
		if err != tt.want || (emitted != "") != tt.emit || strings.Contains(emitted, "undeclared") ||
			strings.Contains(emitted, "withheld") || (tt.emit && !strings.Contains(emitted, "visible")) {
			t.Fatalf("invalid %s frame outcome: %v %s", tt.kind, err, emitted)
		}
	}
}

func TestGuardStreamRejectsToolSlotAnnouncedTooLate(t *testing.T) {
	catalog, err := DeclaredTools(object(t, `{"tools":[{"type":"function","name":"safe"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var dispatched int
	guard := GuardStream(catalog, func([]byte) error {
		dispatched++
		return nil
	})
	later := `{"type":"response.output_item.added","output_index":1,"item":{"type":"message"}}`
	if err := guard([]byte("data: " + later + "\n\n")); err != nil {
		t.Fatal(err)
	}
	terminal := `{"type":"response.completed","response":{"output":[{"type":"function_call","name":"safe","arguments":"{}"},{"type":"message"}]}}`
	if err := guard([]byte("data: " + terminal + "\n\n")); !errors.Is(err, ErrInvalidToolResponse) || dispatched != 1 {
		t.Fatalf("stream exposed a late-announced earlier slot: emissions=%d, error=%v", dispatched, err)
	}
}

func TestGuardStreamBoundsPendingSuffix(t *testing.T) {
	for _, countLimited := range []bool{true, false} {
		guard := GuardStream(ToolCatalog{}, func([]byte) error {
			t.Error("withheld event emitted before terminal")
			return nil
		})
		if err := guard([]byte("data: {\"type\":\"response.function_call_arguments.delta\"}\n\n")); err != nil {
			t.Fatal(err)
		}
		frame := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"x\"}\n\n")
		if countLimited {
			for i := 0; i < maxToolSuffixFrames; i++ {
				if err := guard(frame); err != nil {
					t.Fatalf("frame limit rejected too early: %v", err)
				}
			}
		} else {
			frame = []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + strings.Repeat("x", maxToolSuffixBytes) + "\"}\n\n")
		}
		if err := guard(frame); !errors.Is(err, ErrInvalidToolResponse) {
			t.Fatalf("oversized suffix accepted: %v", err)
		}
	}
}
