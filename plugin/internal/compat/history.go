package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
)

var ErrInvalidHistory = errors.New("invalid encrypted Responses history")

const omittedHistoryNotice = `{"type":"message","role":"developer","content":[{"type":"input_text","text":"An earlier opaque reasoning item was omitted from the conversation history."}]}`

// PrepareHistory preserves ordinary input items and opaque compaction or agent
// state. A recognized configuration update supplies a fallback reasoning effort.
// When requested, only standalone reasoning before the current user turn loses
// its ciphertext. The operation leaves body untouched if validation fails.
func PrepareHistory(body map[string]json.RawMessage, omitEncryptedHistory bool) error {
	var items []json.RawMessage
	if json.Unmarshal(body["input"], &items) != nil || len(items) == 0 {
		// String input and other input formats remain handled by the upstream.
		return nil
	}
	currentTurn := 0
	if omitEncryptedHistory {
		currentTurn = latestUserTurn(items)
	}
	prepared := make([]json.RawMessage, 0, len(items))
	var fallback string
	var changed, omitted bool
	for index, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil {
			prepared = append(prepared, raw)
			continue
		}
		kind := stringValue(item["type"])
		if err := validateEncryptedItem(kind, item); err != nil {
			return err
		}
		if kind == "configuration_update" {
			if effort, recognized := configurationEffort(item); recognized {
				fallback = effort
				changed = true
				continue
			}
		}
		if index < currentTurn && kind == "reasoning" && !absent(item["encrypted_content"]) {
			delete(item, "encrypted_content")
			if !omitted {
				prepared = append(prepared, json.RawMessage(omittedHistoryNotice))
				omitted = true
			}
			changed = true
			raw, _ = json.Marshal(item)
		}
		prepared = append(prepared, raw)
	}
	if changed {
		body["input"], _ = json.Marshal(prepared)
	}
	if fallback != "" {
		applyFallbackEffort(body, fallback)
	}
	return nil
}

func latestUserTurn(items []json.RawMessage) int {
	for index := len(items) - 1; index > 0; index-- {
		var item struct{ Type, Role string }
		if json.Unmarshal(items[index], &item) == nil && item.Role == "user" &&
			(item.Type == "" || item.Type == "message" || item.Type == "agent_message") {
			return index
		}
	}
	return 0
}

func validateEncryptedItem(kind string, item map[string]json.RawMessage) error {
	value := item["encrypted_content"]
	switch kind {
	case "reasoning":
		if !absent(value) && stringValue(value) == "" {
			return ErrInvalidHistory
		}
	case "compaction":
		if stringValue(value) == "" {
			return ErrInvalidHistory
		}
	case "agent_message":
		if !absent(value) && stringValue(value) == "" {
			return ErrInvalidHistory
		}
		var parts []json.RawMessage
		if absent(item["content"]) && (!absent(value) || stringValue(item["text"]) != "") {
			return nil
		}
		if json.Unmarshal(item["content"], &parts) != nil || parts == nil {
			return ErrInvalidHistory
		}

		for _, raw := range parts {
			var part struct {
				Type string          `json:"type"`
				Data json.RawMessage `json:"encrypted_content"`
			}
			if json.Unmarshal(raw, &part) == nil && part.Type == "encrypted_content" && stringValue(part.Data) == "" {
				return ErrInvalidHistory
			}
		}
	case "message", "":
		var parts []struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(item["content"], &parts) == nil {
			for _, part := range parts {
				if part.Type == "agent_message" || part.Type == "encrypted_content" {
					// Agent ciphertext is only valid in its explicit top-level
					// envelope. Never forward a malformed nested cipher part.
					return ErrInvalidHistory
				}
			}
		}
	}
	return nil
}

func configurationEffort(item map[string]json.RawMessage) (string, bool) {
	if len(item) != 2 {
		return "", false
	}
	var reasoning map[string]json.RawMessage
	if json.Unmarshal(item["reasoning"], &reasoning) != nil {
		return "", false
	}
	for field := range reasoning {
		if field != "effort" && field != "mode" {
			return "", false
		}
	}
	if mode, exists := reasoning["mode"]; exists && stringValue(mode) != "standard" {
		return "", false
	}
	effort := strings.ToLower(strings.TrimSpace(stringValue(reasoning["effort"])))
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh":
		return effort, true
	case "x-high", "extra-high", "extra_high":
		return "xhigh", true
	default:
		return "", false
	}
}

func applyFallbackEffort(body map[string]json.RawMessage, fallback string) {
	if !absent(body["reasoning_effort"]) && stringValue(body["reasoning_effort"]) != "" {
		return
	}
	var reasoning map[string]json.RawMessage
	if !absent(body["reasoning"]) && json.Unmarshal(body["reasoning"], &reasoning) != nil {
		return
	}
	if !absent(reasoning["effort"]) {
		return
	}
	if reasoning == nil {
		reasoning = make(map[string]json.RawMessage)
	}
	reasoning["effort"], _ = json.Marshal(fallback)
	body["reasoning"], _ = json.Marshal(reasoning)
}

func absent(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) == 0 || bytes.Equal(raw, []byte("null"))
}

func stringValue(raw json.RawMessage) string {
	var value string
	_ = json.Unmarshal(raw, &value)
	return value
}
