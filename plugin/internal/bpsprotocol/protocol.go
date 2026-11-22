// Adapted from JaxsonWang/cpa-plugin-oai-basispoints v0.2.10.
// Copyright (c) 2026 JaxsonWang. See LICENSE in this directory.
package bpsprotocol

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	transportName               = "run_officejs"
	transportAlias              = "functions.run_officejs"
	nativeRelayReference        = "External client tool"
	nativeReferencesInstruction = `Set the outer references field to ["External client tool"]. This required nonempty transport label is not a client tool argument, file path or workbook target.`
	toolCatalogPrefix           = "This request is relayed by an external Responses API client, not by the live Excel workbook. The native run_officejs function is a transport endpoint owned by this proxy. The proxy intercepts it before execution, so it never runs Office code or changes the workbook."
	toolCatalogReminder         = "Call each client tool using one native run_officejs call and the transport specified by its catalog: FUNCTION uses a name/arguments JSON envelope; FUNCTION_CODE and FUNCTION_CMD use raw code or cmd plus metadata JSON in extended_summary; CUSTOM uses its exact marker and raw input. " + nativeReferencesInstruction
)

type toolSpec struct {
	Key       string
	Name      string
	Namespace string
	Type      string
	Spec      map[string]any
}

var nativeCallCache = struct {
	sync.Mutex
	items map[string]map[string]any
	order []string
}{items: map[string]map[string]any{}}

func iterToolValues(tools any, namespace string, callback func(toolSpec)) {
	list, ok := tools.([]any)
	if !ok {
		return
	}
	for _, value := range list {
		tool, ok := value.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(strings.TrimSpace(stringValue(tool["type"])))
		name := strings.TrimSpace(stringValue(tool["name"]))
		if (toolType == "function" || toolType == "custom") && name != "" {
			key := name
			if namespace != "" {
				key = namespace + "." + name
			}
			callback(toolSpec{Key: key, Name: name, Namespace: namespace, Type: toolType, Spec: tool})
		}
		if toolType == "namespace" && name != "" {
			if namespace != "" {
				name = namespace + "." + name
			}
			iterToolValues(tool["tools"], name, callback)
		}
	}
}

func clientToolSpecs(source map[string]any) map[string]toolSpec {
	result := map[string]toolSpec{}
	add := func(spec toolSpec) { result[spec.Key] = spec }
	iterToolValues(source["tools"], "", add)
	// Codex 将动态工具目录放在输入历史中；后续声明覆盖同名工具。
	items, _ := source["input"].([]any)
	for _, value := range items {
		item := objectValue(value)
		if strings.EqualFold(strings.TrimSpace(stringValue(item["type"])), "additional_tools") {
			iterToolValues(item["tools"], "", add)
		}
	}
	return result
}

// 当前回合的工具限制不应改变历史调用的身份及回放。
func callableClientToolSpecs(source map[string]any) map[string]toolSpec {
	specs := clientToolSpecs(source)
	if stringValue(source["tool_choice"]) == "none" {
		return map[string]toolSpec{}
	}
	choice := objectValue(source["tool_choice"])
	if choice == nil {
		return specs
	}
	selected := map[string]toolSpec{}
	selectTool := func(value any) {
		tool := objectValue(value)
		key := clientToolCallName(tool)
		if spec, ok := specs[key]; ok && spec.Type == stringValue(tool["type"]) {
			selected[key] = spec
		}
	}
	if stringValue(choice["type"]) == "allowed_tools" {
		tools, _ := choice["tools"].([]any)
		for _, tool := range tools {
			selectTool(tool)
		}
	} else {
		selectTool(choice)
	}
	return selected
}

func clientToolCallRequired(source map[string]any) bool {
	if stringValue(source["tool_choice"]) == "required" {
		return true
	}
	choice := objectValue(source["tool_choice"])
	switch stringValue(choice["type"]) {
	case "function", "custom":
		return true
	case "allowed_tools":
		return stringValue(choice["mode"]) == "required"
	}
	return false
}

func messageItem(role, text string) map[string]any {
	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}
	return map[string]any{
		"type":    "message",
		"role":    role,
		"content": []any{map[string]any{"type": contentType, "text": text}},
	}
}

func clientToolProtocolInstructions(source map[string]any) string {
	specs := callableClientToolSpecs(source)
	if len(specs) == 0 {
		return "This request is relayed by an external Responses API client, not by the live Excel workbook. Do not call server-injected Excel, Office, connector, or workbook tools. Return the answer as assistant text."
	}
	catalog := make([]string, 0, len(specs))
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		spec := specs[name]
		line := "- " + spec.Key + " (" + spec.Type + ")"
		if description := stringValue(spec.Spec["description"]); description != "" {
			line += ": " + description
		}
		if spec.Type == "function" {
			if parameters := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); parameters != nil {
				line += ". Its arguments are an object with " + describeParameterNames(parameters) + ". JSON Schema: " + string(jsonBytes(parameters))
			}
			if field := functionRelayField(spec); field != "" {
				line += ". Use FUNCTION_" + strings.ToUpper(field) + " transport: summary=" + functionRelayPrefix(field) + spec.Key + ", exact raw " + field + " in native code, all other arguments as one JSON object in extended_summary ({} if none). Never include " + field + " in extended_summary."
			}
		} else {
			line += ". It receives raw text in input."
			if format := objectValue(spec.Spec["format"]); format != nil {
				line += " Input format: " + string(jsonBytes(format))
			}
		}
		catalog = append(catalog, line)
	}
	catalogText := strings.Join(catalog, "\n")
	if choice, exists := source["tool_choice"]; exists && choice != nil {
		catalogText += "\nClient tool_choice: " + string(jsonBytes(choice))
	}
	if parallel, ok := source["parallel_tool_calls"].(bool); ok && !parallel {
		catalogText += "\nInvoke at most one client tool in this response."
	}
	required := ""
	if clientToolCallRequired(source) {
		required = " This response must contain a call to a tool selected by the client's tool_choice."
	}
	return toolCatalogPrefix + " The native run_officejs call carries a request for the external client to execute." +
		" For ordinary FUNCTION transport, set code to JSON containing the exact catalog name in name and the parameter object in arguments. Follow a catalog FUNCTION_CODE or FUNCTION_CMD override instead when specified." +
		" For a custom tool, set summary to codex2api.custom/CATALOG_NAME and set code to its exact raw input, preserving whitespace." +
		" Include extended_summary and destructive=false in the outer arguments. " + nativeReferencesInstruction +
		" Treat tool results as the named external tool's results, then continue the task without repeating completed calls." +
		" Use only the tools listed below. Answer as assistant text when no tool is required." +
		required + " " + functionRelayEncoding + clientToolRelayExamples(names, specs) +
		"\nAvailable client tools\n" + catalogText + "\n" + toolCatalogReminder
}

func describeParameterNames(parameters map[string]any) string {
	properties := objectValue(parameters["properties"])
	if len(properties) == 0 {
		return "the arguments required by the client"
	}
	required := map[string]bool{}
	if list, ok := parameters["required"].([]any); ok {
		for _, value := range list {
			required[stringValue(value)] = true
		}
	}
	names := make([]string, 0, len(properties))
	for name := range properties {
		suffix := "optional"
		if required[name] {
			suffix = "required"
		}
		names = append(names, name+" ("+suffix+")")
	}
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	return strings.Join(names, ", ")
}

func clientToolProtocolReminder(source map[string]any) string {
	specs := callableClientToolSpecs(source)
	if len(specs) == 0 {
		return ""
	}
	names := make([]string, 0, len(specs))
	for name := range specs {
		names = append(names, name)
	}
	// Small deterministic ordering without importing sort in every caller.
	for i := 0; i < len(names); i++ {
		for j := i + 1; j < len(names); j++ {
			if names[j] < names[i] {
				names[i], names[j] = names[j], names[i]
			}
		}
	}
	reminder := toolCatalogReminder + " " + functionRelayEncoding + " Client tools: " + strings.Join(names, ", ") + "."
	for _, name := range names {
		spec := specs[name]
		if spec.Type == "custom" {
			reminder += " Custom tool " + name + " uses summary=codex2api.custom/" + name + " and the exact input in code."
		}
	}
	return reminder
}

func firstMap(object map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if value := objectValue(object[key]); value != nil {
			return value
		}
	}
	return nil
}

func objectValue(value any) map[string]any {
	object, _ := value.(map[string]any)
	return object
}

func stripClientMetadata(item map[string]any) map[string]any {
	if _, exists := item["internal_chat_message_metadata_passthrough"]; !exists {
		return item
	}
	copy := cloneObject(item)
	delete(copy, "internal_chat_message_metadata_passthrough")
	return copy
}

// BPS replays reasoning only through its opaque envelope. History preparation
// can intentionally remove an old ciphertext; neither tool transport should
// put that now-incomplete reasoning item back on the wire.
func reasoningHistoryItem(item map[string]any) map[string]any {
	if encrypted := stringValue(item["encrypted_content"]); encrypted != "" {
		return map[string]any{"type": "reasoning", "summary": []any{}, "encrypted_content": encrypted}
	}
	return nil
}

func cloneObject(object map[string]any) map[string]any {
	if object == nil {
		return nil
	}
	raw, _ := json.Marshal(object)
	var copy map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	_ = decoder.Decode(&copy)
	return copy
}

func rememberNativeCall(item, client map[string]any, scope string) {
	callID := stringValue(item["call_id"])
	if callID == "" {
		return
	}
	if len(jsonBytes(item)) > 128<<10 {
		return
	}
	callID = replayKey(scope, client)
	copy := cloneObject(item)
	nativeCallCache.Lock()
	defer nativeCallCache.Unlock()
	if _, exists := nativeCallCache.items[callID]; !exists {
		nativeCallCache.order = append(nativeCallCache.order, callID)
	}
	nativeCallCache.items[callID] = copy
	for len(nativeCallCache.order) > 512 {
		oldest := nativeCallCache.order[0]
		nativeCallCache.order = nativeCallCache.order[1:]
		delete(nativeCallCache.items, oldest)
	}
}

func rememberedNativeCall(client map[string]any, scope string) map[string]any {
	callID := replayKey(scope, client)
	nativeCallCache.Lock()
	defer nativeCallCache.Unlock()
	return cloneObject(nativeCallCache.items[callID])
}

func functionItemID(callID string) string {
	if callID == "" {
		return ""
	}
	if strings.HasPrefix(callID, "fc_") {
		return callID
	}
	return "fc_" + callID
}

func clientToolCallName(item map[string]any) string {
	name := stringValue(item["name"])
	if namespace := stringValue(item["namespace"]); namespace != "" {
		return namespace + "." + name
	}
	return name
}

func fallbackTransportCall(item map[string]any) map[string]any {
	name := clientToolCallName(item)
	callID := stringValue(item["call_id"])
	if callID == "" {
		callID = "call_bp_" + shortHash(fmt.Sprintf("%v", time.Now().UnixNano()))
	}
	payload, _ := item["arguments"].(string)
	if stringValue(item["type"]) == "custom_tool_call" {
		payload, _ = item["input"].(string)
	}
	outerArguments := map[string]any{
		"summary":          "Run client tool " + name,
		"extended_summary": "Relay " + name + " through the external client",
		"code":             payload,
		"destructive":      false,
		"references":       []any{nativeRelayReference},
	}
	if item["type"] == "custom_tool_call" {
		outerArguments["summary"] = customRelayPrefix + name
	} else {
		arguments, _ := parseRelayObject(item["arguments"])
		if field := historicalFunctionRelayField(name, arguments); field != "" {
			outerArguments = rawFunctionRelayArguments(name, field, arguments)
		} else {
			outerArguments["code"] = string(jsonBytes(map[string]any{"name": name, "arguments": arguments}))
		}
	}
	return map[string]any{
		"type":      "function_call",
		"id":        functionItemID(callID),
		"call_id":   callID,
		"name":      transportName,
		"arguments": string(jsonBytes(outerArguments)),
		"status":    "completed",
	}
}

func translateInputItems(rawInput any, scope string) []any {
	if text, ok := rawInput.(string); ok {
		return []any{messageItem("user", text)}
	}
	items, ok := rawInput.([]any)
	if !ok {
		return []any{}
	}
	result := make([]any, 0, len(items))
	origins := map[string]string{}
	for _, value := range items {
		item, ok := value.(map[string]any)
		if !ok {
			continue
		}
		item = stripClientMetadata(item)
		itemType := strings.ToLower(strings.TrimSpace(stringValue(item["type"])))
		if itemType == "function_call" || itemType == "custom_tool_call" {
			callID := stringValue(item["call_id"])
			if native := rememberedNativeCall(item, scope); native != nil {
				if callID != "" {
					origins[callID] = stringValue(native["name"])
				}
				result = append(result, native)
				continue
			}
			name := clientToolCallName(item)
			if name == transportName || name == transportAlias {
				if callID != "" {
					origins[callID] = transportName
				}
				result = append(result, item)
				continue
			}
			// 压缩请求可能没有工具目录；历史调用自身已包含名称和载荷。
			if callID != "" {
				origins[callID] = transportName
			}
			result = append(result, fallbackTransportCall(item))
			continue
		}
		if itemType == "function_call_output" || itemType == "custom_tool_call_output" {
			callID := stringValue(item["call_id"])
			if origins[callID] == transportName {
				copy := cloneObject(item)
				copy["type"] = "function_call_output"
				copy["id"] = functionItemID(callID)
				// 结果由 call_id 关联；客户端工具名不属于上游原生调用。
				delete(copy, "name")
				delete(copy, "namespace")
				result = append(result, copy)
			} else {
				result = append(result, item)
			}
			continue
		}
		if itemType == "reasoning" {
			if reasoning := reasoningHistoryItem(item); reasoning != nil {
				result = append(result, reasoning)
			}
			continue
		}
		// 工具目录已转换为中继协议说明，不向上游注入另一份原生工具。
		if itemType == "item_reference" || itemType == "additional_tools" {
			continue
		}
		// agent_message 的 author/recipient 是原生协议字段，不能作为客户端元数据剥离。
		result = append(result, item)
	}
	return result
}

func itemText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	if list, ok := value.([]any); ok {
		var builder strings.Builder
		for _, part := range list {
			if text := stringValue(part); text != "" {
				builder.WriteString(text)
				continue
			}
			if object := objectValue(part); object != nil {
				builder.WriteString(stringValue(object["text"]))
			}
		}
		return builder.String()
	}
	return ""
}

func conversationKey(source map[string]any, translated []any) string {
	if explicit := explicitConversationKey(source); explicit != "" {
		return explicit
	}
	return conversationFingerprint(translated)
}

func explicitConversationKey(source map[string]any) string {
	for _, key := range []string{"prompt_cache_key", "promptCacheKey", "session_id", "sessionId"} {
		if value := stringValue(source[key]); value != "" {
			return value
		}
	}
	if metadata := objectValue(source["client_metadata"]); metadata != nil {
		for _, key := range []string{"session_id", "sessionId"} {
			if value := stringValue(metadata[key]); value != "" {
				return value
			}
		}
	}
	return ""
}

func conversationFingerprint(items []any) string {
	for _, value := range items {
		if object := objectValue(value); object != nil && strings.EqualFold(stringValue(object["role"]), "user") {
			return shortHash(string(jsonBytes(object)))
		}
	}
	for _, value := range items {
		if object := objectValue(value); object != nil {
			return shortHash(string(jsonBytes(object)))
		}
	}
	return "anonymous"
}

func turnState(rawInput any) (string, string) {
	items, ok := rawInput.([]any)
	if !ok || len(items) == 0 {
		return shortHash(string(jsonBytes(rawInput))), "1"
	}
	lastUser := -1
	for index, value := range items {
		if object := objectValue(value); object != nil && strings.EqualFold(stringValue(object["role"]), "user") {
			lastUser = index
		}
	}
	if lastUser < 0 {
		lastUser = 0
	}
	prefix := items[:lastUser+1]
	fingerprint := shortHash(string(jsonBytes(prefix)))
	iteration := 1
	for _, value := range items[lastUser+1:] {
		if object := objectValue(value); object != nil {
			typeName := stringValue(object["type"])
			if typeName == "function_call_output" || typeName == "custom_tool_call_output" {
				iteration++
			}
		}
	}
	return fingerprint, fmt.Sprintf("%d", iteration)
}

func shortHash(text string) string {
	digest := sha256.Sum256([]byte(text))
	return hex.EncodeToString(digest[:])
}

var urlNamespace = [16]byte{0x6b, 0xa7, 0xb8, 0x11, 0x9d, 0xad, 0x11, 0xd1, 0x80, 0xb4, 0x00, 0xc0, 0x4f, 0xd4, 0x30, 0xc8}

func uuidV5(name string) string {
	hash := sha1.New()
	_, _ = hash.Write(urlNamespace[:])
	_, _ = hash.Write([]byte(name))
	digest := hash.Sum(nil)
	digest[6] = (digest[6] & 0x0f) | 0x50
	digest[8] = (digest[8] & 0x3f) | 0x80
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", digest[0:4], digest[4:6], digest[6:8], digest[8:10], digest[10:16])
}

func appendBeforeCompaction(items []any, injected []any) []any {
	if len(items) > 0 {
		if last := objectValue(items[len(items)-1]); last != nil && stringValue(last["type"]) == "compaction_trigger" {
			result := append([]any{}, items[:len(items)-1]...)
			result = append(result, injected...)
			return append(result, items[len(items)-1])
		}
	}
	return append(items, injected...)
}

func prependBeforeCompaction(items []any, prefix []any) []any {
	result := append([]any{}, prefix...)
	return append(result, items...)
}

func prepareResponsesBody(source map[string]any, cfg Config) (map[string]any, error) {
	if previous, exists := source["previous_response_id"]; exists && previous != nil {
		return nil, fail(400, "unsupported_continuation", "oai-basispoints does not support previous_response_id; omit it and send the complete input history, including tool calls and results")
	}
	// 已验证的上游普通模式不接受 service_tier，不能将 Fast 静默降级。
	if tier := source["service_tier"]; tier != nil && tier != "auto" && tier != "default" {
		return nil, fail(400, "unsupported_service_tier", "oai-basispoints supports only the standard service tier; omit service_tier or use auto/default; Fast/priority is not supported")
	}
	if err := validateTextFormat(source["text"]); err != nil {
		return nil, err
	}
	if err := validateAgentMessageEncryption(source["input"]); err != nil {
		return nil, err
	}
	model := stringValue(source["model"])
	upstream, ok := cfg.resolveUpstreamModel(model)
	if !ok {
		return nil, fail(400, "unsupported_model", "model is not enabled in oai-basispoints: "+model)
	}
	if clientToolCallRequired(source) && len(callableClientToolSpecs(source)) == 0 {
		return nil, fail(400, "invalid_tool_choice", "tool_choice does not select any available client tool")
	}
	inputItems := translateInputItems(source["input"], cfg.Scope)
	historyRoot := conversationFingerprint(inputItems)
	prologue := []any{}
	if instructions, _ := source["instructions"].(string); instructions != "" {
		prologue = append(prologue, messageItem("developer", instructions))
	}
	prologue = append(prologue, messageItem("developer", clientToolProtocolInstructions(source)))
	if reminder := clientToolProtocolReminder(source); reminder != "" {
		prologue = append(prologue, messageItem("developer", reminder))
	}
	if cfg.FormatInstructions != "" {
		prologue = append(prologue, messageItem("developer", cfg.FormatInstructions))
	}
	inputItems = prependBeforeCompaction(inputItems, prologue)

	output := map[string]any{
		"model":            upstream,
		"model_selection":  "explicit",
		"stream":           source["stream"] == true,
		"store":            false,
		"input":            inputItems,
		"reasoning_effort": reasoningEffortFromSource(source),
	}
	// 未指定或为空时省略可选字段，不发送服务端拒绝的空数组。
	if policy, exists := source["context_management"]; exists && policy != nil {
		if entries, isArray := policy.([]any); !isArray || len(entries) > 0 {
			output["context_management"] = policy
		}
	}
	conversation := explicitConversationKey(source)
	if conversation == "" {
		conversation = historyRoot
	}
	// Cache routing follows a conversation within one account/caller/model.
	// Only the bounded digest leaves the process, including for explicit keys.
	cacheIdentity := jsonBytes([]string{"bps-cache-v1", cfg.Scope, cfg.Model, conversation})
	output["prompt_cache_key"] = shortHash(string(cacheIdentity))
	metadata := map[string]any{}
	if rawMetadata := objectValue(source["metadata"]); rawMetadata != nil {
		for key, value := range rawMetadata {
			if key == "turn_id" || key == "task_id" || key == "agent_iteration" {
				continue
			}
			switch typed := value.(type) {
			case string:
				metadata[key[:minInt(len(key), 64)]] = typed[:minInt(len(typed), 512)]
			case json.Number, bool, float64:
				text := fmt.Sprint(typed)
				metadata[key[:minInt(len(key), 64)]] = text[:minInt(len(text), 512)]
			}
		}
	}
	turnFingerprint, iteration := turnState(source["input"])
	metadata["task_id"] = uuidV5("cpa-oai-basispoints/" + cfg.Scope + "/" + conversation)
	metadata["turn_id"] = uuidV5("cpa-oai-basispoints/" + cfg.Scope + "/" + conversation + "/turn/" + turnFingerprint)
	metadata["agent_iteration"] = iteration
	if cfg.ToolsVersionID != "" {
		metadata["bps_tools_version_id"] = cfg.ToolsVersionID
	}
	output["metadata"] = metadata
	return output, nil
}

func validateAgentMessageEncryption(input any) error {
	items, _ := input.([]any)
	for _, value := range items {
		item := objectValue(value)
		if strings.ToLower(strings.TrimSpace(stringValue(item["type"]))) != "agent_message" {
			continue
		}
		if encrypted, exists := item["encrypted_content"]; exists && encrypted != nil {
			if text, ok := encrypted.(string); !ok || strings.TrimSpace(text) == "" {
				return fail(400, "bps_invalid_agent_message", "agent_message encrypted content must be a nonempty opaque string")
			}
		}
		if value, exists := item["content"]; exists && value != nil {
			if _, valid := value.([]any); !valid {
				return fail(400, "bps_invalid_agent_message", "agent_message content must be an array")
			}
		}
		content, _ := item["content"].([]any)
		for _, value := range content {
			if part := objectValue(value); stringValue(part["type"]) == "encrypted_content" {
				if text, ok := part["encrypted_content"].(string); !ok || strings.TrimSpace(text) == "" {
					return fail(400, "bps_invalid_agent_message", "agent_message encrypted content must be a nonempty opaque string")
				}
			}
		}
	}
	return nil
}

// 尚未接入结构化输出契约，不能丢弃格式后把普通正文当成成功结果。
// verbosity 的上游契约仍待验证，本次不改变既有客户端的默认请求处理。
func validateTextFormat(value any) error {
	if value == nil {
		return nil
	}
	text, ok := value.(map[string]any)
	if !ok {
		return fail(400, "invalid_text_config", "text must be an object")
	}
	value = text["format"]
	if value == nil {
		return nil
	}
	format, ok := value.(map[string]any)
	if !ok {
		return fail(400, "invalid_text_format", "text.format must be an object")
	}
	switch format["type"] {
	case "text":
		if len(format) != 1 {
			return fail(400, "invalid_text_format", "plain text.format accepts only type")
		}
		return nil
	case "json_object", "json_schema":
		return fail(400, "unsupported_text_format", "oai-basispoints does not implement structured text.format output; omit the format or use type=text")
	default:
		return fail(400, "invalid_text_format", "text.format.type must be text, json_object, or json_schema")
	}
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func reasoningEffortFromSource(source map[string]any) string {
	if reasoning := objectValue(source["reasoning"]); reasoning != nil {
		return normalizeEffort(reasoning["effort"])
	}
	return normalizeEffort(source["reasoning_effort"])
}

func isTransportName(name string) bool {
	return name == transportName || name == transportAlias
}

func parseArguments(value any) map[string]any {
	object, _ := parseRelayObject(value)
	return object
}

// 诊断只返回类别及偏移，不包含工具参数、补丁正文或认证信息。
func parseRelayObject(value any) (map[string]any, string) {
	if object := objectValue(value); object != nil {
		return object, ""
	}
	text, ok := value.(string)
	if !ok {
		return nil, "not_object_or_json_string"
	}
	var object map[string]any
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil {
		var syntax *json.SyntaxError
		if errors.As(err, &syntax) {
			return nil, fmt.Sprintf("invalid_json byte_offset=%d", syntax.Offset)
		}
		return nil, "invalid_json_object"
	}
	if object == nil {
		return nil, "null_object"
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, "trailing_content"
	}
	return object, ""
}

func relayError(reason string) error {
	// CPA v7.3.17 的 JSON ABI 没有 request-scoped 标志；422 不会冷却凭据。
	return &Error{status: 422, code: "invalid_tool_call",
		message:          "Basis Points returned an invalid client tool relay: " + reason,
		diagnosticReason: safeRelayReason(reason)}
}

func historyToolCollisionError() error {
	return fail(422, "bps_duplicate_tool_call", "BPS reused a tool identifier from the supplied request history")
}

func transportEnvelope(native map[string]any, declared map[string]toolSpec) (map[string]any, error) {
	if stringValue(native["type"]) != "function_call" || !isTransportName(stringValue(native["name"])) {
		return nil, relayError("outer_not_transport")
	}
	arguments, reason := parseRelayObject(native["arguments"])
	if reason != "" {
		return nil, relayError("outer_arguments " + reason)
	}
	code, ok := arguments["code"].(string)
	if !ok || len(code) > 1<<20 {
		return nil, relayError("code_invalid")
	}
	if name, field, marked := rawFunctionRelayMarker(arguments["summary"]); marked {
		if !validRelayName(name) {
			return nil, relayError("function_marker_invalid")
		}
		metadata, reason := parseRelayObject(arguments["extended_summary"])
		if reason != "" {
			return nil, relayError("function_metadata_invalid")
		}
		if _, duplicate := metadata[field]; duplicate {
			return nil, relayError("function_metadata_duplicates_payload")
		}
		if references, exists := arguments["references"]; exists {
			if !validRawRelayReferences(references) {
				return nil, relayError("function_marker_references_invalid")
			}
		}
		if destructive, exists := arguments["destructive"]; exists && destructive != false {
			return nil, relayError("function_marker_destructive_invalid")
		}
		metadata[field] = code
		return map[string]any{"tool": name, "args": metadata, "kind": "function", "raw_field": field}, nil
	}
	if summary, ok := arguments["summary"].(string); ok && strings.HasPrefix(summary, customRelayPrefix) {
		name := strings.TrimPrefix(summary, customRelayPrefix)
		if !validRelayName(name) {
			return nil, relayError("custom_marker_invalid")
		}
		return map[string]any{"tool": name, "args": code, "kind": "custom"}, nil
	}
	// A legacy selector must name a declared tool. Native references may also
	// contain filenames or workbook references; those must not override an
	// explicit name/arguments envelope. Keep declared selectors authoritative
	// so raw custom JSON and conflicting tool names remain unambiguous.
	if references, ok := arguments["references"].([]any); ok && len(references) == 1 {
		if name, ok := references[0].(string); ok && validRelayName(name) {
			if _, legacy := declared[name]; legacy {
				return map[string]any{"tool": name, "args": code, "legacy_reference": true}, nil
			}
		}
	}
	if envelope, reason := parseRelayObject(code); reason == "" {
		if name, ok := envelope["name"].(string); ok && validRelayName(name) {
			// The original relay accepts both an arguments object and its
			// JSON string representation. Decode exactly one object; the
			// declared function schema is still enforced before delivery.
			if arguments, reason := parseRelayObject(envelope["arguments"]); reason == "" && len(envelope) == 2 {
				return map[string]any{"tool": name, "args": arguments, "kind": "function"}, nil
			}
			if input, ok := envelope["input"].(string); ok && len(envelope) == 2 {
				return map[string]any{"tool": name, "args": input, "kind": "custom"}, nil
			}
		}
	}
	// Older plugin histories carry the client name in references.
	references, ok := arguments["references"].([]any)
	if !ok || len(references) != 1 {
		return nil, relayError("references_must_select_one_tool")
	}
	name, ok := references[0].(string)
	if !ok || name == "" || isTransportName(name) {
		return nil, relayError("invalid_client_tool_reference")
	}
	return map[string]any{"tool": name, "args": code, "legacy_reference": true}, nil
}

const customRelayPrefix = "codex2api.custom/"

func validRelayName(name string) bool {
	if name == "" || len(name) > 256 || strings.TrimSpace(name) != name || isTransportName(name) {
		return false
	}
	for _, character := range name {
		if character <= ' ' || character == '/' || character == '\\' || character == 127 {
			return false
		}
	}
	return true
}

func schemaMatches(value any, schema map[string]any) bool {
	if len(schema) == 0 {
		return true
	}
	if alternatives, ok := schema["type"].([]any); ok {
		for _, alternative := range alternatives {
			copy := cloneObject(schema)
			copy["type"] = alternative
			if schemaMatches(value, copy) {
				return true
			}
		}
		return false
	}
	switch stringValue(schema["type"]) {
	case "object":
		object := objectValue(value)
		if object == nil {
			return false
		}
		if required, ok := schema["required"].([]any); ok {
			for _, name := range required {
				if _, exists := object[stringValue(name)]; !exists {
					return false
				}
			}
		}
		properties := objectValue(schema["properties"])
		for key, nested := range object {
			if properties == nil {
				continue
			}
			nestedSchema := objectValue(properties[key])
			if nestedSchema == nil {
				if schema["additionalProperties"] == false {
					return false
				}
				continue
			}
			if !schemaMatches(nested, nestedSchema) {
				return false
			}
		}
	case "array":
		items, ok := value.([]any)
		if !ok {
			return false
		}
		if itemSchema := objectValue(schema["items"]); itemSchema != nil {
			for _, item := range items {
				if !schemaMatches(item, itemSchema) {
					return false
				}
			}
		}
	case "string":
		if _, ok := value.(string); !ok {
			return false
		}
	case "integer", "number":
		switch value.(type) {
		case json.Number, float64, int, int64:
		default:
			return false
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return false
		}
	case "null":
		if value != nil {
			return false
		}
	}
	if enum, ok := schema["enum"].([]any); ok && len(enum) > 0 {
		matched := false
		for _, option := range enum {
			if fmt.Sprint(option) == fmt.Sprint(value) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
	}
	return true
}

func extractNativeClientToolCall(native map[string]any, specs map[string]toolSpec) (map[string]any, error) {
	return extractNativeClientToolCallIn(native, specs, specs)
}

// callable 限制本轮调用权限，declared 只用于区分目录缺失与本轮禁用。
func extractNativeClientToolCallIn(native map[string]any, callable, declared map[string]toolSpec) (map[string]any, error) {
	if !isTransportName(stringValue(native["name"])) {
		return directClientCall(native, callable)
	}
	inner, err := transportEnvelope(native, declared)
	if err != nil {
		return nil, err
	}
	name, _ := inner["tool"].(string)
	spec, exists := callable[name]
	if !exists {
		if _, declaredOnly := declared[name]; declaredOnly {
			return nil, relayError("tool_not_allowed_by_tool_choice")
		}
		return nil, relayError("tool_not_in_catalog")
	}
	if kind := inner["kind"]; kind != nil && kind != spec.Type {
		return nil, relayError("relay_tool_kind_mismatch")
	}
	if field, _ := inner["raw_field"].(string); field != "" && functionRelayField(spec) != field {
		return nil, relayError("function_transport_not_in_catalog")
	}
	callID := stringValue(native["call_id"])
	if callID == "" {
		return nil, relayError("missing_call_id")
	}
	result := map[string]any{
		"type":    "function_call",
		"id":      stringValue(native["id"]),
		"call_id": callID,
		"name":    spec.Name,
		"status":  "completed",
	}
	if result["id"] == "" {
		result["id"] = functionItemID(callID)
	}
	if spec.Namespace != "" {
		result["namespace"] = spec.Namespace
	}
	if spec.Type == "custom" {
		result["type"] = "custom_tool_call"
		result["id"] = "ctc_" + strings.TrimPrefix(stringValue(result["id"]), "fc_")
		result["input"] = inner["args"]
	} else {
		parsed, reason := parseRelayObject(inner["args"])
		if reason != "" {
			return nil, relayError("code " + reason)
		}
		if contract := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema"); contract != nil {
			schema, err := compileSchema(contract)
			if err != nil {
				return nil, err
			}
			if schema.Validate(parsed) != nil {
				// Some native responses combine the old references selector with
				// the current name/arguments envelope. Resolve this only after the
				// direct legacy arguments fail the complete declared schema. A
				// legitimate function with name/arguments fields must stay intact.
				if inner["legacy_reference"] == true && len(parsed) == 2 {
					envelopeName, named := parsed["name"].(string)
					envelopeArgs, reason := parseRelayObject(parsed["arguments"])
					if named && reason == "" {
						if envelopeName != name {
							return nil, relayError("conflicting_tool_reference")
						}
						parsed = envelopeArgs
					}
				}
				if schema.Validate(parsed) != nil {
					return nil, relayError("arguments_schema_mismatch")
				}
			}
		}
		result["arguments"] = string(jsonBytes(parsed))
		result["status"] = "completed"
		// Native encrypted function arguments belong to the transport, not to
		// the declared client function. Keep the original plugin's empty marker.
		result["encrypted_function_args"] = []any{}
	}
	return result, nil
}

func directClientCall(native map[string]any, callable map[string]toolSpec) (map[string]any, error) {
	key := clientToolCallName(native)
	spec, ok := callable[key]
	if !ok || stringValue(native["call_id"]) == "" {
		return nil, relayError("direct_tool_not_in_catalog")
	}
	result := cloneObject(native)
	switch spec.Type {
	case "function":
		if native["type"] != "function_call" {
			return nil, relayError("direct_tool_kind_mismatch")
		}
		arguments, reason := parseRelayObject(native["arguments"])
		if reason != "" {
			return nil, relayError("direct_arguments_invalid")
		}
		result["arguments"] = string(jsonBytes(arguments))
	case "custom":
		if native["type"] != "custom_tool_call" {
			return nil, relayError("direct_tool_kind_mismatch")
		}
		if _, ok := native["input"].(string); !ok {
			return nil, relayError("direct_custom_input_invalid")
		}
	default:
		return nil, relayError("direct_tool_kind_unavailable")
	}
	result["name"] = spec.Name
	if spec.Namespace != "" {
		result["namespace"] = spec.Namespace
	}
	return result, nil
}

func transformResponseBody(body []byte, source map[string]any, scope string) ([]byte, map[string]any, bool, error) {
	var response map[string]any
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.UseNumber()
	if err := decoder.Decode(&response); err != nil || response == nil {
		return nil, nil, false, fail(502, "invalid_upstream_response", "Basis Points returned invalid JSON")
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return nil, nil, false, fail(502, "invalid_upstream_response", "Basis Points returned trailing response data")
	}
	output, _ := response["output"].([]any)
	specs := callableClientToolSpecs(source)
	declared := clientToolSpecs(source)
	replaced := make([]any, 0, len(output))
	natives := make([]map[string]any, 0)
	clientCalls := make([]map[string]any, 0)
	callIDs := map[string]bool{}
	itemIDs := map[string]bool{}
	checkItemID := func(item map[string]any) error {
		id := stringValue(item["id"])
		if id != "" {
			if itemIDs[id] {
				return relayError("duplicate_output_item_id")
			}
			itemIDs[id] = true
		}
		return nil
	}
	for _, value := range output {
		item := objectValue(value)
		if item == nil || (stringValue(item["type"]) != "function_call" && stringValue(item["type"]) != "custom_tool_call") {
			if err := checkItemID(item); err != nil {
				return nil, nil, false, err
			}
			replaced = append(replaced, value)
			continue
		}
		call, err := extractNativeClientToolCallIn(item, specs, declared)
		if err != nil {
			// 不把服务器注入工具或损坏的中转载荷交给客户端执行。
			return nil, nil, false, err
		}
		if err := checkItemID(call); err != nil {
			return nil, nil, false, err
		}
		callID := stringValue(call["call_id"])
		if callIDs[callID] {
			return nil, nil, false, relayError("duplicate_call_id")
		}
		callIDs[callID] = true
		replaced = append(replaced, call)
		natives = append(natives, item)
		clientCalls = append(clientCalls, call)
	}
	if len(natives) == 0 {
		if clientToolCallRequired(source) {
			return nil, nil, false, relayError("required_tool_choice_not_satisfied")
		}
		return body, response, false, nil
	}
	if parallel, ok := source["parallel_tool_calls"].(bool); ok && !parallel && len(natives) > 1 {
		return nil, nil, false, relayError("parallel_tool_calls_disabled")
	}
	if responseReusesHistoryToolID(source, replaced) {
		return nil, nil, false, historyToolCollisionError()
	}
	for index, native := range natives {
		replay := native
		if !isTransportName(stringValue(native["name"])) {
			replay = fallbackTransportCall(clientCalls[index])
		}
		rememberNativeCall(replay, clientCalls[index], scope)
	}
	response["output"] = replaced
	return jsonBytes(response), response, true, nil
}
