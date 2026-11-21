package adapter

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"cpa-bps-reconstruction/internal/bpsprotocol"
)

func sessionCacheConfig() config {
	return config{OAuthPreset: "bps", Models: []modelConfig{{Name: "fixture", Alias: "fixture"}, {Name: "other-model", Alias: "other-model"}}}
}

func sessionCacheRequest(payload string) executionRequest {
	return executionRequest{
		AuthID: "account-a", Model: "fixture", Payload: []byte(payload),
		Metadata: map[string]any{
			"canonical_session_id": "header:private-client-session", "caller_scope": "caller-a",
			"unrelated_host_metadata": "must-stay-local",
		},
	}
}

func preparedSessionCacheBody(t *testing.T, req executionRequest) map[string]json.RawMessage {
	t.Helper()
	payload, _, err := buildPayload(req, sessionCacheConfig(), true)
	if err != nil {
		t.Fatal(err)
	}
	payload, _, err = bpsprotocol.PrepareWithTextTools(payload, bpsCallerScope(req))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{[]byte("private-client-session"), []byte("canonical_session_id"), []byte("must-stay-local")} {
		if bytes.Contains(payload, forbidden) {
			t.Fatalf("host session metadata leaked into upstream payload: %s", forbidden)
		}
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	return body
}

func TestBPSHostSessionSurvivesHistoryReplacement(t *testing.T) {
	first := sessionCacheRequest(`{"input":[{"role":"user","content":"Original conversation history."}]}`)
	continued := first
	continued.Payload = []byte(`{"input":[{"role":"user","content":"Replacement summary and next question."}]}`)
	before, after := preparedSessionCacheBody(t, first), preparedSessionCacheBody(t, continued)
	if !bytes.Equal(before["prompt_cache_key"], after["prompt_cache_key"]) {
		t.Fatal("replacing history changed the header-derived cache key")
	}
	var beforeMeta, afterMeta map[string]json.RawMessage
	_ = json.Unmarshal(before["metadata"], &beforeMeta)
	_ = json.Unmarshal(after["metadata"], &afterMeta)
	if !bytes.Equal(beforeMeta["task_id"], afterMeta["task_id"]) {
		t.Fatal("replacing history changed the header-derived task ID")
	}
	if bytes.Equal(beforeMeta["turn_id"], afterMeta["turn_id"]) {
		t.Fatal("a stable conversation identity incorrectly froze the turn ID")
	}
	if bytes.Contains(after["input"], []byte("Original conversation history.")) ||
		!bytes.Contains(after["input"], []byte("Replacement summary and next question.")) {
		t.Fatal("session bridging replaced or replayed the supplied history")
	}

	// Before protocol conversion, session bridging is the only body change.
	without := first
	without.Metadata = nil
	withSession, _, _ := buildPayload(first, sessionCacheConfig(), true)
	withoutSession, _, _ := buildPayload(without, sessionCacheConfig(), true)
	var withBody, withoutBody map[string]json.RawMessage
	_ = json.Unmarshal(withSession, &withBody)
	_ = json.Unmarshal(withoutSession, &withoutBody)
	var injected string
	if err := json.Unmarshal(withBody["prompt_cache_key"], &injected); err != nil || len(injected) != 64 {
		t.Fatal("host session was not converted to a bounded digest")
	}
	delete(withBody, "prompt_cache_key")
	if !reflect.DeepEqual(withBody, withoutBody) {
		t.Fatal("session bridging changed fields other than the missing cache key")
	}
}

func TestBPSHostSessionPreservesExplicitBodyIdentity(t *testing.T) {
	for _, identity := range []string{
		`"prompt_cache_key":"body-session"`,
		`"promptCacheKey":"body-session"`,
		`"session_id":"body-session"`,
		`"sessionId":"body-session"`,
		`"client_metadata":{"session_id":"body-session"}`,
		`"client_metadata":{"sessionId":"body-session"}`,
		`"prompt_cache_key":"", "session_id":"body-session"`,
	} {
		t.Run(identity, func(t *testing.T) {
			req := sessionCacheRequest(`{"input":"hello",` + identity + `}`)
			req.Metadata["canonical_session_id"] = "session:body-derived-session"
			without := req
			without.Metadata = nil
			withBody, _, err := buildPayload(req, sessionCacheConfig(), true)
			if err != nil {
				t.Fatal(err)
			}
			withoutBody, _, err := buildPayload(without, sessionCacheConfig(), true)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(withBody, withoutBody) {
				t.Fatal("host session overrode or shadowed an explicit body identity")
			}
			first := preparedSessionCacheBody(t, req)
			req.Metadata = map[string]any{"canonical_session_id": "session:different-body-derived-session", "caller_scope": "caller-a"}
			second := preparedSessionCacheBody(t, req)
			if !bytes.Equal(first["prompt_cache_key"], second["prompt_cache_key"]) {
				t.Fatal("non-header canonical identity took precedence over the explicit body identity")
			}
		})
	}
}

func TestBPSStableHeaderOverridesChangingBodyCacheHints(t *testing.T) {
	for _, prefix := range []string{"header:", "codex:", "agy:", "affinity:", "slot:"} {
		t.Run(prefix, func(t *testing.T) {
			first := sessionCacheRequest(`{"input":"First turn.","prompt_cache_key":"per-turn-hint-one","session_id":"conflicting-body-session-one"}`)
			first.Metadata["canonical_session_id"] = prefix + "private-client-session"
			second := first
			second.Payload = []byte(`{"input":"Replacement summary and next turn.","prompt_cache_key":"per-turn-hint-two","session_id":"conflicting-body-session-two"}`)
			before, after := preparedSessionCacheBody(t, first), preparedSessionCacheBody(t, second)
			if !bytes.Equal(before["prompt_cache_key"], after["prompt_cache_key"]) {
				t.Fatal("per-turn cache hint or body session overrode the stable host header session")
			}
			var beforeMeta, afterMeta map[string]json.RawMessage
			_ = json.Unmarshal(before["metadata"], &beforeMeta)
			_ = json.Unmarshal(after["metadata"], &afterMeta)
			if !bytes.Equal(beforeMeta["task_id"], afterMeta["task_id"]) || bytes.Equal(beforeMeta["turn_id"], afterMeta["turn_id"]) {
				t.Fatal("header session did not preserve task continuity and independent turn identity")
			}
			if !bytes.Contains(after["input"], []byte("Replacement summary and next turn.")) || bytes.Contains(after["input"], []byte("First turn.")) {
				t.Fatal("stable header identity replaced the actual supplied history")
			}
			other := first
			other.Metadata = map[string]any{"canonical_session_id": prefix + "different-session", "caller_scope": "caller-a"}
			otherBody := preparedSessionCacheBody(t, other)
			if bytes.Equal(before["prompt_cache_key"], otherBody["prompt_cache_key"]) {
				t.Fatal("two header sessions with the same body cache hint shared an upstream identity")
			}
		})
	}
}

func TestBPSHeaderPriorityIsLimitedToProvenHeaderNamespaces(t *testing.T) {
	for _, prefix := range []string{"pck:", "session:", "claude:", "task:", "conv:", "thread:", "derived:", "execution:", "clientreq:", "geminicache:", "lcp:v1:", "unknown:"} {
		t.Run(prefix, func(t *testing.T) {
			req := sessionCacheRequest(`{"input":"hello","prompt_cache_key":"body-authority"}`)
			req.Metadata["canonical_session_id"] = prefix + "host-session"
			with, _, err := buildPayload(req, sessionCacheConfig(), true)
			if err != nil {
				t.Fatal(err)
			}
			req.Metadata = nil
			without, _, err := buildPayload(req, sessionCacheConfig(), true)
			if err != nil || !bytes.Equal(with, without) {
				t.Fatalf("non-header canonical namespace %q overrode the body: %v", prefix, err)
			}
		})
	}
}

func TestBPSHostSessionKeepsAccountCallerModelAndSessionIsolated(t *testing.T) {
	base := sessionCacheRequest(`{"input":"shared initial message"}`)
	first := preparedSessionCacheBody(t, base)
	for _, change := range []struct {
		name string
		edit func(*executionRequest)
	}{
		{"account", func(req *executionRequest) { req.AuthID = "account-b" }},
		{"caller", func(req *executionRequest) {
			req.Metadata = map[string]any{"canonical_session_id": "header:private-client-session", "caller_scope": "caller-b"}
		}},
		{"model", func(req *executionRequest) { req.Model = "other-model" }},
		{"session", func(req *executionRequest) {
			req.Metadata = map[string]any{"canonical_session_id": "header:other-session", "caller_scope": "caller-a"}
		}},
	} {
		t.Run(change.name, func(t *testing.T) {
			other := base
			change.edit(&other)
			body := preparedSessionCacheBody(t, other)
			if bytes.Equal(first["prompt_cache_key"], body["prompt_cache_key"]) {
				t.Fatalf("host-derived cache key crossed %s boundary", change.name)
			}
		})
	}
}

func TestBPSHostSessionSurvivesTokenRotationWithinAccount(t *testing.T) {
	req := sessionCacheRequest(`{"input":"same conversation","prompt_cache_key":"body-cache-hint"}`)
	req.StorageJSON = []byte(`{"type":"cpa-bps","access_token":"fixture-old-access","refresh_token":"fixture-old-refresh","expires_at":"2026-11-21T12:00:00Z"}`)
	before := preparedSessionCacheBody(t, req)
	req.StorageJSON = []byte(`{"type":"cpa-bps","access_token":"fixture-new-access","refresh_token":"fixture-new-refresh","expires_at":"2026-11-22T12:00:00Z"}`)
	after := preparedSessionCacheBody(t, req)
	var beforeMeta, afterMeta map[string]json.RawMessage
	_ = json.Unmarshal(before["metadata"], &beforeMeta)
	_ = json.Unmarshal(after["metadata"], &afterMeta)
	if !bytes.Equal(before["prompt_cache_key"], after["prompt_cache_key"]) ||
		!bytes.Equal(beforeMeta["task_id"], afterMeta["task_id"]) {
		t.Fatal("token rotation changed the same account's conversation identity")
	}
	for _, body := range []map[string]json.RawMessage{before, after} {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"fixture-old-access", "fixture-new-access", "fixture-old-refresh", "fixture-new-refresh", "access_token", "refresh_token"} {
			if bytes.Contains(encoded, []byte(forbidden)) {
				t.Fatal("credential storage leaked into the prepared conversation payload")
			}
		}
	}
	req.AuthID = "account-b"
	other := preparedSessionCacheBody(t, req)
	var otherMeta map[string]json.RawMessage
	_ = json.Unmarshal(other["metadata"], &otherMeta)
	if bytes.Equal(after["prompt_cache_key"], other["prompt_cache_key"]) ||
		bytes.Equal(afterMeta["task_id"], otherMeta["task_id"]) {
		t.Fatal("credential storage reuse collapsed distinct account identities")
	}
}

func TestBPSHostSessionLeavesGatewayAndRoutingOnlyIdentitiesUnchanged(t *testing.T) {
	for _, preset := range []string{"", "custom"} {
		cfg := sessionCacheConfig()
		cfg.OAuthPreset = preset
		req := sessionCacheRequest(`{"input":"gateway request"}`)
		with, _, err := buildPayload(req, cfg, false)
		if err != nil {
			t.Fatal(err)
		}
		req.Metadata = nil
		without, _, err := buildPayload(req, cfg, false)
		if err != nil || !bytes.Equal(with, without) {
			t.Fatalf("non-BPS preset %q received a host session change: %v", preset, err)
		}
	}
	for _, metadata := range []map[string]any{
		nil,
		{"canonical_session_id": "  "},
		{"canonical_session_id": 42},
		{"canonical_session_id": "lcp:v1:routing-only"},
		{"lcp_affinity_session_id": "lcp:v1:routing-only"},
	} {
		req := sessionCacheRequest(`{"input":"existing history fallback"}`)
		req.Metadata = metadata
		payload, _, err := buildPayload(req, sessionCacheConfig(), false)
		if err != nil {
			t.Fatal(err)
		}
		var body map[string]json.RawMessage
		_ = json.Unmarshal(payload, &body)
		if _, exists := body["prompt_cache_key"]; exists {
			t.Fatalf("non-conversation host identity was injected: %+v", metadata)
		}
	}
}
