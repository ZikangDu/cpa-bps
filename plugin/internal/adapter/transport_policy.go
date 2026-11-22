package adapter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"cpa-bps-reconstruction/internal/upstream"
)

const transportPolicyTTL = 24 * time.Hour
const maxTransportPolicies = 2048

// A successful transport choice belongs to an account/model/conversation. Only
// its digest and mode are retained; no prompts, token strings or tool bodies
// are stored in this process cache.
type transportPolicies struct {
	mu      sync.Mutex
	entries map[[32]byte]transportPolicyEntry
	now     func() time.Time
}
type transportPolicyEntry struct {
	mode    string
	expires time.Time
}

func newTransportPolicies() *transportPolicies {
	return &transportPolicies{entries: map[[32]byte]transportPolicyEntry{}, now: time.Now}
}
func (p *transportPolicies) read(key [32]byte) (string, bool) {
	if p == nil {
		return "", false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	value, ok := p.entries[key]
	if !ok {
		return "", false
	}
	if !p.now().Before(value.expires) {
		delete(p.entries, key)
		return "", false
	}
	return value.mode, true
}
func (p *transportPolicies) learn(key [32]byte, mode string) {
	if p == nil || (mode != "text" && mode != "native") {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	for k, v := range p.entries {
		if !now.Before(v.expires) {
			delete(p.entries, k)
		}
	}
	if _, ok := p.entries[key]; !ok && len(p.entries) >= maxTransportPolicies {
		var oldest [32]byte
		var earliest time.Time
		for k, v := range p.entries {
			if earliest.IsZero() || v.expires.Before(earliest) {
				oldest, earliest = k, v.expires
			}
		}
		delete(p.entries, oldest)
	}
	p.entries[key] = transportPolicyEntry{mode: mode, expires: now.Add(transportPolicyTTL)}
}

type transportTool struct {
	kind, name, namespace string
	rawCode               bool
}
type transportRequestProfile struct {
	buffer, preferNative bool
	family               string
}

// selectAutoTransport classifies the declared request structure, never user
// prompt keywords. Call only for auto policy; explicit overrides bypass it.
func (r *Runtime) selectAutoTransport(req executionRequest, cfg config, source []byte) (mode string, key [32]byte, buffer bool, err error) {
	body, valid := transportObject(source)
	if !valid {
		return "", key, false, failure(http.StatusBadRequest, "invalid_request", "The Responses payload must contain one JSON object.")
	}
	profile := profileTransportRequest(body)
	key = transportCapabilityKey(transportConversationKey(req, cfg, body), profile.family)
	if !profile.buffer {
		return "text", key, false, nil
	}
	mode = "text"
	if profile.preferNative {
		mode = "native"
	}
	if learned, ok := r.transports.read(key); ok {
		mode = learned
	}
	return mode, key, true, nil
}

func transportObject(raw []byte) (map[string]any, bool) {
	var body map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&body) != nil || body == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return body, true
}
func transportText(v any) string        { s, _ := v.(string); return strings.TrimSpace(s) }
func transportMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func transportToolKey(namespace, name string) string {
	if namespace == "" {
		return name
	}
	return namespace + "." + name
}

func profileTransportRequest(body map[string]any) transportRequestProfile {
	specs := map[string]transportTool{}
	remaining := 4096
	var collect func(any, string, int)
	collect = func(raw any, namespace string, depth int) {
		if depth > 8 || remaining <= 0 {
			return
		}
		list, _ := raw.([]any)
		for _, v := range list {
			if remaining <= 0 {
				return
			}
			remaining--
			item := transportMap(v)
			kind, name := transportText(item["type"]), transportText(item["name"])
			if name == "" || len(name) > 256 {
				continue
			}
			if kind == "namespace" {
				collect(item["tools"], transportToolKey(namespace, name), depth+1)
				continue
			}
			if kind != "function" && kind != "custom" {
				continue
			}
			rawCode := false
			for _, field := range []string{"parameters", "inputSchema", "input_schema"} {
				schema := transportMap(item[field])
				if schema == nil {
					continue
				}
				properties := transportMap(schema["properties"])
				rawCode = transportText(transportMap(properties["code"])["type"]) == "string" ||
					((name == "exec_command" || strings.HasSuffix(name, ".exec_command")) && transportText(transportMap(properties["cmd"])["type"]) == "string")
				break
			}
			specs[transportToolKey(namespace, name)] = transportTool{kind: kind, name: name, namespace: namespace, rawCode: rawCode}
		}
	}
	collect(body["tools"], "", 0)
	history := false
	input, _ := body["input"].([]any)
	for _, v := range input {
		item := transportMap(v)
		switch transportText(item["type"]) {
		case "additional_tools":
			collect(item["tools"], "", 0)
		case "function_call", "custom_tool_call", "function_call_output", "custom_tool_call_output":
			history = true
		}
	}
	profile := transportRequestProfile{buffer: len(specs) > 0 || history, family: "plain"}
	if !profile.buffer {
		return profile
	}
	callable := specs
	if transportText(body["tool_choice"]) == "none" {
		callable = map[string]transportTool{}
	} else if choice := transportMap(body["tool_choice"]); choice != nil {
		callable = map[string]transportTool{}
		selectTool := func(raw any) {
			t := transportMap(raw)
			key := transportToolKey(transportText(t["namespace"]), transportText(t["name"]))
			if spec, ok := specs[key]; ok && spec.kind == transportText(t["type"]) {
				callable[key] = spec
			}
		}
		if transportText(choice["type"]) == "allowed_tools" {
			list, _ := choice["tools"].([]any)
			for _, tool := range list {
				selectTool(tool)
			}
		} else {
			selectTool(choice)
		}
	}
	// For a result-only turn the most recent call batch describes the protocol
	// actually being continued. A broad catalog may include unrelated tools;
	// fall back to it only when the history contains no call to identify.
	classification := callable
	if len(classification) == 0 {
		classification = specs
		if history {
			if recent := transportHistoricalTools(input); len(recent) > 0 {
				for name, tool := range recent {
					if declared, ok := specs[name]; ok && declared.kind == tool.kind {
						recent[name] = declared
					}
				}
				classification = recent
			}
		}
	}
	parallel, explicitParallel := body["parallel_tool_calls"].(bool)
	profile.family = transportCapabilityClass(classification, explicitParallel && parallel)
	// Every callable client tool starts on the original native transport.
	// Some upstream models ignore text envelopes and invoke hosted tools even
	// for one optional function; such uncertain output cannot be retried.
	profile.preferNative = len(callable) > 0
	if len(callable) == 0 {
		profile.family += "/no_callable_tools"
	} else if transportToolCallRequired(body) {
		// Optional text successes do not establish required-call capability.
		profile.family += "/required"
	}

	return profile
}

func transportToolCallRequired(body map[string]any) bool {
	if transportText(body["tool_choice"]) == "required" {
		return true
	}
	choice := transportMap(body["tool_choice"])
	switch transportText(choice["type"]) {
	case "function", "custom":
		return true
	case "allowed_tools":
		return transportText(choice["mode"]) == "required"
	default:
		return false
	}
}

func transportCapabilityKey(conversation [32]byte, family string) [32]byte {
	raw, _ := json.Marshal([]any{"bps-capability-v1", hex.EncodeToString(conversation[:]), family})
	return sha256.Sum256(raw)
}

// Higher-ranked capabilities determine a mixed catalog's stable family.
// Learned success is reused only within that family and tool-choice contract.
func transportCapabilityClass(tools map[string]transportTool, explicitParallel bool) string {
	custom, code, namespaced := false, false, false
	for _, tool := range tools {
		custom = custom || tool.kind == "custom"
		code = code || tool.rawCode
		namespaced = namespaced || tool.namespace != ""
	}
	switch {
	case custom:
		return "custom"
	case code:
		return "code_command"
	case namespaced:
		return "namespaced"
	case len(tools) > 1 || (len(tools) > 0 && explicitParallel):
		return "multi_function"
	default:
		return "simple_function"
	}
}

func transportHistoricalTools(input []any) map[string]transportTool {
	tools := map[string]transportTool{}
	found := false
	for index := len(input) - 1; index >= 0; index-- {
		item := transportMap(input[index])
		kind := transportText(item["type"])
		if kind != "function_call" && kind != "custom_tool_call" {
			if found && transportText(item["role"]) == "user" {
				break
			}
			continue
		}
		found = true
		name, namespace := transportText(item["name"]), transportText(item["namespace"])
		if name == "" || len(name) > 256 {
			continue
		}
		tool := transportTool{kind: "function", name: name, namespace: namespace}
		if kind == "custom_tool_call" {
			tool.kind = "custom"
		} else {
			args := transportMap(item["arguments"])
			if raw, ok := item["arguments"].(string); ok {
				args, _ = transportObject([]byte(raw))
			}
			_, code := args["code"].(string)
			_, command := args["cmd"].(string)
			tool.rawCode = code || ((name == "exec_command" || strings.HasSuffix(name, ".exec_command")) && command)
		}
		tools[transportToolKey(namespace, name)] = tool
		if len(tools) >= 4096 {
			break
		}
	}
	return tools
}

func transportConversationKey(req executionRequest, cfg config, body map[string]any) [32]byte {
	account := ""
	if auth, ok := credentialFromStorage(req.StorageJSON); ok && auth.Grant != nil {
		account, _ = upstream.BPSProfile(auth.Grant.AccessToken)
		if account == "" {
			account, _ = upstream.BPSProfile(auth.Grant.IDToken)
		}
	}
	if account == "" {
		// In the absence of an account claim, invalidating on credential rotation
		// is safer than reusing a success learned for a different account.
		digest := sha256.Sum256(req.StorageJSON)
		account = "credential:" + hex.EncodeToString(digest[:])
	}
	canonical := transportText(req.Metadata["canonical_session_id"])
	conversation := ""
	if isBPSHeaderSession(canonical) {
		conversation = "canonical:" + canonical
	}
	if conversation == "" {
		for _, field := range []string{"prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
			if v := transportText(body[field]); v != "" {
				conversation = "body:" + v
				break
			}
		}
	}
	if conversation == "" {
		metadata := transportMap(body["client_metadata"])
		for _, field := range []string{"session_id", "sessionId"} {
			if v := transportText(metadata[field]); v != "" {
				conversation = "body:" + v
				break
			}
		}
	}
	if conversation == "" && canonical != "" {
		conversation = "canonical:" + canonical
	}
	if conversation == "" {
		var initial any
		if text, ok := body["input"].(string); ok {
			initial = transportInitialMessage(map[string]any{"role": "user", "content": text})
		} else {
			items, _ := body["input"].([]any)
			for _, v := range items {
				item := transportMap(v)
				if transportText(item["role"]) == "user" {
					initial = transportInitialMessage(item)
					break
				}
			}
			if initial == nil {
				for _, v := range items {
					item := transportMap(v)
					if transportText(item["type"]) == "message" || transportText(item["role"]) != "" {
						initial = transportInitialMessage(item)
						break
					}
				}
			}
		}
		if initial == nil {
			initial = body["input"]
		}
		encoded, _ := json.Marshal(initial)
		digest := sha256.Sum256(encoded)
		conversation = "initial:" + hex.EncodeToString(digest[:])
	}
	model := transportText(body["model"])
	if model == "" {
		model = cfg.upstreamModel(req.Model)
	}
	if model == "" {
		model = req.Model
	}
	clientID, tokenURL := "", ""
	if cfg.OAuth != nil {
		clientID, tokenURL = cfg.OAuth.ClientID, cfg.OAuth.TokenURL
	}
	partition, _ := json.Marshal([]any{"bps-transport-v1", req.AuthID, account, bpsCallerScope(req), model, cfg.ResponsesURL, cfg.OAuthPreset, clientID, tokenURL, cfg.BPSToolsVersionID, conversation})
	return sha256.Sum256(partition)
}

// Normalize equivalent first-turn shorthand so a string request and its later
// inline message history retain the same conversation binding.
func transportInitialMessage(item map[string]any) map[string]any {
	out := map[string]any{"type": transportText(item["type"]), "role": item["role"], "content": item["content"]}
	if out["type"] == "" {
		out["type"] = "message"
	}
	if text, ok := out["content"].(string); ok {
		out["content"] = []any{map[string]any{"type": "input_text", "text": text}}
	}
	if parts, ok := out["content"].([]any); ok {
		normalized := make([]any, 0, len(parts))
		for _, v := range parts {
			if text, ok := v.(string); ok {
				normalized = append(normalized, map[string]any{"type": "input_text", "text": text})
				continue
			}
			part := transportMap(v)
			kind := transportText(part["type"])
			if kind == "text" || kind == "input_text" || kind == "output_text" {
				normalized = append(normalized, map[string]any{"type": "input_text", "text": part["text"]})
			} else {
				normalized = append(normalized, v)
			}
		}
		out["content"] = normalized
	}
	if v, ok := item["encrypted_content"]; ok {
		out["encrypted_content"] = v
	}
	return out
}
