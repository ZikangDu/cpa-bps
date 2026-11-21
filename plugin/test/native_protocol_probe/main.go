// native_protocol_probe compares two bounded tool transports on one explicitly
// supplied test credential. It never selects another account or stores tokens.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
)

const maximumRequests = 4

var fixtureInput = json.RawMessage(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"Call fixture_sum exactly once with a=2 and b=3. After its result is supplied, answer only the resulting number."}]}]`)
var fixtureTools = json.RawMessage(`[{"type":"function","name":"fixture_sum","description":"Add the two fixture integers.","parameters":{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}},"required":["a","b"],"additionalProperties":false}}]`)

type privateInput struct {
	Token         string   `json:"token"`
	IDToken       string   `json:"id_token,omitempty"`
	Model         string   `json:"model"`
	EvidencePath  string   `json:"evidence_path"`
	RequestBudget int      `json:"request_budget,omitempty"`
	Transports    []string `json:"transports,omitempty"`
}

type safeDiagnostic struct {
	Code       string `json:"code"`
	Message    string `json:"message,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
	RetryAfter int    `json:"retry_after_seconds,omitempty"`
}

type step struct {
	NativeShape           map[string]any    `json:"native_relay_shape,omitempty"`
	Stage                 string            `json:"stage"`
	HTTPStatus            int               `json:"http_status,omitempty"`
	Status                string            `json:"status"`
	ActualModel           string            `json:"actual_model,omitempty"`
	ModelVerified         bool              `json:"model_verified"`
	NativeToolNames       []string          `json:"upstream_tool_names,omitempty"`
	ClientToolNames       []string          `json:"client_tool_names,omitempty"`
	ExpectedAnswer        string            `json:"expected_answer,omitempty"`
	AnswerMatches         bool              `json:"answer_matches,omitempty"`
	Diagnostic            *safeDiagnostic   `json:"diagnostic,omitempty"`
	CorrectionAttempts    int               `json:"correction_attempts"`
	CorrectionDiagnostics []*safeDiagnostic `json:"correction_diagnostics,omitempty"`
}

type variant struct {
	Transport string `json:"transport"`
	Status    string `json:"status"`
	Steps     []step `json:"steps"`
}

type report struct {
	StartedAt          string          `json:"started_at"`
	RequestedModel     string          `json:"requested_model"`
	RequestBudget      int             `json:"request_budget"`
	Transports         []string        `json:"transports"`
	GenerationRequests int             `json:"generation_requests"`
	QuotaStatus        string          `json:"quota_status"`
	QuotaDiagnostic    *safeDiagnostic `json:"quota_diagnostic,omitempty"`
	Variants           []variant       `json:"variants"`
	Stopped            bool            `json:"stopped"`
	StopReason         string          `json:"stop_reason,omitempty"`
	Passed             bool            `json:"passed"`
}

type responseBody struct {
	Status string            `json:"status"`
	Model  string            `json:"model"`
	Output []json.RawMessage `json:"output"`
}

type callItem struct {
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	CallID    string          `json:"call_id"`
	Arguments json.RawMessage `json:"arguments"`
}

func identifier(value string) bool {
	if len(value) == 0 || len(value) > 96 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.:-", c)) {
			return false
		}
	}
	return true
}

func safeIdentifier(value string, secrets ...string) string {
	if !identifier(value) {
		return ""
	}
	for _, secret := range secrets {
		if secret != "" && strings.Contains(value, secret) {
			return ""
		}
	}
	return value
}

func diagnosticCode(err error) string {
	var coded interface{ Code() string }
	if errors.As(err, &coded) && identifier(coded.Code()) {
		return coded.Code()
	}
	return "protocol_validation_failed"
}

func quotaReady(result upstream.BPSQuotaResult, now time.Time) bool {
	observed, err := time.Parse(time.RFC3339Nano, result.ObservedAt)
	if err != nil || result.Source != "active_usage" || result.Stale || result.Warning != "" || now.Sub(observed) > 30*time.Second || observed.After(now.Add(time.Second)) {
		return false
	}
	var data struct {
		Groups []struct {
			Buckets []struct {
				Remaining *float64 `json:"remainingFraction"`
			} `json:"buckets"`
		} `json:"groups"`
	}
	if json.Unmarshal(result.Quota, &data) != nil {
		return false
	}
	count := 0
	for _, group := range data.Groups {
		for _, bucket := range group.Buckets {
			if bucket.Remaining == nil || *bucket.Remaining <= 0 || *bucket.Remaining > 1 {
				return false
			}
			count++
		}
	}
	return count > 0
}

func sourcePayload(model string, history []json.RawMessage, choice string) []byte {
	body := map[string]any{"model": model, "input": history, "tools": fixtureTools, "tool_choice": choice,
		"reasoning": map[string]string{"effort": "low"}, "stream": true, "store": false}
	raw, _ := json.Marshal(body)
	return raw
}

func names(items []json.RawMessage, secrets ...string) []string {
	out := []string{}
	for _, raw := range items {
		var item callItem
		if json.Unmarshal(raw, &item) != nil || item.Type != "function_call" && item.Type != "custom_tool_call" {
			continue
		}
		name := item.Name
		if item.Namespace != "" {
			name = item.Namespace + "." + name
		}
		if value := safeIdentifier(name, secrets...); value != "" {
			out = append(out, value)
		}
		if len(out) == 8 {
			break
		}
	}
	return out
}

func protocolDiagnostic(err error, input privateInput) *safeDiagnostic {
	diagnostic := &safeDiagnostic{Code: diagnosticCode(err)}
	var problem *bpsprotocol.Error
	if errors.As(err, &problem) {
		// BPS protocol errors are fixed reason identifiers and JSON offsets;
		// raw transport errors and upstream bodies never enter this field.
		message := problem.Error()
		for _, secret := range []string{input.Token, input.IDToken} {
			if secret != "" {
				message = strings.ReplaceAll(message, secret, "[redacted]")
			}
		}
		message = strings.ReplaceAll(strings.ReplaceAll(message, "\r", " "), "\n", " ")
		if len(message) > 512 {
			message = message[:512]
		}
		diagnostic.Message = message
	}
	return diagnostic
}

func selectedTransports(input privateInput) ([]string, bool) {
	if len(input.Transports) == 0 {
		return []string{"native", "text"}, true
	}
	if len(input.Transports) > 2 {
		return nil, false
	}
	seen := map[string]bool{}
	for _, transport := range input.Transports {
		if transport != "native" && transport != "text" || seen[transport] {
			return nil, false
		}
		seen[transport] = true
	}
	return append([]string(nil), input.Transports...), true
}

func request(ctx context.Context, client *upstream.Client, input privateInput, accountID, transport, scope, stage string, history []json.RawMessage, choice string, remaining *int, evidence *report) (responseBody, step, bool) {
	info := step{Stage: stage, Status: "failed"}
	wire, bridge, err := bpsprotocol.PrepareWithOptions(sourcePayload(input.Model, history, choice), scope, bpsprotocol.Options{ToolTransport: transport})
	if err != nil {
		info.Diagnostic = protocolDiagnostic(err, input)
		return responseBody{}, info, false
	}
	for attempt := 0; ; attempt++ {
		if ctx.Err() != nil {
			info.Diagnostic = &safeDiagnostic{Code: "probe_canceled"}
			return responseBody{}, info, true
		}
		if *remaining <= 0 {
			info.Diagnostic = &safeDiagnostic{Code: "request_budget_exhausted"}
			return responseBody{}, info, true
		}
		requestCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
		if attempt > 0 {
			info.CorrectionAttempts++
		}
		*remaining--
		evidence.GenerationRequests++
		var response *http.Response
		if transport == "native" {
			response, err = client.DoNativeBPS(requestCtx, input.Token, accountID, wire)
		} else {
			response, err = client.DoForAccount(requestCtx, oauthflow.BPSResponsesURL, input.Token, accountID, wire, true)
		}
		if err != nil {
			cancel()
			info.Diagnostic = &safeDiagnostic{Code: "transport_error_no_replay"}
			return responseBody{}, info, true
		}
		info.HTTPStatus = response.StatusCode
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			d := upstream.DescribeRejection(response)
			response.Body.Close()
			cancel()
			info.Diagnostic = &safeDiagnostic{Code: d.Code, Message: d.Message, HTTPStatus: d.HTTPStatus, RetryAfter: d.RetryAfterSeconds}
			return responseBody{}, info, response.StatusCode == 401 || response.StatusCode == 403 || response.StatusCode == 429
		}
		media, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
		var terminal []byte
		if media == "text/event-stream" {
			terminal, err = upstream.ReadEvents(requestCtx, response.Body, nil)
		} else {
			terminal, err = upstream.ReadJSON(response.Body)
		}
		response.Body.Close()
		cancel()
		if err != nil {
			info.Diagnostic = &safeDiagnostic{Code: "upstream_response_incomplete_no_replay"}
			return responseBody{}, info, true
		}
		var native responseBody
		if json.Unmarshal(terminal, &native) != nil {
			info.Diagnostic = &safeDiagnostic{Code: "invalid_upstream_response"}
			return responseBody{}, info, false
		}
		info.NativeToolNames = names(native.Output, input.Token, input.IDToken)
		info.ActualModel = safeIdentifier(native.Model, input.Token, input.IDToken)
		info.ModelVerified = info.ActualModel == input.Model
		if native.Status != "completed" {
			info.Diagnostic = &safeDiagnostic{Code: "upstream_not_completed"}
			return responseBody{}, info, false
		}
		if info.ActualModel != "" && !info.ModelVerified {
			info.Diagnostic = &safeDiagnostic{Code: "response_model_mismatch"}
			return responseBody{}, info, false
		}
		transformed, transformErr := bridge.Transform(terminal)
		if transformErr != nil {
			info.NativeShape = nativeShape(native.Output)
			diagnostic := protocolDiagnostic(transformErr, input)
			info.Diagnostic = diagnostic
			if transport == "native" && response.StatusCode == http.StatusOK && attempt < 2 {
				if corrected, ok := bpsprotocol.RepairNativePayload(wire, terminal, transformErr); ok {
					info.CorrectionDiagnostics = append(info.CorrectionDiagnostics, diagnostic)
					if *remaining <= 0 {
						info.Diagnostic = &safeDiagnostic{Code: "request_budget_exhausted"}
						return responseBody{}, info, true
					}
					wire = corrected
					continue
				}
			}
			return responseBody{}, info, false
		}
		var result responseBody
		if json.Unmarshal(transformed, &result) != nil || result.Status != "completed" {
			info.Diagnostic = &safeDiagnostic{Code: "invalid_transformed_response"}
			return responseBody{}, info, false
		}
		info.ClientToolNames = names(result.Output, input.Token, input.IDToken)
		info.Diagnostic = nil
		info.Status = "validated"
		return result, info, false
	}
}

func fixtureCall(result responseBody) (string, bool) {
	calls := 0
	callID := ""
	for _, raw := range result.Output {
		var item callItem
		if json.Unmarshal(raw, &item) != nil {
			return "", false
		}
		if item.Type != "function_call" && item.Type != "custom_tool_call" {
			continue
		}
		calls++
		if item.Type != "function_call" || item.Name != "fixture_sum" || item.Namespace != "" || item.CallID == "" || len(item.CallID) > 256 {
			return "", false
		}
		arguments := item.Arguments
		var encoded string
		if json.Unmarshal(arguments, &encoded) == nil {
			arguments = []byte(encoded)
		}
		var values map[string]int
		if json.Unmarshal(arguments, &values) != nil || len(values) != 2 || values["a"] != 2 || values["b"] != 3 {
			return "", false
		}
		callID = item.CallID
	}
	return callID, calls == 1
}

func finalMatches(result responseBody) bool {
	var text strings.Builder
	for _, raw := range result.Output {
		var item struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(raw, &item) != nil || item.Type == "function_call" || item.Type == "custom_tool_call" {
			return false
		}
		if item.Type == "message" {
			for _, part := range item.Content {
				if part.Type == "output_text" {
					text.WriteString(part.Text)
				}
			}
		}
	}
	return strings.TrimSpace(text.String()) == "5"
}

func probe(ctx context.Context, client *upstream.Client, input privateInput, save func(report)) report {
	evidence := report{StartedAt: time.Now().UTC().Format(time.RFC3339), RequestedModel: input.Model, RequestBudget: input.RequestBudget, QuotaStatus: "not_checked", Variants: []variant{}}
	transports, validTransports := selectedTransports(input)
	evidence.Transports = transports
	save(evidence)
	if !validTransports {
		evidence.Stopped, evidence.StopReason = true, "invalid_transports"
		return evidence
	}
	if ctx.Err() != nil {
		evidence.Stopped, evidence.StopReason = true, "probe_canceled"
		return evidence
	}
	accountID, _ := upstream.BPSProfile(input.Token)
	if accountID == "" {
		accountID, _ = upstream.BPSProfile(input.IDToken)
	}
	if accountID == "" {
		evidence.QuotaStatus = "blocked"
		evidence.Stopped = true
		evidence.StopReason = "account_identity_unavailable"
		return evidence
	}
	quotaCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	quota, err := client.BPSQuota(quotaCtx, input.Token, accountID)
	cancel()
	if err != nil || !quotaReady(quota, time.Now()) {
		code := "fresh_positive_quota_unavailable"
		if errors.Is(err, upstream.ErrBPSReauthorization) {
			code = "reauthorization_required"
		}
		if errors.Is(err, upstream.ErrBPSQuotaDenied) {
			code = "quota_access_denied"
		}
		evidence.QuotaStatus = "blocked"
		evidence.QuotaDiagnostic = &safeDiagnostic{Code: code}
		evidence.Stopped = true
		evidence.StopReason = code
		return evidence
	}
	evidence.QuotaStatus = "fresh_positive"
	save(evidence)
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		evidence.Stopped = true
		evidence.StopReason = "probe_identity_unavailable"
		return evidence
	}
	remaining := input.RequestBudget
	for _, transport := range transports {
		variantReport := variant{Transport: transport, Status: "failed", Steps: []step{}}
		var history []json.RawMessage
		_ = json.Unmarshal(fixtureInput, &history)
		scope := "isolated-native-protocol-probe/" + hex.EncodeToString(nonce[:]) + "/" + transport
		first, firstStep, stop := request(ctx, client, input, accountID, transport, scope, "tool_call", history, "required", &remaining, &evidence)
		id, valid := fixtureCall(first)
		if firstStep.Status == "validated" && !valid {
			firstStep.Status = "failed"
			firstStep.Diagnostic = &safeDiagnostic{Code: "fixture_tool_call_mismatch"}
		}
		if firstStep.Status == "validated" {
			firstStep.Status = "passed"
		}
		variantReport.Steps = append(variantReport.Steps, firstStep)
		evidence.Variants = append(evidence.Variants, variantReport)
		save(evidence)
		if !stop && firstStep.Status == "passed" {
			history = append(history, first.Output...)
			// Only the fixed 2+3 fixture is evaluated. No model code is run.
			output, _ := json.Marshal(map[string]string{"type": "function_call_output", "call_id": id, "output": "5"})
			history = append(history, output)
			final, finalStep, stopFinal := request(ctx, client, input, accountID, transport, scope, "tool_result", history, "none", &remaining, &evidence)
			finalStep.ExpectedAnswer = "5"
			finalStep.AnswerMatches = finalStep.Status == "validated" && finalMatches(final)
			if finalStep.AnswerMatches {
				finalStep.Status = "passed"
				variantReport.Status = "passed"
			} else if finalStep.Status == "validated" {
				finalStep.Status = "failed"
				finalStep.Diagnostic = &safeDiagnostic{Code: "fixture_final_answer_mismatch"}
			}
			variantReport.Steps = append(variantReport.Steps, finalStep)
			stop = stopFinal
		}
		evidence.Variants[len(evidence.Variants)-1] = variantReport
		save(evidence)
		if stop {
			evidence.Stopped = true
			evidence.StopReason = "upstream_refusal_or_uncertain_execution"
			if diagnostic := variantReport.Steps[len(variantReport.Steps)-1].Diagnostic; diagnostic != nil && (diagnostic.Code == "request_budget_exhausted" || diagnostic.Code == "probe_canceled") {
				evidence.StopReason = diagnostic.Code
			}
			break
		}
	}
	evidence.Passed = len(evidence.Variants) == len(transports)
	for _, v := range evidence.Variants {
		evidence.Passed = evidence.Passed && v.Status == "passed"
	}
	return evidence
}

func main() {
	var input privateInput
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 256<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(new(any)) != io.EOF || input.Token == "" || !identifier(input.Model) || input.Model == input.Token || input.Model == input.IDToken || !filepath.IsAbs(input.EvidencePath) {
		_, _ = os.Stdout.WriteString("{\"failure\":\"invalid_private_input\"}\n")
		os.Exit(2)
	}
	if _, valid := selectedTransports(input); !valid {
		_, _ = os.Stdout.WriteString("{\"failure\":\"invalid_transports\"}\n")
		os.Exit(2)
	}
	if input.RequestBudget == 0 {
		input.RequestBudget = maximumRequests
	}
	if input.RequestBudget < 1 || input.RequestBudget > maximumRequests {
		_, _ = os.Stdout.WriteString("{\"failure\":\"invalid_request_budget\"}\n")
		os.Exit(2)
	}
	file, err := os.OpenFile(input.EvidencePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		_, _ = os.Stdout.WriteString("{\"failure\":\"evidence_path_unavailable\"}\n")
		os.Exit(2)
	}
	defer file.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	writeFailed := false
	save := func(e report) {
		raw, _ := json.MarshalIndent(e, "", "  ")
		raw = append(raw, '\n')
		if _, err := file.Seek(0, 0); err != nil {
			writeFailed = true
			cancel()
			return
		}
		if err := file.Truncate(0); err != nil {
			writeFailed = true
			cancel()
			return
		}
		if _, err := file.Write(raw); err != nil {
			writeFailed = true
			cancel()
			return
		}
		if err := file.Sync(); err != nil {
			writeFailed = true
			cancel()
		}
	}
	client := upstream.New()
	defer client.CloseIdleConnections()
	result := probe(ctx, client, input, save)
	save(result)
	if writeFailed {
		_, _ = os.Stdout.WriteString("{\"failure\":\"evidence_write_failed\",\"stopped\":true}\n")
		os.Exit(2)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if !result.Passed {
		os.Exit(1)
	}
}

// nativeShape reports only known fixture keys and JSON types, never code,
// arguments, identifiers, ciphertext, references, or arbitrary upstream text.
func nativeShape(items []json.RawMessage) map[string]any {
	for _, raw := range items {
		var item map[string]any
		if json.Unmarshal(raw, &item) != nil || item["type"] != "function_call" {
			continue
		}
		args, ok := item["arguments"].(string)
		if !ok {
			return map[string]any{"outer_arguments_string": false}
		}
		var outer map[string]any
		if json.Unmarshal([]byte(args), &outer) != nil {
			return map[string]any{"outer_json": false}
		}
		code, _ := outer["code"].(string)
		var value map[string]any
		out := map[string]any{"code_json_object": json.Unmarshal([]byte(code), &value) == nil && value != nil}
		refs, _ := outer["references"].([]any)
		out["references_count"] = len(refs)
		out["reference_matches_fixture"] = len(refs) == 1 && refs[0] == "fixture_sum"
		out["envelope_matches_fixture"] = value["name"] == "fixture_sum"
		switch typed := value["arguments"].(type) {
		case map[string]any:
			out["arguments_kind"] = "object"
			value = typed
		case string:
			out["arguments_kind"] = "string"
		default:
			out["arguments_kind"] = "absent_or_other"
		}
		out["argument_count"] = len(value)
		for _, k := range []string{"a", "b"} {
			kind := "missing"
			switch value[k].(type) {
			case float64:
				kind = "number"
			case string:
				kind = "string"
			case map[string]any:
				kind = "object"
			case []any:
				kind = "array"
			}
			out[k+"_type"] = kind
		}
		return out
	}
	return nil
}
