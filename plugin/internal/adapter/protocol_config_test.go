package adapter

import (
	"encoding/json"
	"testing"
)

func TestBPSProtocolConfiguration(t *testing.T) {
	for _, value := range []string{"", "auto", "text", "native"} {
		cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nbps_tool_transport: " + value + "\nbps_tools_version_id: fixture-version\nbps_compact_threshold: 200000\n"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.BPSToolTransport == "" || bpsProtocolOptions(cfg).ToolsVersionID != "fixture-version" {
			t.Fatal("protocol options lost")
		}
		for _, policy := range []string{"", `,"context_management":null`, `,"context_management":[{"type":"compaction","compact_threshold":12345}]`, `,"context_management":[]`} {
			raw := []byte(`{"model":"fixture","input":"hi"` + policy + `}`)
			cfg.Models = []modelConfig{{Name: "fixture", Alias: "fixture"}}
			body, _, err := buildPayload(executionRequest{Model: "fixture", Payload: raw}, cfg, false)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			_ = json.Unmarshal(body, &got)
			if policy == "" || policy == `,"context_management":null` {
				var entries []struct {
					Threshold int `json:"compact_threshold"`
				}
				_ = json.Unmarshal(got["context_management"], &entries)
				if len(entries) != 1 || entries[0].Threshold != 200000 {
					t.Fatal("optional threshold not added")
				}
			} else {
				var original map[string]json.RawMessage
				_ = json.Unmarshal(raw, &original)
				var a, b any
				_ = json.Unmarshal(original["context_management"], &a)
				_ = json.Unmarshal(got["context_management"], &b)
				x, _ := json.Marshal(a)
				y, _ := json.Marshal(b)
				if string(x) != string(y) {
					t.Fatal("explicit context policy overwritten")
				}
			}
		}
	}
	for _, extra := range []string{"bps_tool_transport: invalid", "bps_compact_threshold: -1", "bps_tools_version_id: 'contains space'"} {
		if _, err := parseConfig([]byte("enabled: true\noauth_preset: bps\n" + extra)); err == nil {
			t.Fatal("invalid protocol setting accepted")
		}
	}
}
