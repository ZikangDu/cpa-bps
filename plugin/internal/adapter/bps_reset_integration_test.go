package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
)

type resetMemoryTransport func(*http.Request) (*http.Response, error)

func (f resetMemoryTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

type resetScenario struct {
	available             int
	applicable            int
	consumeStatus         int
	consumeTransportError bool
	refreshFailure        bool
	consumes              atomic.Int32
	reads                 atomic.Int32
	cooldownResets        atomic.Int32
}

func resetManagementFixture(t *testing.T, scenario *resetScenario, modify ...func(*credential)) *Runtime {
	t.Helper()
	claims := `{"https://api.openai.com/auth":{"chatgpt_account_id":"selected-account","chatgpt_plan_type":"team"}}`
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".synthetic-selected-token"
	auth := credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Grant: &managedGrant{
		ClientID: oauthflow.BPSClientID, TokenURL: oauthflow.BPSConfig().TokenURL,
		Tokens: oauthflow.Tokens{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour)},
	}}
	for _, mutate := range modify {
		mutate(&auth)
	}
	raw, _ := json.Marshal(auth)
	r := New(func(method string, payload any, result any) error {
		request := payload.(map[string]any)
		if request["host_callback_id"] != "synthetic-host-context" {
			t.Error("host context lost")
			return errors.New("wrong host context")
		}
		switch method {
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"other-auth","auth_index":"other-index","provider":"cpa-bps"},{"id":"selected-auth","auth_index":"selected-index","provider":"cpa-bps"}]}`), result)
		case "host.auth.get":
			if request["auth_index"] != "selected-index" {
				t.Error("another account was read")
				return errors.New("wrong account")
			}
			wrapped, _ := json.Marshal(map[string]json.RawMessage{"json": raw})
			return json.Unmarshal(wrapped, result)
		case "host.routing.reset_cooldown":
			if request["auth_index"] != "selected-index" {
				t.Error("another account cooldown was reset")
				return errors.New("wrong account")
			}
			scenario.cooldownResets.Add(1)
			return nil
		default:
			t.Errorf("unexpected host callback %s", method)
			return errors.New("unexpected callback")
		}
	})
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nmodels: [fixture]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = cfg
	r.client.CloseIdleConnections()
	r.client = upstream.NewWithTransport(resetMemoryTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "chatgpt.com" || request.Header.Get("Authorization") != "Bearer "+token || request.Header.Get("ChatGPT-Account-ID") != "selected-account" {
			t.Error("request left the selected account/origin")
			return nil, errors.New("wrong account or origin")
		}
		status, body := http.StatusOK, ""
		switch request.URL.Path {
		case "/backend-api/wham/usage":
			scenario.reads.Add(1)
			if request.Method != http.MethodGet {
				t.Error("usage was not GET")
			}
			body = fmt.Sprintf(`{"plan_type":"team","rate_limit_reset_credits":{"available_count":3,"applicable_available_count":%d}}`, scenario.applicable)
			if scenario.refreshFailure && scenario.consumes.Load() > 0 {
				status = 503
				body = `{}`
			}
		case "/backend-api/wham/rate-limit-reset-credits":
			scenario.reads.Add(1)
			if request.Method != http.MethodGet {
				t.Error("credits were not GET")
			}
			body = fmt.Sprintf(`{"available_count":%d}`, scenario.available)
			if scenario.refreshFailure && scenario.consumes.Load() > 0 {
				status = 503
				body = `{}`
			}
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			scenario.consumes.Add(1)
			if request.Method != http.MethodPost || request.GetBody != nil {
				t.Error("consume method or replay policy is incorrect")
			}
			var posted map[string]string
			if json.NewDecoder(request.Body).Decode(&posted) != nil || len(posted) != 1 || posted["redeem_request_id"] != "fixture-redeem-request-12345678" {
				t.Error("consume did not retain request id")
			}
			if scenario.consumeTransportError {
				return nil, errors.New("synthetic connection lost after POST")
			}
			status = scenario.consumeStatus
			body = `{}`
		default:
			t.Errorf("unexpected fixed-domain path %s", request.URL.Path)
			return nil, errors.New("unexpected path")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	}))
	t.Cleanup(r.Close)
	return r
}

func TestPausedBPSCredentialAllowsOnlyQuotaInspectionAndDoesNotPolluteCache(t *testing.T) {
	s := &resetScenario{available: 3, applicable: 1, consumeStatus: 204}
	r := resetManagementFixture(t, s, func(auth *credential) { auth.Disabled = true })
	request := managementRequest{Method: http.MethodGet, Path: "/v0/management/cpa-bps/quota", Query: url.Values{"auth_index": {"selected-index"}}, HostCallbackID: "synthetic-host-context"}
	reply, err := r.manageBPSQuota(request)
	if err != nil {
		t.Fatal(err)
	}
	response := reply.(map[string]any)
	var observed upstream.BPSQuotaResult
	if json.Unmarshal(response["Body"].([]byte), &observed) != nil || response["StatusCode"] != http.StatusOK || !observed.CredentialDisabled || observed.ResetEligible || observed.ResetUnavailableReason != "bps_credential_disabled" || observed.ResetCardsAvailable == nil || *observed.ResetCardsAvailable != 3 || observed.ApplicableResetCardsAvailable == nil || *observed.ApplicableResetCardsAvailable != 1 || s.reads.Load() != 2 {
		t.Fatalf("paused credential quota inspection failed: status=%v result=%+v", response["StatusCode"], observed)
	}
	status, reset := submitResetManagement(t, r, "fixture-redeem-request-12345678")
	if status != http.StatusConflict || reset.Error != "bps_credential_disabled" || reset.Submitted || s.consumes.Load() != 0 || s.reads.Load() != 2 {
		t.Fatal("paused credential redemption was not rejected before any upstream mutation")
	}
	var stored struct {
		JSON json.RawMessage `json:"json"`
	}
	if r.host("host.auth.get", map[string]any{"auth_index": "selected-index", "host_callback_id": "synthetic-host-context"}, &stored) != nil {
		t.Fatal("fixture read failed")
	}
	if _, authErr := resolveToken(stored.JSON, r.cfg.ResponsesURL, r.cfg.OAuth); authErr == nil {
		t.Fatal("readonly quota handling enabled inference on a paused credential")
	}
	active, _ := credentialFromStorage(stored.JSON)
	active.Disabled = false
	activeRaw, _ := json.Marshal(active)
	activeView, quotaErr := r.loadBPSQuota(context.Background(), r.cfg, "selected-auth", activeRaw, false)
	if quotaErr != nil || activeView.CredentialDisabled || !activeView.ResetEligible || activeView.ResetUnavailableReason != "" || s.reads.Load() != 2 {
		t.Fatal("paused-only override polluted token-scoped quota cache")
	}
}

func TestPausedBPSQuotaStillChecksExpiryOriginAndOAuthRegistration(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*credential)
		status int
	}{
		{"expired", func(auth *credential) { auth.Grant.ExpiresAt = time.Now().Add(-time.Minute) }, http.StatusUnauthorized},
		{"wrong-origin", func(auth *credential) { auth.UpstreamOrigin = "https://other.example.invalid" }, http.StatusForbidden},
		{"wrong-client", func(auth *credential) { auth.Grant.ClientID = "other-fixture-client" }, http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &resetScenario{available: 3, applicable: 1, consumeStatus: 204}
			r := resetManagementFixture(t, s, func(auth *credential) { auth.Disabled = true; tc.mutate(auth) })
			reply, err := r.manageBPSQuota(managementRequest{Method: http.MethodGet, Path: "/v0/management/cpa-bps/quota", Query: url.Values{"auth_index": {"selected-index"}}, HostCallbackID: "synthetic-host-context"})
			if err != nil || reply.(map[string]any)["StatusCode"] != tc.status || s.reads.Load() != 0 || s.consumes.Load() != 0 {
				t.Fatal("paused quota bypassed credential validation")
			}
		})
	}
}

func submitResetManagement(t *testing.T, r *Runtime, id string) (int, bpsResetReply) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"confirm": true, "redeem_request_id": id})
	reply, err := r.manageBPSReset(managementRequest{Method: http.MethodPost, Path: "/v0/management/cpa-bps/reset", Query: url.Values{"auth_index": {"selected-index"}}, Body: body, HostCallbackID: "synthetic-host-context"})
	if err != nil {
		t.Fatal(err)
	}
	response := reply.(map[string]any)
	var result bpsResetReply
	if err := json.Unmarshal(response["Body"].([]byte), &result); err != nil {
		t.Fatal(err)
	}
	return response["StatusCode"].(int), result
}

func TestManageBPSResetFreshInapplicabilityAndInconsistentCountsDoNotConsume(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		available, applicable int
		reason                string
	}{
		{"owned-but-inapplicable", 3, 0, "bps_reset_not_applicable"},
		{"conflicting-counts", 1, 2, "bps_reset_inconsistent_credits"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &resetScenario{available: tc.available, applicable: tc.applicable, consumeStatus: 204}
			r := resetManagementFixture(t, s)
			status, result := submitResetManagement(t, r, "fixture-redeem-request-12345678")
			if status != 409 || result.Submitted || result.Error != tc.reason || s.consumes.Load() != 0 || s.reads.Load() != 2 || s.cooldownResets.Load() != 0 {
				t.Fatalf("invalid redemption: status=%d reply=%+v reads=%d consumes=%d", status, result, s.reads.Load(), s.consumes.Load())
			}
		})
	}
}

func TestManageBPSResetConsumesOnceAndRepeatsKnownSuccess(t *testing.T) {
	s := &resetScenario{available: 3, applicable: 1, consumeStatus: 204}
	r := resetManagementFixture(t, s)
	for range 2 {
		status, result := submitResetManagement(t, r, "fixture-redeem-request-12345678")
		if status != 200 || !result.Submitted || result.Error != "" {
			t.Fatalf("reset failed: %d %+v", status, result)
		}
	}
	if s.consumes.Load() != 1 || s.reads.Load() != 4 || s.cooldownResets.Load() != 1 {
		t.Fatalf("redemption replayed or scope drifted: reads=%d consumes=%d resets=%d", s.reads.Load(), s.consumes.Load(), s.cooldownResets.Load())
	}
}

func TestManageBPSResetAmbiguousPostBlocksSameAndNewIDs(t *testing.T) {
	for _, transportError := range []bool{false, true} {
		t.Run(fmt.Sprint(transportError), func(t *testing.T) {
			s := &resetScenario{available: 3, applicable: 1, consumeStatus: 503, consumeTransportError: transportError}
			r := resetManagementFixture(t, s)
			status, result := submitResetManagement(t, r, "fixture-redeem-request-12345678")
			if status != 502 || result.Submitted || result.Error != "bps_reset_outcome_unknown" {
				t.Fatalf("ambiguous result %d %+v", status, result)
			}
			for _, id := range []string{"fixture-redeem-request-12345678", "new-redeem-request-12345678"} {
				_, again := submitResetManagement(t, r, id)
				if again.Submitted || again.Error != "bps_reset_outcome_unknown" {
					t.Fatal("ambiguous POST repeated")
				}
			}
			if s.consumes.Load() != 1 || s.reads.Load() != 2 || s.cooldownResets.Load() != 0 {
				t.Fatal("ambiguous redemption was retried or cooldown cleared")
			}
		})
	}
}

func TestManageBPSResetRefreshFailureStillReportsKnownSubmission(t *testing.T) {
	s := &resetScenario{available: 3, applicable: 1, consumeStatus: 204, refreshFailure: true}
	r := resetManagementFixture(t, s)
	status, result := submitResetManagement(t, r, "fixture-redeem-request-12345678")
	if status != 200 || !result.Submitted || result.Warning != "reset_submitted_refresh_required" {
		t.Fatalf("refresh failure hid consumption: %d %+v", status, result)
	}
	_, again := submitResetManagement(t, r, "fixture-redeem-request-12345678")
	if !again.Submitted || s.consumes.Load() != 1 {
		t.Fatal("refresh failure caused repeat redemption")
	}
}
