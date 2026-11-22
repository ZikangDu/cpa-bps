package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestSSERejectionRetainsOnlyAllowlistedDiagnostic(t *testing.T) {
	for _, tc := range []struct {
		name, event, code string
		status            int
	}{
		{"flat_rate", `{"type":"error","code":"rate_limit_exceeded","message":"SECRET account","param":"SECRET input","id":"event-safe","sequence_number":4}`, "bps_rate_limited", 429},
		{"nested_quota", `{"type":"error","error":{"code":"insufficient_quota","message":"SECRET token","param":"SECRET input"}}`, "bps_quota_exhausted", 429},
		{"response_error", `{"type":"response.error","error":{"type":"rate_limit_error","message":"SECRET token"}}`, "bps_rate_limited", 429},
		{"response_error_envelope", `{"type":"response.error","response":{"id":"resp-safe","status":"failed","error":{"code":"rate_limit_exceeded","message":"SECRET token"}}}`, "bps_rate_limited", 429},
		{"failed_quota", `{"type":"response.failed","sequence_number":5,"response":{"id":"resp-safe","status":"failed","output":[],"error":{"code":"credits_exhausted","message":"SECRET identity"}}}`, "bps_quota_exhausted", 429},
		{"nested_credentials", `{"type":"error","error":{"code":"invalid_token","message":"SECRET token"}}`, "bps_reauthorization_required", 401},
		{"unknown_code", `{"type":"error","code":"SECRET-code","message":"rate_limit_exceeded SECRET"}`, "bps_upstream_rejected", 502},
		{"human_text", `{"type":"error","message":"insufficient_quota SECRET","status":429}`, "bps_upstream_rejected", 502},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var frames []string
			raw := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-safe\"}}\n\n" + "data: " + tc.event + "\n\n"
			result, err := ReadEvents(context.Background(), strings.NewReader(raw), func(frame []byte) error {
				frames = append(frames, string(frame))
				return nil
			})
			var rejection *EventRejection
			if !errors.Is(err, ErrUpstreamEvent) || !errors.As(err, &rejection) {
				t.Fatalf("SSE rejection lost typed diagnostic: %v", err)
			}
			if rejection.Diagnostic.Code != tc.code || rejection.Diagnostic.HTTPStatus != tc.status || rejection.Diagnostic.BodyFormat != "sse" {
				t.Fatalf("diagnostic = %+v", rejection.Diagnostic)
			}
			encoded, _ := json.Marshal(rejection)
			if strings.Contains(string(encoded), "SECRET") || strings.Contains(err.Error(), "SECRET") || strings.Contains(string(result), "SECRET") || strings.Contains(strings.Join(frames, ""), "SECRET") {
				t.Fatal("upstream private error text escaped the diagnostic boundary")
			}
			if len(frames) != 2 {
				t.Fatalf("terminal error emitted %d frames; want one prefix and one error", len(frames))
			}
			if tc.name == "flat_rate" && (!strings.Contains(frames[1], `"sequence_number":4`) || !strings.Contains(frames[1], `"id":"event-safe"`)) {
				t.Fatal("error protocol identity or ordering was lost")
			}
			if tc.name == "failed_quota" && (string(result) == "" || !strings.Contains(string(result), `"id":"resp-safe"`) || !strings.Contains(string(result), `"status":"failed"`)) {
				t.Fatal("failed terminal response identity was lost")
			}
		})
	}
}

func TestSSERejectionTypeFromEventName(t *testing.T) {
	var emitted string
	_, err := ReadEvents(context.Background(), strings.NewReader("event: error\ndata: {\"code\":\"rate_limit_exceeded\",\"message\":\"SECRET\"}\n\n"), func(frame []byte) error {
		emitted = string(frame)
		return nil
	})
	var rejection *EventRejection
	if !errors.As(err, &rejection) || rejection.Diagnostic.HTTPStatus != 429 || !strings.Contains(emitted, `"type":"error"`) || strings.Contains(emitted, "SECRET") {
		t.Fatalf("named SSE error was not classified safely: %v, %s", err, emitted)
	}
}

func TestSSEUnknownFailedResponseDoesNotInventQuota(t *testing.T) {
	const raw = "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"unrecognized\",\"message\":\"quota_exceeded\"}}}\n\n"
	body, err := ReadEvents(context.Background(), strings.NewReader(raw), nil)
	if err != nil || !strings.Contains(string(body), `"status":"failed"`) {
		t.Fatalf("unknown terminal response changed its existing contract: %s, %v", body, err)
	}
}

func TestSSERejectionSurvivesDownstreamDisconnectDuringEmission(t *testing.T) {
	for _, event := range []string{
		`{"type":"error","code":"rate_limit_exceeded","message":"SECRET account"}`,
		`{"type":"response.failed","response":{"id":"resp-safe","status":"failed","error":{"code":"insufficient_quota","message":"SECRET account"}}}`,
	} {
		ctx, cancel := context.WithCancel(context.Background())
		disconnected := errors.New("fixture downstream closed")
		_, err := ReadEvents(ctx, strings.NewReader("data: "+event+"\n\n"), func([]byte) error {
			cancel()
			return disconnected
		})
		cancel()
		var rejection *EventRejection
		if !errors.As(err, &rejection) || !errors.Is(err, ErrUpstreamEvent) || !errors.Is(err, disconnected) || rejection.Diagnostic.HTTPStatus != 429 {
			t.Fatalf("delivery failure erased a definitive upstream rejection: %v", err)
		}
		if strings.Contains(err.Error(), "SECRET") {
			t.Fatal("joining the delivery failure leaked upstream error text")
		}
	}
}
