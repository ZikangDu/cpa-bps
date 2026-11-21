package adapter

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"time"
)

const quotaCacheWindow = 20 * time.Second

type quotaEntry struct {
	remote     json.RawMessage
	validUntil time.Time
	completed  int64
	input      int64
	output     int64
}

func quotaDescription() any {
	return map[string]any{
		"supported_providers": []string{provider},
		"display_name":        "BPS quota and local usage",
		"supports_reset":      true,
	}
}

func (r *Runtime) handleQuota(method string, raw json.RawMessage) (any, error) {
	var request struct {
		AuthID      string `json:"auth_id"`
		Provider    string `json:"provider"`
		StorageJSON []byte `json:"storage_json"`
	}
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	auth, valid := credentialFromStorage(request.StorageJSON)
	if request.AuthID == "" || request.Provider != provider || !valid || auth.Disabled {
		return nil, failure(http.StatusNotFound, "bps_credential_unavailable", "the selected credential is unavailable")
	}
	cfg, ok := r.begin()
	if !ok {
		return nil, failure(http.StatusServiceUnavailable, "bps_plugin_unavailable", "BPS plugin is unavailable")
	}
	defer r.tasks.Done()
	if !auth.allows(cfg.ResponsesURL) || (auth.Grant != nil && !auth.Grant.matches(cfg.OAuth)) {
		return nil, failure(http.StatusForbidden, "bps_credential_origin_mismatch", "the selected credential cannot access this Responses origin")
	}

	r.mu.Lock()
	if method == "quota.reset" {
		delete(r.quota, request.AuthID)
		r.mu.Unlock()
		return map[string]any{"success": true, "message": "Local BPS usage counters and quota cache cleared."}, nil
	}
	entry := r.quotaEntry(request.AuthID)
	local := localMetrics(*entry)
	remote := entry.remote
	fresh := len(remote) > 0 && time.Now().Before(entry.validUntil)
	r.mu.Unlock()

	if cfg.OAuthPreset == "bps" {
		ctx, cancel := context.WithTimeout(r.ctx, bpsQuotaTimeout)
		defer cancel()
		result, err := r.loadBPSQuota(ctx, cfg, request.AuthID, request.StorageJSON, false)
		if err != nil {
			return nil, err
		}
		remote = result.Quota
	} else if cfg.QuotaURL == "" {
		return map[string]any{"summary": local}, nil
	} else if !fresh {
		token, authErr := resolveToken(request.StorageJSON, cfg.QuotaURL, cfg.OAuth)
		if authErr != nil {
			return nil, authErr
		}
		ctx, cancel := context.WithTimeout(r.ctx, discoveryWindow)
		defer cancel()
		var err error
		remote, err = r.client.Quota(ctx, cfg.QuotaURL, token)
		if err != nil {
			return nil, failure(http.StatusBadGateway, "bps_quota_unavailable", "the configured quota service did not return usable quota data")
		}
		r.mu.Lock()
		if r.quota[request.AuthID] == entry {
			entry.remote, entry.validUntil = remote, time.Now().Add(quotaCacheWindow)
			local = localMetrics(*entry)
		}
		r.mu.Unlock()
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(remote, &result); err != nil {
		return nil, failure(http.StatusBadGateway, "bps_quota_unavailable", "the configured quota service returned invalid data")
	}
	var summary []json.RawMessage
	_ = json.Unmarshal(result["summary"], &summary)
	for _, metric := range local {
		rawMetric, _ := json.Marshal(metric)
		summary = append(summary, rawMetric)
	}
	result["summary"], _ = json.Marshal(summary)
	return result, nil
}

// quotaEntry requires the runtime mutex.
func (r *Runtime) quotaEntry(id string) *quotaEntry {
	entry := r.quota[id]
	if entry == nil {
		entry = &quotaEntry{}
		r.quota[id] = entry
	}
	return entry
}

type usageMetric struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Value  int64  `json:"value"`
	Format string `json:"format"`
}

func localMetrics(entry quotaEntry) []usageMetric {
	return []usageMetric{
		{Key: "local_completed", Label: "Local completed requests", Value: entry.completed, Format: "number"},
		{Key: "local_input_tokens", Label: "Local observed input tokens", Value: entry.input, Format: "number"},
		{Key: "local_output_tokens", Label: "Local observed output tokens", Value: entry.output, Format: "number"},
	}
}

func (r *Runtime) observeUsage(id string, body []byte) {
	var response struct {
		Status string
		Usage  struct {
			Input  int64 `json:"input_tokens"`
			Output int64 `json:"output_tokens"`
		}
	}
	if err := json.Unmarshal(body, &response); err != nil || response.Status != "completed" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	entry := r.quotaEntry(id)
	entry.completed = addUsage(entry.completed, 1)
	entry.input = addUsage(entry.input, response.Usage.Input)
	entry.output = addUsage(entry.output, response.Usage.Output)
}

func addUsage(base, inc int64) int64 {
	if inc <= 0 {
		return base
	}
	return base + min(inc, math.MaxInt64-base)
}
