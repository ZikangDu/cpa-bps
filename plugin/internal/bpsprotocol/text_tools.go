package bpsprotocol

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Text tool relays avoid depending on any specific workbook tool catalog.
// The response is withheld until its declared client tool contract validates.
func usesTextTools(source map[string]any) bool {
	if len(clientToolSpecs(source)) > 0 {
		return true
	}
	items, _ := source["input"].([]any)
	for _, raw := range items {
		switch stringValue(objectValue(raw)["type"]) {
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
			return true
		}
	}
	return false
}

func prepareTextTools(source, body map[string]any, formatInstructions string) {
	declared := clientToolSpecs(source)
	catalog := make([]any, 0, len(declared))
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tool := declared[name]
		catalog = append(catalog, map[string]any{
			"name": tool.Key, "type": tool.Type, "declaration": tool.Spec,
		})
	}
	instruction := "This conversation is connected to an external client that executes its own tools. " +
		"Communicate a client tool request as one JSON object in your assistant text, without Markdown fences or surrounding prose. " +
		"For a function use {\"client_tool\":\"FULL_NAME\",\"arguments\":{...}}. " +
		"For a custom tool use {\"client_tool\":\"FULL_NAME\",\"input\":\"exact raw text\"}. " +
		"The client parses and validates this JSON and returns the tool result in the conversation. " +
		"By default, every tool in the declared catalog is callable. If a tool is needed, return its JSON request; otherwise answer in ordinary assistant text. " +
		"A current-turn tool policy, when present at the end of the request, overrides this default and selects which declared tools are callable. " +
		"Use only permitted catalog names, including namespace prefixes. Do not invoke native workbook tools for these requests. " +
		"Catalog: " + string(jsonBytes(catalog)) + ". "
	input := []any{messageItem("developer", instruction)}
	if prior, _ := source["instructions"].(string); prior != "" {
		input = append(input, messageItem("developer", prior))
	}
	if formatInstructions != "" {
		input = append(input, messageItem("developer", formatInstructions))
	}
	// Lift before rendering client results as text, so image parts remain real
	// attachments. Keep source intact for turn/cache accounting and retries.
	normalizedHistory := liftToolOutputImages(source["input"])
	switch items := normalizedHistory.(type) {
	case string:
		input = append(input, messageItem("user", items))
	case []any:
		for _, raw := range items {
			item := objectValue(raw)
			switch stringValue(item["type"]) {
			case "additional_tools", "configuration_update":
				continue
			case "function_call":
				args, reason := parseRelayObject(item["arguments"])
				if reason == "" {
					input = append(input, messageItem("assistant", string(jsonBytes(map[string]any{
						"client_tool": clientToolCallName(item), "arguments": args,
					}))))
				}
			case "custom_tool_call":
				input = append(input, messageItem("assistant", string(jsonBytes(map[string]any{
					"client_tool": clientToolCallName(item), "input": item["input"],
				}))))
			case "function_call_output", "custom_tool_call_output":
				input = append(input, messageItem("user", "Client tool result: "+string(jsonBytes(map[string]any{
					"call_id": item["call_id"], "output": item["output"],
				}))))
			case "reasoning":
				if reasoning := reasoningHistoryItem(item); reasoning != nil {
					input = append(input, reasoning)
				}
			default:
				if item != nil {
					raw = stripClientMetadata(item)
				}
				input = append(input, raw)
			}
		}
	}
	if policy := currentTextToolPolicy(source); policy != "" {
		input = appendBeforeCompaction(input, []any{messageItem("developer", policy)})
	}
	body["input"] = input
}

func currentTextToolPolicy(source map[string]any) string {
	// Default auto behavior is already in the stable prologue. Re-appending
	// that policy after history would prevent the previous input from remaining
	// an exact prefix when the next turn appends assistant/tool results.
	choice := source["tool_choice"]
	if choice == nil {
		return ""
	}
	if text, ok := choice.(string); ok && (strings.TrimSpace(text) == "" || strings.TrimSpace(text) == "auto") {
		return ""
	}
	callable := callableClientToolSpecs(source)
	names := make([]string, 0, len(callable))
	for name := range callable {
		names = append(names, name)
	}
	sort.Strings(names)
	policy := "Current-turn tool policy. Callable client tool names: " + string(jsonBytes(names)) + ". "
	switch {
	case clientToolCallRequired(source):
		return policy + "The client requires exactly one tool request this turn. Return its JSON object and wait for the tool result."
	case len(names) == 0:
		return policy + "No client tool is callable this turn. Use the supplied tool results to answer in ordinary assistant text."
	default:
		return policy + "If a tool is needed, return its JSON request. Otherwise answer in ordinary assistant text."
	}
}

func (b *Bridge) transformTextTool(response map[string]any) error {
	output, _ := response["output"].([]any)
	var text strings.Builder
	for _, raw := range output {
		item := objectValue(raw)
		if item["type"] == "function_call" || item["type"] == "custom_tool_call" {
			return relayError("unexpected_native_tool")
		}
		if item["type"] != "message" {
			continue
		}
		parts, _ := item["content"].([]any)
		for _, part := range parts {
			if objectValue(part)["type"] == "output_text" {
				content, _ := objectValue(part)["text"].(string)
				text.WriteString(content)
			}
		}
	}
	envelope, reason := parseRelayObject(text.String())
	name := stringValue(envelope["client_tool"])
	if reason != "" || name == "" {
		if clientToolCallRequired(b.source) {
			return relayError("required_tool_choice_not_satisfied")
		}
		return nil
	}
	tool, ok := callableClientToolSpecs(b.source)[name]
	if !ok {
		return relayError("text_tool_not_in_catalog")
	}
	hash := sha256.Sum256(jsonBytes([]any{b.scope, response["id"], envelope}))
	callID := "call_" + hex.EncodeToString(hash[:16])
	item := map[string]any{"name": tool.Name, "call_id": callID, "id": "fc_" + hex.EncodeToString(hash[:16]), "status": "completed"}
	if tool.Namespace != "" {
		item["namespace"] = tool.Namespace
	}
	switch tool.Type {
	case "function":
		args, ok := envelope["arguments"].(map[string]any)
		if !ok || len(envelope) != 2 {
			return relayError("text_arguments_invalid")
		}
		if schema := b.toolSchemas[tool.Key]; schema != nil && schema.Validate(args) != nil {
			return relayError("arguments_schema_mismatch")
		}
		item["type"], item["arguments"] = "function_call", string(jsonBytes(args))
	case "custom":
		raw, ok := envelope["input"].(string)
		if !ok || len(envelope) != 2 {
			return relayError("text_custom_input_invalid")
		}
		item["type"], item["input"] = "custom_tool_call", raw
		item["id"] = "ctc_" + hex.EncodeToString(hash[:16])
	default:
		return fmt.Errorf("unknown client tool kind")
	}
	kept := make([]any, 0, len(output)+1)
	for _, raw := range output {
		if objectValue(raw)["type"] != "message" {
			kept = append(kept, raw)
		}
	}
	response["output"] = append(kept, item)
	return nil
}

func (b *Bridge) transformTextResponse(raw json.RawMessage) (json.RawMessage, error) {
	response, reason := parseRelayObject(string(raw))
	if reason != "" {
		return nil, fail(502, "invalid_upstream_response", "BPS returned invalid JSON")
	}
	if stringValue(response["status"]) == "completed" {
		if err := b.transformTextTool(response); err != nil {
			return nil, err
		}
		if b.structured != nil && !responseHasClientTool(response) {
			if err := b.structured.validate(response); err != nil {
				return nil, err
			}
			response["text"] = map[string]any{"format": b.structured.format}
		}
	}
	return jsonBytes(response), nil
}

func responseHasClientTool(response map[string]any) bool {
	output, _ := response["output"].([]any)
	for _, raw := range output {
		switch objectValue(raw)["type"] {
		case "function_call", "custom_tool_call":
			return true
		}
	}
	return false
}
