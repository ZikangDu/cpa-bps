package adapter

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/upstream"
)

func orderedNativeResponse(attempt int, valid bool) string {
	arguments := `{"q":42}`
	if valid {
		arguments = `{"q":"ok"}`
	}
	calls := autoNative("lookup", arguments)
	call := calls[0].(map[string]any)
	call["id"], call["call_id"] = fmt.Sprintf("fc_order_%d", attempt), fmt.Sprintf("call_order_%d", attempt)
	return autoResponse("", calls, 5+attempt)
}

func TestAutomaticNativeRepairPrecedesOneProtocolSwitch(t *testing.T) {
	for _, test := range []struct {
		name       string
		initial    string
		repairs    int
		replies    []string
		modes      []string
		wantRepair int
		wantError  bool
	}{
		{"native correction succeeds", "native", 2, []string{"bad-native", "good-native"}, []string{"native", "native"}, 1, false},
		{"native correction then text", "native", 2, []string{"bad-native", "bad-native", "good-text"}, []string{"native", "native", "text"}, 1, false},
		{"one configured correction", "native", 1, []string{"bad-native", "bad-native", "good-text"}, []string{"native", "native", "text"}, 1, false},
		{"correction disabled", "native", 0, []string{"bad-native", "good-text"}, []string{"native", "text"}, -1, false},
		{"text native then correction", "text", 2, []string{"plain", "bad-native", "good-native"}, []string{"text", "native", "native"}, 2, false},
		{"no native call to correct", "native", 2, []string{"plain", "good-text"}, []string{"native", "text"}, -1, false},
		{"unknown hosted after correction", "native", 2, []string{"bad-native", "bad-native", "hosted"}, []string{"native", "native", "text"}, 1, true},
		{"three attempt hard bound", "native", 2, []string{"bad-native", "bad-native", "bad-native"}, []string{"native", "native", "text"}, 1, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _, auth := quotaFixture(t)
			r.cfg.BPSToolTransport, r.cfg.NativeToolRepairAttempts = "auto", test.repairs
			var modes []string
			var payloads [][]byte
			r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if strings.HasSuffix(req.URL.Path, "/usage") {
						return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
					}
					return qualityResponseFixture(200, `{"available_count":0}`, false), nil
				}
				if req.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
					t.Error("correction changed the selected account")
				}
				mode := "text"
				if req.Header.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") == "excel" {
					mode = "native"
				}
				modes = append(modes, mode)
				body, _ := io.ReadAll(req.Body)
				payloads = append(payloads, body)
				attempt := len(modes) - 1
				if attempt >= len(test.replies) {
					t.Fatal("automatic execution exceeded its generation budget")
				}
				var response string
				switch test.replies[attempt] {
				case "bad-native":
					response = orderedNativeResponse(attempt, false)
				case "good-native":
					response = orderedNativeResponse(attempt, true)
				case "good-text":
					response = autoResponse(`{"client_tool":"lookup","arguments":{"q":"ok"}}`, nil, 5+attempt)
				case "plain":
					response = autoResponse("I will use the tool.", nil, 5+attempt)
				case "hosted":
					response = autoResponse("", []any{map[string]any{"type": "function_call", "name": "list_skills", "id": "fc_hosted", "call_id": "call_hosted", "status": "completed", "arguments": `{"private_payload":"never expose this"}`}}, 5+attempt)
				}
				return qualityResponseFixture(200, response, false), nil
			}))
			source := autoSource(false)
			original := bytes.Clone(source)
			req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: auth, Payload: source}
			if test.initial == "text" {
				_, key, _, err := r.selectAutoTransport(req, r.cfg, source)
				if err != nil {
					t.Fatal(err)
				}
				r.transports.learn(key, "text")
			}
			raw, _ := json.Marshal(req)
			reply, err := r.execute(raw, false)
			if !reflect.DeepEqual(modes, test.modes) || (err != nil) != test.wantError || len(modes) > 3 {
				t.Fatalf("wrong attempt order or result: modes=%v err=%v", modes, err)
			}
			if !bytes.Equal(source, original) {
				t.Fatal("correction mutated the original client request")
			}
			if test.wantError {
				message := err.Error()
				if !strings.Contains(message, "initial_native=(response_tool_conversion: arguments_schema_mismatch)") || strings.Contains(message, "private_payload") || strings.Contains(message, "list_skills") || strings.Contains(message, "never expose") {
					t.Fatal("first native diagnosis was lost or leaked upstream content", message)
				}
			} else {
				var response struct {
					Output []map[string]any
					Usage  struct {
						InputTokens int `json:"input_tokens"`
					}
					Metadata map[string]string
				}
				_ = json.Unmarshal(reply.(map[string]any)["Payload"].([]byte), &response)
				wantUsage := len(modes)*5 + len(modes)*(len(modes)-1)/2
				if response.Usage.InputTokens != wantUsage || response.Metadata["cpa_bps_attempts"] != fmt.Sprint(len(modes)) || len(response.Output) != 1 || response.Output[0]["name"] != "lookup" {
					t.Fatal("correction lost attempt usage or delivered multiple calls")
				}
			}
			for index, payload := range payloads {
				var body map[string]any
				_ = json.Unmarshal(payload, &body)
				input := body["input"].([]any)
				results := 0
				for _, raw := range input {
					item := raw.(map[string]any)
					if item["type"] == "function_call_output" {
						results++
						var result map[string]any
						_ = json.Unmarshal([]byte(item["output"].(string)), &result)
						if result["executed"] != false || item["call_id"] != fmt.Sprintf("call_order_%d", index-1) {
							t.Fatal("repair did not explicitly correlate an unexecuted call")
						}
					}
				}
				if (results == 1) != (index == test.wantRepair) {
					t.Fatal("repair history leaked into the wrong generation")
				}
				if index == test.wantRepair {
					var previous map[string]any
					_ = json.Unmarshal(payloads[index-1], &previous)
					prefix := previous["input"].([]any)
					if len(input) != len(prefix)+3 || !reflect.DeepEqual(prefix, input[:len(prefix)]) || previous["prompt_cache_key"] != body["prompt_cache_key"] {
						t.Fatal("native correction rewrote the prior conversation/cache prefix")
					}
				}
			}
			if status := r.queue.Snapshot("fixture-id", 20); status.Active != 0 || status.Waiting != 0 {
				t.Fatal("automatic correction retained an account slot")
			}
		})
	}
}

func TestInitialNativeDiagnosticUsesOnlyProtocolAllowlist(t *testing.T) {
	_, bridge, err := bpsprotocol.PrepareWithOptions(autoSource(false), "safe-native-diagnostic", bpsprotocol.Options{ToolTransport: "native"})
	if err != nil {
		t.Fatal(err)
	}
	_, firstErr := bridge.Transform([]byte(orderedNativeResponse(0, false)))
	first, ok := firstErr.(*bpsprotocol.Error)
	if !ok {
		t.Fatal("missing fixture protocol failure")
	}
	converted := protocolErrorWithInitialNative(fmt.Errorf("private raw terminal body"), first)
	if strings.Contains(converted.Error(), "private") || !strings.Contains(converted.Error(), "initial_native=(response_tool_conversion: arguments_schema_mismatch)") {
		t.Fatal("diagnostic included free-form errors or lost the allowlisted category")
	}
}
