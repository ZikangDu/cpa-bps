package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/candy"
	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/quality"
	"cpa-bps-reconstruction/internal/upstream"
)

type qualityMemoryTransport func(*http.Request) (*http.Response, error)

func (f qualityMemoryTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

type qualityHostFixture struct {
	mu       sync.Mutex
	disabled bool
	status   string
	token    string
	gets     []string
}

func qualityRuntimeFixture(t *testing.T, transport qualityMemoryTransport) (*Runtime, *qualityHostFixture) {
	t.Helper()
	host := &qualityHostFixture{token: "synthetic-B-access-token"}
	r := New(func(method string, request any, response any) error {
		host.mu.Lock()
		defer host.mu.Unlock()
		var encoded []byte
		switch method {
		case "host.auth.list":
			encoded, _ = json.Marshal(map[string]any{"files": []any{
				map[string]any{"id": "account-A", "auth_index": "A", "provider": provider, "status_message": "bps_usage_policy_blocked"},
				map[string]any{"id": "account-B", "auth_index": "B", "provider": provider, "disabled": host.disabled, "status_message": host.status},
			}})
		case "host.auth.get":
			index, _ := request.(map[string]any)["auth_index"].(string)
			if index != "B" {
				return errors.New("unexpected credential requested")
			}
			host.gets = append(host.gets, index)
			auth := credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Disabled: host.disabled, Grant: &managedGrant{
				ClientID: oauthflow.BPSClientID, TokenURL: oauthflow.BPSConfig().TokenURL,
				Tokens: oauthflow.Tokens{AccessToken: host.token, RefreshToken: "synthetic-refresh-secret", IDToken: "synthetic-id-secret", ExpiresAt: time.Now().Add(time.Hour)},
			}}
			encoded, _ = json.Marshal(map[string]any{"json": auth})
		default:
			return errors.New("unexpected host callback")
		}
		return json.Unmarshal(encoded, response)
	})
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.QualityStatePath = filepath.Join(t.TempDir(), "quality.json")
	cfg.ConcurrencyPolicyPath = filepath.Join(t.TempDir(), "concurrency.json")
	r.client.CloseIdleConnections()
	r.client = upstream.NewWithTransport(qualityMemoryTransport(func(req *http.Request) (*http.Response, error) {
		// These tests count generation calls; quota reads have their own fixture.
		if req.Method == http.MethodGet && req.URL.Host == "chatgpt.com" {
			switch req.URL.Path {
			case "/backend-api/wham/usage":
				return qualityResponseFixture(200, `{"rate_limit":{"primary_window":{"used_percent":10,"limit_window_seconds":18000}}}`, false), nil
			case "/backend-api/wham/rate-limit-reset-credits":
				return qualityResponseFixture(200, `{"available_count":0}`, false), nil
			}
		}
		return transport.RoundTrip(req)
	}))
	r.mu.Lock()
	r.cfg = cfg
	err = r.startQualityLocked(cfg)
	r.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Close)
	return r, host
}

func qualityResponseFixture(status int, body string, event bool) *http.Response {
	contentType := "application/json"
	if event {
		contentType = eventStream
	}
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}
}

func qualityManagement(t *testing.T, r *Runtime, method, endpoint, index string, body any) (int, []byte) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	reply, err := r.manageQuality(managementRequest{Method: method, Path: "/v0/management/cpa-bps/quality/" + endpoint, Query: url.Values{"auth_index": {index}}, Body: raw})
	if err != nil {
		t.Fatal(err)
	}
	response := reply.(map[string]any)
	return response["StatusCode"].(int), response["Body"].([]byte)
}

func waitQualityResult(t *testing.T, r *Runtime, index string) quality.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		view := r.quality.Snapshot(index)
		if !view.Running && len(view.Results) > 0 {
			return view
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("quality result did not complete")
	return quality.Snapshot{}
}

func TestQualityAsyncBPSExactAccountAndNoReplay(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	r, host := qualityRuntimeFixture(t, func(req *http.Request) (*http.Response, error) {
		if req.Method != "POST" || req.URL.String() != oauthflow.BPSResponsesURL || req.Header.Get("Authorization") != "Bearer synthetic-B-access-token" {
			t.Errorf("unexpected upstream destination or credential")
		}
		posts.Add(1)
		raw, _ := io.ReadAll(req.Body)
		var payload map[string]any
		if json.Unmarshal(raw, &payload) != nil || payload["model"] != "gpt-test" || payload["reasoning_effort"] != "high" || payload["model_selection"] != "explicit" {
			t.Errorf("quality payload lost requested model/effort: %s", raw)
		}
		if _, exists := payload["tools"]; exists {
			t.Error("quality request declared tools")
		}
		if !strings.Contains(string(raw), candy.Instructions) || strings.Contains(string(raw), "expected_answer") || strings.Contains(string(raw), "synthetic-refresh-secret") {
			t.Error("benchmark instructions missing or private reference/credential included")
		}
		close(entered)
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return qualityResponseFixture(200, `data: {"type":"response.completed","response":{"model":"gpt-other","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21"}]}],"usage":{"input_tokens":12,"output_tokens":2,"total_tokens":14}}}`+"\n\n", true), nil
	})
	status, _ := qualityManagement(t, r, "POST", "run", "B", quality.RunRequest{Models: []string{"gpt-test-excel"}, ReasoningEffort: "high"})
	if status != 202 {
		t.Fatalf("manual run did not return immediately: %d", status)
	}
	<-entered
	if status, _ := qualityManagement(t, r, "POST", "run", "B", quality.RunRequest{Models: []string{"gpt-test-excel"}}); status != 409 {
		t.Fatal("concurrent selected-account run accepted")
	}
	close(release)
	view := waitQualityResult(t, r, "B")
	result := view.Results[0]
	if result.Status != "pass" || result.ActualModel != "gpt-other" || !result.ModelMismatch || result.Usage.TotalTokens != 14 || result.BenchmarkVersion != candy.Version || result.Prompt != candy.DefaultPrompt {
		t.Fatalf("wrong observation: %+v", result)
	}
	if posts.Load() != 1 || len(r.quality.Snapshot("A").Results) != 0 {
		t.Fatal("request was retried or attributed to another account")
	}
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.gets) != 1 || host.gets[0] != "B" {
		t.Fatal("unselected credential was used")
	}
}

func TestQualityPausedAndKnownPolicyAccountNeverCallsUpstream(t *testing.T) {
	var posts atomic.Int32
	r, host := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) {
		posts.Add(1)
		return nil, errors.New("must not call upstream")
	})
	host.mu.Lock()
	host.disabled = true
	host.mu.Unlock()
	status, _ := qualityManagement(t, r, "POST", "run", "B", quality.RunRequest{Models: []string{"gpt-test-excel"}})
	if status != 202 {
		t.Fatal(status)
	}
	view := waitQualityResult(t, r, "B")
	if view.Results[0].Status != "skipped" || view.Results[0].ErrorCode != "bps_quality_account_paused" {
		t.Fatal("paused account was graded")
	}
	// Automatic checks still honor known host policy failures. Explicit manual
	// checks are tested separately and always receive the real upstream result.
	blocked := r.runQualityCheck(context.Background(), quality.Check{
		AuthIndex: "A", AuthID: "account-A", Model: "gpt-test-excel", Config: qualityDefaults(), RunID: "scheduled-fixture",
	})
	if blocked.Status != "request_error" || blocked.ErrorCode != "bps_usage_policy_blocked" {
		t.Fatal("automatic policy guard was not retained")
	}
	if status, _ := qualityManagement(t, r, "GET", "models", "A", nil); status != 409 {
		t.Fatal("blocked catalog fetched")
	}
	if posts.Load() != 0 {
		t.Fatal("paused or known blocked account called upstream")
	}
}

func TestQualityManualPolicyRejectionStopsBatchWithoutChangingSchedule(t *testing.T) {
	var posts atomic.Int32
	r, _ := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) {
		posts.Add(1)
		return qualityResponseFixture(403, `{"error":{"message":"403: This request was blocked by our usage policy.","code":null},"private":"synthetic-B-access-token"}`, false), nil
	})
	cfg := qualityDefaults()
	cfg.Enabled = true
	cfg.Models = []string{"gpt-first-excel", "gpt-second-excel"}
	if status, _ := qualityManagement(t, r, "PUT", "config", "B", cfg); status != 200 {
		t.Fatal(status)
	}
	nextDue := r.quality.Snapshot("B").NextDue
	if status, _ := qualityManagement(t, r, "POST", "run", "B", nil); status != 202 {
		t.Fatal(status)
	}
	view := waitQualityResult(t, r, "B")
	if posts.Load() != 1 || len(view.Results) != 2 || view.Results[1].ErrorCode != "bps_usage_policy_blocked" ||
		view.Results[0].Status != "skipped" || view.Results[0].RequestedModel != "gpt-second-excel" ||
		view.Results[0].ErrorCode != "bps_quality_skipped_after_policy" ||
		!view.Results[0].Manual || !view.Config.Enabled || view.Blocked || !view.NextDue.Equal(nextDue) {
		t.Fatalf("manual refusal retried or changed the saved schedule: %+v", view)
	}
	raw, err := os.ReadFile(r.cfg.QualityStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic-B-access-token") || strings.Contains(string(raw), "private") {
		t.Fatal("credential or raw rejection persisted")
	}
	if status, _ := qualityManagement(t, r, "POST", "run", "B", nil); status != 429 {
		t.Fatal("manual failure bypassed the ordinary cooldown")
	}
}

func TestQualityCatalogComesFromSelectedAccount(t *testing.T) {
	var calls atomic.Int32
	r, _ := qualityRuntimeFixture(t, func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Method != "GET" || req.URL.Path != "/basispoints/api/responses/access" || req.URL.Query().Get("include_models") != "true" || req.Header.Get("Authorization") != "Bearer synthetic-B-access-token" {
			t.Error("catalog did not use selected account")
		}
		return qualityResponseFixture(200, `{"allowed":true,"model_catalog":{"models":[{"id":"gpt-test","efforts":[{"value":"high"}]}],"restricted_models":[]}}`, false), nil
	})
	status, raw := qualityManagement(t, r, "GET", "models", "B", nil)
	var result struct {
		Models []qualityModel `json:"models"`
		Source string         `json:"source"`
	}
	if status != 200 || json.Unmarshal(raw, &result) != nil || result.Source != "account_catalog" || len(result.Models) != 1 || result.Models[0].ID != "gpt-test-excel" || calls.Load() != 1 {
		t.Fatalf("wrong catalog response: %d %s", status, raw)
	}
}

func TestQualityCredentialReReadAndFailedAnswerSeparation(t *testing.T) {
	var posts atomic.Int32
	r, host := qualityRuntimeFixture(t, func(req *http.Request) (*http.Response, error) {
		posts.Add(1)
		if req.Header.Get("Authorization") != "Bearer rotated-synthetic-token" {
			t.Error("cached credential used")
		}
		return qualityResponseFixture(200, `{"status":"completed","model":"gpt-test","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"21 is mentioned but my answer is 29. rotated-synthetic-token"}]}],"usage":{"input_tokens":1,"output_tokens":7,"total_tokens":8}}`, false), nil
	})
	host.mu.Lock()
	host.token = "rotated-synthetic-token"
	host.mu.Unlock()
	if status, _ := qualityManagement(t, r, "POST", "run", "B", quality.RunRequest{Models: []string{"gpt-test-excel"}}); status != 202 {
		t.Fatal(status)
	}
	result := waitQualityResult(t, r, "B").Results[0]
	if result.Status != "answer_fail" || result.ErrorCode != "" || strings.Contains(result.Answer, "rotated-synthetic-token") || posts.Load() != 1 {
		t.Fatalf("answer failure or redaction wrong: %+v", result)
	}
}

func TestQualityCorruptStateDoesNotBreakRegisterAndPathChangeFailsClosed(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("broken"), 0600); err != nil {
		t.Fatal(err)
	}
	r := New(nil)
	defer r.Close()
	raw, _ := json.Marshal(map[string]any{"config_yaml": []byte("enabled: true\noauth_preset: bps\nquality_state_path: " + path + "\n")})
	if _, err := r.register(raw); err != nil {
		t.Fatalf("optional quality state broke inference registration: %v", err)
	}
	if !r.cfg.Enabled || r.quality != nil {
		t.Fatal("corrupt quality state was silently replaced")
	}
	saved, _ := os.ReadFile(path)
	if string(saved) != "broken" {
		t.Fatal("corrupt state overwritten")
	}
	good, _ := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) { return nil, errors.New("no requests expected") })
	oldPath := good.cfg.QualityStatePath
	good.mu.Lock()
	good.cfg.QualityStatePath = filepath.Join(t.TempDir(), "other.json")
	err := good.startQualityLocked(good.cfg)
	good.mu.Unlock()
	if err == nil {
		t.Fatal("path change accepted")
	}
	if status, _ := qualityManagement(t, good, "PUT", "config", "B", qualityDefaults()); status != 503 {
		t.Fatal("path mismatch wrote old quality state")
	}
	if _, err := os.Stat(oldPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old state unexpectedly written")
	}
}

func TestQualityReadDeadlineClassification(t *testing.T) {
	r, _ := qualityRuntimeFixture(t, func(req *http.Request) (*http.Response, error) {
		<-req.Context().Done()
		return nil, req.Context().Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	result := r.runQualityCheck(ctx, quality.Check{AuthIndex: "B", AuthID: "account-B", Model: "gpt-test-excel", Config: qualityDefaults(), RunID: "test"})
	if result.Status != "request_error" || !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("transport error unexpectedly graded")
	}
}

func TestQualityRefusalCannotBeOverriddenByOutputText(t *testing.T) {
	for _, raw := range []string{
		`{"output_text":"21","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","text":"no"}]}]}`,
		`{"output_text":"21","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","text":"no"},{"type":"output_text","text":"21"}]}]}`,
		`{"output_text":"21","output":[]}`,
		`{"output_text":"21","output":[{"type":"message","role":"user","content":[{"type":"output_text","text":"21"}]}]}`,
		`{"output_text":"21","output":[{"type":"function_call","name":"execute"}]}`,
	} {
		var response qualityResponse
		if err := json.Unmarshal([]byte(raw), &response); err != nil {
			t.Fatal(err)
		}
		if answer, ok := response.answer(); ok {
			t.Errorf("refusal or invalid structured output graded: %q", answer)
		}
	}
	var fallback qualityResponse
	_ = json.Unmarshal([]byte(`{"output_text":"21"}`), &fallback)
	if answer, ok := fallback.answer(); !ok || answer != "21" {
		t.Fatal("valid legacy output_text failed")
	}
}

func TestQualityPauseWhileWaitingForAccountSlotPreventsDispatch(t *testing.T) {
	for _, kind := range []string{"quality_plan", "plugin"} {
		t.Run(kind, func(t *testing.T) {
			var posts atomic.Int32
			r, _ := qualityRuntimeFixture(t, func(*http.Request) (*http.Response, error) {
				posts.Add(1)
				return nil, errors.New("paused check must not dispatch")
			})
			r.mu.Lock()
			r.cfg.ConcurrencyLimit = 1
			cfg := r.cfg
			r.mu.Unlock()
			limit, err := r.accountLimit(cfg, "account-B")
			if err != nil {
				t.Fatal(err)
			}
			release, err := r.queue.Acquire(context.Background(), "account-B", limit)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if status, _ := qualityManagement(t, r, "POST", "run", "B", quality.RunRequest{Models: []string{"gpt-test-excel"}}); status != 202 {
				t.Fatal(status)
			}
			deadline := time.Now().Add(time.Second)
			for r.queue.Snapshot("account-B", limit).Waiting != 1 {
				if time.Now().After(deadline) {
					t.Fatal("check did not enter account admission")
				}
				time.Sleep(time.Millisecond)
			}
			if kind == "quality_plan" {
				if status, _ := qualityManagement(t, r, "PUT", "config", "B", qualityDefaults()); status != 200 {
					t.Fatal(status)
				}
			} else {
				r.mu.Lock()
				r.cfg.Enabled = false
				err = r.startQualityLocked(r.cfg)
				r.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			}
			release()
			view := waitQualityResult(t, r, "B")
			if posts.Load() != 0 || view.Results[0].Status != "skipped" {
				t.Fatalf("paused check dispatched or graded: %+v", view.Results)
			}
		})
	}
}
