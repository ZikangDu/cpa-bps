package adapter

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

func gateObservation(now time.Time, buckets string) upstream.BPSQuotaResult {
	return upstream.BPSQuotaResult{Quota: json.RawMessage(`{"groups":[{"buckets":[` + buckets + `]}]}`),
		Source: "active_usage", ObservedAt: now.Format(time.RFC3339Nano)}
}

func gateBucket(window string, remaining float64, reset time.Time) string {
	return fmt.Sprintf(`{"window":%q,"remainingFraction":%v,"resetTime":%q}`, window, remaining, reset.Format(time.RFC3339Nano))
}

func TestQuotaGateSnapshotUsesActualZeroAndLatestRequiredReset(t *testing.T) {
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, test := range []struct {
		name string
		view upstream.BPSQuotaResult
		zero int
		good bool
	}{
		{"zero", gateObservation(now, gateBucket("5h", 0, now.Add(time.Hour))), 1, true},
		{"positive", gateObservation(now, gateBucket("5h", .5, now.Add(time.Hour))), 0, true},
		{"missing", gateObservation(now, `{"window":"5h"}`), 0, false},
		{"negative", gateObservation(now, `{"remainingFraction":-1}`), 0, false},
		{"above-one", gateObservation(now, `{"remainingFraction":1.2}`), 0, false},
		{"empty", gateObservation(now, ""), 0, false},
		{"old-observation", gateObservation(now.Add(-10*time.Minute), gateBucket("5h", 0, now.Add(time.Hour))), 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := inspectQuotaGate(test.view, now)
			if len(got.zeros) != test.zero || got.complete != test.good {
				t.Fatalf("unexpected evidence: %+v", got)
			}
		})
	}
	zero := gateObservation(now, gateBucket("5h", 0, now.Add(time.Hour))+","+gateBucket("7d", 0, now.Add(24*time.Hour)))
	got := inspectQuotaGate(zero, now)
	if len(got.zeros) != 2 || !got.until.Equal(now.Add(24*time.Hour)) || !got.resetAt.Equal(got.until) {
		t.Fatalf("recovery did not wait for both windows: %+v", got)
	}
	zero.Stale = true
	if inspectQuotaGate(zero, now).fresh {
		t.Fatal("stale observation became fresh exhaustion evidence")
	}
	zero.Stale, zero.Source = false, "token_claims"
	if inspectQuotaGate(zero, now).fresh {
		t.Fatal("token plan became fresh exhaustion evidence")
	}
	unknown := inspectQuotaGate(gateObservation(now, `{"window":"7d","remainingFraction":0}`), now)
	if !unknown.until.Equal(now.Add(quotaUnknownRecheck)) || !unknown.resetAt.IsZero() {
		t.Fatal("unknown reset invented a quota recovery time")
	}
}

func TestQuotaGateStopsRequestsUntilVerifiedRecovery(t *testing.T) {
	g := newQuotaGate()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g.now = func() time.Time { return now }
	key := [32]byte{1}
	reads := 0
	view := gateObservation(now, gateBucket("7d", 0, now.Add(time.Hour)))
	fetch := func() upstream.BPSQuotaResult { reads++; return view }
	problem := g.check(context.Background(), key, false, false, fetch)
	if problem == nil || problem.PublicCode != "bps_quota_exhausted" || problem.ErrorCode != "credential_quota" || !problem.CredentialScoped || problem.RetryAfter != 3600 || problem.ResetAt == "" {
		t.Fatalf("not an account-wide timed block: %+v", problem)
	}
	for range 32 {
		if g.check(context.Background(), key, false, false, fetch) == nil {
			t.Fatal("known exhausted account was dispatched")
		}
	}
	if reads != 1 {
		t.Fatalf("blocked account repeatedly queried quota: %d", reads)
	}
	now = now.Add(time.Hour)
	// A partial report that omits the exhausted weekly window cannot reopen it.
	view = gateObservation(now, gateBucket("5h", 1, now.Add(time.Hour)))
	if g.check(context.Background(), key, false, false, fetch) == nil || reads != 2 {
		t.Fatal("partial quota cleared account-wide exhaustion")
	}
	now = now.Add(quotaUnknownRecheck)
	view = gateObservation(now, gateBucket("7d", 1, now.Add(7*24*time.Hour)))
	if problem := g.check(context.Background(), key, false, false, fetch); problem != nil || reads != 3 {
		t.Fatalf("verified recovery not admitted: %+v reads=%d", problem, reads)
	}
	if problem := g.check(context.Background(), key, false, false, fetch); problem != nil || reads != 3 {
		t.Fatal("positive TTL not shared")
	}
}

func TestQuotaGateCoalescesConcurrentProbesAndPost429Refresh(t *testing.T) {
	g := newQuotaGate()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g.now = func() time.Time { return now }
	key := [32]byte{2}
	var reads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	fetch := func() upstream.BPSQuotaResult {
		if reads.Add(1) == 1 {
			close(started)
		}
		<-release
		return gateObservation(now, gateBucket("7d", 1, now.Add(time.Hour)))
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			if err := g.check(context.Background(), key, false, false, fetch); err != nil {
				t.Errorf("unexpected block: %v", err)
			}
		})
	}
	<-started
	close(release)
	wg.Wait()
	if reads.Load() != 1 {
		t.Fatalf("initial reads not coalesced: %d", reads.Load())
	}
	zero := func() upstream.BPSQuotaResult {
		reads.Add(1)
		return gateObservation(now, gateBucket("7d", 0, now.Add(time.Hour)))
	}
	for range 32 {
		wg.Go(func() {
			if err := g.check(context.Background(), key, true, false, zero); err == nil || err.PublicCode != "bps_quota_exhausted" {
				t.Error("429 did not refresh positive cache and gate account")
			}
		})
	}
	wg.Wait()
	if reads.Load() != 2 {
		t.Fatalf("429 refresh storm: %d", reads.Load())
	}
}

func TestQuotaGateUnknown429IsBoundedButNotLabeledExhausted(t *testing.T) {
	g := newQuotaGate()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g.now = func() time.Time { return now }
	key := [32]byte{3}
	reads := 0
	unknown := func() upstream.BPSQuotaResult { reads++; return upstream.BPSQuotaResult{} }
	if problem := g.check(context.Background(), key, false, false, unknown); problem != nil {
		t.Fatal("optional metadata failure prevented first execution")
	}
	problem := g.check(context.Background(), key, true, false, unknown)
	if problem == nil || problem.PublicCode != "bps_quota_status_unknown" || problem.RetryAfter != 300 || problem.ResetAt != "" || !problem.CredentialScoped {
		t.Fatalf("incorrect unconfirmed recovery: %+v", problem)
	}
	for range 8 {
		_ = g.check(context.Background(), key, false, false, unknown)
	}
	if reads != 2 {
		t.Fatalf("unknown blocked account kept probing: %d", reads)
	}
	now = now.Add(quotaUnknownRecheck)
	if problem := g.check(context.Background(), key, false, false, unknown); problem == nil || reads != 3 {
		t.Fatal("unknown recovery admitted generation instead of bounded metadata probe")
	}
}

func TestQuotaGateAccountIdentitySurvivesTokenRefresh(t *testing.T) {
	r, auth, raw := quotaFixture(t)
	first := quotaGateKey(r.cfg, "same-auth", raw, auth.Grant.AccessToken)
	rotated := auth.Grant.AccessToken + "-rotated-signature"
	if first != quotaGateKey(r.cfg, "same-auth", raw, rotated) {
		t.Fatal("OAuth rotation lost account gate")
	}
	if first == quotaGateKey(r.cfg, "other-auth", raw, rotated) {
		t.Fatal("different credential inherited account gate")
	}
	claims := `{"https://api.openai.com/auth":{"chatgpt_account_id":"other-account"}}`
	otherToken := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".fixture"
	if first == quotaGateKey(r.cfg, "same-auth", raw, otherToken) {
		t.Fatal("replacement account inherited old gate")
	}
}

func TestQuotaGateBPSReadsAndErrorClassification(t *testing.T) {
	r, auth, raw := quotaFixture(t)
	var usageReads, creditReads atomic.Int32
	var exhausted atomic.Bool
	r.client = upstream.NewWithTransport(resetMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "chatgpt.com" || req.Method != http.MethodGet {
			t.Fatal("unexpected external request")
		}
		body := `{"available_count":0}`
		if strings.HasSuffix(req.URL.Path, "/usage") {
			usageReads.Add(1)
			percent := 25
			if exhausted.Load() {
				percent = 100
			}
			body = fmt.Sprintf(`{"plan_type":"team","rate_limit":{"secondary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}}}`, percent, time.Now().Add(time.Hour).Unix())
		} else {
			creditReads.Add(1)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	ctx := context.Background()
	if err := r.beforeQuotaDispatch(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken); err != nil {
		t.Fatal(err)
	}
	denied := upstream.Rejection{HTTPStatus: 403, Code: "bps_usage_policy_blocked", Message: "policy"}
	if err := r.afterQuotaRejection(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken, denied); err.HTTPStatus != 403 || err.CredentialScoped || usageReads.Load() != 1 {
		t.Fatal("403 was reclassified as quota")
	}
	rate := upstream.Rejection{HTTPStatus: 429, Code: "bps_rate_limited", Message: "rate", RetryAfterSeconds: 7}
	if err := r.afterQuotaRejection(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken, rate); err.RetryAfter != 7 || err.CredentialScoped || err.Code() != "bps_rate_limited" {
		t.Fatalf("transient rate limit changed classification: %+v", err)
	}
	// Expire only the rejection-probe throttle; no real clock waiting is needed.
	r.quotaGate.mu.Lock()
	r.quotaGate.entries[quotaGateKey(r.cfg, "fixture-id", raw, auth.Grant.AccessToken)].rejectionRead = time.Time{}
	r.quotaGate.mu.Unlock()
	exhausted.Store(true)
	err := r.afterQuotaRejection(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken, rate)
	if err == nil || err.PublicCode != "bps_quota_exhausted" || !err.CredentialScoped {
		t.Fatalf("zero weekly quota not classified: %+v", err)
	}
	if usageReads.Load() != 3 || creditReads.Load() != 3 {
		t.Fatal("BPS quota path or forced post429 refresh not used")
	}
	for range 8 {
		if r.beforeQuotaDispatch(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken) == nil {
			t.Fatal("known quota bypassed")
		}
	}
	if usageReads.Load() != 3 {
		t.Fatal("blocked BPS account kept querying metadata")
	}
	r.forgetQuotaGate(r.cfg, "fixture-id", raw, auth.Grant.AccessToken)
	exhausted.Store(false)
	if err := r.beforeQuotaDispatch(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken); err != nil {
		t.Fatalf("confirmed reset did not reopen quota check: %+v", err)
	}
}

func TestQuotaGateResetInvalidationRetainsUnverifiedExhaustion(t *testing.T) {
	r := New(nil)
	t.Cleanup(r.Close)
	cfg := config{ResponsesURL: "https://fixture.test/responses", QuotaURL: "https://fixture.test/quota"}
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	r.quotaGate.now = func() time.Time { return now }
	key := quotaGateKey(cfg, "auth", nil, "token")
	zero := func() upstream.BPSQuotaResult {
		return gateObservation(now, gateBucket("5h", 0, now.Add(time.Hour))+","+gateBucket("7d", 0, now.Add(24*time.Hour)))
	}
	if r.quotaGate.check(context.Background(), key, false, false, zero) == nil {
		t.Fatal("initial quota not gated")
	}
	r.forgetQuotaGate(cfg, "auth", nil, "token")
	if r.quotaGate.check(context.Background(), key, false, false, func() upstream.BPSQuotaResult { return upstream.BPSQuotaResult{} }) == nil {
		t.Fatal("successful credit with failed verification reopened exhausted account")
	}
	now = now.Add(quotaUnknownRecheck)
	partial := func() upstream.BPSQuotaResult { return gateObservation(now, gateBucket("5h", 1, now.Add(time.Hour))) }
	if r.quotaGate.check(context.Background(), key, false, false, partial) == nil {
		t.Fatal("partial reset omitted still-exhausted week")
	}
	now = now.Add(quotaUnknownRecheck)
	both := func() upstream.BPSQuotaResult {
		return gateObservation(now, gateBucket("5h", 1, now.Add(time.Hour))+","+gateBucket("7d", 1, now.Add(24*time.Hour)))
	}
	if problem := r.quotaGate.check(context.Background(), key, false, false, both); problem != nil {
		t.Fatalf("verified recovery stayed blocked: %+v", problem)
	}
}

func TestQuotaGatePost429DoesNotReusePreflightInFlight(t *testing.T) {
	g := newQuotaGate()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g.now = func() time.Time { return now }
	key := [32]byte{8}
	started, release := make(chan struct{}), make(chan struct{})
	preflightDone := make(chan *rpcError, 1)
	go func() {
		preflightDone <- g.check(context.Background(), key, false, false, func() upstream.BPSQuotaResult {
			close(started)
			<-release
			return gateObservation(now, gateBucket("7d", 1, now.Add(time.Hour)))
		})
	}()
	<-started
	var forced atomic.Int32
	postDone := make(chan *rpcError, 1)
	go func() {
		postDone <- g.check(context.Background(), key, true, false, func() upstream.BPSQuotaResult {
			forced.Add(1)
			return gateObservation(now, gateBucket("7d", 0, now.Add(time.Hour)))
		})
	}()
	close(release)
	if problem := <-preflightDone; problem != nil {
		t.Fatalf("preflight failed: %+v", problem)
	}
	if problem := <-postDone; problem == nil || problem.PublicCode != "bps_quota_exhausted" || forced.Load() != 1 {
		t.Fatalf("pre-429 observation was reused: %+v probes=%d", problem, forced.Load())
	}
}

func TestQuotaGateCanceledProbeDoesNotInventExhaustion(t *testing.T) {
	g := newQuotaGate()
	ctx, cancel := context.WithCancel(context.Background())
	problem := g.check(ctx, [32]byte{9}, true, false, func() upstream.BPSQuotaResult { cancel(); return upstream.BPSQuotaResult{} })
	if problem == nil || problem.HTTPStatus != 499 || problem.CredentialScoped {
		t.Fatalf("cancellation became quota failure: %+v", problem)
	}
}

func TestQuotaGateAlternatingPartialWindowsCannotForgetExhaustion(t *testing.T) {
	g := newQuotaGate()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g.now = func() time.Time { return now }
	key := [32]byte{11}
	read := func(buckets string) *rpcError {
		return g.check(context.Background(), key, false, false, func() upstream.BPSQuotaResult {
			return gateObservation(now, buckets)
		})
	}
	if read(gateBucket("5h", 0, now.Add(time.Minute))+","+gateBucket("7d", 0, now.Add(time.Minute))) == nil {
		t.Fatal("both windows should block")
	}
	now = now.Add(time.Minute)
	problem := read(gateBucket("5h", 0, now.Add(time.Minute)))
	if problem == nil || problem.ResetAt != "" || len(g.entries[key].zeroWindows) != 2 {
		t.Fatalf("partial zero reply forgot weekly exhaustion: %+v", problem)
	}
	now = now.Add(quotaUnknownRecheck)
	problem = read(gateBucket("5h", 1, now.Add(time.Hour)))
	if problem == nil || problem.ResetAt != "" || len(g.entries[key].zeroWindows) != 1 || !g.entries[key].zeroWindows["0:7d"] {
		t.Fatalf("short-window recovery reopened missing weekly window: %+v", problem)
	}
	now = now.Add(quotaUnknownRecheck)
	// The short window has already been explicitly recovered; only the
	// remaining weekly obligation requires a fresh positive confirmation.
	if problem := read(gateBucket("7d", 1, now.Add(7*24*time.Hour))); problem != nil {
		t.Fatalf("all previously blocking windows were confirmed positive: %+v", problem)
	}
}

func TestQuotaGatePost429DoesNotJoinOlderUIQuotaRead(t *testing.T) {
	r, auth, raw := quotaFixture(t)
	var reads atomic.Int32
	uiStarted, releaseUI := make(chan struct{}), make(chan struct{})
	var exhausted atomic.Bool
	r.client = upstream.NewWithTransport(resetMemoryTransport(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host != "chatgpt.com" || req.Method != http.MethodGet {
			return nil, fmt.Errorf("unexpected fixture request")
		}
		body := `{"available_count":0}`
		if strings.HasSuffix(req.URL.Path, "/usage") {
			sequence := reads.Add(1)
			percent := 25
			if exhausted.Load() {
				percent = 100
			}
			// Snapshot the old positive answer before allowing the rejection.
			body = fmt.Sprintf(`{"rate_limit":{"secondary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}}}`, percent, time.Now().Add(time.Hour).Unix())
			if sequence == 2 {
				close(uiStarted)
				select {
				case <-releaseUI:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
			}
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if problem := r.beforeQuotaDispatch(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken); problem != nil {
		t.Fatal(problem)
	}
	uiDone := make(chan upstream.BPSQuotaResult, 1)
	go func() {
		view, _ := r.loadBPSQuota(ctx, r.cfg, "fixture-id", raw, true)
		uiDone <- view
	}()
	<-uiStarted
	exhausted.Store(true)
	postDone := make(chan *rpcError, 1)
	go func() {
		postDone <- r.afterQuotaRejection(ctx, r.cfg, "fixture-id", raw, auth.Grant.AccessToken,
			upstream.Rejection{HTTPStatus: 429, Code: "bps_rate_limited", RetryAfterSeconds: 7})
	}()
	select {
	case problem := <-postDone:
		close(releaseUI)
		<-uiDone
		if problem == nil || problem.PublicCode != "bps_quota_exhausted" || reads.Load() != 3 {
			t.Fatalf("old UI balance was used instead of post429 metadata: %+v reads=%d", problem, reads.Load())
		}
	case <-ctx.Done():
		close(releaseUI)
		<-uiDone
		<-postDone
		t.Fatal("post429 admission incorrectly waited for the older UI cache read")
	}
	// Publishing the delayed positive UI result cannot reopen the gate.
	if problem := r.beforeQuotaDispatch(context.Background(), r.cfg, "fixture-id", raw, auth.Grant.AccessToken); problem == nil {
		t.Fatal("older UI reply cleared current account exhaustion")
	}
}

func TestQuotaPendingReleasesEveryRejectedSlotBeforeSlowObservation(t *testing.T) {
	g := newQuotaGate()
	now := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	g.now = func() time.Time { return now }
	key := [32]byte{17}
	if err := g.check(t.Context(), key, false, false, func() upstream.BPSQuotaResult {
		return gateObservation(now, gateBucket("7d", 1, now.Add(time.Hour)))
	}); err != nil {
		t.Fatal(err)
	}
	ownerPending, joinedPending, nextPending := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseFetch := make(chan struct{})
	release := sync.OnceFunc(func() { close(releaseFetch) })
	t.Cleanup(release)
	results := make(chan *rpcError, 3)
	go func() {
		results <- g.checkWithPending(t.Context(), key, true, false, func() upstream.BPSQuotaResult {
			<-releaseFetch
			return gateObservation(now, gateBucket("7d", 0, now.Add(time.Hour)))
		}, sync.OnceFunc(func() { close(ownerPending) }))
	}()
	select {
	case <-ownerPending:
	case <-time.After(time.Second):
		t.Fatal("owner did not release completed generation slot")
	}
	go func() {
		results <- g.checkWithPending(t.Context(), key, true, false, func() upstream.BPSQuotaResult {
			t.Error("joining rejection performed a duplicate observation")
			return upstream.BPSQuotaResult{}
		}, sync.OnceFunc(func() { close(joinedPending) }))
	}()
	select {
	case <-joinedPending:
	case <-time.After(time.Second):
		t.Fatal("joining rejection retained its completed generation slot")
	}
	go func() {
		results <- g.checkWithPending(t.Context(), key, false, false, func() upstream.BPSQuotaResult {
			t.Error("next dispatch bypassed the pending quota observation")
			return upstream.BPSQuotaResult{}
		}, sync.OnceFunc(func() { close(nextPending) }))
	}()
	select {
	case <-nextPending:
	case <-time.After(time.Second):
		t.Fatal("next dispatch reused the old positive cache instead of waiting")
	}
	select {
	case err := <-results:
		t.Fatalf("request escaped before observation completed: %v", err)
	default:
	}
	release()
	for range 3 {
		select {
		case err := <-results:
			if err == nil || err.PublicCode != "bps_quota_exhausted" {
				t.Fatalf("exhausted quota lost: %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("pending request did not settle")
		}
	}
}
