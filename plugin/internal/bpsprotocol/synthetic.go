// Adapted from JaxsonWang/cpa-plugin-oai-basispoints v0.2.10.
// Copyright (c) 2026 JaxsonWang. See LICENSE in this directory.
package bpsprotocol

import (
	"encoding/json"
	"strings"
)

// ResponseStream emits a complete, already validated buffered response. It
// must not be concatenated after provisional downstream output.
func ResponseStream(raw json.RawMessage) ([]byte, error) {
	if len(raw) > 16<<20 {
		return nil, fail(502, "bps_invalid_response", "BPS final response exceeds the buffered response limit")
	}
	response, reason := parseRelayObject(string(raw))
	if reason != "" || response["status"] != "completed" && response["status"] != "incomplete" {
		return nil, fail(502, "bps_invalid_response", "BPS buffered response is not a final response")
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) > 4096 {
		return nil, fail(502, "bps_invalid_response", "BPS buffered response output is invalid")
	}
	for _, raw := range output {
		if item := objectValue(raw); item == nil || stringValue(item["type"]) == "" {
			return nil, fail(502, "bps_invalid_response", "BPS buffered response item is invalid")
		}
	}
	return syntheticStream(response), nil
}

func syntheticStream(response map[string]any) []byte {
	if response == nil {
		return nil
	}
	created := cloneObject(response)
	created["status"] = "in_progress"
	created["output"] = []any{}
	var builder strings.Builder
	sequence := 0
	emit := func(event string, value map[string]any) {
		value["type"] = event
		value["sequence_number"] = sequence
		sequence++
		writeSSE(&builder, event, value)
	}
	emit("response.created", map[string]any{"response": created})
	emit("response.in_progress", map[string]any{"response": created})
	if output, ok := response["output"].([]any); ok {
		for index, value := range output {
			item := objectValue(value)
			if item == nil {
				continue
			}
			field, event := "", ""
			switch stringValue(item["type"]) {
			case "function_call":
				field, event = "arguments", "response.function_call_arguments"
			case "custom_tool_call":
				field, event = "input", "response.custom_tool_call_input"
			}
			added := cloneObject(item)
			if stringValue(item["type"]) == "message" {
				added["status"] = "in_progress"
				added["content"] = []any{}
			}
			if stringValue(item["type"]) == "reasoning" {
				added["summary"] = []any{}
			}
			if field != "" {
				added[field] = ""
				if field == "arguments" {
					added["status"] = "in_progress"
				}
			}
			emit("response.output_item.added", map[string]any{"output_index": index, "item": added})
			if field != "" {
				text, _ := item[field].(string)
				if text != "" {
					emit(event+".delta", map[string]any{"output_index": index, "item_id": item["id"], "delta": text})
				}
				emit(event+".done", map[string]any{"output_index": index, "item_id": item["id"], field: text})
			} else if stringValue(item["type"]) == "message" {
				emitMessageContent(emit, index, item)
			} else if stringValue(item["type"]) == "reasoning" {
				emitReasoningSummary(emit, index, item)
			}
			emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
		}
	}
	terminalEvent := "response.completed"
	if response["status"] == "incomplete" {
		terminalEvent = "response.incomplete"
	}
	emit(terminalEvent, map[string]any{"response": response})
	builder.WriteString("data: [DONE]\n\n")
	return []byte(builder.String())
}

func emitReasoningSummary(emit func(string, map[string]any), outputIndex int, item map[string]any) {
	summary, _ := item["summary"].([]any)
	for summaryIndex, value := range summary {
		part := objectValue(value)
		if part["type"] != "summary_text" {
			continue
		}
		added := cloneObject(part)
		added["text"] = ""
		emit("response.reasoning_summary_part.added", map[string]any{"output_index": outputIndex, "item_id": item["id"], "summary_index": summaryIndex, "part": added})
		text, _ := part["text"].(string)
		if text != "" {
			emit("response.reasoning_summary_text.delta", map[string]any{"output_index": outputIndex, "item_id": item["id"], "summary_index": summaryIndex, "delta": text})
		}
		emit("response.reasoning_summary_text.done", map[string]any{"output_index": outputIndex, "item_id": item["id"], "summary_index": summaryIndex, "text": text})
		emit("response.reasoning_summary_part.done", map[string]any{"output_index": outputIndex, "item_id": item["id"], "summary_index": summaryIndex, "part": part})
	}
}

// 按原始 content 下标回放，不能因空正文或拒绝片段而压缩索引。
func emitMessageContent(emit func(string, map[string]any), outputIndex int, item map[string]any) {
	content, _ := item["content"].([]any)
	for contentIndex, value := range content {
		part := objectValue(value)
		if part == nil {
			continue
		}
		added := cloneObject(part)
		text, isText := part["text"].(string)
		isText = isText && stringValue(part["type"]) == "output_text"
		if isText {
			added["text"] = ""
			if _, exists := added["logprobs"]; exists {
				added["logprobs"] = []any{}
			}
		}
		emit("response.content_part.added", map[string]any{"output_index": outputIndex, "content_index": contentIndex, "item_id": item["id"], "part": added})
		if isText {
			logprobs, _ := part["logprobs"].([]any)
			if logprobs == nil {
				logprobs = []any{}
			}
			if text != "" {
				emit("response.output_text.delta", map[string]any{"output_index": outputIndex, "content_index": contentIndex, "item_id": item["id"], "delta": text, "logprobs": logprobs})
			}
			emit("response.output_text.done", map[string]any{"output_index": outputIndex, "content_index": contentIndex, "item_id": item["id"], "text": text, "logprobs": logprobs})
		}
		emit("response.content_part.done", map[string]any{"output_index": outputIndex, "content_index": contentIndex, "item_id": item["id"], "part": part})
	}
}

func writeSSE(builder *strings.Builder, event string, value any) {
	builder.WriteString("event: ")
	builder.WriteString(event)
	builder.WriteString("\ndata: ")
	builder.Write(jsonBytes(value))
	builder.WriteString("\n\n")
}
