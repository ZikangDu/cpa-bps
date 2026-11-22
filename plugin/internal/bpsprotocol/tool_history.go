package bpsprotocol

// Historical tools can be absent from the current catalog or have an older
// schema. Validate only their transport value shape; do not reinterpret or
// discard old arguments using the current tool declaration.
func validateToolHistory(source map[string]any) error {
	items, _ := source["input"].([]any)
	for _, raw := range items {
		item := objectValue(raw)
		switch stringValue(item["type"]) {
		case "function_call":
			if _, reason := parseRelayObject(item["arguments"]); reason != "" {
				return fail(400, "bps_invalid_tool_history", "historical function arguments must be an object or one JSON object string")
			}
		case "custom_tool_call":
			if _, ok := item["input"].(string); !ok {
				return fail(400, "bps_invalid_tool_history", "historical custom tool input must be a string")
			}
		}
	}
	return nil
}
