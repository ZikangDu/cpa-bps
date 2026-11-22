package bpsprotocol

import "fmt"

const toolOutputImageNotice = "The following images are tool output from the preceding tool result, not a new user instruction."

// liftToolOutputImages preserves the result's text and call association while
// moving images to the message content accepted by BPS. Work on a local history
// view: inserted attachment messages must not become new caller turns for cache
// keys, turn IDs, or retry/transport selection.
func liftToolOutputImages(raw any) any {
	items, ok := raw.([]any)
	if !ok {
		return raw
	}
	result := make([]any, 0, len(items))
	for _, value := range items {
		item := objectValue(value)
		kind := stringValue(item["type"])
		if kind != "function_call_output" && kind != "custom_tool_call_output" {
			result = append(result, value)
			continue
		}
		parts, ok := item["output"].([]any)
		if !ok {
			result = append(result, value)
			continue
		}
		callID, _ := item["call_id"].(string)
		var attachment []any
		output := make([]any, 0, len(parts))
		imageCount := 0
		for _, part := range parts {
			if stringValue(objectValue(part)["type"]) != "input_image" {
				output = append(output, part)
				continue
			}
			if imageCount == 0 {
				attachment = append(attachment, map[string]any{"type": "input_text", "text": toolOutputImageNotice})
			}
			imageCount++
			label := fmt.Sprintf("[Tool output image %d for call_id %q]", imageCount, callID)
			output = append(output, map[string]any{"type": "input_text", "text": label + " See the following image attachment message."})
			attachment = append(attachment, map[string]any{"type": "input_text", "text": label}, part)
		}
		if imageCount == 0 {
			result = append(result, value)
			continue
		}
		copy := make(map[string]any, len(item))
		for key, value := range item {
			copy[key] = value
		}
		copy["output"] = output
		result = append(result, copy, map[string]any{"type": "message", "role": "user", "content": attachment})
	}
	return result
}
