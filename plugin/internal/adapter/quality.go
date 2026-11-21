package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"cpa-bps-reconstruction/internal/candy"
	"cpa-bps-reconstruction/internal/quality"
	"cpa-bps-reconstruction/internal/upstream"
)

func qualityDefaults() quality.Config {
	return quality.Config{Models: []string{}, IntervalMinutes: 360, ReasoningEffort: "high", Prompt: candy.DefaultPrompt, ExpectedAnswer: candy.DefaultExpectedAnswer}
}

// startQualityLocked is called only after plugin config validation while r.mu
// is held. Construction reads local state; the worker obtains fresh credentials
// only for due/explicit checks after registration releases the runtime lock.
func (r *Runtime) startQualityLocked(cfg config) error {
	path := cfg.QualityStatePath
	if path == "" {
		path = filepath.Join("plugins", "cpa-bps-quality.json")
	}
	if r.quality != nil {
		r.quality.CancelRunning()
		if r.quality.Path() != path {
			r.quality.SetAvailable(false)
			return quality.ErrInvalid
		}
		r.quality.SetAvailable(cfg.Enabled)
		return nil
	}
	if !cfg.Enabled {
		return nil
	}
	service, err := quality.New(path, r.runQualityCheck, quality.Options{Defaults: qualityDefaults()})
	if err != nil {
		return err
	}
	r.quality = service
	service.Start(r.ctx)
	return nil
}

type qualityAuthEntry struct {
	ID            string `json:"id"`
	AuthIndex     string `json:"auth_index"`
	Provider      string `json:"provider"`
	Type          string `json:"type"`
	Disabled      bool   `json:"disabled"`
	Status        string `json:"status"`
	StatusMessage string `json:"status_message"`
}

func (r *Runtime) selectedQualityAuth(index, callback string) (qualityAuthEntry, *rpcError) {
	if r.host == nil {
		return qualityAuthEntry{}, failure(503, "bps_credential_unavailable", "credential directory is unavailable")
	}
	var listing struct {
		Files []qualityAuthEntry `json:"files"`
	}
	if r.host("host.auth.list", map[string]any{"host_callback_id": callback}, &listing) != nil {
		return qualityAuthEntry{}, failure(503, "bps_credential_unavailable", "credential directory is unavailable")
	}
	var selected qualityAuthEntry
	for _, entry := range listing.Files {
		if entry.AuthIndex != index || entry.Provider != provider && entry.Type != provider {
			continue
		}
		if entry.ID == "" || len(entry.ID) > 1024 {
			continue
		}
		if selected.ID != "" {
			return qualityAuthEntry{}, failure(409, "bps_ambiguous_auth_index", "credential index is ambiguous")
		}
		selected = entry
	}
	if selected.ID == "" {
		return selected, failure(404, "bps_credential_unavailable", "selected BPS credential is unavailable")
	}
	return selected, nil
}

func (r *Runtime) qualityStorage(index, callback string) ([]byte, *rpcError) {
	var stored struct {
		JSON json.RawMessage `json:"json"`
	}
	if r.host("host.auth.get", map[string]any{"auth_index": index, "host_callback_id": callback}, &stored) != nil {
		return nil, failure(404, "bps_credential_unavailable", "selected credential is unavailable")
	}
	return stored.JSON, nil
}

func qualityPolicyBlocked(message string) bool {
	if message == "bps_usage_policy_blocked" || message == "403: This request was blocked by our usage policy." {
		return true
	}
	var detail struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	return json.Unmarshal([]byte(message), &detail) == nil && (detail.Error.Code == "bps_usage_policy_blocked" || detail.Error.Message == "403: This request was blocked by our usage policy.")
}

func (r *Runtime) manageQuality(req managementRequest) (any, error) {
	endpoint := strings.TrimPrefix(req.Path, "/v0/management/cpa-bps/quality/")
	methodOK := endpoint == "config" && (req.Method == http.MethodGet || req.Method == http.MethodPut) ||
		(endpoint == "models" || endpoint == "results") && req.Method == http.MethodGet || endpoint == "run" && req.Method == http.MethodPost
	if !methodOK {
		return bpsQuotaHTTP(405, map[string]string{"error": "bps_unsupported_method"})
	}
	indexes := req.Query["auth_index"]
	var run quality.RunRequest
	if endpoint == "run" {
		if len(req.Body) > 4096 || len(req.Body) > 0 && decodePolicyJSON(req.Body, &run) != nil {
			return bpsQuotaHTTP(400, map[string]string{"error": "bps_invalid_quality_config"})
		}
		if len(indexes) == 0 && run.AuthIndex != "" {
			indexes = []string{run.AuthIndex}
		}
	}
	if len(indexes) != 1 || strings.TrimSpace(indexes[0]) == "" || len(indexes[0]) > 256 || run.AuthIndex != "" && run.AuthIndex != indexes[0] {
		return bpsQuotaHTTP(400, map[string]string{"error": "bps_invalid_auth_index"})
	}
	index := indexes[0]
	cfg, ok := r.begin()
	if !ok {
		return bpsQuotaHTTP(503, map[string]string{"error": "bps_plugin_unavailable"})
	}
	defer r.tasks.Done()
	r.mu.Lock()
	service := r.quality
	r.mu.Unlock()
	if service == nil {
		return bpsQuotaHTTP(503, map[string]string{"error": "bps_quality_unavailable"})
	}
	path := cfg.QualityStatePath
	if path == "" {
		path = filepath.Join("plugins", "cpa-bps-quality.json")
	}
	if service.Path() != path {
		return bpsQuotaHTTP(503, map[string]string{"error": "bps_quality_path_restart_required"})
	}
	entry, selectErr := r.selectedQualityAuth(index, req.HostCallbackID)
	if selectErr != nil {
		return bpsQuotaHTTP(selectErr.StatusCode(), map[string]string{"error": selectErr.Code()})
	}
	view := func() quality.Snapshot {
		result := service.Snapshot(index)
		result.CredentialDisabled = entry.Disabled || entry.Status == "disabled"
		return result
	}
	switch endpoint {
	case "config":
		if req.Method == http.MethodPut {
			var update quality.Config
			if len(req.Body) > 12288 || decodePolicyJSON(req.Body, &update) != nil {
				return qualityHTTPError(quality.ErrInvalid)
			}
			if err := service.Put(index, entry.ID, update); err != nil {
				return qualityHTTPError(err)
			}
		}
		return bpsQuotaHTTP(200, view())
	case "results":
		return bpsQuotaHTTP(200, view())
	case "run":
		id, err := service.Run(index, entry.ID, run)
		if err != nil {
			return qualityHTTPError(err)
		}
		return bpsQuotaHTTP(202, map[string]string{"run_id": id, "status": "accepted"})
	case "models":
		// Reading a registered catalog does not test upstream permissions and
		// must remain available when observation scheduling is paused.
		if models, available := r.registeredQualityModels(cfg, entry, req.HostCallbackID); available {
			return bpsQuotaHTTP(200, map[string]any{
				"models": models, "source": "host_registered", "live_verified": false,
				"credential_disabled": entry.Disabled || entry.Status == "disabled",
			})
		}
		if entry.Disabled || entry.Status == "disabled" {
			return bpsQuotaHTTP(409, map[string]string{"error": "bps_quality_account_paused"})
		}
		if service.Snapshot(index).Blocked || qualityPolicyBlocked(entry.StatusMessage) {
			return qualityHTTPError(quality.ErrBlocked)
		}
		return r.qualityModels(cfg, entry, req.HostCallbackID)
	}
	return bpsQuotaHTTP(404, map[string]string{"error": "not_found"})
}

func qualityHTTPError(err error) (any, error) {
	status, code := 503, "bps_quality_state_unavailable"
	switch {
	case errors.Is(err, quality.ErrInvalid):
		status, code = 400, "bps_invalid_quality_config"
	case errors.Is(err, quality.ErrBusy):
		status, code = 409, "bps_quality_busy"
	case errors.Is(err, quality.ErrCooldown):
		status, code = 429, "bps_quality_cooldown"
	case errors.Is(err, quality.ErrBlocked):
		status, code = 409, "bps_usage_policy_blocked"
	case errors.Is(err, quality.ErrClosed):
		status, code = 503, "bps_plugin_unavailable"
	}
	return bpsQuotaHTTP(status, map[string]string{"error": code})
}

type qualityModel struct {
	ID            string   `json:"id"`
	UpstreamModel string   `json:"upstream_model"`
	Label         string   `json:"label"`
	Efforts       []string `json:"efforts,omitempty"`
}

// registeredQualityModels reads only the exact selected credential's registry.
// It does not load credentials, query the network, or merge a global model list.
func (r *Runtime) registeredQualityModels(cfg config, entry qualityAuthEntry, callback string) ([]qualityModel, bool) {
	var response struct {
		Credentials []struct {
			ID        string   `json:"id"`
			AuthIndex string   `json:"auth_index"`
			Provider  string   `json:"provider"`
			Models    []string `json:"models"`
		} `json:"credentials"`
	}
	if r.host("host.auth.models", map[string]any{"auth_index": entry.AuthIndex, "host_callback_id": callback}, &response) != nil || len(response.Credentials) != 1 {
		return nil, false
	}
	registered := response.Credentials[0]
	if registered.ID != entry.ID || registered.AuthIndex != entry.AuthIndex || registered.Provider != provider || len(registered.Models) > 512 {
		return nil, false
	}
	models := make([]qualityModel, 0, len(registered.Models))
	seen := make(map[string]bool, len(registered.Models))
	for _, id := range registered.Models {
		if !quality.ValidModel(id) || seen[id] {
			continue
		}
		upstreamModel := cfg.upstreamModel(id)
		if upstreamModel == "" {
			upstreamModel = strings.TrimSuffix(id, cfg.ModelSuffix)
		}
		models = append(models, qualityModel{ID: id, UpstreamModel: upstreamModel, Label: id})
		seen[id] = true
	}
	if len(models) == 0 {
		return nil, false
	}
	sort.Slice(models, func(i, j int) bool { return models[i].ID < models[j].ID })
	return models, true
}

func (r *Runtime) qualityModels(cfg config, entry qualityAuthEntry, callback string) (any, error) {
	raw, err := r.qualityStorage(entry.AuthIndex, callback)
	if err != nil {
		return bpsQuotaHTTP(err.StatusCode(), map[string]string{"error": err.Code()})
	}
	token, authErr := resolveToken(raw, cfg.ResponsesURL, cfg.OAuth)
	if authErr != nil {
		return bpsQuotaHTTP(authErr.StatusCode(), map[string]string{"error": authErr.Code()})
	}
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	models := []qualityModel{}
	source := "configured"
	if cfg.OAuthPreset == "bps" {
		accountID, _ := upstream.BPSProfile(token)
		if accountID == "" {
			if auth, ok := credentialFromStorage(raw); ok && auth.Grant != nil {
				accountID, _ = upstream.BPSProfile(auth.Grant.IDToken)
			}
		}
		catalog, err := r.client.BPSModels(ctx, token, accountID)
		if err != nil {
			return bpsQuotaHTTP(502, map[string]string{"error": "bps_model_discovery_failed"})
		}
		for _, model := range catalog {
			models = append(models, qualityModel{ID: model.ID + cfg.ModelSuffix, UpstreamModel: model.ID, Label: model.Label, Efforts: model.Efforts})
		}
		source = "account_catalog"
	} else if cfg.discoveryEnabled() {
		names, err := r.client.Models(ctx, cfg.ModelsURL, token)
		if err != nil {
			return bpsQuotaHTTP(502, map[string]string{"error": "bps_model_discovery_failed"})
		}
		seen := map[string]bool{}
		for _, name := range names {
			if quality.ValidModel(name) && !seen[name] {
				models = append(models, qualityModel{ID: name + cfg.ModelSuffix, UpstreamModel: name, Label: name})
				seen[name] = true
			}
		}
		source = "account_catalog"
	} else {
		for _, model := range cfg.Models {
			models = append(models, qualityModel{ID: model.Alias, UpstreamModel: model.Name, Label: model.Alias})
		}
	}
	return bpsQuotaHTTP(200, map[string]any{"models": models, "source": source})
}
