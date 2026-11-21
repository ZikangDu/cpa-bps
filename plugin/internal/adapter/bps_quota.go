package adapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

const bpsQuotaTimeout = 12 * time.Second
const bpsQuotaStaleWindow = 5 * time.Minute

type bpsQuotaCache struct {
	mu       sync.Mutex
	entries  map[[32]byte]*bpsQuotaRead
	lastGood map[[32]byte]upstream.BPSQuotaResult
	resets   map[[32]byte]*bpsResetAttempt
}

type bpsQuotaRead struct {
	done    chan struct{}
	expires time.Time
	result  upstream.BPSQuotaResult
	err     error
}

func newBPSQuotaCache() *bpsQuotaCache {
	return &bpsQuotaCache{entries: make(map[[32]byte]*bpsQuotaRead),
		lastGood: make(map[[32]byte]upstream.BPSQuotaResult), resets: make(map[[32]byte]*bpsResetAttempt)}
}

func (c *bpsQuotaCache) load(ctx context.Context, key [32]byte, force bool, fetch func() (upstream.BPSQuotaResult, error)) (upstream.BPSQuotaResult, error) {
	c.mu.Lock()
	now := time.Now()
	if previous := c.entries[key]; previous != nil {
		select {
		case <-previous.done:
			if !force && now.Before(previous.expires) {
				c.mu.Unlock()
				return copyBPSQuota(previous.result), previous.err
			}
		default:
			c.mu.Unlock()
			select {
			case <-previous.done:
				return copyBPSQuota(previous.result), previous.err
			case <-ctx.Done():
				return upstream.BPSQuotaResult{}, ctx.Err()
			}
		}
	}
	for oldKey, old := range c.entries {
		select {
		case <-old.done:
			if !now.Before(old.expires) {
				delete(c.entries, oldKey)
			}
		default:
		}
	}
	for oldKey, old := range c.lastGood {
		stamp, err := time.Parse(time.RFC3339, old.ObservedAt)
		if err != nil || now.Sub(stamp) > bpsQuotaStaleWindow {
			delete(c.lastGood, oldKey)
		}
	}
	if len(c.entries) >= 512 {
		c.mu.Unlock()
		return upstream.BPSQuotaResult{}, upstream.ErrQuotaUnavailable
	}
	next := &bpsQuotaRead{done: make(chan struct{})}
	c.entries[key] = next
	previousGood, hasPreviousGood := c.lastGood[key]
	c.mu.Unlock()

	next.result, next.err = fetch()
	if hasPreviousGood && !errors.Is(next.err, upstream.ErrBPSReauthorization) && !errors.Is(next.err, upstream.ErrBPSQuotaDenied) &&
		(next.err != nil || next.result.Source == "reset_credits") {
		old := copyBPSQuota(previousGood)
		if next.err == nil && next.result.ResetCardsAvailable != nil {
			old.ResetCardsAvailable = next.result.ResetCardsAvailable
		}
		old.Source, old.Stale, old.Warning = "cached_usage", true, "stale_quota_observation"
		old.UpdateResetEligibility()
		next.result, next.err = old, nil
	}
	ttl := quotaCacheWindow
	if next.err != nil || next.result.Stale || next.result.Warning != "" {
		ttl = 5 * time.Second
	}
	next.expires = time.Now().Add(ttl)
	c.mu.Lock()
	if next.err == nil && next.result.Source == "active_usage" && !next.result.Stale && next.result.ObservedAt != "" && c.entries[key] == next {
		c.lastGood[key] = copyBPSQuota(next.result)
	}
	close(next.done)
	c.mu.Unlock()
	return copyBPSQuota(next.result), next.err
}

func (c *bpsQuotaCache) invalidate(key [32]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
	delete(c.lastGood, key)
}

func copyBPSQuota(result upstream.BPSQuotaResult) upstream.BPSQuotaResult {
	result.Quota = bytes.Clone(result.Quota)
	if result.ResetCardsAvailable != nil {
		count := *result.ResetCardsAvailable
		result.ResetCardsAvailable = &count
	}
	if result.ApplicableResetCardsAvailable != nil {
		count := *result.ApplicableResetCardsAvailable
		result.ApplicableResetCardsAvailable = &count
	}
	return result
}

// manageBPSQuota provides the legacy BPS panel route, with explicit HTTP error
// responses because CPA turns an RPC error at this boundary into generic 502.
func (r *Runtime) manageBPSQuota(req managementRequest) (any, error) {
	if req.Method != http.MethodGet {
		return bpsQuotaHTTP(http.StatusMethodNotAllowed, map[string]string{"error": "bps_unsupported_method"})
	}
	indexes := req.Query["auth_index"]
	if len(indexes) != 1 || strings.TrimSpace(indexes[0]) == "" || len(indexes[0]) > 256 {
		return bpsQuotaHTTP(http.StatusBadRequest, map[string]string{"error": "bps_invalid_auth_index"})
	}
	force := false
	if values, exists := req.Query["refresh"]; exists {
		if len(values) != 1 || (values[0] != "true" && values[0] != "false" && values[0] != "1" && values[0] != "0") {
			return bpsQuotaHTTP(http.StatusBadRequest, map[string]string{"error": "bps_invalid_refresh"})
		}
		force = values[0] == "true" || values[0] == "1"
	}
	cfg, ok := r.begin()
	if !ok {
		return bpsQuotaHTTP(http.StatusServiceUnavailable, map[string]string{"error": "bps_plugin_unavailable"})
	}
	defer r.tasks.Done()
	if cfg.OAuthPreset != "bps" || r.host == nil {
		return bpsQuotaHTTP(http.StatusServiceUnavailable, map[string]string{"error": "bps_quota_unavailable"})
	}

	authID, rawCredential, selectedErr := r.selectedBPSQuotaCredential(req, indexes[0])
	if selectedErr != nil {
		return bpsQuotaHTTP(selectedErr.StatusCode(), map[string]string{"error": selectedErr.Code()})
	}
	ctx, cancel := context.WithTimeout(r.ctx, bpsQuotaTimeout)
	defer cancel()
	result, err := r.loadBPSQuota(ctx, cfg, authID, rawCredential, force)
	if err != nil {
		return bpsQuotaHTTP(err.StatusCode(), map[string]string{"error": err.Code()})
	}
	return bpsQuotaHTTP(http.StatusOK, result)
}

// loadBPSQuota can also be used by the SDK quota provider once credential
// selection has already supplied StorageJSON.
func (r *Runtime) loadBPSQuota(ctx context.Context, cfg config, authID string, raw []byte, force bool) (upstream.BPSQuotaResult, *rpcError) {
	auth, ok := credentialFromStorage(raw)
	if !ok || auth.Grant == nil {
		return upstream.BPSQuotaResult{}, failure(http.StatusBadRequest, "bps_quota_unavailable", "BPS OAuth credentials are required for this quota source")
	}
	// A local pause disables inference/redemption, not management inspection.
	// The temporary copy only clears that local flag; origin, OAuth registration,
	// token and expiry checks still run through the ordinary resolver.
	readCredential := raw
	if auth.Disabled {
		readOnly := auth
		readOnly.Disabled = false
		readCredential, _ = json.Marshal(readOnly)
	}
	token, authErr := resolveToken(readCredential, cfg.ResponsesURL, cfg.OAuth)
	if authErr != nil {
		if authErr.StatusCode() == http.StatusForbidden {
			return upstream.BPSQuotaResult{}, authErr
		}
		return upstream.BPSQuotaResult{}, failure(http.StatusUnauthorized, "bps_reauthorization_required", "authorize the BPS credential again")
	}
	view := func(value upstream.BPSQuotaResult) upstream.BPSQuotaResult {
		// Paused status belongs to the selected credential view, never to the
		// token-scoped cache shared with a later unpaused read.
		value = copyBPSQuota(value)
		value.CredentialDisabled = auth.Disabled
		value.UpdateResetEligibility()
		return value
	}
	accountID, plan := upstream.BPSProfile(token)
	if accountID == "" {
		accountID, _ = upstream.BPSProfile(auth.Grant.IDToken)
	}
	if plan == "" {
		_, plan = upstream.BPSProfile(auth.Grant.IDToken)
	}
	encodedKey, _ := json.Marshal([]string{authID, token, accountID})
	key := sha256.Sum256(encodedKey)
	result, err := r.bpsQuota.load(ctx, key, force, func() (upstream.BPSQuotaResult, error) {
		return r.client.BPSQuota(ctx, token, accountID)
	})
	switch {
	case err == nil:
		if result.Source == "reset_credits" && plan != "" {
			result.Quota, _ = json.Marshal(map[string]any{"subscription": map[string]string{"plan": plan}})
		}
		r.rememberBPSPlan(authID, token, result)
		return view(result), nil
	case errors.Is(err, upstream.ErrBPSReauthorization):
		return upstream.BPSQuotaResult{}, failure(http.StatusUnauthorized, "bps_reauthorization_required", "authorize the BPS credential again")
	case errors.Is(err, upstream.ErrBPSQuotaDenied):
		return upstream.BPSQuotaResult{}, failure(http.StatusForbidden, "bps_quota_access_denied", "the upstream denied this account quota query")
	default:
		// A plan claim is a display hint. No fresh observation or zero quota is
		// invented when the active query did not succeed.
		fallback := map[string]any{}
		if plan != "" {
			fallback["subscription"] = map[string]string{"plan": plan}
		}
		quota, _ := json.Marshal(fallback)
		return view(upstream.BPSQuotaResult{Quota: quota, Source: "token_claims",
			Warning: "active_quota_unavailable", SupportsReset: true,
			ResetUnavailableReason: "bps_reset_eligibility_unknown"}), nil
	}
}

func bpsQuotaHTTP(status int, value any) (any, error) {
	body, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"StatusCode": status,
		"Headers":    http.Header{"Content-Type": {"application/json"}, "Cache-Control": {"no-store"}},
		"Body":       body,
	}, nil
}

func (r *Runtime) selectedBPSQuotaCredential(req managementRequest, index string) (string, []byte, *rpcError) {
	var listing struct {
		Files []struct {
			ID        string `json:"id"`
			AuthIndex string `json:"auth_index"`
			Provider  string `json:"provider"`
			Type      string `json:"type"`
		} `json:"files"`
	}
	if r.host("host.auth.list", map[string]any{"host_callback_id": req.HostCallbackID}, &listing) != nil {
		return "", nil, failure(http.StatusServiceUnavailable, "bps_credential_unavailable", "selected credential is unavailable")
	}
	authID := ""
	for _, entry := range listing.Files {
		if entry.AuthIndex == index && (entry.Provider == provider || entry.Type == provider) &&
			entry.ID != "" && len(entry.ID) <= 1024 {
			if authID != "" {
				return "", nil, failure(http.StatusConflict, "bps_ambiguous_auth_index", "selected credential is ambiguous")
			}
			authID = entry.ID
		}
	}
	if authID == "" {
		return "", nil, failure(http.StatusNotFound, "bps_credential_unavailable", "selected credential is unavailable")
	}
	var stored struct {
		JSON json.RawMessage `json:"json"`
	}
	if r.host("host.auth.get", map[string]any{
		"auth_index": index, "host_callback_id": req.HostCallbackID,
	}, &stored) != nil {
		return "", nil, failure(http.StatusNotFound, "bps_credential_unavailable", "selected credential is unavailable")
	}
	return authID, stored.JSON, nil
}
