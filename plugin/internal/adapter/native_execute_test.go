package adapter

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func nativeFixtureResult(valid bool) string {
	code := "Excel.run(async ctx => ctx.sync())"
	if valid {
		code = `{"name":"fixture_lookup","arguments":{"q":"ok"}}`
	}
	args, _ := json.Marshal(map[string]any{"summary": "Relay fixture", "extended_summary": "", "code": code, "destructive": false, "references": []any{}})
	body, _ := json.Marshal(map[string]any{"id": "response_fixture", "status": "completed", "model": "fixture", "output": []any{map[string]any{"type": "function_call", "name": "run_officejs", "call_id": "call_fixture", "id": "fc_fixture", "arguments": string(args), "status": "completed"}}})
	return string(body)
}

func TestNativeRepairBeforeDeliveryAndBoundedHTTPFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		stream      bool
		status      int
		repairWorks bool
		wantPosts   int
		success     bool
	}{
		{"json repair", false, 200, true, 2, true}, {"stream repair", true, 200, true, 2, true},
		{"invalid bounded", false, 200, false, 3, false},
		{"forbidden once", false, 403, false, 1, false}, {"quota once", false, 429, false, 1, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			r, _, raw := quotaFixture(t)
			r.cfg.BPSToolTransport = "native"
			r.cfg.NativeToolRepairAttempts = 2
			var mu sync.Mutex
			frames := []string{}
			done := make(chan struct{})
			r.host = func(method string, req any, _ any) error {
				switch method {
				case "host.stream.emit":
					mu.Lock()
					frames = append(frames, string(req.(map[string]any)["payload"].([]byte)))
					mu.Unlock()
				case "host.stream.close":
					close(done)
				}
				return nil
			}
			posts := 0
			r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method == http.MethodGet {
					if strings.HasSuffix(req.URL.Path, "/usage") {
						return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":1}}}`, false), nil
					}
					return qualityResponseFixture(200, `{"available_count":0}`, false), nil
				}
				posts++
				data, _ := io.ReadAll(req.Body)
				if posts == 2 && !strings.Contains(string(data), "executed") {
					t.Error("repair omitted explicit not-executed result")
				}
				if req.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
					t.Error("native request lost account context")
				}
				if test.status != 200 {
					return qualityResponseFixture(test.status, `{"error":{"code":"rate_limit_exceeded"}}`, false), nil
				}
				return qualityResponseFixture(200, strings.ReplaceAll(nativeFixtureResult(test.repairWorks && posts > 1), "fixture\"", "fixture_"+strconv.Itoa(posts)+"\""), false), nil
			}))
			payload := []byte(`{"model":"fixture","input":"fixture","tools":[{"type":"function","name":"fixture_lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}],"tool_choice":"required"}`)
			req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: raw, Payload: payload, StreamID: "fixture-stream"}
			encoded, _ := json.Marshal(req)
			_, err := r.execute(encoded, test.stream)
			if (err == nil) != test.success || posts != test.wantPosts {
				t.Fatalf("unexpected repair outcome posts=%d err=%v", posts, err)
			}
			if test.stream && test.success {
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					t.Fatal("stream not closed")
				}
				mu.Lock()
				all := strings.Join(frames, "")
				mu.Unlock()
				if !strings.Contains(all, "fixture_lookup") || strings.Contains(all, "Excel.run") || strings.Contains(all, "run_officejs") {
					t.Fatal("unvalidated native output reached client")
				}
			}
		})
	}
}

func TestNativeBufferedNeverDeliversToolsFromIncompleteOrFailedResponses(t *testing.T) {
	for _, status := range []string{"incomplete", "failed"} {
		for _, upstreamSSE := range []bool{false, true} {
			for _, downstreamStream := range []bool{false, true} {
				name := status + "/upstreamSSE=" + strconv.FormatBool(upstreamSSE) + "/stream=" + strconv.FormatBool(downstreamStream)
				t.Run(name, func(t *testing.T) {
					r, _, raw := quotaFixture(t)
					r.cfg.BPSToolTransport = "native"
					r.cfg.NativeToolRepairAttempts = 2
					posts, emits := 0, 0
					complete := false
					r.host = func(method string, _ any, _ any) error {
						if method == "host.stream.emit" {
							emits++
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
						posts++
						var body map[string]any
						_ = json.Unmarshal([]byte(nativeFixtureResult(true)), &body)
						actualStatus := status
						if complete {
							actualStatus = "completed"
						}
						body["status"] = actualStatus
						payload, _ := json.Marshal(body)
						if upstreamSSE {
							event, _ := json.Marshal(map[string]any{"type": "response." + actualStatus, "response": body})
							return qualityResponseFixture(200, "data: "+string(event)+"\n\n", true), nil
						}
						return qualityResponseFixture(200, string(payload), false), nil
					}))
					payload := []byte(`{"model":"fixture","input":"fixture","tools":[{"type":"function","name":"fixture_lookup","parameters":{"type":"object","properties":{"q":{"type":"string"}},"required":["q"]}}],"tool_choice":"required"}`)
					req := executionRequest{AuthID: "fixture-id", AuthProvider: provider, Model: "fixture", StorageJSON: raw, Payload: payload, StreamID: "unfinished-fixture-stream"}
					encoded, _ := json.Marshal(req)
					reply, err := r.execute(encoded, downstreamStream)
					detail, ok := err.(*rpcError)
					if reply != nil || !ok || detail.StatusCode() != 502 || detail.PublicCode != "bps_invalid_tool_response" || posts != 1 || emits != 0 {
						t.Fatalf("unfinished tool leaked or was repaired: posts=%d emits=%d err=%v", posts, emits, err)
					}
					if state := r.queue.Snapshot(req.AuthID, 20); state.Active != 0 || state.Waiting != 0 {
						t.Fatal("rejected terminal leaked account slot")
					}
					finished := make(chan struct{})
					go func() { r.tasks.Wait(); close(finished) }()
					select {
					case <-finished:
					case <-time.After(time.Second):
						t.Fatal("rejected terminal leaked runtime task")
					}
					// A subsequent complete response can still use this account.
					complete = true
					if _, err := r.execute(encoded, false); err != nil || posts != 2 {
						t.Fatalf("account did not recover after rejected terminal: posts=%d err=%v", posts, err)
					}
				})
			}
		}
	}
}
