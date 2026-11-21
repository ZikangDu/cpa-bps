package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

type fixtureTransport func(*http.Request) (*http.Response, error)

func (f fixtureTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func reply(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}
func inputFixture() privateInput {
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account"}}`)) + ".synthetic"
	return privateInput{Token: token, Model: "gpt-fixture", RequestBudget: 4}
}
func probeFixture(t *testing.T, used int, callback func(int, *http.Request) *http.Response) (*upstream.Client, *atomic.Int32) {
	t.Helper()
	posts := new(atomic.Int32)
	input := inputFixture()
	client := upstream.NewWithTransport(fixtureTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Authorization") != "Bearer "+input.Token || r.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
			t.Error("credential or selected account lost")
		}
		if r.Method == http.MethodGet {
			switch r.URL.Path {
			case "/backend-api/wham/usage":
				return reply(200, fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":18000}}}`, used)), nil
			case "/backend-api/wham/rate-limit-reset-credits":
				return reply(200, `{"available_count":0}`), nil
			}
		}
		if r.Method == http.MethodPost && r.URL.Host == "bps.openai.com" {
			return callback(int(posts.Add(1)), r), nil
		}
		t.Error("unexpected endpoint")
		return nil, fmt.Errorf("unexpected fixture route")
	}))
	t.Cleanup(client.CloseIdleConnections)
	return client, posts
}
func response(output any) string {
	b, _ := json.Marshal(map[string]any{"id": "resp_fixture", "status": "completed", "model": "gpt-fixture", "output": output})
	return string(b)
}
func finalAnswer() string {
	return response([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": "5"}}}})
}
func textCall() string {
	return response([]any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": `{"client_tool":"fixture_sum","arguments":{"a":2,"b":3}}`}}}})
}
func nativeCall() string {
	outer, _ := json.Marshal(map[string]any{"summary": "Run client tool fixture_sum", "extended_summary": "Fixture relay", "code": `{"name":"fixture_sum","arguments":{"a":2,"b":3}}`, "destructive": false, "references": []any{}})
	return response([]any{map[string]string{"type": "function_call", "name": "run_officejs", "id": "fc_fixture", "call_id": "call_fixture", "status": "completed", "arguments": string(outer)}})
}
func TestProbeRequiresFreshPositiveQuotaBeforeGeneration(t *testing.T) {
	client, posts := probeFixture(t, 100, func(int, *http.Request) *http.Response { t.Error("zero quota generated"); return reply(500, `{}`) })
	r := probe(context.Background(), client, inputFixture(), func(report) {})
	if posts.Load() != 0 || !r.Stopped || r.QuotaStatus != "blocked" {
		t.Fatalf("zero quota bypassed: %+v", r)
	}
	good := upstream.BPSQuotaResult{Source: "active_usage", ObservedAt: time.Now().UTC().Format(time.RFC3339), Quota: json.RawMessage(`{"groups":[{"buckets":[{"remainingFraction":0.2}]}]}`)}
	if !quotaReady(good, time.Now()) {
		t.Fatal("positive fixture rejected")
	}
	good.Stale = true
	if quotaReady(good, time.Now()) {
		t.Fatal("stale quota accepted")
	}
}
func TestProbeStopsAllVariantsAfterAccessOrQuotaRefusal(t *testing.T) {
	for _, status := range []int{401, 403, 429} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			client, posts := probeFixture(t, 20, func(int, *http.Request) *http.Response {
				return reply(status, `{"error":{"code":"rate_limit_exceeded","message":"private-token-reflection"}}`)
			})
			r := probe(context.Background(), client, inputFixture(), func(report) {})
			raw, _ := json.Marshal(r)
			if posts.Load() != 1 || !r.Stopped || len(r.Variants) != 1 || len(r.Variants[0].Steps) != 1 || strings.Contains(string(raw), "private-token-reflection") || strings.Contains(string(raw), inputFixture().Token) {
				t.Fatalf("refusal generated again or exposed private data: %+v", r)
			}
		})
	}
}
func TestUnknownNativeToolSkipsContinuationButTestsText(t *testing.T) {
	client, posts := probeFixture(t, 20, func(n int, r *http.Request) *http.Response {
		switch n {
		case 1:
			return reply(200, response([]any{map[string]string{"type": "function_call", "name": "unexpected_native", "arguments": "{}", "call_id": "call_unknown"}}))
		case 2:
			return reply(200, textCall())
		case 3:
			raw, _ := io.ReadAll(r.Body)
			if !strings.Contains(string(raw), "Client tool result") || !strings.Contains(string(raw), "5") {
				t.Error("text continuation lost result")
			}
			return reply(200, finalAnswer())
		default:
			t.Error("request budget exceeded")
			return reply(500, `{}`)
		}
	})
	r := probe(context.Background(), client, inputFixture(), func(report) {})
	if posts.Load() != 3 || len(r.Variants) != 2 || len(r.Variants[0].Steps) != 1 || r.Variants[0].Status != "failed" || r.Variants[1].Status != "passed" || r.Passed {
		t.Fatalf("unexpected native fallback behavior: %+v", r)
	}
}
func TestProbePreservesNativeHistoryAndBudget(t *testing.T) {
	client, posts := probeFixture(t, 20, func(n int, r *http.Request) *http.Response {
		if n == 1 {
			return reply(200, nativeCall())
		}
		if n == 2 {
			raw, _ := io.ReadAll(r.Body)
			var wire struct {
				Input []json.RawMessage `json:"input"`
			}
			_ = json.Unmarshal(raw, &wire)
			if !strings.Contains(string(raw), "fixture_sum") || !strings.Contains(string(raw), "function_call_output") || !strings.Contains(string(raw), "call_fixture") {
				t.Error("native continuation lost full tool history")
			}
			return reply(200, finalAnswer())
		}
		t.Error("budget sent another request")
		return reply(500, `{}`)
	})
	input := inputFixture()
	input.RequestBudget = 2
	r := probe(context.Background(), client, input, func(report) {})
	if posts.Load() != 2 || r.GenerationRequests != 2 || len(r.Variants) != 2 || r.Variants[0].Status != "passed" || !r.Stopped {
		t.Fatalf("budget not enforced: %+v", r)
	}
}

func invalidNativeCall(id string) string {
	outer, _ := json.Marshal(map[string]any{"summary": "Read workbook", "extended_summary": "Fixture invalid native format",
		"code": "await Excel.run(async context => context.sync());", "destructive": false, "references": []any{}})
	return response([]any{map[string]string{"type": "function_call", "name": "run_officejs", "id": "fc_" + id, "call_id": "call_" + id, "status": "completed", "arguments": string(outer)}})
}

func TestNativeOnlyCorrectsTwiceThenReturnsFixedToolResultWithinFourRequests(t *testing.T) {
	var identity string
	client, posts := probeFixture(t, 20, func(n int, r *http.Request) *http.Response {
		raw, _ := io.ReadAll(r.Body)
		var wire map[string]json.RawMessage
		if json.Unmarshal(raw, &wire) != nil {
			t.Fatal("invalid prepared payload")
		}
		if n <= 3 {
			var metadata map[string]json.RawMessage
			_ = json.Unmarshal(wire["metadata"], &metadata)
			var iteration string
			_ = json.Unmarshal(metadata["agent_iteration"], &iteration)
			if iteration != fmt.Sprint(n) {
				t.Error("native correction iteration did not advance")
			}
			current := string(metadata["task_id"]) + "|" + string(metadata["turn_id"]) + "|" + string(wire["prompt_cache_key"])
			if n == 1 {
				identity = current
			} else if current != identity {
				t.Error("repair changed scope/task/cache identity")
			}
			if n > 1 && (!strings.Contains(string(raw), "invalid_client_tool_transport") || !strings.Contains(string(raw), `\"executed\":false`)) {
				t.Error("correction did not report that invalid Office call was never executed")
			}
		}
		switch n {
		case 1:
			return reply(200, invalidNativeCall("bad_first"))
		case 2:
			return reply(200, invalidNativeCall("bad_second"))
		case 3:
			return reply(200, nativeCall())
		case 4:
			if !strings.Contains(string(raw), "function_call_output") || !strings.Contains(string(raw), "call_fixture") {
				t.Error("corrected call's final result history missing")
			}
			return reply(200, finalAnswer())
		default:
			t.Error("more than four generation requests")
			return reply(500, `{}`)
		}
	})
	input := inputFixture()
	input.Transports = []string{"native"}
	r := probe(context.Background(), client, input, func(report) {})
	if !r.Passed || posts.Load() != 4 || r.GenerationRequests != 4 || len(r.Variants) != 1 || r.Variants[0].Steps[0].CorrectionAttempts != 2 || len(r.Variants[0].Steps[0].CorrectionDiagnostics) != 2 || !r.Variants[0].Steps[1].AnswerMatches {
		t.Fatalf("native repair/result sequence failed: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if strings.Contains(string(encoded), input.Token) || strings.Contains(string(encoded), "await Excel.run") {
		t.Fatal("private/opaque model content leaked into diagnostics")
	}
}

func TestNativeCorrectionsAreBoundedAndDoNotSpendResultBudgetAfterFailure(t *testing.T) {
	client, posts := probeFixture(t, 20, func(n int, r *http.Request) *http.Response { return reply(200, invalidNativeCall(fmt.Sprint(n))) })
	input := inputFixture()
	input.Transports = []string{"native"}
	r := probe(context.Background(), client, input, func(report) {})
	if r.Passed || posts.Load() != 3 || r.GenerationRequests != 3 || len(r.Variants) != 1 || len(r.Variants[0].Steps) != 1 || r.Variants[0].Steps[0].CorrectionAttempts != 2 || r.Variants[0].Steps[0].Diagnostic.Code != "invalid_tool_call" {
		t.Fatalf("native correction cap was bypassed: %+v", r)
	}
}

func TestNativeCorrectionStopsImmediatelyOnForbidden(t *testing.T) {
	client, posts := probeFixture(t, 20, func(n int, r *http.Request) *http.Response {
		if n == 1 {
			return reply(200, invalidNativeCall("before_refusal"))
		}
		return reply(403, `{"error":{"code":"insufficient_scope","message":"private-reflected-token"}}`)
	})
	r := probe(context.Background(), client, inputFixture(), func(report) {})
	if posts.Load() != 2 || !r.Stopped || len(r.Variants) != 1 || r.Variants[0].Steps[0].HTTPStatus != 403 || r.Variants[0].Steps[0].CorrectionAttempts != 1 {
		t.Fatalf("403 continued corrections or text mode: %+v", r)
	}
}

func TestNativeCorrectionRequiresHTTP200AndCompleteTerminal(t *testing.T) {
	var partial map[string]any
	_ = json.Unmarshal([]byte(invalidNativeCall("partial")), &partial)
	partial["status"] = "incomplete"
	partialBody, _ := json.Marshal(partial)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"http201", 201, invalidNativeCall("created")},
		{"incomplete", 200, string(partialBody)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, posts := probeFixture(t, 20, func(int, *http.Request) *http.Response { return reply(tc.status, tc.body) })
			input := inputFixture()
			input.Transports = []string{"native"}
			r := probe(context.Background(), client, input, func(report) {})
			if posts.Load() != 1 || r.Passed || r.Variants[0].Steps[0].CorrectionAttempts != 0 {
				t.Fatalf("non-eligible response corrected: %+v", r)
			}
		})
	}
}

func TestTransportSelectionAllowsOneModeAndRejectsDuplicates(t *testing.T) {
	for _, selected := range [][]string{nil, {"native"}, {"text"}, {"text", "native"}} {
		if _, ok := selectedTransports(privateInput{Transports: selected}); !ok {
			t.Fatalf("valid transport rejected: %v", selected)
		}
	}
	for _, selected := range [][]string{{"native", "native"}, {"invalid"}, {"native", "text", "native"}} {
		if _, ok := selectedTransports(privateInput{Transports: selected}); ok {
			t.Fatalf("invalid transport accepted: %v", selected)
		}
	}
}
