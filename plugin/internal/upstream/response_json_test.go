package upstream

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestReadResponseJSONRejectsErrorBodiesWithoutExposingPrivateText(t *testing.T) {
	for _, test := range []struct {
		name, raw, code string
		status          int
	}{
		{"bare_rate", `{"error":{"code":"rate_limit_exceeded","message":"SECRET account"}}`, "bps_rate_limited", 429},
		{"failed_quota", `{"id":"resp-fixture","status":"failed","error":{"code":"insufficient_quota","message":"SECRET token"},"output":[]}`, "bps_quota_exhausted", 429},
		{"completed_error_is_not_success", `{"status":"completed","error":{"code":"invalid_token","message":"SECRET token"},"output":[{"type":"function_call","name":"fixture","call_id":"call-fixture","arguments":"{}"}]}`, "bps_reauthorization_required", 401},
		{"unknown_error", `{"error":{"code":"SECRET-CODE","message":"quota_exceeded SECRET"}}`, "bps_upstream_rejected", 502},
		{"error_string", `{"error":"SECRET upstream message"}`, "bps_upstream_rejected", 502},
		{"flat_event", `{"type":"error","code":"rate_limit_exceeded","message":"SECRET account"}`, "bps_rate_limited", 429},
		{"failed_event", `{"type":"response.failed","response":{"status":"failed","error":{"code":"credits_exhausted","message":"SECRET"}}}`, "bps_quota_exhausted", 429},
	} {
		t.Run(test.name, func(t *testing.T) {
			body, err := ReadResponseJSON(strings.NewReader(test.raw))
			var problem *EventRejection
			if body != nil || !errors.As(err, &problem) || !errors.Is(err, ErrUpstreamEvent) || problem.Diagnostic.HTTPStatus != test.status || problem.Diagnostic.Code != test.code || problem.Diagnostic.BodyFormat != "json" {
				t.Fatalf("HTTP-200 JSON error was accepted or misclassified: body=%s err=%v", body, err)
			}
			encoded, _ := json.Marshal(problem)
			if strings.Contains(string(encoded), "SECRET") || strings.Contains(err.Error(), "SECRET") {
				t.Fatal("raw upstream error escaped the diagnostic boundary")
			}
		})
	}
}

func TestReadResponseJSONKeepsTerminalStatusAndRejectsIncompleteSnapshots(t *testing.T) {
	for _, raw := range []string{
		`{"status":"completed","error":null,"output":[]}`,
		`{"status":"failed","output":[]}`,
		`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`,
		`{"output":[]}`,
	} {
		body, err := ReadResponseJSON(strings.NewReader(raw))
		if err != nil || string(body) != raw {
			t.Fatalf("valid terminal or legacy reply changed: %s, %v", body, err)
		}
	}
	for _, raw := range []string{`null`, `[]`, `"text"`, `{"status":null}`, `{"status":42}`, `{"status":""}`} {
		if _, err := ReadResponseJSON(strings.NewReader(raw)); !errors.Is(err, ErrInvalidResponse) {
			t.Fatalf("invalid response shape accepted: %s, %v", raw, err)
		}
	}
	for _, status := range []string{"queued", "in_progress"} {
		if _, err := ReadResponseJSON(strings.NewReader(`{"status":"` + status + `","output":[]}`)); !errors.Is(err, ErrStreamIncomplete) {
			t.Fatalf("nonterminal JSON snapshot accepted: %s, %v", status, err)
		}
	}
	// The schema-neutral reader remains usable by files, catalogs and quota.
	const metadata = `{"status":"active","error":"metadata value"}`
	if body, err := ReadJSON(strings.NewReader(metadata)); err != nil || string(body) != metadata {
		t.Fatalf("generation validation leaked into metadata reads: %s, %v", body, err)
	}
}
