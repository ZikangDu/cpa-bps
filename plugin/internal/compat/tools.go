package compat

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

const (
	maxToolEntries = 4096
	maxToolNesting = 8
	maxToolName    = 256
)

var (
	ErrInvalidToolDeclaration = errors.New("invalid or ambiguous Responses tool declaration")
	ErrInvalidToolResponse    = errors.New("the upstream returned an invalid or undeclared Responses tool call")
)

type ToolCatalog struct {
	declarations map[string]toolDeclaration
}

type toolDeclaration struct {
	Name, Namespace, Kind string
}

// DeclaredTools collects the client-executable tools for this request. Hosted
// tools are left to the upstream and are never interpreted as client tools.
func DeclaredTools(body map[string]json.RawMessage) (ToolCatalog, error) {
	catalog := ToolCatalog{declarations: make(map[string]toolDeclaration)}
	remaining := maxToolEntries
	if err := catalog.add(body["tools"], "", 0, &remaining); err != nil {
		return ToolCatalog{}, err
	}
	// Responses clients may update the tool catalog inside conversation input.
	// Match the protocol bridge: later catalogs replace earlier declarations,
	// while ambiguity inside one catalog is still rejected.
	var input []json.RawMessage
	if json.Unmarshal(body["input"], &input) == nil {
		for _, raw := range input {
			var item struct {
				Type  string          `json:"type"`
				Tools json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(raw, &item) != nil || item.Type != "additional_tools" {
				continue
			}
			update := ToolCatalog{declarations: make(map[string]toolDeclaration)}
			if err := update.add(item.Tools, "", 0, &remaining); err != nil {
				return ToolCatalog{}, err
			}
			for key, declaration := range update.declarations {
				catalog.declarations[key] = declaration
			}
		}
	}
	if stringValue(body["tool_choice"]) == "none" {
		clear(catalog.declarations)
	}
	return catalog, nil
}

func (catalog ToolCatalog) add(raw json.RawMessage, namespace string, depth int, remaining *int) error {
	if absent(raw) {
		return nil
	}
	var entries []json.RawMessage
	if depth > maxToolNesting || json.Unmarshal(raw, &entries) != nil || len(entries) > *remaining {
		return ErrInvalidToolDeclaration
	}
	*remaining -= len(entries)
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields == nil {
			return ErrInvalidToolDeclaration
		}
		kind, name := stringValue(fields["type"]), stringValue(fields["name"])
		switch kind {
		case "namespace", "function", "custom":
		default:
			continue
		}
		if !validToolName(name) {
			return ErrInvalidToolDeclaration
		}
		if kind == "namespace" {
			if absent(fields["tools"]) {
				return ErrInvalidToolDeclaration
			}
			if err := catalog.add(fields["tools"], toolKey(namespace, name), depth+1, remaining); err != nil {
				return err
			}
			continue
		}
		key := toolKey(namespace, name)
		if !validToolName(key) {
			return ErrInvalidToolDeclaration
		}
		declaration := toolDeclaration{Name: name, Namespace: namespace, Kind: kind}
		if previous, exists := catalog.declarations[key]; exists && previous != declaration {
			return ErrInvalidToolDeclaration
		}
		catalog.declarations[key] = declaration
	}
	return nil
}

func toolKey(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "." + name
}

func validToolName(name string) bool {
	return len(name) > 0 && len(name) <= maxToolName && !strings.ContainsFunc(name, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

func isClientToolCall(kind string) bool {
	return kind == "function_call" || kind == "custom_tool_call"
}

type validatedCall struct {
	index       int
	item        json.RawMessage
	id          string
	argumentKey string
	argument    string
	eventPrefix string
}

// ValidateResponse checks each terminal client tool call without rewriting the
// response or interpreting a tool's argument values.
func (catalog ToolCatalog) ValidateResponse(raw json.RawMessage) error {
	_, err := catalog.validatedCalls(raw)
	return err
}

func (catalog ToolCatalog) validatedCalls(raw json.RawMessage) ([]validatedCall, error) {
	var response *struct {
		Status json.RawMessage
		Output []json.RawMessage
	}
	if json.Unmarshal(raw, &response) != nil || response == nil || len(response.Output) > maxToolEntries {
		return nil, ErrInvalidToolResponse
	}
	var calls []validatedCall
	for index, item := range response.Output {
		var tag struct{ Type string }
		if json.Unmarshal(item, &tag) != nil || !isClientToolCall(tag.Type) {
			continue
		}
		// Older synthetic fixtures omit status. If the upstream supplies one,
		// only a completed response can authorize delivery of a client call.
		if len(response.Status) > 0 && stringValue(response.Status) != "completed" {
			return nil, ErrInvalidToolResponse
		}
		var call struct {
			Name, Namespace, ID string
			Arguments, Input    json.RawMessage
		}
		if json.Unmarshal(item, &call) != nil || call.Name == "" {
			return nil, ErrInvalidToolResponse
		}
		expectedKind := "function"
		if tag.Type == "custom_tool_call" {
			expectedKind = "custom"
		}
		declared, exists := catalog.declarations[toolKey(call.Namespace, call.Name)]
		if !exists || expectedKind != declared.Kind ||
			(call.Namespace != "" && (call.Namespace != declared.Namespace || call.Name != declared.Name)) {
			return nil, ErrInvalidToolResponse
		}
		validated := validatedCall{index: index, item: item, id: call.ID}
		switch tag.Type {
		case "function_call":
			arguments := bytes.TrimSpace(call.Arguments)
			if len(arguments) == 0 {
				return nil, ErrInvalidToolResponse
			}
			if arguments[0] == '"' {
				arguments = bytes.TrimSpace([]byte(stringValue(arguments)))
			}
			if len(arguments) == 0 || arguments[0] != '{' || !json.Valid(arguments) {
				return nil, ErrInvalidToolResponse
			}
			validated.argumentKey = "arguments"
			validated.argument = string(arguments)
			validated.eventPrefix = "response.function_call_arguments."
		case "custom_tool_call":
			if absent(call.Input) || json.Unmarshal(call.Input, &validated.argument) != nil {
				return nil, ErrInvalidToolResponse
			}
			validated.argumentKey = "input"
			validated.eventPrefix = "response.custom_tool_call_input."
		}
		calls = append(calls, validated)
	}
	return calls, nil
}
