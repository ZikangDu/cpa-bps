package bpsprotocol

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strconv"
)

const maxFallbackResponseBytes = 16 << 20

var ErrInvalidResponseUsage = errors.New("response usage cannot be safely combined")

// CanFallbackToolResponse checks only a fully received, buffered protocol
// rejection. The caller must additionally enforce its total attempt budget,
// HTTP success, same selected credential, and no prior client delivery.
// Known Office relays are client-executed by this proxy contract; unknown hosted
// actions cannot be assumed unexecuted and therefore never permit a retry.
func CanFallbackToolResponse(raw []byte, cause error) bool {
	var problem *Error
	if !errors.As(cause, &problem) || problem.Code() != "invalid_tool_call" || problem.StatusCode() != 422 {
		return false
	}
	_, ok := safeAlternateTerminal(raw, false)
	return ok
}

// DecodeAlternateToolResponse decodes a complete buffered reply using the
// other local wire representation. It performs no upstream request and never
// treats ordinary prose as a successful alternative tool response. The caller
// should invoke it only after its primary transform failed, before delivery.
func DecodeAlternateToolResponse(source, terminal []byte, scope string, primary Options) (out []byte, mode string, ok bool) {
	response, valid := safeAlternateTerminal(terminal, true)
	if !valid {
		return nil, "", false
	}
	mode = "native"
	switch primary.ToolTransport {
	case "", "text":
	case "native":
		mode = "text"
	default:
		return nil, "", false
	}
	_, bridge, err := PrepareWithOptions(source, scope, Options{ToolTransport: mode, ToolsVersionID: primary.ToolsVersionID})
	if err != nil || len(callableClientToolSpecs(bridge.source)) == 0 {
		return nil, "", false
	}
	if mode == "native" && !preflightAlternateNative(bridge, response) {
		return nil, "", false
	}
	out, err = bridge.Transform(terminal)
	if err != nil {
		return nil, "", false
	}
	converted, valid := fallbackObject(out)
	if !valid || converted["status"] != "completed" || !responseHasClientTool(converted) || alternateHistoryCollision(bridge.source, converted["output"].([]any)) {
		return nil, "", false
	}
	return out, mode, true
}

// Native Transform records replay state before its final compiled-schema and
// structured checks. Run those checks without mutation before attempting it,
// so an unsuccessful alternate interpretation cannot poison replay history.
func preflightAlternateNative(bridge *Bridge, response map[string]any) bool {
	callable, declared := callableClientToolSpecs(bridge.source), clientToolSpecs(bridge.source)
	items := response["output"].([]any)
	replaced := make([]any, 0, len(items))
	calls := 0
	for _, raw := range items {
		item := objectValue(raw)
		if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
			replaced = append(replaced, raw)
			continue
		}
		call, err := extractNativeClientToolCallIn(item, callable, declared)
		if err != nil {
			return false
		}
		if call["type"] == "function_call" {
			arguments, reason := parseRelayObject(call["arguments"])
			if reason != "" {
				return false
			}
			if schema := bridge.toolSchemas[clientToolCallName(call)]; schema != nil && schema.Validate(arguments) != nil {
				return false
			}
		}
		replaced = append(replaced, call)
		calls++
	}
	if calls == 0 || bridge.source["parallel_tool_calls"] == false && calls > 1 {
		return false
	}
	if alternateHistoryCollision(bridge.source, replaced) {
		return false
	}
	if bridge.structured != nil {
		candidate := cloneObject(response)
		candidate["output"] = replaced
		if !responseHasClientTool(candidate) && bridge.structured.validate(candidate) != nil {
			return false
		}
	}
	return true
}

func alternateHistoryCollision(source map[string]any, output []any) bool {
	usedCalls, usedItems := make(map[string]bool), make(map[string]bool)
	history, _ := source["input"].([]any)
	for _, raw := range history {
		item := objectValue(raw)
		if id, _ := item["call_id"].(string); id != "" {
			usedCalls[id] = true
		}
		if id, _ := item["id"].(string); id != "" {
			usedItems[id] = true
		}
	}
	for _, raw := range output {
		item := objectValue(raw)
		if item["type"] != "function_call" && item["type"] != "custom_tool_call" {
			continue
		}
		if usedCalls[stringValue(item["call_id"])] || usedItems[stringValue(item["id"])] {
			return true
		}
	}
	return false
}

func safeAlternateTerminal(raw []byte, allowClientTools bool) (map[string]any, bool) {
	response, ok := fallbackObject(raw)
	if !ok || response["status"] != "completed" || response["error"] != nil || response["refusal"] != nil || response["incomplete_details"] != nil {
		return nil, false
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) == 0 || len(output) > 4096 {
		return nil, false
	}
	ids, calls := make(map[string]bool), make(map[string]bool)
	for _, value := range output {
		item, ok := value.(map[string]any)
		if !ok || item["error"] != nil || item["refusal"] != nil {
			return nil, false
		}
		if id, present := item["id"]; present {
			text, valid := id.(string)
			if !valid || !validNativeRepairID(text) || ids[text] {
				return nil, false
			}
			ids[text] = true
		}
		if status, present := item["status"]; present && status != "completed" {
			return nil, false
		}
		switch item["type"] {
		case "function_call", "custom_tool_call":
			name, _ := item["name"].(string)
			if !allowClientTools && (item["type"] != "function_call" || !isTransportName(name) || item["namespace"] != nil && item["namespace"] != "") || name == "" || item["executed"] != nil || item["output"] != nil || item["result"] != nil {
				return nil, false
			}
			callID, valid := item["call_id"].(string)
			if !valid || !validNativeRepairID(callID) || calls[callID] {
				return nil, false
			}
			calls[callID] = true
			argumentKey := "arguments"
			if item["type"] == "custom_tool_call" {
				argumentKey = "input"
			}
			if _, valid := item[argumentKey].(string); !valid {
				return nil, false
			}
		case "message":
			if item["role"] != "assistant" || !safeFallbackParts(item["content"], "output_text") {
				return nil, false
			}
		case "reasoning":
			if summary, present := item["summary"]; present && !safeFallbackParts(summary, "summary_text") {
				return nil, false
			}
			if content, present := item["content"]; present && !safeFallbackParts(content, "reasoning_text") {
				return nil, false
			}
		default:
			return nil, false
		}
	}
	return response, true
}

func safeFallbackParts(value any, kind string) bool {
	parts, ok := value.([]any)
	if !ok {
		return false
	}
	for _, raw := range parts {
		part, ok := raw.(map[string]any)
		if !ok || part["type"] != kind || part["refusal"] != nil || part["error"] != nil {
			return false
		}
		if _, ok := part["text"].(string); !ok {
			return false
		}
	}
	return true
}

func fallbackObject(raw []byte) (map[string]any, bool) {
	if len(raw) == 0 || len(raw) > maxFallbackResponseBytes {
		return nil, false
	}
	var object map[string]any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if decoder.Decode(&object) != nil || object == nil || decoder.Decode(new(any)) != io.EOF {
		return nil, false
	}
	return object, true
}

// MergeResponseUsage exposes the cost of all complete responses involved in a
// bounded protocol fallback. Only known additive token counters and numeric
// detail leaves are summed, with exact signed-64-bit arithmetic. Missing values
// are unknown, never zero: incomplete aggregate counters are omitted.
// metadata.cpa_bps_usage_complete and metadata.cpa_bps_attempts are reserved
// string fields owned by this aggregation step. Unknown nonnumeric fields come
// only from the final response; earlier output, account data, and errors are not
// copied. With no earlier attempts the final response is returned unchanged.
func MergeResponseUsage(final []byte, earlier [][]byte) ([]byte, error) {
	if len(earlier) == 0 {
		return final, nil
	}
	if len(earlier) > 16 {
		return nil, ErrInvalidResponseUsage
	}
	response, ok := fallbackObject(final)
	if !ok {
		return nil, ErrInvalidResponseUsage
	}
	usage, hadUsage := response["usage"].(map[string]any)
	if raw, exists := response["usage"]; exists && raw != nil && !hadUsage {
		return nil, ErrInvalidResponseUsage
	}
	all := make([]map[string]any, 0, len(earlier)+1)
	for _, raw := range earlier {
		prior, ok := fallbackObject(raw)
		if !ok {
			return nil, ErrInvalidResponseUsage
		}
		item, isObject := prior["usage"].(map[string]any)
		if value, exists := prior["usage"]; exists && value != nil && !isObject {
			return nil, ErrInvalidResponseUsage
		}
		all = append(all, item)
	}
	all = append(all, usage)
	complete := true
	merged := make(map[string]any)
	for key, value := range usage {
		merged[key] = value
	}
	for _, key := range []string{"input_tokens", "output_tokens", "total_tokens"} {
		value, present, err := sumUsageLeaf(all, key)
		if err != nil {
			return nil, err
		}
		if present {
			merged[key] = value
		} else {
			delete(merged, key)
			complete = false
		}
	}
	for _, key := range []string{"input_tokens_details", "output_tokens_details"} {
		children := make([]map[string]any, len(all))
		anyPresent := false
		for i, item := range all {
			value, exists := item[key]
			if !exists || value == nil {
				continue
			}
			anyPresent = true
			child, ok := value.(map[string]any)
			if !ok {
				return nil, ErrInvalidResponseUsage
			}
			children[i] = child
		}
		if !anyPresent {
			continue
		}
		value, full, err := mergeUsageDetails(children, 0)
		if err != nil {
			return nil, err
		}
		complete = complete && full
		if len(value) > 0 {
			merged[key] = value
		} else {
			delete(merged, key)
		}
	}
	// Unknown top-level numeric counters have no established additive meaning.
	for _, item := range all {
		for key, value := range item {
			if key != "input_tokens" && key != "output_tokens" && key != "total_tokens" && key != "input_tokens_details" && key != "output_tokens_details" && containsUsageNumber(value) {
				delete(merged, key)
				complete = false
			}
		}
	}
	if hadUsage {
		response["usage"] = merged
	}
	metadata, valid := response["metadata"].(map[string]any)
	if value, exists := response["metadata"]; exists && value != nil && !valid {
		return nil, ErrInvalidResponseUsage
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	metadata["cpa_bps_usage_complete"] = strconv.FormatBool(complete)
	metadata["cpa_bps_attempts"] = strconv.Itoa(len(all))
	response["metadata"] = metadata
	encoded, err := json.Marshal(response)
	if err != nil {
		return nil, ErrInvalidResponseUsage
	}
	return encoded, nil
}

func containsUsageNumber(value any) bool {
	switch typed := value.(type) {
	case json.Number:
		return true
	case map[string]any:
		for _, child := range typed {
			if containsUsageNumber(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsUsageNumber(child) {
				return true
			}
		}
	}
	return false
}

func sumUsageLeaf(all []map[string]any, key string) (json.Number, bool, error) {
	var total int64
	complete := true
	for _, item := range all {
		value, exists := item[key]
		if !exists || value == nil {
			complete = false
			continue
		}
		number, ok := value.(json.Number)
		if !ok {
			return "", false, ErrInvalidResponseUsage
		}
		count, err := number.Int64()
		if err != nil || count < 0 || count > math.MaxInt64-total {
			return "", false, ErrInvalidResponseUsage
		}
		total += count
	}
	return json.Number(strconv.FormatInt(total, 10)), complete, nil
}

func mergeUsageDetails(all []map[string]any, depth int) (map[string]any, bool, error) {
	if depth > 8 {
		return nil, false, ErrInvalidResponseUsage
	}
	keys := make(map[string]bool)
	for _, item := range all {
		for key := range item {
			keys[key] = true
		}
	}
	result := make(map[string]any)
	complete := true
	for key := range keys {
		var numeric, nested bool
		for _, item := range all {
			switch item[key].(type) {
			case json.Number:
				numeric = true
			case map[string]any:
				nested = true
			}
		}
		if numeric {
			value, full, err := sumUsageLeaf(all, key)
			if err != nil {
				return nil, false, err
			}
			complete = complete && full
			if full {
				result[key] = value
			}
			continue
		}
		if nested {
			children := make([]map[string]any, len(all))
			for i, item := range all {
				if value, exists := item[key]; exists && value != nil {
					child, ok := value.(map[string]any)
					if !ok {
						return nil, false, ErrInvalidResponseUsage
					}
					children[i] = child
				}
			}
			value, full, err := mergeUsageDetails(children, depth+1)
			if err != nil {
				return nil, false, err
			}
			complete = complete && full
			if len(value) > 0 {
				result[key] = value
			}
			continue
		}
		if value, exists := all[len(all)-1][key]; exists {
			result[key] = value
		}
	}
	return result, complete, nil
}
