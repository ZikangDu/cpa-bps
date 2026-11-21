package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestBPSResetConsumeUsesSelectedAccountAndStableRequestIDOnce(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer fixture-token" || r.Header.Get("ChatGPT-Account-ID") != "selected-account" {
			t.Error("redemption lost the selected credential scope")
		}
		var body map[string]string
		if json.NewDecoder(r.Body).Decode(&body) != nil || len(body) != 1 || body["redeem_request_id"] != "fixture-request-12345678" {
			t.Error("incorrect consume body")
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	c := New()
	defer c.CloseIdleConnections()
	if err := c.consumeBPSResetCredit(context.Background(), server.URL, "fixture-token", "selected-account", "fixture-request-12345678"); err != nil || attempts.Load() != 1 {
		t.Fatalf("consume failed or replayed: %v attempts=%d", err, attempts.Load())
	}
}

func TestBPSResetConsumeNeverReplaysAmbiguousFailureOrFollowsRedirect(t *testing.T) {
	for _, status := range []int{400, 401, 403, 429, 500, 503, 307} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var attempts, redirected atomic.Int32
			target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
			defer target.Close()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				attempts.Add(1)
				w.Header().Set("Location", target.URL)
				w.WriteHeader(status)
				fmt.Fprint(w, `{"email":"private@example.invalid","access_token":"private"}`)
			}))
			defer server.Close()
			c := New()
			defer c.CloseIdleConnections()
			err := c.consumeBPSResetCredit(context.Background(), server.URL, "fixture-token", "selected-account", "fixture-request-12345678")
			want := ErrBPSResetRejected
			if status == 401 {
				want = ErrBPSReauthorization
			}
			if status == 403 {
				want = ErrBPSQuotaDenied
			}
			if status >= 500 {
				want = ErrBPSResetOutcomeUnknown
			}
			if !errors.Is(err, want) || attempts.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("status %d: err=%v calls=%d redirects=%d", status, err, attempts.Load(), redirected.Load())
			}
		})
	}
}

func TestBPSResetInvalidIdentityDoesNotSend(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { attempts.Add(1) }))
	defer server.Close()
	c := New()
	defer c.CloseIdleConnections()
	for _, values := range [][2]string{{"", "fixture-request-12345678"}, {"account", "short"}, {"account", "request-with\nnewline"}} {
		if err := c.consumeBPSResetCredit(context.Background(), server.URL, "fixture", values[0], values[1]); !errors.Is(err, ErrInvalidToken) {
			t.Fatal("invalid identity accepted")
		}
	}
	if attempts.Load() != 0 {
		t.Fatal("invalid consume reached upstream")
	}
}

func TestBPSQuotaPartialFailuresKeepIndependentObservations(t *testing.T) {
	for _, usageFailed := range []bool{true, false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.URL.Path == "/usage") == usageFailed {
				w.WriteHeader(503)
				return
			}
			if r.URL.Path == "/usage" {
				fmt.Fprint(w, `{"plan_type":"team","rate_limit_reset_credits":{"available_count":3,"applicable_available_count":0}}`)
			} else {
				fmt.Fprint(w, `{"available_count":3}`)
			}
		}))
		c := New()
		result, err := c.bpsQuota(context.Background(), server.URL+"/usage", server.URL+"/credits", "fixture", "selected-account")
		c.CloseIdleConnections()
		server.Close()
		if err != nil || result.ResetCardsAvailable == nil || *result.ResetCardsAvailable != 3 || result.ResetEligible {
			t.Fatalf("partial observation lost: %+v %v", result, err)
		}
		if usageFailed && (result.Source != "reset_credits" || result.Warning != "active_quota_unavailable" || result.ApplicableResetCardsAvailable != nil || result.ObservedAt == "") {
			t.Fatal("credit-only source invented applicability or lost observation")
		}
	}
}

func TestBPSResetEligibilityNeedsFreshApplicableCredits(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		want   bool
		reason string
	}{
		{`{"plan_type":"team","rate_limit_reset_credits":{"available_count":3,"applicable_available_count":1}}`, true, ""},
		{`{"plan_type":"team","rate_limit_reset_credits":{"available_count":3,"applicable_available_count":0}}`, false, "bps_reset_not_applicable"},
		{`{"plan_type":"team","rate_limit_reset_credits":{"available_count":0,"applicable_available_count":0}}`, false, "bps_reset_no_credits"},
		{`{"plan_type":"team","rate_limit_reset_credits":{"available_count":3}}`, false, "bps_reset_eligibility_unknown"},
	} {
		r, err := normalizeBPSUsage([]byte(tc.raw), time.Now())
		if err != nil || r.ResetEligible != tc.want || r.ResetUnavailableReason != tc.reason {
			t.Fatalf("eligibility %+v err %v", r, err)
		}
		r.Stale = true
		r.UpdateResetEligibility()
		if r.ResetEligible || r.ResetUnavailableReason != "bps_reset_stale_quota" {
			t.Fatal("stale observation allowed redemption")
		}
	}
}

func TestBPSResetEligibilityRejectsInvalidOrConflictingCounts(t *testing.T) {
	for _, counts := range [][2]int{{1, 2}, {-1, 0}, {3, -1}, {1_000_001, 1}} {
		available, applicable := counts[0], counts[1]
		result := BPSQuotaResult{SupportsReset: true, ResetCardsAvailable: &available, ApplicableResetCardsAvailable: &applicable}
		result.UpdateResetEligibility()
		if result.ResetEligible || result.ResetUnavailableReason != "bps_reset_inconsistent_credits" {
			t.Fatal("invalid or conflicting credit counts allowed redemption")
		}
	}
}
