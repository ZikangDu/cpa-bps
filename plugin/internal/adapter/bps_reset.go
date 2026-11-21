package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

const bpsResetAttemptWindow = time.Hour

type bpsResetReply struct {
	Submitted bool   `json:"submitted"`
	RequestID string `json:"redeem_request_id,omitempty"`
	Error     string `json:"error,omitempty"`
	Warning   string `json:"warning,omitempty"`
}

type bpsResetAttempt struct {
	owner     [32]byte
	expires   time.Time
	done      bool
	uncertain bool
	status    int
	reply     bpsResetReply
}

func bpsResetEligibilityError(quota upstream.BPSQuotaResult) string {
	quota.UpdateResetEligibility()
	if !quota.ResetEligible {
		return quota.ResetUnavailableReason
	}
	observed, err := time.Parse(time.RFC3339, quota.ObservedAt)
	if quota.Source != "active_usage" || err != nil || time.Since(observed) > 30*time.Second || time.Until(observed) > time.Second {
		return "bps_reset_eligibility_unknown"
	}
	return ""
}

// Keep duplicate request IDs and ambiguous outcomes in the same runtime cache.
// Token rotation does not clear this gate for the same credential/account.
func (c *bpsQuotaCache) beginReset(owner [32]byte, requestID string) (*bpsResetAttempt, bool, int, bpsResetReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	keyData, _ := json.Marshal([]any{owner, requestID})
	key := sha256.Sum256(keyData)
	for oldKey, old := range c.resets {
		if old.done && !now.Before(old.expires) {
			delete(c.resets, oldKey)
		}
	}
	if old := c.resets[key]; old != nil {
		if old.done {
			return old, false, old.status, old.reply
		}
		return nil, false, http.StatusConflict, bpsResetReply{Error: "bps_reset_busy"}
	}
	for _, old := range c.resets {
		if old.owner == owner {
			if !old.done {
				return nil, false, http.StatusConflict, bpsResetReply{Error: "bps_reset_busy"}
			}
			if old.uncertain {
				return nil, false, http.StatusConflict, bpsResetReply{Error: "bps_reset_outcome_unknown"}
			}
		}
	}
	if len(c.resets) >= 512 {
		return nil, false, http.StatusServiceUnavailable, bpsResetReply{Error: "bps_reset_busy"}
	}
	next := &bpsResetAttempt{owner: owner, expires: now.Add(bpsResetAttemptWindow)}
	c.resets[key] = next
	return next, true, 0, bpsResetReply{}
}

func (c *bpsQuotaCache) finishReset(attempt *bpsResetAttempt, status int, reply bpsResetReply) {
	c.mu.Lock()
	defer c.mu.Unlock()
	attempt.done, attempt.status, attempt.reply = true, status, reply
	attempt.uncertain = reply.Error == "bps_reset_outcome_unknown"
	attempt.expires = time.Now().Add(bpsResetAttemptWindow)
}

func (r *Runtime) manageBPSReset(req managementRequest) (any, error) {
	respond := func(status int, code string) (any, error) { return bpsQuotaHTTP(status, bpsResetReply{Error: code}) }
	if req.Method != http.MethodPost {
		return respond(http.StatusMethodNotAllowed, "bps_unsupported_method")
	}
	indexes := req.Query["auth_index"]
	if len(indexes) != 1 || strings.TrimSpace(indexes[0]) == "" || len(indexes[0]) > 256 {
		return respond(http.StatusBadRequest, "bps_invalid_auth_index")
	}
	var body struct {
		Confirm   bool   `json:"confirm"`
		RequestID string `json:"redeem_request_id"`
	}
	decoder := json.NewDecoder(bytes.NewReader(req.Body))
	decoder.DisallowUnknownFields()
	if len(req.Body) > 4096 || decoder.Decode(&body) != nil || decoder.Decode(new(any)) != io.EOF ||
		!body.Confirm || !upstream.ValidBPSRedeemRequestID(body.RequestID) {
		return respond(http.StatusBadRequest, "bps_reset_confirmation_required")
	}
	cfg, ok := r.begin()
	if !ok {
		return respond(http.StatusServiceUnavailable, "bps_plugin_unavailable")
	}
	defer r.tasks.Done()
	if cfg.OAuthPreset != "bps" || r.host == nil {
		return respond(http.StatusServiceUnavailable, "bps_reset_unavailable")
	}
	authID, raw, selectedErr := r.selectedBPSQuotaCredential(req, indexes[0])
	if selectedErr != nil {
		return respond(selectedErr.StatusCode(), selectedErr.Code())
	}
	auth, valid := credentialFromStorage(raw)
	if !valid || auth.Grant == nil {
		return respond(http.StatusUnauthorized, "bps_reauthorization_required")
	}
	if auth.Disabled {
		return respond(http.StatusConflict, "bps_credential_disabled")
	}
	token, tokenErr := resolveToken(raw, cfg.ResponsesURL, cfg.OAuth)
	if tokenErr != nil {
		return respond(tokenErr.StatusCode(), tokenErr.Code())
	}
	accountID, _ := upstream.BPSProfile(token)
	if accountID == "" {
		accountID, _ = upstream.BPSProfile(auth.Grant.IDToken)
	}
	if accountID == "" {
		return respond(http.StatusBadRequest, "bps_reset_eligibility_unknown")
	}
	ownerData, _ := json.Marshal([]string{authID, accountID})
	attempt, acquired, status, reply := r.bpsQuota.beginReset(sha256.Sum256(ownerData), body.RequestID)
	if !acquired {
		return bpsQuotaHTTP(status, reply)
	}
	finish := func(status int, reply bpsResetReply) (any, error) {
		r.bpsQuota.finishReset(attempt, status, reply)
		return bpsQuotaHTTP(status, reply)
	}
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	quota, quotaErr := r.loadBPSQuota(ctx, cfg, authID, raw, true)
	if quotaErr != nil {
		return finish(quotaErr.StatusCode(), bpsResetReply{Error: quotaErr.Code()})
	}
	if reason := bpsResetEligibilityError(quota); reason != "" {
		return finish(http.StatusConflict, bpsResetReply{Error: reason})
	}
	keyData, _ := json.Marshal([]string{authID, token, accountID})
	key := sha256.Sum256(keyData)
	// Even an uncertain POST must invalidate the pre-redemption snapshot.
	err := r.client.ConsumeBPSResetCredit(ctx, token, accountID, body.RequestID)
	r.bpsQuota.invalidate(key)
	if err != nil {
		code, status := "bps_reset_rejected", http.StatusConflict
		switch {
		case errors.Is(err, upstream.ErrBPSResetOutcomeUnknown):
			code, status = "bps_reset_outcome_unknown", http.StatusBadGateway
		case errors.Is(err, upstream.ErrBPSReauthorization):
			code, status = "bps_reauthorization_required", http.StatusUnauthorized
		case errors.Is(err, upstream.ErrBPSQuotaDenied):
			code, status = "bps_quota_access_denied", http.StatusForbidden
		}
		return finish(status, bpsResetReply{Error: code, RequestID: body.RequestID})
	}
	reply = bpsResetReply{Submitted: true, RequestID: body.RequestID}
	r.forgetQuotaGate(cfg, authID, raw, token)
	if r.host("host.routing.reset_cooldown", map[string]any{"auth_index": indexes[0], "host_callback_id": req.HostCallbackID}, nil) != nil {
		reply.Warning = "local_cooldown_refresh_required"
	}
	// A refresh failure cannot turn a known successful consumption into failure.
	updated, refreshErr := r.loadBPSQuota(ctx, cfg, authID, raw, true)
	if refreshErr != nil || updated.Source != "active_usage" || updated.Stale {
		reply.Warning = "reset_submitted_refresh_required"
	}
	return finish(http.StatusOK, reply)
}
