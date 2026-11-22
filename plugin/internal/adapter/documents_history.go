package adapter

import (
	"encoding/json"
	"errors"
)

// Restore documents that an older host translator omitted into the matching
// message or tool result. Appending old files to the final turn changes their
// meaning and can make the newest question appear to include an old attachment.
func (d *documentRequest) restoreOriginalDocumentHistory(input []json.RawMessage, raw []byte) ([]json.RawMessage, bool, error) {
	var body map[string]json.RawMessage
	if json.Unmarshal(raw, &body) != nil {
		return input, false, nil
	}
	var original []map[string]json.RawMessage
	if json.Unmarshal(body["input"], &original) != nil {
		_ = json.Unmarshal(body["messages"], &original)
	}
	if len(original) == 0 {
		return input, false, nil
	}
	targets := map[string][]int{}
	remaining := map[string]int{}
	for index, raw := range input {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) == nil {
			key, _ := documentHistorySlot(item)
			if key != "" {
				targets[key] = append(targets[key], index)
			}
			_, field := documentHistorySlot(item)
			var parts []json.RawMessage
			_ = json.Unmarshal(item[field], &parts)
			for _, part := range parts {
				if isDocumentPart(part) {
					remaining[documentReferenceKey(part)]--
				}
			}
		}
	}
	totals := map[string]int{}
	for _, item := range original {
		key, field := documentHistorySlot(item)
		totals[key]++
		var parts []json.RawMessage
		_ = json.Unmarshal(item[field], &parts)
		for _, part := range parts {
			if isDocumentPart(part) {
				remaining[documentReferenceKey(part)]++
			}
		}
	}
	positions := map[string]int{}
	changed := false
	for _, source := range original {
		key, field := documentHistorySlot(source)
		position := positions[key]
		positions[key]++
		var sourceParts []json.RawMessage
		if json.Unmarshal(source[field], &sourceParts) != nil {
			continue
		}
		var documents []json.RawMessage
		for _, part := range sourceParts {
			if isDocumentPart(part) && remaining[documentReferenceKey(part)] > 0 && (d.cfg.OAuthPreset == "bps" || d.supported(part)) {
				documents = append(documents, part)
			}
		}
		if len(documents) == 0 {
			continue
		}
		if key == "" || len(targets[key]) != totals[key] {
			return nil, false, errors.New("document_history_unavailable")
		}
		index := targets[key][position]
		var target map[string]json.RawMessage
		_ = json.Unmarshal(input[index], &target)
		_, targetField := documentHistorySlot(target)
		var parts []json.RawMessage
		if json.Unmarshal(target[targetField], &parts) != nil {
			var text string
			if json.Unmarshal(target[targetField], &text) != nil {
				return nil, false, errors.New("document_history_unavailable")
			}
			if text != "" {
				part, _ := json.Marshal(map[string]string{"type": "input_text", "text": text})
				parts = append(parts, part)
			}
		}
		available := map[string]int{}
		for _, part := range parts {
			if isDocumentPart(part) {
				available[documentReferenceKey(part)]++
			}
		}
		missing := false
		for _, part := range documents {
			identity := documentReferenceKey(part)
			if available[identity] > 0 {
				available[identity]--
				continue
			}
			// Place the file within its original turn. Existing normalized text,
			// image ordering and tool identities are retained without translation.
			parts = append(parts, part)
			missing = true
		}
		if missing {
			target[targetField], _ = json.Marshal(parts)
			input[index], _ = json.Marshal(target)
			changed = true
		}
	}
	return input, changed, nil
}

func documentHistorySlot(item map[string]json.RawMessage) (key, field string) {
	var kind, role, callID string
	_ = json.Unmarshal(item["type"], &kind)
	_ = json.Unmarshal(item["role"], &role)
	if kind == "function_call_output" || kind == "custom_tool_call_output" || role == "tool" {
		field = "output"
		_ = json.Unmarshal(item["call_id"], &callID)
		if role == "tool" {
			field = "content"
			_ = json.Unmarshal(item["tool_call_id"], &callID)
		}
		if callID != "" {
			return "tool:" + callID, field
		}
		return "", field
	}
	if role == "system" {
		// The Chat-to-Responses host translator normalizes system to developer.
		role = "developer"
	}
	if role != "" && (kind == "" || kind == "message") {
		return "role:" + role, "content"
	}
	return "", "content"
}
