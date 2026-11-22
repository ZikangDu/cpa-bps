package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"sync"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

const quotaUnknownRecheck = 5 * time.Minute

// quotaGate is account-scoped. The host also persists its returned cooldown;
// this local gate protects direct quality checks and requests already selected
// before the host learned that the account was exhausted.
type quotaGate struct {
	mu      sync.Mutex
	entries map[[32]byte]*quotaGateEntry
	now     func() time.Time
}

type quotaGateEntry struct {
	pending       chan struct{}
	validUntil    time.Time
	rejectionRead time.Time
	blocked       bool
	confirmed     bool
	until         time.Time
	resetAt       time.Time
	zeroWindows   map[string]bool
}

type quotaGateSnapshot struct {
	fresh    bool
	complete bool
	windows  map[string]float64
	zeros    map[string]bool
	until    time.Time
	resetAt  time.Time
}

func newQuotaGate() *quotaGate {
	return &quotaGate{entries: make(map[[32]byte]*quotaGateEntry), now: time.Now}
}

func quotaGateKey(cfg config, authID string, raw []byte, token string) [32]byte {
	accountID, _ := upstream.BPSProfile(token)
	auth, valid := credentialFromStorage(raw)
	if accountID == "" && valid && auth.Grant != nil {
		accountID, _ = upstream.BPSProfile(auth.Grant.IDToken)
	}
	if accountID == "" && (cfg.OAuthPreset != "bps" || !valid || auth.Grant == nil) {
		// A generic API key has no stable OAuth account identity. Replacing it
		// must not inherit another account's cached observation.
		digest := sha256.Sum256([]byte(token))
		accountID = hex.EncodeToString(digest[:])
	}
	data, _ := json.Marshal([]string{authID, cfg.ResponsesURL, cfg.QuotaURL, cfg.OAuthPreset, accountID})
	return sha256.Sum256(data)
}

func quotaGateEnabled(cfg config) bool { return cfg.OAuthPreset == "bps" || cfg.QuotaURL != "" }

func (r *Runtime) beforeQuotaDispatch(ctx context.Context, cfg config, authID string, raw []byte, token string) *rpcError {
	if !quotaGateEnabled(cfg) {
		return nil
	}
	return r.quotaGate.check(ctx, quotaGateKey(cfg, authID, raw, token), false, false,
		func() upstream.BPSQuotaResult { return r.fetchDispatchQuota(ctx, cfg, authID, raw, token) })
}

// A received 429 remains evidence even if its caller disconnects. Only the
// bounded metadata refresh survives that cancellation; no generation does.
func (r *Runtime) afterGenerationRejection(ctx context.Context, cfg config, authID string, raw []byte, token string, diagnostic upstream.Rejection, release func()) *rpcError {
	if diagnostic.HTTPStatus == http.StatusTooManyRequests {
		observation, cancel := context.WithTimeout(r.ctx, bpsQuotaTimeout)
		defer cancel()
		ctx = observation
	}
	return r.afterQuotaRejectionWithRelease(ctx, cfg, authID, raw, token, diagnostic, release)
}

func (r *Runtime) afterQuotaRejection(ctx context.Context, cfg config, authID string, raw []byte, token string, diagnostic upstream.Rejection) *rpcError {
	return r.afterQuotaRejectionWithRelease(ctx, cfg, authID, raw, token, diagnostic, nil)
}

func (r *Runtime) afterQuotaRejectionWithRelease(ctx context.Context, cfg config, authID string, raw []byte, token string, diagnostic upstream.Rejection, release func()) *rpcError {
	if release != nil {
		release = sync.OnceFunc(release)
		defer release()
	}
	rejected := failure(diagnostic.HTTPStatus, diagnostic.Code, diagnostic.Message)
	rejected.RetryAfter = diagnostic.RetryAfterSeconds
	// A permission/authorization refusal must never become a quota cooldown.
	if diagnostic.HTTPStatus != http.StatusTooManyRequests || !quotaGateEnabled(cfg) {
		return rejected
	}
	if blocked := r.quotaGate.checkWithPending(ctx, quotaGateKey(cfg, authID, raw, token), true,
		diagnostic.Code == "bps_quota_exhausted", func() upstream.BPSQuotaResult {
			return r.fetchDispatchQuota(ctx, cfg, authID, raw, token)
		}, release); blocked != nil {
		return blocked
	}
	return rejected
}

func (r *Runtime) fetchDispatchQuota(ctx context.Context, cfg config, authID string, raw []byte, token string) upstream.BPSQuotaResult {
	readCtx, cancel := context.WithTimeout(ctx, bpsQuotaTimeout)
	defer cancel()
	if cfg.OAuthPreset == "bps" {
		// The gate already owns TTL and singleflight. A UI cache read may have
		// started before the 429/reset, even when load(force=true) joins it;
		// admission therefore makes its own causally fresh metadata request.
		accountID, _ := upstream.BPSProfile(token)
		if accountID == "" {
			if auth, valid := credentialFromStorage(raw); valid && auth.Grant != nil {
				accountID, _ = upstream.BPSProfile(auth.Grant.IDToken)
			}
		}
		result, err := r.client.BPSQuota(readCtx, token, accountID)
		if err != nil {
			return upstream.BPSQuotaResult{}
		}
		r.rememberBPSPlan(authID, token, result)
		return result
	}
	quota, err := r.client.Quota(readCtx, cfg.QuotaURL, token)
	if err != nil {
		return upstream.BPSQuotaResult{}
	}
	return upstream.BPSQuotaResult{Quota: quota, Source: "active_usage", ObservedAt: r.quotaGate.now().UTC().Format(time.RFC3339Nano)}
}

// forgetQuotaGate is only for a known successful reset. It invalidates the old
// observation, but a previously exhausted account must verify that every
// blocking window recovered (a credit may reset only one window).
func (r *Runtime) forgetQuotaGate(cfg config, authID string, raw []byte, token string) {
	key := quotaGateKey(cfg, authID, raw, token)
	r.quotaGate.mu.Lock()
	previous := r.quotaGate.entries[key]
	if previous != nil && previous.blocked {
		r.quotaGate.entries[key] = &quotaGateEntry{blocked: true, confirmed: previous.confirmed,
			until: r.quotaGate.now(), zeroWindows: previous.zeroWindows}
	} else {
		delete(r.quotaGate.entries, key)
	}
	r.quotaGate.mu.Unlock()
}

func (g *quotaGate) check(ctx context.Context, key [32]byte, rejected, explicitQuota bool, fetch func() upstream.BPSQuotaResult) *rpcError {
	return g.checkWithPending(ctx, key, rejected, explicitQuota, fetch, nil)
}

// onPending runs outside the lock after this account has an in-flight metadata
// observation. Completed generations can release capacity without allowing a
// newer request through a cached positive observation. Joiners release too.
func (g *quotaGate) checkWithPending(ctx context.Context, key [32]byte, rejected, explicitQuota bool, fetch func() upstream.BPSQuotaResult, onPending func()) *rpcError {
	force := rejected
	for {
		g.mu.Lock()
		now := g.now()
		entry := g.entries[key]
		if entry == nil {
			for oldKey, old := range g.entries {
				if old.pending == nil && !old.blocked && !now.Before(old.validUntil) {
					delete(g.entries, oldKey)
				}
			}
			if len(g.entries) >= 1024 {
				g.mu.Unlock()
				return failure(http.StatusServiceUnavailable, "bps_quota_gate_unavailable", "BPS quota admission is temporarily unavailable.")
			}
			entry = &quotaGateEntry{}
			g.entries[key] = entry
		}
		if entry.blocked && now.Before(entry.until) {
			err := entry.blockError(now)
			g.mu.Unlock()
			return err
		}
		if entry.pending != nil {
			pending := entry.pending
			g.mu.Unlock()
			if onPending != nil {
				onPending()
			}
			select {
			case <-pending:
				// A read begun before the rejected request is not necessarily a
				// post-rejection observation. Only the rejection throttle below
				// can prove that another caller already performed that refresh.
				force = rejected
				continue
			case <-ctx.Done():
				return failure(499, "bps_quota_check_canceled", "BPS quota inspection was canceled.")
			}
		}
		if !entry.blocked && !explicitQuota && now.Before(entry.validUntil) && (!force || now.Before(entry.rejectionRead)) {
			g.mu.Unlock()
			return nil
		}
		pending := make(chan struct{})
		entry.pending = pending
		wasBlocked := entry.blocked
		previousZeros := entry.zeroWindows
		g.mu.Unlock()

		if onPending != nil {
			onPending()
		}
		result := fetch()
		g.mu.Lock()
		now = g.now()
		snapshot := inspectQuotaGate(result, now)
		// A successful reset may have invalidated this in-flight generation.
		if g.entries[key] != entry {
			close(pending)
			g.mu.Unlock()
			force = false
			continue
		}
		entry.pending = nil
		if ctx.Err() != nil {
			close(pending)
			g.mu.Unlock()
			return failure(499, "bps_quota_check_canceled", "BPS quota inspection was canceled.")
		}
		if rejected {
			entry.rejectionRead = now.Add(5 * time.Second)
		}
		if snapshot.fresh {
			snapshot.retainUnrecoveredWindows(previousZeros, now)
		}
		switch {
		case snapshot.fresh && len(snapshot.zeros) > 0:
			entry.blocked, entry.confirmed = true, true
			entry.until, entry.resetAt, entry.zeroWindows = snapshot.until, snapshot.resetAt, snapshot.zeros
		case snapshot.fresh && snapshot.complete && quotaCoversRecovery(snapshot.windows, previousZeros) && !explicitQuota:
			entry.blocked, entry.confirmed = false, false
			entry.until, entry.resetAt, entry.zeroWindows = time.Time{}, time.Time{}, nil
			entry.validUntil = now.Add(quotaCacheWindow)
		case wasBlocked || rejected:
			// Missing/partial/stale quota is not evidence that an exhausted
			// account recovered. Recheck metadata later without sending work.
			entry.blocked = true
			entry.until = now.Add(quotaUnknownRecheck)
			entry.resetAt = time.Time{}
			if !wasBlocked {
				entry.confirmed = explicitQuota
			}
		default:
			// A healthy account with an unavailable optional metadata endpoint
			// may still execute. A subsequent 429 activates conservative gating.
			entry.validUntil = now.Add(5 * time.Second)
		}
		close(pending)
		var problem *rpcError
		if entry.blocked {
			problem = entry.blockError(now)
		}
		g.mu.Unlock()
		return problem
	}
}

// An omitted previously exhausted window is still unresolved. Only a fresh,
// explicit positive balance can remove it, even if another window is still
// zero in this snapshot. Otherwise alternating partial replies could erase
// the weekly limit and incorrectly reopen the account.
func (snapshot *quotaGateSnapshot) retainUnrecoveredWindows(previous map[string]bool, now time.Time) {
	for name := range previous {
		if remaining, present := snapshot.windows[name]; present && remaining > 0 {
			continue
		}
		if !snapshot.zeros[name] {
			snapshot.zeros[name] = true
			snapshot.resetAt = time.Time{}
			if nextCheck := now.Add(quotaUnknownRecheck); nextCheck.After(snapshot.until) {
				snapshot.until = nextCheck
			}
		}
	}
}

func quotaCoversRecovery(windows map[string]float64, required map[string]bool) bool {
	for name := range required {
		if remaining, exists := windows[name]; !exists || remaining <= 0 {
			return false
		}
	}
	return len(windows) > 0
}

func (entry *quotaGateEntry) blockError(now time.Time) *rpcError {
	wait := max(1, int(math.Ceil(entry.until.Sub(now).Seconds())))
	code := "bps_quota_status_unknown"
	message := "The upstream rejected requests and account quota recovery is not confirmed. This account is paused until the next quota check."
	if entry.confirmed {
		code = "bps_quota_exhausted"
		message = "This account has exhausted its quota. Other available accounts may be used; otherwise wait for quota recovery."
	}
	resetAt := ""
	if !entry.resetAt.IsZero() {
		resetAt = entry.resetAt.UTC().Format(time.RFC3339)
		message += fmt.Sprintf(" Quota resets at %s.", resetAt)
	}
	return &rpcError{ErrorCode: "credential_quota", PublicCode: code, HTTPStatus: http.StatusTooManyRequests,
		RetryAfter: wait, CredentialScoped: true, ResetAt: resetAt, Message: message}
}

func inspectQuotaGate(result upstream.BPSQuotaResult, now time.Time) quotaGateSnapshot {
	snapshot := quotaGateSnapshot{windows: make(map[string]float64), zeros: make(map[string]bool)}
	observed, err := time.Parse(time.RFC3339Nano, result.ObservedAt)
	if result.Source != "active_usage" || result.Stale || err != nil || now.Sub(observed) > bpsQuotaStaleWindow || observed.After(now.Add(time.Second)) {
		return snapshot
	}
	var quota struct {
		Groups []struct {
			Buckets []struct {
				Window    string   `json:"window"`
				Remaining *float64 `json:"remainingFraction"`
				ResetTime string   `json:"resetTime"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(result.Quota, &quota) != nil {
		return snapshot
	}
	snapshot.fresh, snapshot.complete = true, true
	allResetsKnown := true
	for groupIndex, group := range quota.Groups {
		for index, bucket := range group.Buckets {
			name := fmt.Sprintf("%d:%s", groupIndex, bucket.Window)
			if bucket.Window == "" {
				name = fmt.Sprintf("%d:#%d", groupIndex, index)
			}
			if _, duplicate := snapshot.windows[name]; duplicate {
				snapshot.complete = false
			}
			if bucket.Remaining == nil || *bucket.Remaining < 0 || *bucket.Remaining > 1 || math.IsNaN(*bucket.Remaining) {
				snapshot.complete = false
				continue
			}
			snapshot.windows[name] = *bucket.Remaining
			if *bucket.Remaining != 0 {
				continue
			}
			snapshot.zeros[name] = true
			resetAt, parseErr := time.Parse(time.RFC3339Nano, bucket.ResetTime)
			if parseErr != nil || !resetAt.After(now) || resetAt.After(now.Add(366*24*time.Hour)) {
				allResetsKnown = false
				resetAt = now.Add(quotaUnknownRecheck)
			}
			if resetAt.After(snapshot.until) {
				snapshot.until = resetAt
			}
		}
	}
	if len(snapshot.windows) == 0 {
		snapshot.complete = false
	}
	if allResetsKnown && len(snapshot.zeros) > 0 {
		snapshot.resetAt = snapshot.until
	}
	return snapshot
}
