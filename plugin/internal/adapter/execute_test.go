package adapter

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestBuildPayloadNormalizesClientContent(t *testing.T) {
	cfg := config{Models: []modelConfig{{Name: "upstream", Alias: "alias"}}, FastMode: true}
	req := executionRequest{Model: "alias", Payload: []byte(`{
		"model": "alias",
		"service_tier": "default",
		"input": [
			{"type":"message","role":"user","content":["hello",{"type":"image_url","image_url":{"url":"https://fixture.invalid/image.png","detail":"low"}}]},
			{"type":"function_call","call_id":"call_1","name":"sample","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_1","output":["fixture"]},
			{"type":"compaction","encrypted_content":"opaque-fixture"}
		]
	}`)}
	encoded, _, err := buildPayload(req, cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	var output struct {
		Model       string
		ServiceTier string `json:"service_tier"`
		Stream      bool
		Input       []struct {
			Content          []map[string]string
			Output           []map[string]string
			EncryptedContent string `json:"encrypted_content"`
		}
	}
	if err := json.Unmarshal(encoded, &output); err != nil {
		t.Fatal(err)
	}
	if output.Model != "upstream" || output.ServiceTier != "priority" || !output.Stream {
		t.Fatalf("request options were not applied: %s", encoded)
	}
	if part := output.Input[0].Content[0]; part["type"] != "input_text" || part["text"] != "hello" {
		t.Fatalf("incorrect text part: %v", part)
	}
	if part := output.Input[0].Content[1]; part["type"] != "input_image" ||
		part["image_url"] != "https://fixture.invalid/image.png" || part["detail"] != "low" {
		t.Fatalf("incorrect image part: %v", part)
	}
	if part := output.Input[2].Output[0]; part["type"] != "input_text" || part["text"] != "fixture" {
		t.Fatalf("incorrect tool output part: %v", part)
	}
	if output.Input[3].EncryptedContent != "opaque-fixture" {
		t.Fatal("opaque history changed")
	}
}

func TestBuildPayloadRequiresInlineHistory(t *testing.T) {
	cfg := config{Models: []modelConfig{{Name: "upstream", Alias: "alias"}}}
	for _, tt := range []struct{ payload, code string }{
		{`{"input":"hi","previous_response_id":"resp_1"}`, "bps_requires_full_history"},
		{`{"input":[{"type":"item_reference","id":"item_1"}]}`, "bps_requires_full_history"},
		{`{"input":[{"type":"function_call_output","call_id":"call_1","output":"orphan"}]}`, "missing_tool_call"},
	} {
		_, _, err := buildPayload(executionRequest{Model: "alias", Payload: []byte(tt.payload)}, cfg, false)
		var result *rpcError
		if !errors.As(err, &result) || result.StatusCode() != 400 || result.Code() != tt.code {
			t.Fatalf("request %s gave %v, want %s", tt.payload, err, tt.code)
		}
	}
}
