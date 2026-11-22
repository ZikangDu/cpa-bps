package adapter

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

type ttftScenario struct {
	name, transport, choice, phase string
	history, tools, schema, relay  bool
	tool, lateError, invalidRelay  bool
	terminalTail                   bool
	wantEarly                      bool
}

func ttftScenarios() []ttftScenario {
	return []ttftScenario{
		{name: "no_tools", transport: "auto", phase: "final_answer", wantEarly: true},
		{name: "history_tool_choice_none", transport: "auto", choice: "none", phase: "final_answer", history: true, tools: true, wantEarly: true},
		{name: "native_auto_optional_text", transport: "auto", choice: "auto", phase: "final_answer", tools: true, wantEarly: true},
		{name: "native_optional_unphased_text", transport: "auto", choice: "auto", tools: true},
		{name: "native_required_tool", transport: "auto", choice: "required", phase: "commentary", tools: true, tool: true},
		{name: "json_schema", transport: "auto", phase: "final_answer", schema: true},
		{name: "explicit_text_relay", transport: "text", choice: "required", phase: "final_answer", tools: true, tool: true, relay: true},
		{name: "optional_text_then_error", transport: "auto", choice: "auto", phase: "final_answer", tools: true, lateError: true, wantEarly: true},
		{name: "optional_text_then_invalid_relay", transport: "auto", choice: "auto", phase: "final_answer", tools: true, invalidRelay: true, wantEarly: true},
		{name: "history_none_terminal_tail", transport: "auto", choice: "none", phase: "final_answer", history: true, tools: true, terminalTail: true, wantEarly: true},
		{name: "native_optional_terminal_tail", transport: "auto", choice: "auto", phase: "final_answer", tools: true, terminalTail: true, wantEarly: true},
	}
}

func ttftFrame(value any) []byte {
	raw, _ := json.Marshal(value)
	return append(append([]byte("data: "), raw...), '\n', '\n')
}

// The read barriers prove that the preceding complete SSE frames were parsed.
// Terminal delivery is released only after the test inspected client events.
type ttftBody struct {
	ctx                       context.Context
	created, text, final      *bytes.Reader
	createdRead, textRead     chan struct{}
	textRelease, finalRelease <-chan struct{}
	createdOnce, textOnce     sync.Once
	closeOnce                 sync.Once
	closed                    chan struct{}
}

func (b *ttftBody) Read(p []byte) (int, error) {
	if b.created.Len() > 0 {
		return b.created.Read(p)
	}
	b.createdOnce.Do(func() { close(b.createdRead) })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	case <-b.textRelease:
	}
	if b.text.Len() > 0 {
		return b.text.Read(p)
	}
	b.textOnce.Do(func() { close(b.textRead) })
	select {
	case <-b.ctx.Done():
		return 0, b.ctx.Err()
	case <-b.closed:
		return 0, io.ErrClosedPipe
	case <-b.finalRelease:
	}
	return b.final.Read(p)
}
func (b *ttftBody) Close() error { b.closeOnce.Do(func() { close(b.closed) }); return nil }

type ttftEvidence struct {
	Case              string  `json:"case"`
	EarlyText         bool    `json:"real_text_before_terminal"`
	EarlyTool         bool    `json:"executable_tool_before_terminal"`
	FirstTextMS       float64 `json:"first_real_text_ms"`
	TextReleaseMS     float64 `json:"upstream_text_release_ms"`
	TerminalReleaseMS float64 `json:"terminal_release_ms"`
	HTTPReturnMS      float64 `json:"executor_stream_return_ms"`
	Posts             int32   `json:"upstream_generations"`
	Completed         int     `json:"completed_events"`
	Text              string  `json:"delivered_text_deltas"`
	Failed            bool    `json:"failed"`
}

func TestStreamingTTFTSemanticMatrix(t *testing.T) {
	baseline := os.Getenv("CPA_BPS_TTFT_BASELINE") == "1"
	var report []ttftEvidence
	for _, scenario := range ttftScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			report = append(report, runTTFTScenario(t, scenario, baseline))
		})
	}
	if path := os.Getenv("CPA_BPS_TTFT_EVIDENCE"); path != "" {
		raw, _ := json.MarshalIndent(map[string]any{"baseline": baseline, "synthetic_only": true, "cases": report}, "", "  ")
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func runTTFTScenario(t *testing.T, scenario ttftScenario, baseline bool) ttftEvidence {
	t.Helper()
	r, _, auth := quotaFixture(t)
	r.cfg.BPSToolTransport = scenario.transport
	r.cfg.NativeToolRepairAttempts = 2
	start := time.Now()
	var released atomic.Bool
	var posts atomic.Int32
	var mu sync.Mutex
	result := ttftEvidence{Case: scenario.name, FirstTextMS: -1, HTTPReturnMS: -1}
	streamClosed := make(chan struct{})
	closeStream := sync.OnceFunc(func() { close(streamClosed) })
	r.host = func(method string, raw, _ any) error {
		fields, _ := raw.(map[string]any)
		mu.Lock()
		defer mu.Unlock()
		switch method {
		case "host.stream.emit":
			frame, _ := fields["payload"].([]byte)
			if bytes.Contains(frame, []byte("PROVISIONAL_NATIVE_ARGUMENTS")) || bytes.Contains(frame, []byte(`"name":"run_officejs"`)) {
				t.Error("raw native tool transport escaped client validation")
			}
			for _, line := range bytes.Split(frame, []byte("\n")) {
				value, ok := bytes.CutPrefix(line, []byte("data: "))
				if !ok {
					continue
				}
				var event struct {
					Type, Delta string
					Item        struct{ Type string }
				}
				if json.Unmarshal(value, &event) != nil {
					continue
				}
				if event.Type == "response.output_text.delta" && event.Delta != "" {
					if result.FirstTextMS < 0 {
						result.FirstTextMS = float64(time.Since(start).Microseconds()) / 1000
					}
					result.Text += event.Delta
					result.EarlyText = result.EarlyText || !released.Load()
				}
				toolEvent := strings.HasPrefix(event.Type, "response.function_call_arguments.") || strings.HasPrefix(event.Type, "response.custom_tool_call_input.") || event.Item.Type == "function_call" || event.Item.Type == "custom_tool_call"
				if toolEvent && !released.Load() {
					result.EarlyTool = true
				}
				if event.Type == "response.completed" {
					result.Completed++
				}
				if event.Type == "error" || event.Type == "response.error" || event.Type == "response.failed" {
					result.Failed = true
				}
			}
		case "host.stream.close":
			if fields["error_detail"] != nil || fields["error"] != nil {
				result.Failed = true
			}
			closeStream()
		}
		return nil
	}
	createdRead, textRead := make(chan struct{}), make(chan struct{})
	textRelease, finalRelease := make(chan struct{}), make(chan struct{})
	var releaseText, releaseFinal sync.Once
	t.Cleanup(func() { releaseText.Do(func() { close(textRelease) }); releaseFinal.Do(func() { close(finalRelease) }) })
	visible := "TTFT_VISIBLE"
	if scenario.schema {
		visible = `{"answer":"TTFT_VISIBLE"}`
	}
	if scenario.relay {
		visible = `{"client_tool":"fixture_lookup","arguments":{"q":"ok"}}`
	}
	terminalText := visible
	if scenario.terminalTail {
		terminalText += " TAIL"
	}
	message := map[string]any{"id": "msg_ttft", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": terminalText, "annotations": []any{}}}}
	if scenario.phase != "" {
		message["phase"] = scenario.phase
	}
	added := map[string]any{"id": "msg_ttft", "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}}
	if scenario.phase != "" {
		added["phase"] = scenario.phase
	}
	created := append([]byte(": heartbeat\n\n"), ttftFrame(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_ttft", "status": "in_progress", "output": []any{}}})...)
	prefix := ttftFrame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added})
	prefix = append(prefix, ttftFrame(map[string]any{"type": "response.content_part.added", "output_index": 0, "content_index": 0, "item_id": "msg_ttft", "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})...)
	prefix = append(prefix, ttftFrame(map[string]any{"type": "response.output_text.delta", "output_index": 0, "content_index": 0, "item_id": "msg_ttft", "delta": visible})...)
	output := []any{message}
	if (scenario.tool && !scenario.relay) || scenario.invalidRelay {
		var terminal map[string]any
		_ = json.Unmarshal([]byte(nativeFixtureResult(!scenario.invalidRelay)), &terminal)
		call := terminal["output"].([]any)[0]
		output = append(output, call)
		prefix = append(prefix, ttftFrame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": call})...)
		prefix = append(prefix, ttftFrame(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 1, "item_id": "fc_fixture", "delta": "PROVISIONAL_NATIVE_ARGUMENTS"})...)
	}
	terminal := ttftFrame(map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_ttft", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": 8, "output_tokens": 4, "total_tokens": 12}}})
	if scenario.lateError {
		terminal = ttftFrame(map[string]any{"type": "error", "code": "server_error", "message": "Synthetic late failure"})
	}
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			if strings.HasSuffix(req.URL.Path, "/usage") {
				return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
			}
			return qualityResponseFixture(200, `{"available_count":0}`, false), nil
		}
		attempt := posts.Add(1)
		first, second := createdRead, textRead
		if attempt > 1 {
			first, second = make(chan struct{}), make(chan struct{})
		}
		body := &ttftBody{ctx: req.Context(), created: bytes.NewReader(created), text: bytes.NewReader(prefix), final: bytes.NewReader(terminal), createdRead: first, textRead: second, textRelease: textRelease, finalRelease: finalRelease, closed: make(chan struct{})}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body, Request: req}, nil
	}))
	payload := map[string]any{"model": "fixture", "input": "Answer the controlled fixture.", "stream": true}
	if scenario.tools {
		payload["tools"] = []any{map[string]any{"type": "function", "name": "fixture_lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []any{"q"}, "additionalProperties": false}}}
		payload["tool_choice"] = scenario.choice
	}
	if scenario.history {
		payload["input"] = []any{map[string]any{"role": "user", "content": "Use earlier data."}, map[string]any{"type": "function_call", "name": "fixture_lookup", "call_id": "call_old", "arguments": `{"q":"old"}`}, map[string]any{"type": "function_call_output", "call_id": "call_old", "output": "old value"}, map[string]any{"role": "user", "content": "Answer without more tools."}}
	}
	if scenario.schema {
		payload["text"] = map[string]any{"format": map[string]any{"type": "json_schema", "name": "ttft", "strict": true, "schema": map[string]any{"type": "object", "properties": map[string]any{"answer": map[string]any{"type": "string"}}, "required": []any{"answer"}, "additionalProperties": false}}}
	}
	body, _ := json.Marshal(payload)
	req, _ := json.Marshal(executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: body, StreamID: "ttft-stream"})
	executed := make(chan error, 1)
	go func() {
		_, err := r.execute(req, true)
		mu.Lock()
		result.HTTPReturnMS = float64(time.Since(start).Microseconds()) / 1000
		mu.Unlock()
		executed <- err
	}()
	select {
	case <-createdRead:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream created frame was not consumed")
	}
	mu.Lock()
	if result.FirstTextMS >= 0 {
		t.Error("heartbeat/created frame was counted as real text")
	}
	result.TextReleaseMS = float64(time.Since(start).Microseconds()) / 1000
	mu.Unlock()
	releaseText.Do(func() { close(textRelease) })
	select {
	case <-textRead:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream text frames were not consumed")
	}
	mu.Lock()
	early, earlyTool := result.EarlyText, result.EarlyTool
	mu.Unlock()
	wantEarly := scenario.wantEarly
	if baseline {
		wantEarly = scenario.name == "no_tools"
	}
	if early != wantEarly {
		t.Errorf("real text before terminal=%v, want %v (baseline=%v)", early, wantEarly, baseline)
	}
	if earlyTool {
		t.Error("unvalidated tool was exposed before the terminal")
	}
	// A fixed synthetic tail makes baseline/final latency differences visible;
	// correctness above uses read barriers and never depends on this duration.
	<-time.After(120 * time.Millisecond)
	mu.Lock()
	result.TerminalReleaseMS = float64(time.Since(start).Microseconds()) / 1000
	mu.Unlock()
	released.Store(true)
	releaseFinal.Do(func() { close(finalRelease) })
	select {
	case err := <-executed:
		if err != nil {
			mu.Lock()
			result.Failed = true
			mu.Unlock()
			closeStream()
		}
	case <-time.After(3 * time.Second):
		t.Fatal("executor did not return after terminal release")
	}
	select {
	case <-streamClosed:
	case <-time.After(3 * time.Second):
		t.Fatal("client stream did not close")
	}
	r.tasks.Wait()
	mu.Lock()
	defer mu.Unlock()
	result.Posts = posts.Load()
	expectFailure := scenario.lateError || scenario.invalidRelay
	if result.Failed != expectFailure {
		t.Errorf("failed=%v want=%v", result.Failed, expectFailure)
	}
	if expectFailure && result.Completed != 0 {
		t.Error("late failure produced a fake completed event")
	}
	if !expectFailure && result.Completed != 1 {
		t.Errorf("completed events=%d want=1", result.Completed)
	}
	if !baseline || !scenario.invalidRelay {
		if result.Posts != 1 {
			t.Errorf("generation replayed after visible text/error: posts=%d", result.Posts)
		}
	}
	if !scenario.relay && (!expectFailure || result.EarlyText) && result.Text != terminalText {
		t.Errorf("text duplicated or changed: %q", result.Text)
	}
	if scenario.relay && result.Text != "" {
		t.Error("text tool transport JSON leaked as user-facing text")
	}
	t.Logf("TTFT %s: text=%.3fms source-text=%.3fms terminal=%.3fms early=%v posts=%d", scenario.name, result.FirstTextMS, result.TextReleaseMS, result.TerminalReleaseMS, result.EarlyText, result.Posts)
	return result
}
