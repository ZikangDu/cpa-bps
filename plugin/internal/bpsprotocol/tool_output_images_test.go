package bpsprotocol

import (
	"fmt"
	"strings"
	"testing"
)

func TestLiftToolOutputImagesPreservesResultsAndOrder(t *testing.T) {
	firstImage := map[string]any{"type": "input_image", "file_id": "file-first", "detail": "low", "future_field": "keep"}
	secondImage := map[string]any{"type": "input_image", "image_url": "data:image/png;base64,synthetic"}
	futurePart := map[string]any{"type": "future_business_part", "opaque_state": []any{"keep"}}
	for _, kind := range []string{"function_call_output", "custom_tool_call_output"} {
		t.Run(kind, func(t *testing.T) {
			result := map[string]any{"type": kind, "call_id": "call_fixture", "status": "completed", "phase": "commentary", "future_state": map[string]any{"keep": true},
				"output": []any{map[string]any{"type": "input_text", "text": "before"}, firstImage, futurePart, secondImage, map[string]any{"type": "input_text", "text": "after"}}}
			following := messageItem("user", "real following instruction")
			source := []any{messageItem("user", "original instruction"), result, following}
			before := string(jsonBytes(source))
			got := liftToolOutputImages(source).([]any)
			if len(got) != 4 || string(jsonBytes(got[3])) != string(jsonBytes(following)) {
				t.Fatal("attachment did not immediately follow its result or changed later history")
			}
			out := objectValue(got[1])
			for key, value := range result {
				if key != "output" && string(jsonBytes(out[key])) != string(jsonBytes(value)) {
					t.Fatalf("result field %s changed", key)
				}
			}
			parts := out["output"].([]any)
			if len(parts) != 5 || string(jsonBytes(parts[0])) != string(jsonBytes(result["output"].([]any)[0])) ||
				string(jsonBytes(parts[2])) != string(jsonBytes(futurePart)) || objectValue(parts[4])["text"] != "after" {
				t.Fatal("text or future result parts changed")
			}
			attachment := objectValue(got[2])
			content := attachment["content"].([]any)
			if attachment["type"] != "message" || attachment["role"] != "user" || len(content) != 5 || objectValue(content[0])["text"] != toolOutputImageNotice {
				t.Fatal("missing explicit tool-result attachment envelope")
			}
			for i, image := range []any{firstImage, secondImage} {
				label := fmt.Sprintf("[Tool output image %d for call_id %q]", i+1, "call_fixture")
				if objectValue(parts[i*2+1])["text"] != label+" See the following image attachment message." ||
					objectValue(content[i*2+1])["text"] != label || string(jsonBytes(content[i*2+2])) != string(jsonBytes(image)) {
					t.Fatal("an image or its call/ordinal association changed")
				}
			}
			if string(jsonBytes(source)) != before {
				t.Fatal("source history was mutated")
			}
			if string(jsonBytes(liftToolOutputImages(got))) != string(jsonBytes(got)) {
				t.Fatal("normalization was not idempotent")
			}
		})
	}
}

func TestLiftToolOutputImagesLeavesUnrelatedPayloadsUnchanged(t *testing.T) {
	for _, raw := range []any{"plain input", nil, []any{}, []any{
		map[string]any{"type": "function_call_output", "call_id": "string", "output": "keep string"},
		map[string]any{"type": "custom_tool_call_output", "call_id": "object", "output": map[string]any{"keep": true}},
		map[string]any{"type": "function_call_output", "call_id": "empty", "output": []any{}},
		map[string]any{"type": "function_call_output", "call_id": "future", "output": []any{map[string]any{"type": "future_part", "value": "keep"}}},
		map[string]any{"type": "future_business_item", "output": []any{map[string]any{"type": "input_image", "file_id": "opaque"}}},
		map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_image", "file_id": "ordinary-image"}}},
	}} {
		if string(jsonBytes(liftToolOutputImages(raw))) != string(jsonBytes(raw)) {
			t.Fatal("unrelated input was changed")
		}
	}
}

func TestToolOutputImagesRemainAttachmentsInBothTransports(t *testing.T) {
	const scope = "fixture-caller"
	input := []any{
		messageItem("user", "original user turn"),
		map[string]any{"type": "function_call", "name": "echo", "call_id": "call_function", "arguments": `{"text":"fixture"}`},
		map[string]any{"type": "custom_tool_call", "name": "patch", "call_id": "call_custom", "input": "synthetic custom input"},
		map[string]any{"type": "function_call_output", "call_id": "call_function", "output": []any{map[string]any{"type": "input_text", "text": "function text result"}, map[string]any{"type": "input_image", "file_id": "file-function"}}},
		map[string]any{"type": "custom_tool_call_output", "call_id": "call_custom", "output": []any{map[string]any{"type": "input_text", "text": "custom text result"}, map[string]any{"type": "input_image", "file_id": "file-custom"}}},
	}
	source := map[string]any{"model": "fixture", "input": input, "prompt_cache_key": "stable-conversation", "tools": []any{
		map[string]any{"type": "function", "name": "echo", "parameters": map[string]any{"type": "object"}},
		map[string]any{"type": "custom", "name": "patch"},
	}}
	before := string(jsonBytes(source))
	fingerprint, iteration := turnState(input)
	wantTurn := uuidV5("cpa-oai-basispoints/" + scope + "/stable-conversation/turn/" + fingerprint)
	var cacheKey string
	for _, mode := range []string{"native", "text"} {
		t.Run(mode, func(t *testing.T) {
			raw, bridge, err := PrepareWithOptions(jsonBytes(source), scope, Options{ToolTransport: mode})
			if err != nil {
				t.Fatal(err)
			}
			body, _ := parseRelayObject(string(raw))
			metadata := objectValue(body["metadata"])
			if metadata["turn_id"] != wantTurn || metadata["agent_iteration"] != iteration || iteration != "3" {
				t.Fatal("tool attachment messages became new caller turns")
			}
			if cacheKey == "" {
				cacheKey = stringValue(body["prompt_cache_key"])
			} else if body["prompt_cache_key"] != cacheKey {
				t.Fatal("transport image lifting changed cache routing")
			}
			if string(jsonBytes(bridge.source["input"])) != string(jsonBytes(input)) || string(jsonBytes(source)) != before {
				t.Fatal("source history was changed for subsequent retry/transport selection")
			}
			images := map[string]int{}
			attachments := 0
			for index, value := range body["input"].([]any) {
				item := objectValue(value)
				if strings.Contains(string(jsonBytes(item["output"])), `"input_image"`) {
					t.Fatal("an image remained in a native tool result")
				}
				content, _ := item["content"].([]any)
				if len(content) == 0 || objectValue(content[0])["text"] != toolOutputImageNotice {
					continue
				}
				attachments++
				if index == 0 || !strings.Contains(string(jsonBytes(body["input"].([]any)[index-1])), "call_") {
					t.Fatal("attachment detached from the preceding tool result")
				}
				for _, part := range content {
					if objectValue(part)["type"] == "input_image" {
						images[stringValue(objectValue(part)["file_id"])]++
					}
				}
			}
			if attachments != 2 || images["file-function"] != 1 || images["file-custom"] != 1 {
				t.Fatal("tool output images were lost, duplicated, or serialized as text")
			}
			for _, text := range []string{"function text result", "custom text result", "synthetic custom input"} {
				if !strings.Contains(string(raw), text) {
					t.Fatal("tool history text was lost")
				}
			}
		})
	}
}
