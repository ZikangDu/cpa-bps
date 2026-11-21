package adapter

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func TestBPSResetGateSerializesAccountAndDeduplicatesSuccessfulID(t *testing.T) {
	c := newBPSQuotaCache()
	owner := sha256.Sum256([]byte("selected-account"))
	first, acquired, _, _ := c.beginReset(owner, "fixture-request-12345678")
	if !acquired {
		t.Fatal("first request not admitted")
	}
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			_, ok, status, result := c.beginReset(owner, "another-request-12345678")
			if ok {
				admitted.Add(1)
			}
			if status != 409 || result.Error != "bps_reset_busy" {
				t.Error("concurrent redemption not blocked")
			}
		})
	}
	wg.Wait()
	if admitted.Load() != 0 {
		t.Fatal("multiple redemptions entered")
	}
	c.finishReset(first, 200, bpsResetReply{Submitted: true, RequestID: "fixture-request-12345678"})
	_, ok, status, result := c.beginReset(owner, "fixture-request-12345678")
	if ok || status != 200 || !result.Submitted {
		t.Fatal("duplicate successful submission would repost")
	}
	other := sha256.Sum256([]byte("different-account"))
	_, ok, _, _ = c.beginReset(other, "fixture-request-12345678")
	if !ok {
		t.Fatal("request identity crossed account boundaries")
	}
}

func TestBPSResetUnknownOutcomeBlocksAnotherRequestID(t *testing.T) {
	c := newBPSQuotaCache()
	owner := sha256.Sum256([]byte("selected-account"))
	a, _, _, _ := c.beginReset(owner, "fixture-request-12345678")
	c.finishReset(a, 502, bpsResetReply{Error: "bps_reset_outcome_unknown"})
	for _, id := range []string{"fixture-request-12345678", "another-request-12345678"} {
		_, ok, _, result := c.beginReset(owner, id)
		if ok || result.Error != "bps_reset_outcome_unknown" {
			t.Fatal("ambiguous consume would automatically replay")
		}
	}
}

func TestBPSResetEligibilityCannotInferApplicabilityOrUseStaleQuota(t *testing.T) {
	three, zero, one := 3, 0, 1
	base := upstream.BPSQuotaResult{SupportsReset: true, ResetCardsAvailable: &three, ApplicableResetCardsAvailable: &zero, Source: "active_usage", ObservedAt: time.Now().UTC().Format(time.RFC3339)}
	if got := bpsResetEligibilityError(base); got != "bps_reset_not_applicable" {
		t.Fatal("owning credits allowed inapplicable redemption")
	}
	base.ApplicableResetCardsAvailable = nil
	if got := bpsResetEligibilityError(base); got != "bps_reset_eligibility_unknown" {
		t.Fatal("missing applicability became eligible")
	}
	base.ApplicableResetCardsAvailable = &one
	if got := bpsResetEligibilityError(base); got != "" {
		t.Fatal("fresh applicable credit was denied", got)
	}
	base.Stale = true
	if got := bpsResetEligibilityError(base); got != "bps_reset_stale_quota" {
		t.Fatal("stale snapshot allowed redemption")
	}
	base.Stale = false
	base.ObservedAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if got := bpsResetEligibilityError(base); got != "bps_reset_eligibility_unknown" {
		t.Fatal("old snapshot allowed redemption")
	}
}

func TestBPSResetRequiresConfirmationBeforeCredentialLookup(t *testing.T) {
	r, _, _ := quotaFixture(t)
	for _, body := range []string{`{}`, `{"confirm":false,"redeem_request_id":"fixture-request-12345678"}`, `{"confirm":true,"redeem_request_id":"short"}`, `{"confirm":true,"redeem_request_id":"fixture-request-12345678","account_id":"other"}`, `{"confirm":true,"redeem_request_id":"fixture-request-12345678"} {}`} {
		reply, err := r.manageBPSReset(managementRequest{Method: http.MethodPost, Query: url.Values{"auth_index": {"fixture-index"}}, Body: []byte(body)})
		if err != nil || reply.(map[string]any)["StatusCode"] != http.StatusBadRequest {
			t.Fatal("unconfirmed or ambiguous redemption accepted")
		}
	}
}

func TestBPSQuotaCacheRetainsMarkedStaleObservationAndInvalidatesAfterReset(t *testing.T) {
	c := newBPSQuotaCache()
	key := sha256.Sum256([]byte("fixture-token"))
	three, zero := 3, 0
	old := upstream.BPSQuotaResult{Quota: []byte(`{"subscription":{"plan":"team"},"groups":[{"buckets":[{"remainingFraction":0.5}]}]}`), Source: "active_usage", ObservedAt: time.Now().UTC().Format(time.RFC3339), ResetCardsAvailable: &three, ApplicableResetCardsAvailable: &zero, SupportsReset: true}
	if _, err := c.load(context.Background(), key, true, func() (upstream.BPSQuotaResult, error) { return old, nil }); err != nil {
		t.Fatal(err)
	}
	stale, err := c.load(context.Background(), key, true, func() (upstream.BPSQuotaResult, error) {
		return upstream.BPSQuotaResult{}, upstream.ErrQuotaUnavailable
	})
	if err != nil || !stale.Stale || stale.Source != "cached_usage" || stale.ResetEligible || stale.ObservedAt != old.ObservedAt || stale.ResetCardsAvailable == nil || *stale.ResetCardsAvailable != 3 {
		t.Fatalf("stale observation not retained honestly: %+v %v", stale, err)
	}
	key2 := sha256.Sum256([]byte("rotated-token"))
	result, err := c.load(context.Background(), key2, true, func() (upstream.BPSQuotaResult, error) {
		return upstream.BPSQuotaResult{}, upstream.ErrQuotaUnavailable
	})
	if !errors.Is(err, upstream.ErrQuotaUnavailable) || result.Stale {
		t.Fatal("old credential observation crossed token revision")
	}
	c.invalidate(key)
	result, err = c.load(context.Background(), key, true, func() (upstream.BPSQuotaResult, error) {
		return upstream.BPSQuotaResult{}, upstream.ErrQuotaUnavailable
	})
	if !errors.Is(err, upstream.ErrQuotaUnavailable) || result.Stale {
		t.Fatal("pre-redemption stale snapshot survived invalidation")
	}
}
