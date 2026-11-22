// Package bpsprotocol converts Responses content to the Basispoints wire format.
// It owns no credentials, transport, tool execution, or provider authorization.
package bpsprotocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Bridge retains the caller's tool contract for one upstream response.
type Bridge struct {
	source      map[string]any
	scope       string
	structured  *structuredOutput
	toolSchemas map[string]*jsonschema.Schema
	textTools   bool
}

// Prepare uses only the supplied model and caller scope. The scope must identify
// the local account/caller partition; it never comes from upstream credentials.
func Prepare(raw []byte, scope string) ([]byte, *Bridge, error) {
	return prepare(raw, scope, Options{ToolTransport: "native"})
}

// PrepareWithTextTools supports BPS catalogs that no longer expose run_officejs.
func PrepareWithTextTools(raw []byte, scope string) ([]byte, *Bridge, error) {
	return PrepareWithOptions(raw, scope, Options{ToolTransport: "text"})
}

// Options selects the transport contract, not the execution of client tools.
// Native relays require an upstream catalog exposing run_officejs; neither mode
// executes Office JavaScript or silently retries through another transport.
type Options struct {
	ToolTransport         string
	ToolsVersionID        string
	nativeContextDefaults bool
}

func PrepareWithOptions(raw []byte, scope string, options Options) ([]byte, *Bridge, error) {
	if options.ToolTransport == "" {
		options.ToolTransport = "text"
	}
	if options.ToolTransport != "text" && options.ToolTransport != "native" {
		return nil, nil, fail(400, "bps_invalid_tool_transport", "BPS tool transport must be text or native")
	}
	options.nativeContextDefaults = options.ToolTransport == "native"
	if len(options.ToolsVersionID) > 256 || strings.ContainsFunc(options.ToolsVersionID, func(r rune) bool { return r <= ' ' || r == 127 }) {
		return nil, nil, fail(400, "bps_invalid_tools_version", "BPS tools version must be a bounded identifier")
	}
	return prepare(raw, scope, options)
}

func prepare(raw []byte, scope string, options Options) ([]byte, *Bridge, error) {
	source, reason := parseRelayObject(string(raw))
	if reason != "" || scope == "" {
		return nil, nil, fail(400, "bps_invalid_request", "BPS requires a JSON request and an isolated caller scope")
	}
	if options.nativeContextDefaults && source["context_management"] == nil {
		source["context_management"] = []any{map[string]any{"type": "compaction", "compact_threshold": 200000}}
	}
	model := stringValue(source["model"])
	if model == "" {
		return nil, nil, fail(400, "bps_invalid_request", "BPS requires a model")
	}
	if err := validateAgentMessageEncryption(source["input"]); err != nil {
		return nil, nil, err
	}
	source["input"] = normalizeAgentMessages(source["input"])
	source["stream"] = true
	structured, err := prepareStructured(source)
	if err != nil {
		return nil, nil, err
	}
	toolSchemas := make(map[string]*jsonschema.Schema)
	for key, tool := range callableClientToolSpecs(source) {
		if tool.Type != "function" {
			continue
		}
		spec := firstMap(tool.Spec, "parameters", "inputSchema", "input_schema")
		if spec == nil {
			continue
		}
		compiled, err := compileSchema(spec)
		if err != nil {
			return nil, nil, err
		}
		toolSchemas[key] = compiled
	}
	// Fast tier is owned by the adapter's final config policy.
	tier, hasTier := source["service_tier"]
	delete(source, "service_tier")
	prepareSource := source
	if structured != nil {
		prepareSource = cloneObject(source)
		delete(prepareSource, "text")
	}
	formatInstructions := ""
	if structured != nil {
		formatInstructions = structured.instructions()
	}
	body, err := prepareResponsesBody(prepareSource, Config{
		Model: model, Scope: scope, FormatInstructions: formatInstructions, ToolsVersionID: options.ToolsVersionID,
	})
	if err != nil {
		return nil, nil, err
	}
	if hasTier {
		body["service_tier"] = tier
	}
	// Explicit [] disables the native default; omission/null use the captured
	// original-plugin default. Keep the established text transport unchanged.
	if options.nativeContextDefaults {
		if policy, isArray := source["context_management"].([]any); isArray && len(policy) == 0 {
			body["context_management"] = policy
		}
	}
	textTools := options.ToolTransport == "text" && usesTextTools(source)
	if textTools {
		prepareTextTools(source, body, formatInstructions)
	} else {
		body["input"] = liftToolOutputImages(body["input"])
	}
	return jsonBytes(body), &Bridge{source: source, scope: scope, structured: structured, toolSchemas: toolSchemas, textTools: textTools}, nil
}

// Transform translates terminal BPS-native client tool relays to Responses calls.
func (b *Bridge) Transform(raw json.RawMessage) (json.RawMessage, error) {
	if b == nil {
		return raw, nil
	}
	if b.textTools {
		return b.transformTextResponse(raw)
	}
	result, response, _, err := transformResponseBody(raw, b.source, b.scope)
	if err != nil {
		return nil, err
	}
	output, _ := response["output"].([]any)
	for _, value := range output {
		call := objectValue(value)
		if call["type"] != "function_call" {
			continue
		}
		if schema := b.toolSchemas[clientToolCallName(call)]; schema != nil {
			arguments, _ := parseRelayObject(call["arguments"])
			if schema.Validate(arguments) != nil {
				return nil, relayError("arguments_schema_mismatch")
			}
		}
	}
	if b.structured != nil && !responseHasClientTool(response) {
		if err := b.structured.validate(response); err != nil {
			return nil, err
		}
		response["text"] = map[string]any{"format": b.structured.format}
		result = jsonBytes(response)
	}
	return result, nil
}

// WrapEmitter translates terminal responses before an existing Responses event
// validator. Provisional native tool events remain withheld by that validator.
func (b *Bridge) WrapEmitter(emit func([]byte) error) func([]byte) error {
	return func(frame []byte) error {
		var data bytes.Buffer
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			if value, ok := bytes.CutPrefix(line, []byte("data:")); ok {
				if data.Len() > 0 {
					data.WriteByte('\n')
				}
				data.Write(bytes.TrimPrefix(value, []byte(" ")))
			}
		}
		var event map[string]json.RawMessage
		if json.Unmarshal(data.Bytes(), &event) != nil {
			return fail(502, "bps_invalid_event", "BPS returned invalid SSE JSON")
		}
		var kind string
		_ = json.Unmarshal(event["type"], &kind)
		if kind != "response.completed" && kind != "response.incomplete" {
			if (b.structured != nil || b.textTools) && kind != "error" && kind != "response.failed" {
				return nil
			}
			return emit(frame)
		}
		body, err := b.Transform(event["response"])
		if err != nil {
			return err
		}
		if b.structured != nil || b.textTools {
			final, _ := parseRelayObject(string(body))
			frames := syntheticStream(final)
			for _, frame := range bytes.Split(frames, []byte("\n\n")) {
				if len(frame) > 0 && !bytes.Equal(frame, []byte("data: [DONE]")) {
					if err := emit(append(frame, '\n', '\n')); err != nil {
						return err
					}
				}
			}
			return nil
		}
		event["response"] = body
		encoded, err := json.Marshal(event)
		if err != nil {
			return fail(502, "bps_invalid_event", "BPS response could not be encoded")
		}
		output := append([]byte("event: "+kind+"\ndata: "), encoded...)
		return emit(append(output, '\n', '\n'))
	}
}

type Config struct {
	Model              string
	Scope              string
	ToolsVersionID     string
	FormatInstructions string
}

func (c Config) resolveUpstreamModel(model string) (string, bool) {
	return c.Model, model == c.Model
}

type Error struct {
	status  int
	code    string
	message string
}

func (e *Error) Error() string   { return e.message }
func (e *Error) StatusCode() int { return e.status }
func (e *Error) Code() string    { return e.code }

func fail(status int, code, message string) error {
	return &Error{status: status, code: code, message: message}
}

func jsonBytes(value any) []byte {
	data, _ := json.Marshal(value)
	return data
}

func stringValue(value any) string {
	s, _ := value.(string)
	return strings.TrimSpace(s)
}

func normalizeEffort(value any) string {
	effort := strings.ToLower(stringValue(value))
	switch effort {
	case "x-high", "extra-high", "extra_high", "max", "ultra":
		return "xhigh"
	case "low", "medium", "high", "xhigh":
		return effort
	default:
		return "medium"
	}
}

func replayKey(scope string, client map[string]any) string {
	payload := []any{scope, client["call_id"], client["type"], client["name"], client["namespace"], client["arguments"], client["input"]}
	digest := sha256.Sum256(jsonBytes(payload))
	return hex.EncodeToString(digest[:])
}
