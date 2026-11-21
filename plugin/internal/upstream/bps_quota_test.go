package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalizeBPSUsageUsesActualWindowAndCardData(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	raw := []byte(`{
		"plan_type":"team",
		"rate_limit":{"allowed":true,"limit_reached":false,
			"primary_window":{"used_percent":11,"limit_window_seconds":18000,"reset_at":1795285966},
			"secondary_window":{"used_percent":2,"limit_window_seconds":604800,"reset_after_seconds":565165}},
		"rate_limit_reset_credits":{"available_count":3,"applicable_available_count":0},
		"email":"must-not-leak@example.invalid","credits":{"balance":"ignored"}
	}`)
	result, err := normalizeBPSUsage(raw, now)
	if err != nil {
		t.Fatal(err)
	}
	var normalized quotaSnapshot
	if err := json.Unmarshal(result.Quota, &normalized); err != nil {
		t.Fatal(err)
	}
	if normalized.Subscription.Plan != "team" || result.Source != "active_usage" || result.ObservedAt != now.Format(time.RFC3339) ||
		result.ResetCardsAvailable == nil || *result.ResetCardsAvailable != 3 ||
		result.ApplicableResetCardsAvailable == nil || *result.ApplicableResetCardsAvailable != 0 || !result.SupportsReset ||
		result.ResetEligible || result.ResetUnavailableReason != "bps_reset_not_applicable" {
		t.Fatalf("incorrect normalized quota: %+v", result)
	}
	buckets := normalized.Groups[0].Buckets
	if buckets[0].Window != "300m" || *buckets[0].RemainingFraction != .89 ||
		buckets[0].ResetTime != time.Unix(1795285966, 0).UTC().Format(time.RFC3339) ||
		buckets[1].Window != "10080m" || *buckets[1].RemainingFraction != .98 ||
		buckets[1].ResetTime != now.Add(565165*time.Second).Format(time.RFC3339) {
		t.Fatalf("incorrect window normalization: %+v", buckets)
	}
	if strings.Contains(string(result.Quota), "must-not-leak") || strings.Contains(string(result.Quota), "balance") {
		t.Fatal("unrelated account data escaped normalization")
	}
}

func TestBPSQuotaMissingAndInvalidValues(t *testing.T) {
	result, err := normalizeBPSUsage([]byte(`{"plan_type":"plus","rate_limit":{"primary_window":null}}`), time.Now())
	if err != nil || result.ResetCardsAvailable != nil || strings.Contains(string(result.Quota), "remainingFraction") {
		t.Fatal("missing quota was converted to a numeric limit")
	}
	for _, raw := range []string{
		`{}`,
		`{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":101}}}`,
		`{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":-1}}}`,
		`{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":0,"reset_at":-1}}}`,
		`{"plan_type":"team","rate_limit_reset_credits":{"available_count":-1}}`,
	} {
		if _, err := normalizeBPSUsage([]byte(raw), time.Now()); !errors.Is(err, ErrInvalidQuota) {
			t.Fatalf("invalid upstream quota accepted: %s", raw)
		}
	}
}

func TestBPSQuotaUsesReadOnlyAuthenticatedRequests(t *testing.T) {
	var usageReads, cardReads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet || req.Header.Get("Authorization") != "Bearer fixture" ||
			req.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
			t.Error("quota request is not an authorized read")
		}
		switch req.URL.Path {
		case "/usage":
			usageReads.Add(1)
			fmt.Fprint(w, `{"plan_type":"team","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":18000,"reset_after_seconds":3600}}}`)
		case "/credits":
			cardReads.Add(1)
			fmt.Fprint(w, `{"available_count":3,"credits":[{"secret":"discard"}]}`)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	result, err := client.bpsQuota(context.Background(), server.URL+"/usage", server.URL+"/credits", "fixture", "fixture-account")
	if err != nil || result.ResetCardsAvailable == nil || *result.ResetCardsAvailable != 3 ||
		result.Warning != "" || usageReads.Load() != 1 || cardReads.Load() != 1 {
		t.Fatalf("authenticated quota lookup failed: %+v %v", result, err)
	}
}

func TestBPSQuotaRejectsRedirectsAndClassifiesDenials(t *testing.T) {
	var reached atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached.Add(1) }))
	defer target.Close()
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusFound} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			w.Header().Set("Location", target.URL)
			w.WriteHeader(status)
		}))
		client := New()
		_, err := client.bpsQuota(context.Background(), server.URL, server.URL+"/credits", "fixture", "")
		client.CloseIdleConnections()
		server.Close()
		expected := ErrQuotaUnavailable
		if status == http.StatusUnauthorized {
			expected = ErrBPSReauthorization
		} else if status == http.StatusForbidden {
			expected = ErrBPSQuotaDenied
		}
		if !errors.Is(err, expected) {
			t.Fatalf("status %d returned %v", status, err)
		}
	}
	if reached.Load() != 0 {
		t.Fatal("quota authorization followed a redirect")
	}
}

func TestBPSProfileReturnsOnlyValidHints(t *testing.T) {
	payload := `{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account","chatgpt_plan_type":"Team"},"email":"discard"}`
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	accountID, plan := BPSProfile(token)
	if accountID != "fixture-account" || plan != "team" {
		t.Fatal("profile hints not parsed")
	}
	if id, plan := BPSProfile("malformed"); id != "" || plan != "" {
		t.Fatal("malformed token produced profile hints")
	}
}
