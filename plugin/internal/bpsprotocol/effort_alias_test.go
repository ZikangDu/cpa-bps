package bpsprotocol

import (
	"encoding/json"
	"testing"
)

func TestBPSWireNormalizesOriginalPluginEffortAliases(t *testing.T) {
	for _, transport := range []string{"native", "text"} {
		for _, field := range []string{"reasoning", "reasoning_effort"} {
			for _, effort := range []string{"max", "ultra", "x-high", "high", "low"} {
				t.Run(transport+"/"+field+"/"+effort, func(t *testing.T) {
					request := map[string]any{
						"model": "gpt-5.6-sol", "input": "Use the declared tool if needed.",
						"tools": []any{map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}}},
					}
					if field == "reasoning" {
						request[field] = map[string]any{"effort": effort}
					} else {
						request[field] = effort
					}
					raw, _, err := PrepareWithOptions(jsonBytes(request), "effort-alias-fixture", Options{ToolTransport: transport})
					if err != nil {
						t.Fatal(err)
					}
					var body map[string]any
					if err := json.Unmarshal(raw, &body); err != nil {
						t.Fatal(err)
					}
					want := effort
					if effort == "max" || effort == "ultra" || effort == "x-high" {
						want = "xhigh"
					}
					if body["reasoning_effort"] != want || body["reasoning"] != nil || body["model"] != "gpt-5.6-sol" {
						t.Fatalf("wire effort = %v, want %q; nested reasoning = %v", body["reasoning_effort"], want, body["reasoning"])
					}
				})
			}
		}
	}
}
