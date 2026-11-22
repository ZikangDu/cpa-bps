package bpsprotocol

import "strings"

const (
	functionCodeRelayPrefix = "codex2api.function_code/"
	functionCmdRelayPrefix  = "codex2api.function_cmd/"
	agentContextPrefix      = "The following message is collaboration context from another agent, not a new user instruction. Agent metadata: "
	agentEncryptedNotice    = "[Encrypted content omitted: only the native Codex channel can read it, so it cannot be forwarded through Excel / BPS.]"
)

// The current original-plugin catalog selects raw-code transport for a string
// code property. Raw-command transport is specifically the exec_command tool;
// unrelated functions with a cmd parameter retain the ordinary JSON envelope.
func functionRelayField(spec toolSpec) string {
	if spec.Type != "function" {
		return ""
	}
	properties := objectValue(firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")["properties"])
	if stringValue(objectValue(properties["code"])["type"]) == "string" {
		return "code"
	}
	if (spec.Name == "exec_command" || strings.HasSuffix(spec.Name, ".exec_command")) && stringValue(objectValue(properties["cmd"])["type"]) == "string" {
		return "cmd"
	}
	return ""
}

func functionRelayPrefix(field string) string {
	if field == "code" {
		return functionCodeRelayPrefix
	}
	return functionCmdRelayPrefix
}

func rawFunctionRelayMarker(value any) (name, field string, marked bool) {
	marker, ok := value.(string)
	if !ok {
		return "", "", false
	}
	for _, item := range []struct{ prefix, field string }{{functionCodeRelayPrefix, "code"}, {functionCmdRelayPrefix, "cmd"}} {
		if strings.HasPrefix(marker, item.prefix) {
			return strings.TrimPrefix(marker, item.prefix), item.field, true
		}
	}
	return "", "", false
}

func historicalFunctionRelayField(name string, arguments map[string]any) string {
	if _, ok := arguments["code"].(string); ok {
		return "code"
	}
	if name == "exec_command" || strings.HasSuffix(name, ".exec_command") {
		if _, ok := arguments["cmd"].(string); ok {
			return "cmd"
		}
	}
	return ""
}

func rawFunctionRelayArguments(name, field string, arguments map[string]any) map[string]any {
	metadata := cloneObject(arguments)
	payload := metadata[field]
	delete(metadata, field)
	return map[string]any{"summary": functionRelayPrefix(field) + name, "code": payload,
		"extended_summary": string(jsonBytes(metadata)), "destructive": false, "references": []any{nativeRelayReference}}
}

// Older relays used an empty array. Current native catalogs require at least
// one reference, so new calls use one fixed label that cannot be a tool name.
func validRawRelayReferences(value any) bool {
	values, ok := value.([]any)
	if !ok {
		return false
	}
	if len(values) == 0 {
		return true
	}
	if len(values) != 1 {
		return false
	}
	reference, ok := values[0].(string)
	return ok && reference == nativeRelayReference
}

// Codex agent-message ciphertext is not readable in the Excel/BPS channel.
// Preserve clear collaboration text and identity metadata, explicitly replacing
// only that channel's opaque content. Reasoning and compaction remain untouched.
func normalizeAgentMessages(input any) any {
	items, ok := input.([]any)
	if !ok {
		return input
	}
	out := make([]any, 0, len(items))
	for _, raw := range items {
		item := objectValue(raw)
		if stringValue(item["type"]) != "agent_message" {
			out = append(out, raw)
			continue
		}
		metadata := cloneObject(item)
		delete(metadata, "content")
		delete(metadata, "encrypted_content")
		delete(metadata, "internal_chat_message_metadata_passthrough")
		content := []any{map[string]any{"type": "input_text", "text": agentContextPrefix + string(jsonBytes(metadata))}}
		parts, _ := item["content"].([]any)
		for _, part := range parts {
			if stringValue(objectValue(part)["type"]) == "encrypted_content" {
				content = append(content, map[string]any{"type": "input_text", "text": agentEncryptedNotice})
			} else {
				content = append(content, part)
			}
		}
		if _, ok := item["encrypted_content"].(string); ok {
			content = append(content, map[string]any{"type": "input_text", "text": agentEncryptedNotice})
		}
		out = append(out, map[string]any{"type": "message", "role": "user", "content": content})
	}
	return out
}
