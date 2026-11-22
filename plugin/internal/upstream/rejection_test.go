package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestDescribeRejectionOnlyUsesRecognizedCodes(t *testing.T) {
	for _, tt := range []struct {
		name, body, code string
		status           int
	}{
		{"cloudflare", `{"cloudflare_error":true,"error_code":1010,"detail":"SECRET@example.invalid"}`, "bps_upstream_access_blocked", 403},
		{"cloudflare_string", `{"cloudflare_error":true,"error_code":"1010"}`, "bps_upstream_access_blocked", 403},
		{"bare_1010_is_unknown", `{"error_code":1010,"detail":"The site owner blocked a browser"}`, "bps_upstream_rejected", 403},
		{"expired", `{"error":{"code":"token_expired","message":"SECRET@example.invalid"}}`, "bps_reauthorization_required", 403},
		{"invalid_grant", `{"error":"invalid_grant","error_description":"SECRET"}`, "bps_reauthorization_required", 400},
		{"scope", `{"error":{"code":"insufficient_scope"}}`, "bps_insufficient_scope", 403},
		{"detail_scope", `{"detail":{"code":"insufficient_scope","message":"SECRET"}}`, "bps_insufficient_scope", 403},
		{"detail_error_blocked", `{"detail":{"error":{"code":"user_blocked","message":"SECRET"}}}`, "bps_user_blocked", 403},
		{"detail_string_error_blocked", `{"detail":{"error":"bps_user_blocked","message":"SECRET"}}`, "bps_user_blocked", 403},
		{"detail_text_not_code", `{"detail":"user_blocked"}`, "bps_upstream_rejected", 403},
		{"workspace", `{"error":{"code":"account_id_mismatch"}}`, "bps_account_context_mismatch", 403},
		{"account", `{"code":"account_deactivated"}`, "bps_account_unavailable", 403},
		{"permission", `{"code":"workspace_access_denied"}`, "bps_account_access_denied", 403},
		{"quota", `{"error":{"code":"insufficient_quota"}}`, "bps_quota_exhausted", 429},
		{"rate", `{"error":{"type":"rate_limit_error"}}`, "bps_rate_limited", 429},
		{"unauthorized", `{}`, "bps_credential_rejected", 401},
		{"model", `{"code":"model_not_supported"}`, "bps_model_unavailable", 404},
		{"unknown", `{"error":{"code":"PRIVATE_TOKEN","message":"invalid_token for SECRET"}}`, "bps_upstream_rejected", 403},
		{"human_text_not_a_code", `{"message":"insufficient_scope"}`, "bps_upstream_rejected", 403},
		{"html", `<html>Cloudflare 1010 SECRET</html>`, "bps_upstream_rejected", 403},
		{"malformed", `{"code":"insufficient_scope","broken":`, "bps_upstream_rejected", 403},
		{"multiple_documents", `{"code":"insufficient_scope"}{"code":"invalid_token"}`, "bps_upstream_rejected", 403},
		{"redirect", `{}`, "bps_upstream_rejected", 302},
	} {
		t.Run(tt.name, func(t *testing.T) {
			response := &http.Response{StatusCode: tt.status, Header: http.Header{"X-Private-Header": {"SECRET"}},
				Body: io.NopCloser(strings.NewReader(tt.body))}
			diagnostic := DescribeRejection(response)
			response.Body.Close()
			status := tt.status
			if status < 400 {
				status = 502
			}
			if diagnostic.Code != tt.code || diagnostic.HTTPStatus != status {
				t.Fatalf("got %+v, want %d %s", diagnostic, status, tt.code)
			}
			raw, err := json.Marshal(diagnostic)
			if err != nil || strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "PRIVATE_TOKEN") {
				t.Fatal("upstream private text escaped the diagnostic allowlist")
			}
		})
	}
	if got := DescribeRejection(nil); got.HTTPStatus != 502 || got.Code != "bps_upstream_rejected" {
		t.Fatal("nil response did not produce a bounded generic failure")
	}
}

func TestRejectionShapeNeverContainsUnknownKeysOrValues(t *testing.T) {
	body := `{"SECRET_KEY":"SECRET_VALUE","detail":{"error":{"code":"SECRET_CODE","message":"SECRET_MESSAGE"}},"error":{"type":"SECRET_TYPE"}}`
	response := &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader(body))}
	defer response.Body.Close()
	got := DescribeRejection(response)
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET") || got.BodyFormat != "json" ||
		got.DiagnosticShape["detail.error.code"] != "string" || got.DiagnosticShape["error.type"] != "string" {
		t.Fatalf("invalid safe diagnostic shape: %s", raw)
	}
}

func TestExactSecurityRejectionRequiresFullMessageAndEnvelope(t *testing.T) {
	known := "Your request was blocked by our security systems. Please try again later."
	for _, tt := range []struct {
		name, message, kind string
		status              int
		code                any
		want                string
	}{
		{"your", known, "invalid_request_error", 403, nil, "bps_upstream_security_blocked"},
		{"this", "This request was blocked by our security systems. Please try again later.", "invalid_request_error", 403, "", "bps_upstream_security_blocked"},
		{"prefixed", "SECRET " + known, "invalid_request_error", 403, nil, "bps_upstream_rejected"},
		{"suffixed", known + " SECRET", "invalid_request_error", 403, nil, "bps_upstream_rejected"},
		{"whitespace", known + "\n", "invalid_request_error", 403, nil, "bps_upstream_rejected"},
		{"wrong_type", known, "permission_error", 403, nil, "bps_upstream_rejected"},
		{"wrong_status", known, "invalid_request_error", 400, nil, "bps_upstream_rejected"},
		{"nonempty_code", known, "invalid_request_error", 403, "unknown", "bps_upstream_rejected"},
		{"wrong_code_type", known, "invalid_request_error", 403, map[string]any{}, "bps_upstream_rejected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": tt.message, "type": tt.kind, "code": tt.code}})
			response := &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(string(raw)))}
			defer response.Body.Close()
			got := DescribeRejection(response)
			if got.Code != tt.want || got.HTTPStatus != tt.status || strings.Contains(got.Message, "SECRET") {
				t.Fatalf("unexpected strict security diagnostic: %+v", got)
			}
		})
	}
}

func TestExactUsagePolicyRejectionDoesNotInferQuotaOrAuthentication(t *testing.T) {
	known := "403: This request was blocked by our usage policy."
	for _, tt := range []struct {
		name, message, kind string
		status              int
		code                any
		want                string
	}{
		{"unknown_type", known, "undocumented_upstream_type", 403, nil, "bps_usage_policy_blocked"},
		{"empty_code", known, "invalid_request_error", 403, "", "bps_usage_policy_blocked"},
		{"prefixed", "SECRET " + known, "invalid_request_error", 403, nil, "bps_upstream_rejected"},
		{"suffixed", known + " SECRET", "invalid_request_error", 403, nil, "bps_upstream_rejected"},
		{"whitespace", known + "\n", "invalid_request_error", 403, nil, "bps_upstream_rejected"},
		{"wrong_status", known, "invalid_request_error", 400, nil, "bps_upstream_rejected"},
		{"nonempty_code", known, "invalid_request_error", 403, "unknown", "bps_upstream_rejected"},
		{"invalid_code_type", known, "invalid_request_error", 403, map[string]any{}, "bps_upstream_rejected"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"error": map[string]any{"message": tt.message, "type": tt.kind, "code": tt.code}})
			response := &http.Response{StatusCode: tt.status, Body: io.NopCloser(strings.NewReader(string(raw)))}
			defer response.Body.Close()
			got := DescribeRejection(response)
			if got.Code != tt.want || got.HTTPStatus != tt.status || strings.Contains(got.Message, "SECRET") {
				t.Fatalf("unexpected strict usage-policy diagnostic: %+v", got)
			}
			if tt.want == "bps_usage_policy_blocked" && !strings.Contains(got.Message, "reset credits cannot remove") {
				t.Fatal("usage policy refusal was represented as a resettable quota error")
			}
		})
	}
}

type observedRejectionBody struct {
	reader io.Reader
	read   int
	closed bool
}

func (b *observedRejectionBody) Read(p []byte) (int, error) {
	n, err := b.reader.Read(p)
	b.read += n
	return n, err
}

func (b *observedRejectionBody) Close() error {
	b.closed = true
	return nil
}

func TestDescribeRejectionBoundsInputAndLeavesBodyOwnershipToCaller(t *testing.T) {
	body := &observedRejectionBody{reader: strings.NewReader(strings.Repeat(" ", maxRejectionBodyBytes) + `{"code":"invalid_token"}`)}
	response := &http.Response{StatusCode: 403, Body: body}
	diagnostic := DescribeRejection(response)
	if body.read != maxRejectionBodyBytes || body.closed || diagnostic.Code != "bps_upstream_rejected" {
		t.Fatalf("invalid read/ownership bounds: read=%d closed=%t code=%s", body.read, body.closed, diagnostic.Code)
	}
	response.Body.Close()
	if !body.closed {
		t.Fatal("caller did not close response")
	}
}

func TestDescribeRejectionChallengeAndRetryAfter(t *testing.T) {
	body := &observedRejectionBody{reader: strings.NewReader("SECRET")}
	response := &http.Response{StatusCode: 403, Header: http.Header{"Cf-Mitigated": {"challenge"}, "Retry-After": {"90"}}, Body: body}
	got := DescribeRejection(response)
	if got.Code != "bps_upstream_challenge" || got.HTTPStatus != 403 || got.RetryAfterSeconds != 0 || body.read != 0 {
		t.Fatalf("challenge classification = %+v; bytes read=%d", got, body.read)
	}
	response.Body.Close()
	now := time.Date(2026, 11, 21, 0, 0, 0, 500_000_000, time.UTC)
	for _, tt := range []struct {
		status int
		value  string
		want   int
	}{
		{429, "12", 12}, {503, " 15 ", 15}, {429, "-1", 0}, {429, "SECRET", 0},
		{429, "2147483648", 0}, {429, now.Add(1500 * time.Millisecond).Format(http.TimeFormat), 2},
		{503, now.Add(-time.Second).Format(http.TimeFormat), 0}, {403, "12", 0},
	} {
		response := &http.Response{StatusCode: tt.status, Header: http.Header{"Retry-After": {tt.value}}}
		got := describeRejection(response, now)
		if got.RetryAfterSeconds != tt.want {
			t.Errorf("status=%d value=%q got=%d want=%d", tt.status, tt.value, got.RetryAfterSeconds, tt.want)
		}
	}
}

func TestRequestValidationRejectionUsesOnlySchemaLocations(t *testing.T) {
	body := `{"detail":[
		{"type":"missing","loc":["body","input",2,"encrypted_content"],"msg":"SECRET_MESSAGE for account@example.invalid","input":{"access_token":"SECRET_TOKEN"},"ctx":{"SECRET_KEY":"SECRET_CONTEXT"}},
		{"type":"union_tag_invalid","loc":["body","input",3,"function_call_output","output",0,"type"],"msg":"SECRET_MESSAGE","input":"SECRET_INPUT"}
	],"error":{"code":"insufficient_quota","message":"SECRET_QUOTA_TEXT"},"SECRET_TOP":"SECRET_VALUE"}`
	response := &http.Response{StatusCode: 422, Header: http.Header{"Retry-After": {"123"}, "X-Private": {"SECRET_HEADER"}}, Body: io.NopCloser(strings.NewReader(body))}
	defer response.Body.Close()
	got := DescribeRejection(response)
	want := "The upstream rejected the request schema. Invalid fields: body.input[2].encrypted_content (missing); body.input[3].function_call_output.output[0].type (union_tag_invalid)."
	if got.Code != "bps_request_validation_failed" || got.HTTPStatus != 422 || got.RetryAfterSeconds != 0 || got.Message != want {
		t.Fatalf("unexpected validation diagnosis: %+v", got)
	}
	raw, _ := json.Marshal(got)
	if strings.Contains(string(raw), "SECRET") || strings.Contains(string(raw), "account@example.invalid") || strings.Contains(string(raw), "access_token") || got.DiagnosticShape["detail"] != "array" {
		t.Fatal("validation diagnostics exposed request or upstream data")
	}
}

func TestExactRequestValidationWrapperRequiresCompleteSignature(t *testing.T) {
	const known = "422: Invalid request body."
	for _, tt := range []struct {
		name, message, kind string
		status              int
		code, param         any
		remove, extra       string
		want                string
	}{
		{name: "exact", message: known, kind: "server_error", status: 422, want: "bps_request_validation_failed"},
		{name: "prefix", message: "SECRET " + known, kind: "server_error", status: 422},
		{name: "suffix", message: known + " SECRET", kind: "server_error", status: 422},
		{name: "whitespace", message: known + "\n", kind: "server_error", status: 422},
		{name: "case", message: "422: invalid request body.", kind: "server_error", status: 422},
		{name: "wrong_status", message: known, kind: "server_error", status: 400},
		{name: "server_status", message: known, kind: "server_error", status: 500},
		{name: "wrong_type", message: known, kind: "invalid_request_error", status: 422},
		{name: "empty_type", message: known, status: 422},
		{name: "nonempty_code", message: known, kind: "server_error", status: 422, code: "SECRET_CODE"},
		{name: "empty_code", message: known, kind: "server_error", status: 422, code: ""},
		{name: "object_code", message: known, kind: "server_error", status: 422, code: map[string]any{"SECRET": true}},
		{name: "nonempty_param", message: known, kind: "server_error", status: 422, param: "SECRET_PARAM"},
		{name: "empty_param", message: known, kind: "server_error", status: 422, param: ""},
		{name: "object_param", message: known, kind: "server_error", status: 422, param: map[string]any{"SECRET": true}},
		{name: "missing_code", message: known, kind: "server_error", status: 422, remove: "code"},
		{name: "missing_param", message: known, kind: "server_error", status: 422, remove: "param"},
		{name: "extra_error_field", message: known, kind: "server_error", status: 422, extra: "error"},
		{name: "extra_top_field", message: known, kind: "server_error", status: 422, extra: "top"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			detail := map[string]any{"message": tt.message, "type": tt.kind, "code": tt.code, "param": tt.param}
			if tt.remove != "" {
				delete(detail, tt.remove)
			}
			body := map[string]any{"error": detail}
			if tt.extra == "error" {
				detail["SECRET_KEY"] = "SECRET_VALUE"
			} else if tt.extra == "top" {
				body["SECRET_KEY"] = "SECRET_VALUE"
			}
			raw, _ := json.Marshal(body)
			response := &http.Response{StatusCode: tt.status, Header: http.Header{"Retry-After": {"120"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}
			defer response.Body.Close()
			got := DescribeRejection(response)
			want := tt.want
			if want == "" {
				want = "bps_upstream_rejected"
			}
			if got.Code != want || got.HTTPStatus != tt.status || got.RetryAfterSeconds != 0 {
				t.Fatalf("unexpected strict validation wrapper diagnosis: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "SECRET") {
				t.Fatal("unknown validation wrapper text leaked")
			}
			if got.Code == "bps_request_validation_failed" && (strings.Contains(got.Message, "Invalid fields:") || strings.Contains(got.Message, "image")) {
				t.Fatal("an uncoded wrapper was attributed to an unproven field")
			}
		})
	}
}

func TestRequestValidationRejectsUnknownOrMalformedDetails(t *testing.T) {
	valid := map[string]any{"type": "missing", "loc": []any{"body", "input", 0, "type"}}
	tooDeep := []any{"body"}
	for range 16 {
		tooDeep = append(tooDeep, "input")
	}
	tooLong := []any{"body"}
	for range 8 {
		tooLong = append(tooLong, "encrypted_content")
	}
	tooMany := make([]any, 129)
	for index := range tooMany {
		tooMany[index] = valid
	}
	for _, tt := range []struct {
		name   string
		detail any
	}{
		{"null", nil},
		{"empty", []any{}},
		{"object", valid},
		{"text", "SECRET missing body.input"},
		{"null_entry", []any{nil}},
		{"text_entry", []any{"SECRET"}},
		{"unknown_code", []any{map[string]any{"type": "SECRET_CODE", "loc": []any{"body", "input"}}}},
		{"non_validation_code", []any{map[string]any{"type": "insufficient_quota", "loc": []any{"body", "input"}}}},
		{"missing_code", []any{map[string]any{"loc": []any{"body", "input"}, "msg": "missing"}}},
		{"object_code", []any{map[string]any{"type": map[string]any{"SECRET": "missing"}, "loc": []any{"body"}}}},
		{"missing_location", []any{map[string]any{"type": "missing"}}},
		{"null_location", []any{map[string]any{"type": "missing", "loc": nil}}},
		{"empty_location", []any{map[string]any{"type": "missing", "loc": []any{}}}},
		{"text_location", []any{map[string]any{"type": "missing", "loc": "body.input"}}},
		{"unknown_root", []any{map[string]any{"type": "missing", "loc": []any{"SECRET_ROOT", "input"}}}},
		{"header_location", []any{map[string]any{"type": "missing", "loc": []any{"header", "authorization"}}}},
		{"unknown_field", []any{map[string]any{"type": "missing", "loc": []any{"body", "metadata", "SECRET_KEY"}}}},
		{"inline_location", []any{map[string]any{"type": "missing", "loc": []any{"body", "input[0]"}}}},
		{"homograph_field", []any{map[string]any{"type": "missing", "loc": []any{"body", "іnput"}}}},
		{"negative_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", -1}}}},
		{"oversized_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", 65536}}}},
		{"fractional_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", 0.5}}}},
		{"null_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", nil}}}},
		{"boolean_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", true}}}},
		{"text_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", "0"}}}},
		{"object_index", []any{map[string]any{"type": "missing", "loc": []any{"body", "input", map[string]any{"SECRET": "index"}}}}},
		{"deep_location", []any{map[string]any{"type": "missing", "loc": tooDeep}}},
		{"long_location", []any{map[string]any{"type": "missing", "loc": tooLong}}},
		{"too_many_errors", tooMany},
		{"mixed_unknown", []any{valid, map[string]any{"type": "SECRET_CODE", "loc": []any{"body", "input"}}}},
		{"mixed_malformed", []any{valid, nil}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"detail": tt.detail})
			response := &http.Response{StatusCode: 422, Header: http.Header{"Retry-After": {"99"}}, Body: io.NopCloser(strings.NewReader(string(raw)))}
			defer response.Body.Close()
			got := DescribeRejection(response)
			if got.Code != "bps_upstream_rejected" || got.HTTPStatus != 422 || got.RetryAfterSeconds != 0 {
				t.Fatalf("unknown validation shape was classified: %+v", got)
			}
			encoded, _ := json.Marshal(got)
			if strings.Contains(string(encoded), "SECRET") {
				t.Fatal("unknown validation data leaked")
			}
		})
	}
}

func TestRequestValidationMessageIsBoundedAndDeduplicated(t *testing.T) {
	entries := []any{}
	for index := range 6 {
		entry := map[string]any{"type": "string_type", "loc": []any{"body", "input", index, "content", 0, "text"}}
		entries = append(entries, entry, entry)
	}
	raw, _ := json.Marshal(map[string]any{"detail": entries})
	response := &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader(string(raw)))}
	defer response.Body.Close()
	got := DescribeRejection(response)
	if got.Code != "bps_request_validation_failed" || strings.Count(got.Message, "body.input[") != 4 ||
		strings.Contains(got.Message, "body.input[4]") || !strings.Contains(got.Message, "Additional validation fields were omitted.") || len(got.Message) > 800 {
		t.Fatalf("validation output was not bounded: %+v", got)
	}
	// The display cap must not prevent validation of later entries.
	entries = append(entries, map[string]any{"type": "PRIVATE_VALUE", "loc": []any{"body", "input"}})
	raw, _ = json.Marshal(map[string]any{"detail": entries})
	response = &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader(string(raw)))}
	defer response.Body.Close()
	if got := DescribeRejection(response); got.Code != "bps_upstream_rejected" {
		t.Fatal("malformed tail was ignored after reaching the display cap")
	}
}

func TestRequestValidationRequires422AndACompleteBoundedBody(t *testing.T) {
	for _, status := range []int{400, 401, 403, 409, 422, 429, 500} {
		response := &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"detail":[{"type":"model_type","loc":["body"],"input":"SECRET"}]}`))}
		got := DescribeRejection(response)
		response.Body.Close()
		if (got.Code == "bps_request_validation_failed") != (status == 422) || got.HTTPStatus != status {
			t.Fatalf("validation classification crossed status semantics: %+v", got)
		}
	}
	for _, body := range []string{
		`{"detail":[{"type":"missing","loc":["body","input"]}]`,
		`{"detail":[{"type":"missing","loc":["body","input"]}]} {"error":"SECRET"}`,
		`{"detail":[{"type":"missing","loc":["body","input",1e0]}]}`,
	} {
		response := &http.Response{StatusCode: 422, Body: io.NopCloser(strings.NewReader(body))}
		got := DescribeRejection(response)
		response.Body.Close()
		if got.Code != "bps_upstream_rejected" {
			t.Fatal("malformed or non-integer validation data was accepted")
		}
	}
	raw, _ := json.Marshal(map[string]any{"detail": []any{map[string]any{"type": "missing", "loc": []any{"body", "input"}, "input": strings.Repeat("S", maxRejectionBodyBytes)}}})
	body := &observedRejectionBody{reader: strings.NewReader(string(raw))}
	got := DescribeRejection(&http.Response{StatusCode: 422, Body: body})
	if got.Code != "bps_upstream_rejected" || got.BodyFormat != "at_limit" || body.read != maxRejectionBodyBytes || body.closed {
		t.Fatal("validation parsing changed response body bounds or ownership")
	}
	body.Close()
}
