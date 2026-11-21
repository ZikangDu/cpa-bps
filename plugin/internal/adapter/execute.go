package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"

	"cpa-bps-reconstruction/internal/admission"
	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/compat"
	"cpa-bps-reconstruction/internal/upstream"
)

const eventStream = "text/event-stream"

type executionRequest struct {
	AuthID          string
	AuthProvider    string
	Model           string
	SourceFormat    string
	Format          string
	Alt             string
	StorageJSON     []byte
	Payload         []byte
	OriginalRequest []byte
	Metadata        map[string]any
	StreamID        string `json:"stream_id"`
	HostCallbackID  string `json:"host_callback_id,omitempty"`
	parentContext   context.Context
}

func (r *Runtime) execute(raw json.RawMessage, stream bool) (any, error) {
	var req executionRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.Alt != "" || !responsesFormat(req.SourceFormat) || !responsesFormat(req.Format) {
		return nil, failure(http.StatusBadRequest, "unsupported_executor_route", "BPS supports the Responses endpoint")
	}
	if stream && (req.StreamID == "" || r.host == nil) {
		return nil, failure(http.StatusBadRequest, "invalid_request", "the CPA stream bridge is required")
	}
	if req.AuthProvider != provider || req.AuthID == "" {
		return nil, failure(http.StatusUnauthorized, "bps_credential_unavailable", "a selected BPS credential is required")
	}
	cfg, ok := r.begin()
	if !ok {
		return nil, failure(http.StatusServiceUnavailable, "bps_plugin_unavailable", "BPS plugin is disabled or shutting down")
	}
	payload, tools, err := buildPayload(req, cfg, stream)
	if err != nil {
		r.tasks.Done()
		return nil, err
	}
	var bridge *bpsprotocol.Bridge
	prepared, documentErr := r.prepareDocuments(cfg, req, payload)
	if documentErr != nil {
		r.tasks.Done()
		return nil, documentErr
	}
	payload = prepared
	if cfg.OAuthPreset == "bps" {
		if cfg.BPSToolTransport == "auto" || cfg.BPSToolTransport == "" {
			mode, key, buffered, selectErr := r.selectAutoTransport(req, cfg, payload)
			if selectErr != nil {
				r.tasks.Done()
				return nil, failure(http.StatusBadRequest, "bps_invalid_tool_catalog", "BPS automatic transport could not validate this request")
			}
			if buffered {
				return r.executeAutoBuffered(cfg, req, payload, tools, key, mode, stream)
			}
			cfg.BPSToolTransport = mode
		}
		payload, bridge, err = bpsprotocol.PrepareWithOptions(payload, bpsCallerScope(req), bpsProtocolOptions(cfg))
		if err != nil {
			r.tasks.Done()
			return nil, protocolError(err)
		}
	}
	if cfg.OAuthPreset == "bps" && cfg.BPSToolTransport == "native" && cfg.NativeToolRepairAttempts > 0 {
		return r.executeNativeBuffered(cfg, req, payload, tools, bridge, stream)
	}
	response, ctx, finishOpen, err := r.open(cfg, req, payload, stream)
	if err != nil {
		r.tasks.Done()
		return nil, err
	}
	finish := func() {
		response.Body.Close()
		finishOpen()
		r.tasks.Done()
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if stream && mediaType == eventStream {
		go r.forward(ctx, req.AuthID, req.StreamID, tools, bridge, response, finish)
		return map[string]any{"headers": http.Header{"Content-Type": {eventStream}}}, nil
	}
	defer finish()
	if stream {
		return nil, interrupted("bps_stream_interrupted", "the upstream did not establish a Responses event stream")
	}
	var body []byte
	var readErr error
	if mediaType == eventStream {
		body, readErr = upstream.ReadEvents(ctx, response.Body, nil)
	} else {
		body, readErr = upstream.ReadJSON(response.Body)
	}
	if readErr != nil {
		return nil, resultError(ctx, readErr, "bps_stream_interrupted")
	}
	if bridge != nil {
		body, err = bridge.Transform(body)
		if err != nil {
			return nil, protocolError(err)
		}
	}
	if err := tools.ValidateResponse(body); err != nil {
		return nil, resultError(ctx, err, "bps_stream_interrupted")
	}
	r.observeUsage(req.AuthID, body)
	return map[string]any{
		"Payload": body,
		"Headers": http.Header{"Content-Type": {"application/json"}},
	}, nil
}

func responsesFormat(format string) bool {
	switch format {
	case "", "openai-response", "codex":
		return true
	default:
		return false
	}
}

func buildPayload(req executionRequest, cfg config, stream bool) ([]byte, compat.ToolCatalog, error) {
	var tools compat.ToolCatalog
	model := cfg.upstreamModel(req.Model)
	if model == "" {
		return nil, tools, failure(http.StatusNotFound, "model_not_found", "the requested BPS model is unavailable")
	}
	raw := req.Payload
	if len(raw) == 0 {
		raw = req.OriginalRequest
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil || body == nil {
		return nil, tools, failure(http.StatusBadRequest, "invalid_request", "the Responses payload must be a JSON object")
	}
	if hasPreviousResponse(body["previous_response_id"]) {
		return nil, tools, failure(http.StatusBadRequest, "bps_requires_full_history", "send the full conversation inline")
	}
	if historyErr := validateInlineHistory(body["input"]); historyErr != nil {
		return nil, tools, historyErr
	}
	if input, ok := body["input"]; ok {
		body["input"] = normalizeContent(input)
	}
	if err := compat.PrepareHistory(body, cfg.OmitEncryptedHistory); err != nil {
		return nil, tools, failure(http.StatusBadRequest, "bps_invalid_history", "the Responses history has an invalid or unsupported item")
	}
	tools, err := compat.DeclaredTools(body)
	if err != nil {
		return nil, tools, failure(http.StatusBadRequest, "bps_invalid_tool_catalog", "the Responses tool catalog is invalid")
	}
	delete(body, "previous_response_id")
	body["model"], _ = json.Marshal(model)
	body["stream"], _ = json.Marshal(stream)
	if cfg.FastMode {
		body["service_tier"] = json.RawMessage(`"priority"`)
	} else {
		delete(body, "service_tier")
	}
	if cfg.OAuthPreset == "bps" {
		fillBPSHostSession(body, req.Metadata)
		// The original plugin supports inline upstream compaction. Keep the
		// established default unchanged; operators may opt into its threshold.
		if cfg.BPSCompactThreshold > 0 && (len(body["context_management"]) == 0 || string(body["context_management"]) == "null") {
			body["context_management"], _ = json.Marshal([]any{map[string]any{"type": "compaction", "compact_threshold": cfg.BPSCompactThreshold}})
		}
	}
	payload, err := json.Marshal(body)
	return payload, tools, err
}

func bpsProtocolOptions(cfg config) bpsprotocol.Options {
	mode := cfg.BPSToolTransport
	// Quality probes and requests without client tools need no transport trial.
	if mode == "" || mode == "auto" {
		mode = "text"
	}
	return bpsprotocol.Options{ToolTransport: mode, ToolsVersionID: cfg.BPSToolsVersionID}
}

func bpsCallerScope(req executionRequest) string {
	raw, _ := json.Marshal([]any{req.AuthID, req.Metadata["caller_scope"]})
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

// fillBPSHostSession follows CPA's explicit-header authority over body cache
// hints. Other canonical identities only fill a missing body identity. Only a
// digest reaches PrepareWithTextTools, which scopes it by account/caller/model.
func fillBPSHostSession(body map[string]json.RawMessage, metadata map[string]any) {
	sessionID, _ := metadata["canonical_session_id"].(string)
	sessionID = strings.TrimSpace(sessionID)
	// CPA explicitly reserves LCP identities for local routing, not provider
	// conversations. Leave those requests on the existing body-history fallback.
	if sessionID == "" || strings.HasPrefix(sessionID, "lcp:") {
		return
	}
	hasID := func(raw json.RawMessage) bool {
		var value string
		return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
	}
	if !isBPSHeaderSession(sessionID) {
		for _, key := range []string{"prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
			if hasID(body[key]) {
				return
			}
		}
		var clientMetadata map[string]json.RawMessage
		if json.Unmarshal(body["client_metadata"], &clientMetadata) == nil {
			if hasID(clientMetadata["session_id"]) || hasID(clientMetadata["sessionId"]) {
				return
			}
		}
	}
	digest := sha256.Sum256([]byte("cpa-bps:host-session:v1\x00" + sessionID))
	body["prompt_cache_key"], _ = json.Marshal(hex.EncodeToString(digest[:]))
}

func isBPSHeaderSession(sessionID string) bool {
	// CPA v8.0.75 ExtractSessionInfo creates these namespaces only from stable
	// client session headers. claude/task/conv/thread also have body sources;
	// clientreq denotes a request ID. None of those may override a body identity.
	for _, prefix := range []string{"header:", "codex:", "agy:", "affinity:", "slot:"} {
		if strings.HasPrefix(sessionID, prefix) && len(sessionID) > len(prefix) {
			return true
		}
	}
	return false
}

func hasPreviousResponse(value json.RawMessage) bool {
	var id string
	if len(value) == 0 {
		return false
	}
	return json.Unmarshal(value, &id) != nil || strings.TrimSpace(id) != ""
}

func validateInlineHistory(value json.RawMessage) *rpcError {
	var input []struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
	}
	if err := json.Unmarshal(value, &input); err != nil {
		// String input and other validation are handled by the Responses upstream.
		return nil
	}
	calls := make(map[string]string)
	for _, item := range input {
		switch item.Type {
		case "item_reference":
			return failure(http.StatusBadRequest, "bps_requires_full_history", "send the full conversation inline")
		case "function_call", "custom_tool_call":
			if item.CallID != "" {
				calls[item.CallID] = item.Type
			}
		case "function_call_output", "custom_tool_call_output":
			if calls[item.CallID] != strings.TrimSuffix(item.Type, "_output") {
				return failure(http.StatusBadRequest, "missing_tool_call", "send the preceding tool call along with its result")
			}
		}
	}
	return nil
}

func (r *Runtime) open(cfg config, req executionRequest, payload []byte, stream bool) (*http.Response, context.Context, func(), error) {
	token, authErr := resolveToken(req.StorageJSON, cfg.ResponsesURL, cfg.OAuth)
	if authErr != nil {
		return nil, nil, nil, authErr
	}
	parent := r.ctx
	if req.parentContext != nil {
		parent = req.parentContext
	}
	wait, cancelWait := context.WithTimeout(parent, executionWindow)
	if quotaErr := r.beforeQuotaDispatch(wait, cfg, req.AuthID, req.StorageJSON, token); quotaErr != nil {
		cancelWait()
		return nil, nil, nil, quotaErr
	}
	limit, limitErr := r.accountLimit(cfg, req.AuthID)
	if limitErr != nil {
		cancelWait()
		return nil, nil, nil, failure(http.StatusServiceUnavailable, "bps_concurrency_policy_unavailable", "BPS account concurrency policy could not be loaded")
	}
	release, waitErr := r.queue.Acquire(wait, req.AuthID, limit)
	cancelWait()
	if waitErr != nil {
		switch {
		case errors.Is(waitErr, context.DeadlineExceeded):
			return nil, nil, nil, failure(http.StatusRequestTimeout, "bps_concurrency_wait_timeout", "the BPS account queue timed out")
		case errors.Is(waitErr, context.Canceled):
			return nil, nil, nil, failure(499, "bps_concurrency_wait_canceled", "the BPS account queue was canceled")
		case errors.Is(waitErr, admission.ErrLimitChanged):
			return nil, nil, nil, failure(http.StatusServiceUnavailable, "bps_concurrency_policy_unavailable", "the previous BPS concurrency policy is still draining")
		default:
			return nil, nil, nil, failure(http.StatusServiceUnavailable, "bps_concurrency_policy_unavailable", "BPS account admission failed")
		}
	}
	ctx, cancel := context.WithTimeout(parent, executionWindow)
	finish := func() {
		cancel()
		release()
	}
	// A queued request must honor exhaustion discovered while it was waiting.
	if quotaErr := r.beforeQuotaDispatch(ctx, cfg, req.AuthID, req.StorageJSON, token); quotaErr != nil {
		finish()
		return nil, nil, nil, quotaErr
	}
	if cfg.FilesURL != "" || cfg.OAuthPreset == "bps" {
		var rewritten []byte
		var err error
		if cfg.OAuthPreset == "bps" {
			rewritten, err = r.client.RewriteBPSImages(ctx, "https://bps.openai.com/basispoints/api/attachments", token, payload)
		} else {
			rewritten, err = r.client.RewriteInlineImages(ctx, cfg.FilesURL, token, payload)
		}
		if err != nil {
			finish()
			switch {
			case errors.Is(err, upstream.ErrUnsupportedBPSFile):
				return nil, nil, nil, failure(http.StatusBadRequest, "bps_unsupported_file_content", "BPS supports image attachments, not input_file document content")
			case errors.Is(err, upstream.ErrInvalidInlineImage):
				return nil, nil, nil, failure(http.StatusBadRequest, "bps_invalid_inline_image", "inline image format is invalid or unsupported")
			case errors.Is(err, upstream.ErrInlineImageLimit):
				return nil, nil, nil, failure(http.StatusRequestEntityTooLarge, "bps_inline_image_limit", "inline images exceed the supported limits")
			default:
				return nil, nil, nil, interrupted("bps_image_upload_failed", "an inline image could not be uploaded to the configured Files API")
			}
		}
		payload = rewritten
	}
	response, doErr := r.dispatchResponse(ctx, cfg, req.StorageJSON, token, payload, stream)
	if doErr != nil {
		executionErr := resultError(ctx, doErr, "bps_transport_failed_no_replay")
		finish()
		return nil, nil, nil, executionErr
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		diagnostic := upstream.DescribeRejection(response)
		response.Body.Close()
		rejected := r.afterQuotaRejection(ctx, cfg, req.AuthID, req.StorageJSON, token, diagnostic)
		finish()
		return nil, nil, nil, rejected
	}
	return response, ctx, finish, nil
}

func (r *Runtime) dispatchResponse(ctx context.Context, cfg config, raw []byte, token string, payload []byte, stream bool) (*http.Response, error) {
	accountID := bpsExecutionAccount(cfg, raw, token)
	if cfg.OAuthPreset == "bps" && cfg.BPSToolTransport == "native" {
		return r.client.DoNativeBPS(ctx, token, accountID, payload)
	}
	return r.client.DoForAccount(ctx, cfg.ResponsesURL, token, accountID, payload, stream || cfg.OAuthPreset == "bps")
}

func bpsExecutionAccount(cfg config, raw []byte, token string) string {
	if cfg.OAuthPreset != "bps" {
		return ""
	}
	accountID, _ := upstream.BPSProfile(token)
	if accountID == "" {
		if auth, ok := credentialFromStorage(raw); ok && auth.Grant != nil {
			accountID, _ = upstream.BPSProfile(auth.Grant.IDToken)
		}
	}
	return accountID
}

func (r *Runtime) forward(ctx context.Context, authID, streamID string, tools compat.ToolCatalog, bridge *bpsprotocol.Bridge, response *http.Response, finish func()) {
	defer finish()
	var downstreamClosed bool
	emit := compat.GuardStream(tools, func(frame []byte) error {
		callErr := r.host("host.stream.emit", map[string]any{"stream_id": streamID, "payload": frame}, nil)
		downstreamClosed = callErr != nil
		return callErr
	})
	if bridge != nil {
		emit = bridge.WrapEmitter(emit)
	}
	body, err := upstream.ReadEvents(ctx, response.Body, emit)
	if err == nil && bridge != nil {
		body, err = bridge.Transform(body)
	}
	if err == nil {
		r.observeUsage(authID, body)
	}
	if downstreamClosed {
		return
	}
	request := map[string]any{"stream_id": streamID}
	if err != nil && !errors.Is(err, upstream.ErrUpstreamEvent) {
		detail := resultError(ctx, err, "bps_stream_interrupted")
		request["error_detail"] = map[string]any{
			"code": detail.Code(), "http_status": detail.StatusCode(), "message": detail.Error(),
		}
	}
	_ = r.host("host.stream.close", request, nil)
}

func protocolError(err error) error {
	var detail interface {
		Code() string
		StatusCode() int
	}
	if errors.As(err, &detail) {
		return failure(detail.StatusCode(), detail.Code(), "BPS request or response does not satisfy the protocol contract")
	}
	return interrupted("bps_protocol_error", "BPS request or response conversion failed")
}

func resultError(ctx context.Context, err error, publicCode string) *rpcError {
	var bpsError *bpsprotocol.Error
	if errors.As(err, &bpsError) {
		return interrupted(bpsError.Code(), bpsError.Error())
	}
	switch {
	case errors.Is(err, compat.ErrInvalidToolResponse):
		return interrupted("bps_invalid_tool_response", "the upstream returned a tool call outside the request contract")
	case errors.Is(err, upstream.ErrInvalidToken):
		return failure(http.StatusUnauthorized, "bps_credential_unavailable", "the configured BPS credential is invalid")
	case errors.Is(ctx.Err(), context.Canceled):
		return failure(499, "connection_lifecycle", "BPS execution was canceled")
	default:
		return interrupted(publicCode, "the upstream did not return a complete Responses result")
	}
}

func interrupted(publicCode, message string) *rpcError {
	// CPA recognizes this internal code and excludes uncertain executions from replay.
	err := failure(http.StatusBadGateway, "stream_truncated", message)
	err.PublicCode = publicCode
	return err
}
