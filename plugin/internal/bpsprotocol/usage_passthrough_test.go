package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// These are the upstream counters from a warm-cache acceptance response. Cache
// reads and writes are subsets/details of this observation, not additions to input.
const passthroughUsageFixture = `{"input_tokens":19752,"input_tokens_details":{"cached_tokens":10162,"cache_write_tokens":19,"cache_write_tokens_details":{"ttl_30m_tokens":19,"ttl_12h_tokens":0}},"output_tokens":6,"output_tokens_details":{"reasoning_tokens":0},"total_tokens":19758}`

func TestUsagePassthroughAcrossResponsePaths(t *testing.T) {
	cases := []struct {
		name       string
		request    string
		answer     string
		textTools  bool
		outputType string
	}{
		{
			name: "ordinary", request: `{"model":"fixture","input":"Answer briefly"}`,
			answer: "fixture answer", outputType: "message",
		},
		{
			name:      "text_tool",
			request:   `{"model":"fixture","input":"Look up abc","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}}],"tool_choice":{"type":"function","name":"lookup"}}`,
			answer:    `{"client_tool":"lookup","arguments":{"value":"abc"}}`,
			textTools: true, outputType: "function_call",
		},
		{
			name:    "structured_schema",
			request: `{"model":"fixture","input":"Return a count","text":{"format":{"type":"json_schema","name":"answer","strict":true,"schema":{"type":"object","properties":{"count":{"type":"integer","minimum":3}},"required":["count"],"additionalProperties":false}}}}`,
			answer:  `{"count":4}`, outputType: "message",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepare := Prepare
			if tc.textTools {
				prepare = PrepareWithTextTools
			}
			_, bridge, err := prepare([]byte(tc.request), "usage-passthrough/"+tc.name)
			if err != nil {
				t.Fatal(err)
			}
			response := jsonBytes(map[string]any{
				"id": "resp_usage_fixture", "object": "response", "status": "completed",
				"usage": json.RawMessage(passthroughUsageFixture),
				"output": []any{map[string]any{
					"id": "msg_usage_fixture", "type": "message", "role": "assistant", "status": "completed",
					"content": []any{map[string]any{"type": "output_text", "text": tc.answer}},
				}},
			})

			t.Run("json", func(t *testing.T) {
				// Repeated observations on the same bridge must remain per-response.
				for range 2 {
					got, err := bridge.Transform(response)
					if err != nil {
						t.Fatal(err)
					}
					assertPassthroughUsage(t, got, tc.outputType)
				}
			})

			t.Run("stream", func(t *testing.T) {
				var frames [][]byte
				emit := bridge.WrapEmitter(func(frame []byte) error {
					frames = append(frames, bytes.Clone(frame))
					return nil
				})
				for _, event := range []map[string]any{
					{"type": "response.created", "response": map[string]any{"id": "resp_usage_fixture", "status": "in_progress"}},
					{"type": "response.output_text.delta", "delta": tc.answer},
					{"type": "response.completed", "response": json.RawMessage(response)},
				} {
					frame := append(append([]byte("data: "), jsonBytes(event)...), '\n', '\n')
					if err := emit(frame); err != nil {
						t.Fatal(err)
					}
				}
				terminalCount := 0
				for _, frame := range frames {
					for _, line := range bytes.Split(frame, []byte("\n")) {
						payload, ok := bytes.CutPrefix(line, []byte("data:"))
						if !ok {
							continue
						}
						var event struct {
							Type     string          `json:"type"`
							Response json.RawMessage `json:"response"`
						}
						if err := json.Unmarshal(bytes.TrimSpace(payload), &event); err != nil {
							t.Fatalf("invalid emitted SSE data: %v", err)
						}
						switch event.Type {
						case "response.completed", "response.incomplete", "response.failed":
							terminalCount++
							if event.Type != "response.completed" {
								t.Fatalf("unexpected terminal %q", event.Type)
							}
							assertPassthroughUsage(t, event.Response, tc.outputType)
						}
					}
				}
				if terminalCount != 1 {
					t.Fatalf("terminal count = %d, want exactly 1", terminalCount)
				}
			})
		})
	}
}

func assertPassthroughUsage(t *testing.T, raw []byte, outputType string) {
	t.Helper()
	var response struct {
		Usage  json.RawMessage `json:"usage"`
		Output []struct {
			Type string `json:"type"`
		} `json:"output"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	var got, want any
	for _, item := range []struct {
		raw []byte
		out *any
	}{{response.Usage, &got}, {[]byte(passthroughUsageFixture), &want}} {
		decoder := json.NewDecoder(bytes.NewReader(item.raw))
		decoder.UseNumber()
		if err := decoder.Decode(item.out); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("usage changed during transformation: got %s, want %s", response.Usage, passthroughUsageFixture)
	}
	if len(response.Output) != 1 || response.Output[0].Type != outputType {
		t.Fatalf("expected transformed %s output, got %+v", outputType, response.Output)
	}
}
