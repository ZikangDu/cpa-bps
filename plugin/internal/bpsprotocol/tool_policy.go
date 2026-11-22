package bpsprotocol

import "strings"

// Validate tool selection before generating a response. Malformed policies
// must not silently become automatic selection or an empty callable catalog.
func validateToolPolicy(source map[string]any) error {
	if parallel := source["parallel_tool_calls"]; parallel != nil {
		if _, ok := parallel.(bool); !ok {
			return fail(400, "bps_invalid_parallel_tool_calls", "parallel_tool_calls must be boolean")
		}
	}
	choice := source["tool_choice"]
	if choice == nil {
		return nil
	}
	invalid := func() error {
		return fail(400, "invalid_tool_choice", "tool_choice must select declared client tools using a supported policy")
	}
	if text, ok := choice.(string); ok {
		switch strings.TrimSpace(text) {
		case "", "auto", "none", "required":
			return nil
		default:
			return invalid()
		}
	}
	policy := objectValue(choice)
	if policy == nil {
		return invalid()
	}
	declared := clientToolSpecs(source)
	validSelection := func(value any) bool {
		selection := objectValue(value)
		if selection == nil {
			return false
		}
		kind := stringValue(selection["type"])
		if kind != "function" && kind != "custom" {
			return false
		}
		if namespace := selection["namespace"]; namespace != nil {
			if _, ok := namespace.(string); !ok {
				return false
			}
		}
		if stringValue(selection["name"]) == "" {
			return false
		}
		spec, ok := declared[clientToolCallName(selection)]
		return ok && spec.Type == kind
	}
	switch stringValue(policy["type"]) {
	case "function", "custom":
		if !validSelection(policy) {
			return invalid()
		}
	case "allowed_tools":
		if mode := policy["mode"]; mode != nil {
			text, ok := mode.(string)
			if !ok || (strings.TrimSpace(text) != "auto" && strings.TrimSpace(text) != "required") {
				return invalid()
			}
		}
		tools, ok := policy["tools"].([]any)
		if !ok {
			return invalid()
		}
		for _, tool := range tools {
			if !validSelection(tool) {
				return invalid()
			}
		}
	default:
		return invalid()
	}
	return nil
}
