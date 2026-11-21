// Adapted from JaxsonWang/cpa-plugin-oai-basispoints v0.2.10.
// Copyright (c) 2026 JaxsonWang. See LICENSE in this directory.
package bpsprotocol

import "strings"

// 函数载荷和外层 arguments 是两层不同的 JSON；custom 只有外层需要编码。
const functionRelayEncoding = `For a function tool, JSON-serialize {"name":"CATALOG_NAME","arguments":{...}} into the outer code string, then JSON-serialize the outer run_officejs arguments. Preserve quotes, backslashes, newlines and tabs at both layers. The arguments member is an object with the fields required by the declared schema. For catalog FUNCTION_CODE tools, use summary codex2api.function_code/CATALOG_NAME with the exact code text in native code and all other arguments JSON in extended_summary. For catalog FUNCTION_CMD tools, use summary codex2api.function_cmd/CATALOG_NAME with the exact cmd text in native code and all other arguments JSON in extended_summary. These raw transports must not re-encode the code or cmd as another JSON string. Custom tools use the exact codex2api.custom/CATALOG_NAME summary marker and raw input in code.`

// 示例只使用当前允许的已知工具和匹配的参数结构，不把固定工具名或类型写进所有请求。
func clientToolRelayExamples(names []string, specs map[string]toolSpec) string {
	const patch = "*** Begin Patch\n*** Add File: hello.js\n+console.log(\"hello\");\n*** End Patch"
	var examples strings.Builder
	for _, name := range names {
		spec := specs[name]
		var arguments map[string]any
		var payload string
		switch {
		case spec.Name == "exec_command" || strings.HasSuffix(spec.Name, "_exec_command"):
			if spec.Type != "function" {
				continue
			}
			arguments = map[string]any{"cmd": "printf '%s\\n' \"hello\""}
		case spec.Name == "apply_patch" || strings.HasSuffix(spec.Name, "_apply_patch"):
			if spec.Type == "custom" {
				payload = patch
			} else {
				arguments = map[string]any{"patch": patch}
			}
		default:
			continue
		}
		if spec.Type == "function" {
			schema := firstMap(spec.Spec, "parameters", "inputSchema", "input_schema")
			if schema == nil || !schemaMatches(arguments, schema) {
				continue
			}
			payload = string(jsonBytes(map[string]any{"name": name, "arguments": arguments}))
		}
		outer := map[string]any{
			"summary": "Run client tool " + name, "extended_summary": "Relay one client tool through the external client",
			"destructive": false, "references": []any{}, "code": payload,
		}
		if spec.Type == "custom" {
			outer["summary"] = customRelayPrefix + name
		} else if field := functionRelayField(spec); field != "" {
			outer = rawFunctionRelayArguments(name, field, arguments)
		}
		examples.WriteString(" Example outer arguments for " + name + " (" + spec.Type + "): " + string(jsonBytes(outer)) + ".")
	}
	return examples.String()
}
