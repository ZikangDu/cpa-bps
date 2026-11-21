package bpsprotocol

import (
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

const schemaURL = "https://local.invalid/bps-response-schema"

var schemaName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type structuredOutput struct {
	format map[string]any
	schema *jsonschema.Schema
}

type localSchemas struct{}

func (localSchemas) Load(string) (any, error) {
	return nil, errors.New("external schema references are unavailable")
}

func compileSchema(value map[string]any) (*jsonschema.Schema, error) {
	if value == nil || len(jsonBytes(value)) > 1<<20 {
		return nil, fail(400, "bps_invalid_schema", "the requested schema is missing or exceeds 1 MiB")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(localSchemas{})
	if err := compiler.AddResource(schemaURL, value); err != nil {
		return nil, fail(400, "bps_invalid_schema", "the requested schema is invalid")
	}
	compiled, err := compiler.Compile(schemaURL)
	if err != nil {
		return nil, fail(400, "bps_invalid_schema", "the requested schema is invalid or has an external reference")
	}
	return compiled, nil
}

func prepareStructured(source map[string]any) (*structuredOutput, error) {
	config := objectValue(source["text"])
	if source["text"] != nil && config == nil {
		return nil, fail(400, "bps_invalid_text", "text must be an object")
	}
	if config == nil || config["format"] == nil {
		return nil, nil
	}
	format := objectValue(config["format"])
	if format == nil {
		return nil, fail(400, "bps_invalid_text", "text.format must be an object")
	}
	kind := stringValue(format["type"])
	if kind == "text" {
		return nil, nil
	}
	if kind != "json_object" && kind != "json_schema" {
		return nil, fail(400, "bps_invalid_text", "text.format is unsupported")
	}
	result := &structuredOutput{format: format}
	for key := range format {
		if key != "type" && !(kind == "json_schema" &&
			(key == "name" || key == "schema" || key == "strict" || key == "description")) {
			return nil, fail(400, "bps_invalid_text", "text.format contains unsupported fields")
		}
	}
	if kind == "json_schema" {
		if !schemaName.MatchString(stringValue(format["name"])) {
			return nil, fail(400, "bps_invalid_schema", "JSON schema requires a bounded format name")
		}
		if strict, exists := format["strict"]; exists {
			if _, ok := strict.(bool); !ok {
				return nil, fail(400, "bps_invalid_schema", "JSON schema strict must be boolean")
			}
		}
		if description, exists := format["description"]; exists {
			if _, ok := description.(string); !ok {
				return nil, fail(400, "bps_invalid_schema", "JSON schema description must be text")
			}
		}
		var err error
		result.schema, err = compileSchema(objectValue(format["schema"]))
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (s *structuredOutput) instructions() string {
	prompt := "Return your final assistant answer as one JSON value, without Markdown fences or surrounding prose. The client's output format is " +
		string(jsonBytes(s.format)) + ". The client validates the JSON before delivery. Tool calls remain protocol items."
	if s.schema == nil {
		prompt += " The JSON value must be an object."
	}
	return prompt
}

func (s *structuredOutput) validate(response map[string]any) error {
	if s == nil || response["status"] != "completed" {
		return nil
	}
	items, _ := response["output"].([]any)
	var text strings.Builder
	refused := false
	for _, raw := range items {
		item := objectValue(raw)
		if kind := stringValue(item["type"]); kind == "function_call" || kind == "custom_tool_call" {
			return nil
		}
		if item["type"] != "message" {
			continue
		}
		content, _ := item["content"].([]any)
		for _, value := range content {
			part := objectValue(value)
			refused = refused || part["type"] == "refusal"
			if part["type"] == "output_text" {
				value, ok := part["text"].(string)
				if !ok || text.Len()+len(value) > 16<<20 {
					return fail(502, "bps_invalid_structured_output", "the upstream structured output is invalid or too large")
				}
				text.WriteString(value)
			}
		}
	}
	if refused && text.Len() == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(strings.NewReader(text.String()))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fail(502, "bps_invalid_structured_output", "the upstream answer does not contain valid JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return fail(502, "bps_invalid_structured_output", "the upstream answer contains trailing output")
	}
	if s.schema == nil {
		if _, ok := value.(map[string]any); !ok {
			return fail(502, "bps_invalid_structured_output", "the upstream answer is not a JSON object")
		}
	} else if s.schema.Validate(value) != nil {
		return fail(502, "bps_invalid_structured_output", "the upstream answer violates the requested JSON schema")
	}
	return nil
}
