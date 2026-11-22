package adapter

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func autoResponse(text string, calls []any, usage int) string {
	output := calls
	if output == nil {
		output = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text}}}}
	}
	b, _ := json.Marshal(map[string]any{"id": "resp_auto_fixture", "status": "completed", "model": "fixture", "output": output, "usage": map[string]any{"input_tokens": usage, "output_tokens": 2, "total_tokens": usage + 2, "input_tokens_details": map[string]any{"cached_tokens": usage / 2}}})
	return string(b)
}
func autoNative(tool, args string) []any {
	envelope := fmt.Sprintf(`{"name":%q,"arguments":%s}`, tool, args)
	outer, _ := json.Marshal(map[string]any{"code": envelope, "summary": "Relay client tool", "extended_summary": "", "references": []any{}})
	return []any{map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "call_auto_native", "id": "fc_auto_native", "arguments": string(outer), "status": "completed"}}
}
func autoSource(complex bool) []byte {
	tools := []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}, "additionalProperties": false}}}
	if complex {
		tools = append(tools, map[string]any{"type": "custom", "name": "patch", "format": map[string]any{"type": "text"}})
	}
	raw, _ := json.Marshal(map[string]any{"model": "fixture", "input": "fixture", "prompt_cache_key": "auto-session", "tools": tools, "tool_choice": "required"})
	return raw
}
func TestAutoTransportFallbackAndStableBinding(t *testing.T) {
	for _, nativeFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(nativeFirst), func(t *testing.T) {
			r, _, auth := quotaFixture(t)
			r.cfg.BPSToolTransport = "auto"
			var modes []string
			r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if strings.HasSuffix(req.URL.Path, "/usage") {
						return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
					}
					return qualityResponseFixture(200, `{"available_count":0}`, false), nil
				}
				mode := "text"
				if req.Header.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") == "excel" {
					mode = "native"
				}
				modes = append(modes, mode)
				if len(modes) == 1 {
					if nativeFirst {
						return qualityResponseFixture(200, nativeFixtureResult(false), false), nil
					}
					return qualityResponseFixture(200, autoResponse("I will call a tool.", nil, 7), false), nil
				}
				if mode == "native" {
					return qualityResponseFixture(200, autoResponse("", autoNative("lookup", `{"q":"ok"}`), 9), false), nil
				}
				return qualityResponseFixture(200, autoResponse(`{"client_tool":"lookup","arguments":{"q":"ok"}}`, nil, 9), false), nil
			}))
			source := autoSource(nativeFirst)
			req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: source}
			if !nativeFirst {
				// A successful required-call text fallback can be reused. Seed
				// that established mode to retain text-to-native retry coverage;
				// an unseen required request now starts with native transport.
				_, key, _, err := r.selectAutoTransport(req, r.cfg, source)
				if err != nil {
					t.Fatal(err)
				}
				r.transports.learn(key, "text")
			}
			raw, _ := json.Marshal(req)
			reply, err := r.execute(raw, false)
			if err != nil {
				t.Fatal(err)
			}
			initial, alternate := "text", "native"
			if nativeFirst {
				initial, alternate = alternate, initial
			}
			if len(modes) != 2 || modes[0] != initial || modes[1] != alternate {
				t.Fatal("wrong automatic traversal", modes)
			}
			var response struct {
				Usage    map[string]any
				Metadata map[string]any
			}
			_ = json.Unmarshal(reply.(map[string]any)["Payload"].([]byte), &response)
			if !nativeFirst && response.Usage["input_tokens"] != float64(16) {
				t.Fatal("fallback attempt cost omitted", response.Usage)
			}
			reply, err = r.execute(raw, false)
			if err != nil || reply == nil || len(modes) != 3 || modes[2] != alternate {
				t.Fatalf("successful mode was not sticky: modes=%v error=%v", modes, err)
			}
			r.mu.Lock()
			entry := *r.quotaEntry("fixture-id")
			r.mu.Unlock()
			if entry.completed != 2 || (!nativeFirst && entry.input != 25) {
				t.Fatal("attempt count confused with successful requests", entry)
			}
		})
	}
}
func TestAutoNoFallbackOnUncertainOrRejectedExecution(t *testing.T) {
	for _, scenario := range []string{"403", "429", "transport", "unknown-tool", "incomplete", "failed", "refusal"} {
		t.Run(scenario, func(t *testing.T) {
			r, _, auth := quotaFixture(t)
			r.cfg.BPSToolTransport = "auto"
			calls := 0
			var mu sync.Mutex
			emits := 0
			r.host = func(method string, _ any, _ any) error {
				if method == "host.stream.emit" {
					mu.Lock()
					emits++
					mu.Unlock()
				}
				return nil
			}
			r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if strings.HasSuffix(req.URL.Path, "/usage") {
						return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
					}
					return qualityResponseFixture(200, `{"available_count":0}`, false), nil
				}
				calls++
				switch scenario {
				case "403":
					return qualityResponseFixture(403, `{}`, false), nil
				case "429":
					return qualityResponseFixture(429, `{}`, false), nil
				case "transport":
					return nil, fmt.Errorf("fixture ambiguous transport")
				case "unknown-tool":
					return qualityResponseFixture(200, autoResponse("", []any{map[string]any{"type": "function_call", "name": "run_connector_action", "call_id": "unknown", "arguments": "{}"}}, 1), false), nil
				case "incomplete", "failed":
					var body map[string]any
					_ = json.Unmarshal([]byte(autoResponse("", autoNative("lookup", `{"q":"ok"}`), 1)), &body)
					body["status"] = scenario
					encoded, _ := json.Marshal(body)
					return qualityResponseFixture(200, string(encoded), false), nil
				default:
					return qualityResponseFixture(200, `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}]}`, false), nil
				}
			}))
			req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: autoSource(true), StreamID: "stream"}
			raw, _ := json.Marshal(req)
			_, err := r.execute(raw, true)
			if err == nil || calls != 1 {
				t.Fatalf("unsafe retry calls=%d err=%v", calls, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if emits != 0 {
				t.Fatal("failed protocol exposed events")
			}
			if s := r.queue.Snapshot("fixture-id", 20); s.Active != 0 || s.Waiting != 0 {
				t.Fatal("automatic failure leaked slot")
			}
		})
	}
}

func TestAutoSingleFunctionUsesNativeWithoutRetryingHostedCalls(t *testing.T) {
	for _, choice := range []string{"auto", "required"} {
		for _, hosted := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/hosted=%v", choice, hosted), func(t *testing.T) {
				r, _, auth := quotaFixture(t)
				r.cfg.BPSToolTransport = "auto"
				calls := 0
				r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
					if req.Method == http.MethodGet {
						if strings.HasSuffix(req.URL.Path, "/usage") {
							return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
						}
						return qualityResponseFixture(200, `{"available_count":0}`, false), nil
					}
					calls++
					if req.Header.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") != "excel" {
						t.Fatal("single callable function did not use the native upstream profile")
					}
					output := autoNative("lookup", `{"q":"ok"}`)
					if hosted {
						output = []any{map[string]any{"type": "function_call", "name": "list_skills", "call_id": "call_hosted", "id": "fc_hosted", "arguments": `{"limit":3}`, "status": "completed"}}
					}
					return qualityResponseFixture(200, autoResponse("", output, 5), false), nil
				}))
				var source map[string]any
				_ = json.Unmarshal(autoSource(false), &source)
				source["tool_choice"] = choice
				payload, _ := json.Marshal(source)
				req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: payload}
				raw, _ := json.Marshal(req)
				reply, err := r.execute(raw, false)
				if calls != 1 || (err != nil) != hosted {
					t.Fatalf("single function or unsafe hosted-call handling changed: calls=%d err=%v", calls, err)
				}
				if !hosted && !strings.Contains(string(reply.(map[string]any)["Payload"].([]byte)), `"name":"lookup"`) {
					t.Fatal("native result did not become the declared client function")
				}
			})
		}
	}
}
func TestAutoStreamDeliversOnlyFinalValidatedCall(t *testing.T) {
	r, _, auth := quotaFixture(t)
	r.cfg.BPSToolTransport = "auto"
	calls := 0
	var mu sync.Mutex
	frames := []string{}
	done := make(chan struct{})
	r.host = func(method string, v any, _ any) error {
		if method == "host.stream.emit" {
			mu.Lock()
			frames = append(frames, string(v.(map[string]any)["payload"].([]byte)))
			mu.Unlock()
		}
		if method == "host.stream.close" {
			close(done)
		}
		return nil
	}
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			if strings.HasSuffix(req.URL.Path, "/usage") {
				return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
			}
			return qualityResponseFixture(200, `{"available_count":0}`, false), nil
		}
		calls++
		if calls == 1 {
			return qualityResponseFixture(200, nativeFixtureResult(false), false), nil
		}
		return qualityResponseFixture(200, autoResponse(`{"client_tool":"lookup","arguments":{"q":"ok"}}`, nil, 9), false), nil
	}))
	req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: autoSource(true), StreamID: "stream"}
	raw, _ := json.Marshal(req)
	if _, err := r.execute(raw, true); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stream not completed")
	}
	mu.Lock()
	defer mu.Unlock()
	s := strings.Join(frames, "")
	if calls != 2 || strings.Count(s, "event: response.completed\n") != 1 || strings.Contains(s, "Excel.run") || strings.Contains(s, "run_officejs") || !strings.Contains(s, `"name":"lookup"`) {
		t.Fatal("invalid or duplicate delivery")
	}
}

func TestAutoDoesNotRegenerateConflictingHistoryCalls(t *testing.T) {
	source := []byte(`{"input":[{"type":"function_call","id":"fc_past","call_id":"call_past","name":"edit","arguments":"{}"},{"type":"function_call_output","call_id":"call_past","output":"already executed"}]}`)
	for _, response := range []string{`{"output":[{"type":"function_call","id":"fc_new","call_id":"call_past"}]}`, `{"output":[{"type":"function_call","id":"fc_past","call_id":"call_new"}]}`} {
		if !autoHistoryConflict(source, []byte(response)) {
			t.Fatal("executed history replay allowed")
		}
	}
	if autoHistoryConflict(source, []byte(`{"output":[{"type":"function_call","id":"fc_new","call_id":"call_new"}]}`)) {
		t.Fatal("new unexecuted call falsely conflicted")
	}
}
func TestAutoUnknownUsageDoesNotInvalidateVerifiedAnswer(t *testing.T) {
	final := []byte(autoResponse("verified", nil, 2))
	earlier := [][]byte{[]byte(`{"status":"completed","usage":{"input_tokens":"invalid"}}`)}
	raw, err := mergeObservedUsage(final, earlier)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	if _, ok := body["usage"]; ok || !strings.Contains(string(raw), "verified") || body["metadata"].(map[string]any)["cpa_bps_usage_complete"] != "false" {
		t.Fatal("unknown cost was invented or answer dropped")
	}
}

func TestAutoTextGeneratedCallIDCannotReplayHistory(t *testing.T) {
	r, _, auth := quotaFixture(t)
	r.cfg.BPSToolTransport = "auto"
	calls := 0
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.Method == http.MethodGet {
			if strings.HasSuffix(req.URL.Path, "/usage") {
				return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
			}
			return qualityResponseFixture(200, `{"available_count":0}`, false), nil
		}
		calls++
		return qualityResponseFixture(200, autoResponse(`{"client_tool":"lookup","arguments":{"q":"ok"}}`, nil, 9), false), nil
	}))
	req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: autoSource(false)}
	raw, _ := json.Marshal(req)
	reply, err := r.execute(raw, false)
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		Output []map[string]any `json:"output"`
	}
	_ = json.Unmarshal(reply.(map[string]any)["Payload"].([]byte), &first)
	var source map[string]any
	_ = json.Unmarshal(req.Payload, &source)
	source["input"] = []any{map[string]any{"role": "user", "content": "fixture"}, first.Output[0], map[string]any{"type": "function_call_output", "call_id": first.Output[0]["call_id"], "output": "already executed"}}
	req.Payload, _ = json.Marshal(source)
	raw, _ = json.Marshal(req)
	_, err = r.execute(raw, false)
	problem, ok := err.(*rpcError)
	if !ok || problem.Code() != "bps_duplicate_tool_call" || calls != 2 {
		t.Fatalf("generated duplicate was delivered or regenerated: calls=%d err=%v", calls, err)
	}
}
