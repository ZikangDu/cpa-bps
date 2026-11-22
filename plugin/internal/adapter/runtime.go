package adapter

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"sync"
	"time"

	"cpa-bps-reconstruction/internal/admission"
	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/quality"
	"cpa-bps-reconstruction/internal/upstream"
)

const (
	provider        = "cpa-bps"
	version         = "0.5.11-reconstructed"
	schemaVersion   = 6
	executionWindow = 600 * time.Second
	discoveryWindow = 5 * time.Second
)

type rpcError struct {
	ErrorCode        string
	PublicCode       string
	Message          string
	HTTPStatus       int
	RetryAfter       int
	CredentialScoped bool
	ResetAt          string
}

func (e *rpcError) Error() string {
	kind := "invalid_request_error"
	switch {
	case e.HTTPStatus == http.StatusUnauthorized:
		kind = "authentication_error"
	case e.HTTPStatus == http.StatusForbidden:
		kind = "permission_error"
	case e.HTTPStatus == http.StatusTooManyRequests:
		kind = "rate_limit_error"
	case e.HTTPStatus >= http.StatusInternalServerError:
		kind = "server_error"
	}
	code := e.PublicCode
	if code == "" {
		code = e.ErrorCode
	}
	// The CPA HTTP handler preserves a valid JSON error body from the plugin.
	detail := map[string]any{"message": e.Message, "type": kind, "code": code}
	if e.RetryAfter > 0 {
		detail["retry_after_seconds"] = e.RetryAfter
	}
	if e.ResetAt != "" {
		detail["reset_at"] = e.ResetAt
	}
	body, _ := json.Marshal(map[string]any{"error": detail})
	return string(body)
}

func (e *rpcError) Code() string           { return e.ErrorCode }
func (e *rpcError) StatusCode() int        { return e.HTTPStatus }
func (e *rpcError) Retryable() bool        { return false }
func (e *rpcError) RetryAfterSeconds() int { return e.RetryAfter }
func (e *rpcError) IsCredentialScoped() bool {
	return e.CredentialScoped || e.ErrorCode == "credential_quota"
}

func failure(status int, code, message string) *rpcError {
	return &rpcError{ErrorCode: code, Message: message, HTTPStatus: status}
}

type Runtime struct {
	host             func(method string, request any, response any) error
	client           *upstream.Client
	queue            *admission.Queue
	concurrency      *concurrencyPolicies
	oauth            *oauthflow.Flow
	polls            sync.Map
	refreshes        map[string]*refreshAttempt
	quota            map[string]*quotaEntry
	bpsQuota         *bpsQuotaCache
	quotaGate        *quotaGate
	transports       *transportPolicies
	documents        *documentProcesses
	planObservations map[string]planObservation
	quality          *quality.Service

	ctx    context.Context
	cancel context.CancelFunc
	tasks  sync.WaitGroup
	mu     sync.Mutex
	cfg    config
	closed bool
}

func New(host func(method string, request any, response any) error) *Runtime {
	ctx, cancel := context.WithCancel(context.Background())
	return &Runtime{
		host: host, client: upstream.New(), queue: admission.New(), oauth: oauthflow.New(),
		concurrency:      newConcurrencyPolicies(),
		bpsQuota:         newBPSQuotaCache(),
		quotaGate:        newQuotaGate(),
		transports:       newTransportPolicies(),
		documents:        newDocumentProcesses(),
		planObservations: make(map[string]planObservation),
		refreshes:        make(map[string]*refreshAttempt), quota: make(map[string]*quotaEntry),
		ctx: ctx, cancel: cancel,
	}
}

func (r *Runtime) Close() {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	qualityService := r.quality
	r.mu.Unlock()
	if qualityService != nil {
		qualityService.Close()
	}
	r.tasks.Wait()
	r.client.CloseIdleConnections()
	r.oauth.Close()
}

func (r *Runtime) begin() (config, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || !r.cfg.Enabled {
		return config{}, false
	}
	r.tasks.Add(1)
	return r.cfg, true
}

func (r *Runtime) Handle(method string, raw json.RawMessage) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		return r.register(raw)
	case "plugin.quiesce", "plugin.shutdown":
		r.Close()
		return struct{}{}, nil
	case "auth.identifier", "executor.identifier":
		return map[string]string{"identifier": provider}, nil
	case "auth.parse":
		return parseAuth(raw)
	case "auth.login.start":
		return r.startLogin(raw)
	case "auth.login.poll":
		return r.pollLogin(raw)
	case "auth.refresh":
		return r.refreshAuth(raw)
	case "quota.identifier":
		return map[string]string{"identifier": provider}, nil
	case "quota.describe":
		return quotaDescription(), nil
	case "quota.fetch", "quota.reset":
		return r.handleQuota(method, raw)
	case "management.register":
		return registerManagement(), nil
	case "management.handle":
		return r.handleManagement(raw)
	case "model.static", "model.register":
		return modelResponse(nil), nil
	case "model.for_auth":
		return r.models(raw)
	case "executor.execute", "executor.execute_stream":
		return r.execute(raw, method == "executor.execute_stream")
	default:
		return nil, failure(http.StatusNotImplemented, "not_implemented", "this plugin method is unavailable")
	}
}

func (r *Runtime) register(raw json.RawMessage) (any, error) {
	var request struct {
		ConfigYAML    []byte `json:"config_yaml"`
		SchemaVersion uint32 `json:"schema_version"`
	}
	if len(raw) > 0 {
		if err := decode(raw, &request); err != nil {
			return nil, err
		}
	}
	cfg, err := parseConfig(request.ConfigYAML)
	if err != nil {
		return nil, failure(http.StatusBadRequest, "bps_invalid_config", err.Error())
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, failure(http.StatusServiceUnavailable, "bps_plugin_unavailable", "BPS plugin is shutting down")
	}
	r.cfg = cfg
	// Quality schedules are optional; a corrupt state file must not disable inference.
	_ = r.startQualityLocked(cfg)
	// An in-flight quota read from the previous configuration must not
	// repopulate its cache after reconfiguration.
	for id, entry := range r.quota {
		r.quota[id] = &quotaEntry{completed: entry.completed, input: entry.input, output: entry.output}
	}
	versionForHost := uint32(schemaVersion)
	if request.SchemaVersion > 0 && request.SchemaVersion < versionForHost {
		versionForHost = request.SchemaVersion
	}
	return map[string]any{
		"schema_version": versionForHost,
		"metadata": map[string]any{
			"Name":             "CPA BPS Reconstruction",
			"Version":          version,
			"Author":           "CPA BPS Reconstruction",
			"GitHubRepository": "ZikangDu/cpa-bps",
			"ConfigFields": []map[string]any{
				{"Name": "responses_url", "Type": "string", "Description": "Authorized Responses endpoint."},
				{"Name": "models_url", "Type": "string", "Description": "Optional same-origin model catalog."},
				{"Name": "files_url", "Type": "string", "Description": "Optional same-origin Files endpoint for inline images."},
				{"Name": "quota_url", "Type": "string", "Description": "Optional same-origin provider quota source."},
				{"Name": "oauth", "Type": "object", "Description": "Authorization Code settings for a registered OAuth client."},
				{"Name": "oauth_preset", "Type": "enum", "Default": "", "EnumValues": []string{"", "bps"}, "Description": "BPS independent login using the public browser authorization flow."},
				{"Name": "oauth_proxy_url", "Type": "string", "Description": "Optional explicit proxy for BPS token exchange and refresh."},
				{"Name": "delegated_gateway", "Type": "boolean", "Default": false, "Description": "Show that the upstream CPA gateway manages BPS authorization."},
				{"Name": "omit_encrypted_history", "Type": "boolean", "Default": false},
				{"Name": "models", "Type": "array", "Description": "Manual upstream model names and optional aliases."},
				{"Name": "auto_discover", "Type": "boolean", "Default": true},
				{"Name": "model_suffix", "Type": "enum", "Default": "-excel", "EnumValues": []string{"-excel", "无后缀"}},
				{"Name": "fast_mode", "Type": "boolean", "Default": false},
				{"Name": "bps_tool_transport", "Type": "enum", "Default": "auto", "EnumValues": []string{"auto", "text", "native"}, "Description": "Automatic request-aware tool transport with stable sessions and bounded compatibility fallback. Manual modes are diagnostic overrides."},
				{"Name": "document_worker_path", "Type": "string", "Description": "Installed local document parser helper. Inline PDF and Excel files are parsed automatically."},
				{"Name": "document_python", "Type": "string", "Description": "Private document runtime Python for PDFium and legacy XLS parsing."},
				{"Name": "bps_tools_version_id", "Type": "string", "Description": "Optional authorized BPS tool catalog version; empty uses the upstream default."},
				{"Name": "bps_compact_threshold", "Type": "integer", "Default": 0, "Description": "Optional inline BPS compaction threshold; 0 keeps transport default (native: 200000, text: omitted)."},
				{"Name": "native_tool_repair_attempts", "Type": "integer", "Default": 2, "Description": "Native relay format corrections before any client output, 0–2; never retries HTTP failures or executed tools."},
				{"Name": "account_concurrency_limit", "Type": "integer", "Default": 20},
				{"Name": "concurrency_policy_path", "Type": "string", "Description": "Persistent per-account concurrency overrides."},
				{"Name": "quality_state_path", "Type": "string", "Description": "Private persisted candy-test schedules and results."},
			},
		},
		"capabilities": map[string]any{
			"auth_provider":           true,
			"model_provider":          true,
			"executor":                true,
			"executor_model_scope":    "oauth",
			"executor_input_formats":  []string{"openai-response", "codex"},
			"executor_output_formats": []string{"openai-response", "codex"},
			"quota_provider":          true,
			"management_api":          true,
		},
	}, nil
}

type modelInfo struct {
	ID, Name, Object, OwnedBy, DisplayName string
	SupportedGenerationMethods             []string
	SupportedInputModalities               []string
	SupportedOutputModalities              []string
	Thinking                               *modelThinking
}

type modelThinking struct{ Levels []string }

// BPS accepts these client compatibility aliases for xhigh. Advertise them
// only where the account catalog supports their real upstream level, so
// CPA's capability validation does not reject it before plugin normalization.
func bpsClientEffortLevels(efforts []string) []string {
	levels := slices.Clone(efforts)
	if slices.Contains(levels, "xhigh") {
		for _, alias := range []string{"max", "ultra", "x-high"} {
			if !slices.Contains(levels, alias) {
				levels = append(levels, alias)
			}
		}
	}
	return levels
}

func modelResponse(models []modelConfig) any {
	result := make([]modelInfo, 0, len(models))
	for _, model := range models {
		result = append(result, modelInfo{
			ID: model.Alias, Name: model.Name, Object: "model", OwnedBy: provider,
			DisplayName: model.Alias, SupportedGenerationMethods: []string{"responses"},
		})
	}
	return map[string]any{"Provider": provider, "Models": result}
}

func (r *Runtime) models(raw json.RawMessage) (any, error) {
	var request struct {
		AuthProvider string
		StorageJSON  []byte
	}
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	auth, ok := credentialFromStorage(request.StorageJSON)
	if request.AuthProvider != provider || !ok || auth.Disabled {
		return modelResponse(nil), nil
	}
	cfg, ok := r.begin()
	if !ok {
		return modelResponse(nil), nil
	}
	defer r.tasks.Done()
	if !auth.allows(cfg.ResponsesURL) || (auth.Grant != nil && !auth.Grant.matches(cfg.OAuth)) {
		return modelResponse(nil), nil
	}
	if !cfg.discoveryEnabled() {
		return modelResponse(cfg.Models), nil
	}
	tokenEndpoint := cfg.ModelsURL
	if cfg.OAuthPreset == "bps" {
		tokenEndpoint = cfg.ResponsesURL
	}
	token, authErr := resolveToken(request.StorageJSON, tokenEndpoint, cfg.OAuth)
	if authErr != nil {
		return nil, authErr
	}
	ctx, cancel := context.WithTimeout(r.ctx, 15*time.Second)
	defer cancel()
	if cfg.OAuthPreset == "bps" {
		accountID := bpsExecutionAccount(cfg, request.StorageJSON, token)
		catalog, err := r.client.BPSModels(ctx, token, accountID)
		if err != nil {
			return nil, failure(http.StatusBadGateway, "bps_model_discovery_failed", "BPS model discovery did not return a usable catalog")
		}
		models := make([]modelInfo, 0, len(catalog))
		for _, model := range catalog {
			entry := modelInfo{
				ID: model.ID + cfg.ModelSuffix, Name: model.ID, Object: "model", OwnedBy: provider,
				DisplayName: model.Label, SupportedGenerationMethods: []string{"responses"},
				SupportedInputModalities:  []string{"text", "image"},
				SupportedOutputModalities: []string{"text"},
			}
			if len(model.Efforts) > 0 {
				entry.Thinking = &modelThinking{Levels: bpsClientEffortLevels(model.Efforts)}
			}
			models = append(models, entry)
		}
		return map[string]any{"Provider": provider, "Models": models}, nil
	}
	names, err := r.client.Models(ctx, cfg.ModelsURL, token)
	if err != nil {
		return nil, failure(http.StatusBadGateway, "bps_model_discovery_failed", "BPS model discovery did not return a usable catalog")
	}
	models := make([]modelConfig, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !seen[name] {
			models = append(models, modelConfig{Name: name, Alias: name + cfg.ModelSuffix})
			seen[name] = true
		}
	}
	return modelResponse(models), nil
}

func decode(raw json.RawMessage, target any) *rpcError {
	if err := json.Unmarshal(raw, target); err != nil {
		return failure(http.StatusBadRequest, "invalid_request", "the RPC request is invalid")
	}
	return nil
}
