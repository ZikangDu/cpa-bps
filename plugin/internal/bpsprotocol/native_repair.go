package bpsprotocol

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode"
)

const maxNativeRepairAddition = 1 << 20

const nativeRepairInstruction = "The previous native run_officejs calls were rejected as invalid client-tool transports and were not executed. Correct only the transport format using the existing declared client tool catalog and current-turn tool policy. FUNCTION uses one name/arguments JSON envelope; eligible FUNCTION_CODE and FUNCTION_CMD use the exact summary marker, raw code or cmd, and remaining arguments JSON in extended_summary; CUSTOM uses its exact marker and raw input. " + nativeReferencesInstruction + " Do not execute Office JavaScript or call undeclared tools. Do not repeat any client tool that previously completed. If no tool is permitted, return assistant text."

const nativeRepairFailure = "The proxy did not execute this call because it was not a valid declared client-tool transport. Follow the existing catalog transport and tool-choice rules."

// RepairNativePayload prepares a bounded model correction, never a tool retry.
// The adapter must keep all output buffered, retain the same selected account,
// and enforce its maximum correction count before sending the returned payload.
func RepairNativePayload(prepared json.RawMessage, response json.RawMessage, cause error) (json.RawMessage, bool) {
	var problem *Error
	if !errors.As(cause, &problem) || problem.Code() != "invalid_tool_call" || problem.StatusCode() != 422 || len(response) > maxNativeRepairAddition {
		return nil, false
	}
	body, why := parseRelayObject(string(prepared))
	if why != "" {
		return nil, false
	}
	terminal, safe := safeAlternateTerminal(response, false)
	if !safe {
		return nil, false
	}
	input, ok := body["input"].([]any)
	if !ok {
		return nil, false
	}
	output, ok := terminal["output"].([]any)
	if !ok || len(output) == 0 || len(output) > 128 {
		return nil, false
	}
	usedCalls, usedIDs := map[string]bool{}, map[string]bool{}
	for _, raw := range input {
		item := objectValue(raw)
		if id, _ := item["call_id"].(string); id != "" {
			usedCalls[id] = true
		}
		if id, _ := item["id"].(string); id != "" {
			usedIDs[id] = true
		}
	}
	calls := make([]any, 0, len(output))
	results := make([]any, 0, len(output))
	for _, raw := range output {
		item := objectValue(raw)
		if item == nil {
			return nil, false
		}
		switch stringValue(item["type"]) {
		case "message", "reasoning":
			// They are withheld output, not evidence of any executed tool.
			continue
		case "function_call":
			if !isTransportName(stringValue(item["name"])) {
				return nil, false
			}
		default:
			// A hosted or direct client tool may have different execution
			// semantics. Never manufacture a non-execution result for it.
			return nil, false
		}
		callID, ok := item["call_id"].(string)
		if !ok || !validNativeRepairID(callID) || usedCalls[callID] {
			return nil, false
		}
		id, ok := item["id"].(string)
		if !ok || !validNativeRepairID(id) || usedIDs[id] {
			return nil, false
		}
		arguments, ok := item["arguments"].(string)
		if !ok || strings.TrimSpace(arguments) == "" {
			return nil, false
		}
		usedCalls[callID], usedIDs[id] = true, true
		// Replaying the known native call is required to correlate the
		// synthetic failure with call_id. Nothing in this function runs code.
		calls = append(calls, cloneObject(item))
		results = append(results, map[string]any{"type": "function_call_output", "call_id": callID,
			"output": string(jsonBytes(map[string]any{"executed": false, "error": map[string]string{"code": "invalid_client_tool_transport", "message": nativeRepairFailure}}))})
	}
	if len(calls) == 0 {
		return nil, false
	}
	addition := append(calls, results...)
	addition = append(addition, messageItem("developer", nativeRepairInstruction))
	if len(jsonBytes(addition)) > maxNativeRepairAddition {
		return nil, false
	}
	body["input"] = appendBeforeCompaction(input, addition)
	metadata := objectValue(body["metadata"])
	if metadata != nil {
		iteration, err := strconv.Atoi(stringValue(metadata["agent_iteration"]))
		if err != nil || iteration < 1 || iteration >= 1_000_000 {
			return nil, false
		}
		metadata["agent_iteration"] = strconv.Itoa(iteration + 1)
	}
	next, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	return next, true
}

func validNativeRepairID(value string) bool {
	return value != "" && len(value) <= 256 && !strings.ContainsFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) })
}
