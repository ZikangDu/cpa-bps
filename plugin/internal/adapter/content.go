package adapter

import (
	"bytes"
	"encoding/json"
)

// Normalize client shorthand while retaining opaque fields and standard parts.
// The adapter leaves image bytes and URLs for the configured upstream to handle.
func normalizeContent(raw json.RawMessage) json.RawMessage {
	var items []json.RawMessage
	if json.Unmarshal(raw, &items) != nil {
		return raw
	}
	changed := false
	for index, entry := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(entry, &item) != nil || item == nil {
			continue
		}
		field := "content"
		var kind string
		_ = json.Unmarshal(item["type"], &kind)
		if kind == "function_call_output" || kind == "custom_tool_call_output" {
			field = "output"
		}
		if parts, ok := normalizeParts(item[field]); ok {
			item[field] = parts
			items[index], _ = json.Marshal(item)
			changed = true
		}
	}
	if !changed {
		return raw
	}
	normalized, _ := json.Marshal(items)
	return normalized
}

func normalizeParts(raw json.RawMessage) (json.RawMessage, bool) {
	var parts []json.RawMessage
	if json.Unmarshal(raw, &parts) != nil {
		return nil, false
	}
	changed := false
	for index, part := range parts {
		data := bytes.TrimSpace(part)
		if len(data) == 0 {
			continue
		}
		var normalized []byte
		switch data[0] {
		case '"':
			normalized, _ = json.Marshal(map[string]json.RawMessage{
				"type": json.RawMessage(`"input_text"`), "text": part,
			})
		case '{':
			normalized = normalizeImage(part)
		}
		if normalized != nil {
			parts[index] = normalized
			changed = true
		}
	}
	if !changed {
		return nil, false
	}
	normalized, _ := json.Marshal(parts)
	return normalized, true
}

func normalizeImage(raw json.RawMessage) json.RawMessage {
	var part map[string]json.RawMessage
	if json.Unmarshal(raw, &part) != nil {
		return nil
	}
	var kind string
	_ = json.Unmarshal(part["type"], &kind)
	if kind == "image" {
		var source struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		}
		var imageURL string
		if json.Unmarshal(part["source"], &source) == nil {
			switch source.Type {
			case "base64":
				if source.MediaType != "" && source.Data != "" {
					imageURL = "data:" + source.MediaType + ";base64," + source.Data
				}
			case "url":
				imageURL = source.URL
			}
		} else {
			var mediaType, data string
			if json.Unmarshal(part["mimeType"], &mediaType) == nil && json.Unmarshal(part["data"], &data) == nil && mediaType != "" && data != "" {
				imageURL = "data:" + mediaType + ";base64," + data
			}
		}
		if imageURL == "" {
			// Keep unknown/malformed representations for request validation;
			// never invent a URL or reinterpret opaque image bytes as text.
			return nil
		}
		part["image_url"], _ = json.Marshal(imageURL)
		part["type"] = json.RawMessage(`"input_image"`)
		delete(part, "source")
		delete(part, "mimeType")
		delete(part, "data")
		normalized, _ := json.Marshal(part)
		return normalized
	}
	if kind != "image_url" && kind != "input_image" {
		return nil
	}
	var nested struct {
		URL    string
		Detail json.RawMessage
	}
	if json.Unmarshal(part["image_url"], &nested) == nil && nested.URL != "" {
		part["image_url"], _ = json.Marshal(nested.URL)
		if len(nested.Detail) > 0 {
			part["detail"] = nested.Detail
		}
	} else {
		var plain string
		if kind != "image_url" || json.Unmarshal(part["image_url"], &plain) != nil || plain == "" {
			return nil
		}
	}
	part["type"] = json.RawMessage(`"input_image"`)
	normalized, _ := json.Marshal(part)
	return normalized
}
