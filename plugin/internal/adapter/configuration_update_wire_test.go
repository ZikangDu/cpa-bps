package adapter

import (
	"bytes"
	"encoding/json"
	"testing"

	"cpa-bps-reconstruction/internal/bpsprotocol"
)

func TestConfigurationEffortAliasesReachFinalBPSWire(t *testing.T) {
	for _, mode := range []string{"native", "text"} {
		for _, effort := range []string{"max", "ultra"} {
			for _, override := range []string{"", "low"} {
				t.Run(mode+"/"+effort+"/"+override, func(t *testing.T) {
					r, _, _ := quotaFixture(t)
					r.cfg.Models = []modelConfig{{Name: "gpt-5.6-sol", Alias: "gpt-5.6-sol"}}
					body := map[string]any{
						"model": "gpt-5.6-sol",
						"input": []any{map[string]any{"type": "configuration_update", "reasoning": map[string]any{"effort": effort, "mode": "standard"}}, map[string]any{"role": "user", "content": "Synthetic configuration update fixture."}},
						"tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
					}
					if override != "" {
						body["reasoning"] = map[string]any{"effort": override}
					}
					original, _ := json.Marshal(body)
					req := executionRequest{Model: "gpt-5.6-sol", Payload: original, OriginalRequest: bytes.Clone(original)}
					payload, _, err := buildPayload(req, r.cfg, false)
					if err != nil {
						t.Fatal(err)
					}
					payload, documentErr := r.prepareDocuments(r.cfg, req, payload)
					if documentErr != nil {
						t.Fatal(documentErr)
					}
					wire, _, err := bpsprotocol.PrepareWithOptions(payload, "configuration-wire-fixture", bpsprotocol.Options{ToolTransport: mode})
					if err != nil {
						t.Fatal(err)
					}
					var result map[string]any
					if err := json.Unmarshal(wire, &result); err != nil {
						t.Fatal(err)
					}
					want := "xhigh"
					if override != "" {
						want = override
					}
					if result["reasoning_effort"] != want || bytes.Contains(wire, []byte(`"type":"configuration_update"`)) {
						t.Fatalf("configuration alias reached upstream incorrectly: effort=%v, want %s", result["reasoning_effort"], want)
					}
					if !bytes.Equal(req.Payload, original) || !bytes.Equal(req.OriginalRequest, original) {
						t.Fatal("normalization mutated the caller request")
					}
				})
			}
		}
	}
}
