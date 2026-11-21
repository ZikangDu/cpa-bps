package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"mime"
	"strings"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/candy"
	"cpa-bps-reconstruction/internal/quality"
	"cpa-bps-reconstruction/internal/upstream"
)

func qualityBenchmarkVersion(cfg quality.Config) string {
	return quality.BenchmarkVersion(cfg)
}

func (r *Runtime) runQualityCheck(ctx context.Context, check quality.Check) quality.Result {
	result := quality.Result{Status: "request_error", BenchmarkVersion: qualityBenchmarkVersion(check.Config)}
	fail := func(code string) quality.Result { result.ErrorCode = code; return result }
	skip := func(code string) quality.Result { result.Status = "skipped"; return fail(code) }
	cfg, ok := r.begin()
	if !ok {
		return skip("bps_plugin_unavailable")
	}
	defer r.tasks.Done()
	if ctx.Err() != nil {
		return skip("bps_quality_canceled")
	}
	entry, selectErr := r.selectedQualityAuth(check.AuthIndex, "")
	if selectErr != nil {
		return skip(selectErr.Code())
	}
	if entry.ID != check.AuthID {
		return skip("bps_quality_account_changed")
	}
	if entry.Disabled || entry.Status == "disabled" {
		return skip("bps_quality_account_paused")
	}
	if qualityPolicyBlocked(entry.StatusMessage) && !check.Manual {
		return fail("bps_usage_policy_blocked")
	}
	model := cfg.upstreamModel(check.Model)
	if model == "" && cfg.OAuthPreset == "bps" && strings.HasSuffix(check.Model, cfg.ModelSuffix) {
		// The quality picker uses this account's native catalog even when normal
		// inference is configured with a narrower list of public aliases.
		model = strings.TrimSuffix(check.Model, cfg.ModelSuffix)
		if !quality.ValidModel(model) {
			model = ""
		}
	}
	if model == "" {
		return skip("bps_model_unavailable")
	}
	// Reuse ordinary account admission, but bound queue + transport + answer to
	// this check's 90-second deadline. Each model makes exactly one POST.
	limit, limitErr := r.accountLimit(cfg, entry.ID)
	if limitErr != nil {
		return skip("bps_concurrency_policy_unavailable")
	}
	release, waitErr := r.queue.Acquire(ctx, entry.ID, limit)
	if waitErr != nil {
		return fail("bps_quality_admission_failed")
	}
	defer release()
	if ctx.Err() != nil {
		return skip("bps_quality_canceled")
	}
	// Admission may have waited: re-read account state and credential immediately
	// before dispatch so a pause, removal or OAuth rotation is respected.
	entry, selectErr = r.selectedQualityAuth(check.AuthIndex, "")
	if selectErr != nil {
		return skip(selectErr.Code())
	}
	if entry.ID != check.AuthID {
		return skip("bps_quality_account_changed")
	}
	if entry.Disabled || entry.Status == "disabled" {
		return skip("bps_quality_account_paused")
	}
	if qualityPolicyBlocked(entry.StatusMessage) && !check.Manual {
		return fail("bps_usage_policy_blocked")
	}
	raw, storageErr := r.qualityStorage(check.AuthIndex, "")
	if storageErr != nil {
		return skip(storageErr.Code())
	}
	auth, valid := credentialFromStorage(raw)
	if !valid {
		return fail("bps_credential_unavailable")
	}
	if auth.Disabled {
		return skip("bps_quality_account_paused")
	}
	token, authErr := resolveToken(raw, cfg.ResponsesURL, cfg.OAuth)
	if authErr != nil {
		return fail(authErr.Code())
	}
	if quotaErr := r.beforeQuotaDispatch(ctx, cfg, entry.ID, raw, token); quotaErr != nil {
		code := quotaErr.PublicCode
		if code == "" {
			code = quotaErr.Code()
		}
		return skip(code)
	}
	payload, _ := json.Marshal(map[string]any{
		"model": model, "input": []any{map[string]any{"role": "user", "content": check.Config.Prompt}},
		"instructions": candy.Instructions, "reasoning": map[string]string{"effort": check.Config.ReasoningEffort},
		"stream": true, "store": false, "tool_choice": "none",
	})
	var bridge *bpsprotocol.Bridge
	if cfg.OAuthPreset == "bps" {
		var prepareErr error
		scope := sha256.Sum256([]byte("quality\x00" + entry.ID + "\x00" + check.RunID))
		payload, bridge, prepareErr = bpsprotocol.PrepareWithOptions(payload, hex.EncodeToString(scope[:]), bpsProtocolOptions(cfg))
		if prepareErr != nil {
			return fail("bps_quality_protocol_error")
		}
	}
	response, doErr := r.dispatchResponse(ctx, cfg, raw, token, payload, true)
	if doErr != nil {
		return fail("bps_transport_failed_no_replay")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		diagnostic := upstream.DescribeRejection(response)
		response.Body.Close()
		quotaErr := r.afterQuotaRejection(ctx, cfg, entry.ID, raw, token, diagnostic)
		code := quotaErr.PublicCode
		if code == "" {
			code = quotaErr.Code()
		}
		return fail(code)
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	var body []byte
	var readErr error
	if mediaType == eventStream {
		body, readErr = upstream.ReadEvents(ctx, response.Body, nil)
	} else {
		body, readErr = upstream.ReadJSON(response.Body)
	}
	if readErr != nil {
		return fail("bps_quality_response_incomplete")
	}
	if bridge != nil {
		var transformErr error
		body, transformErr = bridge.Transform(body)
		if transformErr != nil {
			return fail("bps_quality_protocol_error")
		}
	}
	var parsed qualityResponse
	if json.Unmarshal(body, &parsed) != nil {
		return fail("bps_quality_invalid_response")
	}
	if quality.ValidModel(parsed.Model) {
		result.ActualModel = parsed.Model
		result.ModelMismatch = parsed.Model != model
	}
	result.Usage = parsed.Usage
	if parsed.Status != "completed" {
		return fail("bps_quality_response_incomplete")
	}
	answer, valid := parsed.answer()
	if !valid {
		return fail("bps_quality_invalid_response")
	}
	// Grade the complete answer before bounded storage. Never persist a key that
	// an upstream may reflect, even if it looks like ordinary answer text.
	grade := candy.ParseGrade(answer, check.Config.ExpectedAnswer)
	for _, secret := range []string{token, grantSecret(auth, "refresh"), grantSecret(auth, "id")} {
		if secret != "" {
			answer = strings.ReplaceAll(answer, secret, "[redacted]")
			grade.Answer = strings.ReplaceAll(grade.Answer, secret, "[redacted]")
			if strings.Contains(result.ActualModel, secret) {
				result.ActualModel = ""
				result.ModelMismatch = false
			}
		}
	}
	result.Answer = quality.SafeText(answer, quality.MaxAnswerBytes)
	result.ExtractedAnswer = quality.SafeText(grade.Answer, 128)
	result.ValidForRequestedModel = result.ActualModel != "" && !result.ModelMismatch
	result.Status = grade.Status
	return result
}

func grantSecret(auth credential, kind string) string {
	if auth.Grant == nil {
		return ""
	}
	if kind == "refresh" {
		return auth.Grant.RefreshToken
	}
	return auth.Grant.IDToken
}

type qualityResponse struct {
	Model      string        `json:"model"`
	Status     string        `json:"status"`
	OutputText string        `json:"output_text"`
	Usage      quality.Usage `json:"usage"`
	Output     []struct {
		Type    string `json:"type"`
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
}

func (response qualityResponse) answer() (string, bool) {
	parts := []string{}
	for _, item := range response.Output {
		switch item.Type {
		case "reasoning":
			continue
		case "message":
			if item.Role != "assistant" {
				return "", false
			}
			for _, content := range item.Content {
				if content.Type == "output_text" {
					parts = append(parts, content.Text)
				} else {
					return "", false
				}
			}
		default:
			return "", false // Tool execution is never part of this benchmark.
		}
	}
	if len(parts) > 0 {
		return strings.Join(parts, "\n"), true
	}
	if response.Output == nil && response.OutputText != "" {
		return response.OutputText, true
	}
	return "", false
}
