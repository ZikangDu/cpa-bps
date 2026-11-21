package adapter

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

type planObservation struct {
	token    [32]byte
	plan     string
	observed time.Time
}

func (r *Runtime) rememberBPSPlan(authID, token string, result upstream.BPSQuotaResult) {
	if result.Source != "active_usage" {
		return
	}
	observed, err := time.Parse(time.RFC3339, result.ObservedAt)
	var quota struct {
		Subscription struct {
			Plan string `json:"plan"`
		} `json:"subscription"`
	}
	if err != nil || json.Unmarshal(result.Quota, &quota) != nil || quota.Subscription.Plan == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, previous := range r.planObservations {
		if time.Since(previous.observed) > 5*time.Minute {
			delete(r.planObservations, id)
		}
	}
	r.planObservations[authID] = planObservation{
		token: sha256.Sum256([]byte(token)), plan: quota.Subscription.Plan, observed: observed,
	}
}

// Prefer a recent server observation; a stored OAuth plan is a display-only
// fallback. Listing performs no upstream I/O and never exposes tokens.
func (r *Runtime) manageBPSPlans(req managementRequest) (any, error) {
	type entry struct {
		Index    string `json:"auth_index"`
		Plan     string `json:"plan_type"`
		Observed string `json:"observed_at"`
		Source   string `json:"source"`
	}
	out := []entry{}
	cfg, ok := r.begin()
	if !ok {
		return bpsQuotaHTTP(http.StatusServiceUnavailable, map[string]string{"error": "bps_plugin_unavailable"})
	}
	defer r.tasks.Done()
	if cfg.OAuthPreset != "bps" || r.host == nil {
		return bpsQuotaHTTP(http.StatusOK, map[string]any{"accounts": out})
	}
	var listing struct {
		Files []struct {
			ID       string `json:"id"`
			Index    string `json:"auth_index"`
			Provider string `json:"provider"`
		} `json:"files"`
	}
	if r.host("host.auth.list", map[string]any{"host_callback_id": req.HostCallbackID}, &listing) != nil {
		return bpsQuotaHTTP(http.StatusServiceUnavailable, map[string]string{"error": "bps_credential_unavailable"})
	}
	for _, file := range listing.Files {
		r.mu.Lock()
		plan, exists := r.planObservations[file.ID]
		r.mu.Unlock()
		if file.Provider != provider {
			continue
		}
		var stored struct {
			JSON json.RawMessage `json:"json"`
		}
		if r.host("host.auth.get", map[string]any{"auth_index": file.Index, "host_callback_id": req.HostCallbackID}, &stored) != nil {
			continue
		}
		auth, valid := credentialFromStorage(stored.JSON)
		if !valid || auth.Grant == nil || !auth.allows(cfg.ResponsesURL) || !auth.Grant.matches(cfg.OAuth) {
			continue
		}
		if exists && time.Since(plan.observed) <= 5*time.Minute && sha256.Sum256([]byte(auth.Grant.AccessToken)) == plan.token {
			out = append(out, entry{Index: file.Index, Plan: plan.plan, Observed: plan.observed.UTC().Format(time.RFC3339), Source: "active_usage"})
			continue
		}
		_, claim := upstream.BPSProfile(auth.Grant.AccessToken)
		if claim == "" {
			_, claim = upstream.BPSProfile(auth.Grant.IDToken)
		}
		if claim != "" {
			out = append(out, entry{Index: file.Index, Plan: claim, Source: "token_claims"})
		}
	}
	return bpsQuotaHTTP(http.StatusOK, map[string]any{"accounts": out})
}
