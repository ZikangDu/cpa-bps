package adapter

import (
	"bytes"
	"encoding/json"
	"testing"
)

func TestNormalizeAnthropicAndMCPImagesPreservesOpaqueParts(t *testing.T) {
	raw := json.RawMessage(`[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"encoded-fixture"},"detail":"high","future_extension":"keep"},{"type":"image","source":{"type":"url","url":"https://example.invalid/fixture.png"}}]},{"type":"function_call_output","output":[{"type":"image","mimeType":"image/png","data":"mcp-fixture"}]},{"type":"agent_message","author":"/root","recipient":"/root/child","content":[{"type":"encrypted_content","encrypted_content":" opaque-state "},{"type":"input_text","text":"  exact text\n"}]}]`)
	before := bytes.Clone(raw)
	got := normalizeContent(raw)
	var items []struct {
		Content   []map[string]any
		Output    []map[string]any
		Author    string
		Recipient string
	}
	if json.Unmarshal(got, &items) != nil {
		t.Fatal("bad normalization")
	}
	first := items[0].Content[0]
	if first["type"] != "input_image" || first["image_url"] != "data:image/png;base64,encoded-fixture" || first["detail"] != "high" || first["future_extension"] != "keep" || first["source"] != nil {
		t.Fatalf("base64 representation changed: %v", first)
	}
	if items[0].Content[1]["image_url"] != "https://example.invalid/fixture.png" {
		t.Fatal("URL representation changed")
	}
	if items[1].Output[0]["image_url"] != "data:image/png;base64,mcp-fixture" || items[1].Output[0]["mimeType"] != nil || items[1].Output[0]["data"] != nil {
		t.Fatal("MCP tool output was not normalized")
	}
	if items[2].Author != "/root" || items[2].Recipient != "/root/child" || items[2].Content[0]["encrypted_content"] != " opaque-state " || items[2].Content[1]["text"] != "  exact text\n" {
		t.Fatal("opaque state, routing, or text was modified")
	}
	if !bytes.Equal(raw, before) || !bytes.Equal(got, normalizeContent(got)) {
		t.Fatal("normalization mutated source or was not idempotent")
	}
}

func TestNormalizeUnknownImageDoesNotInventURL(t *testing.T) {
	for _, raw := range []string{
		`[{"role":"user","content":[{"type":"image","source":{"type":"base64","data":"private-bytes"}}]}]`,
		`[{"role":"user","content":[{"type":"image","source":{"type":"future","data":"private-bytes"}}]}]`,
		`[{"role":"user","content":[{"type":"image","mimeType":"image/png"}]}]`,
		`[{"type":"compaction","encrypted_content":"keep-opaque"}]`,
	} {
		if got := normalizeContent([]byte(raw)); string(got) != raw {
			t.Fatalf("unknown content was reinterpreted: %s", got)
		}
	}
}
