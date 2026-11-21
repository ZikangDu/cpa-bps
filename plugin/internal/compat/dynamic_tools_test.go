package compat

import (
	"encoding/json"
	"testing"
)

func TestDynamicToolCatalogAndNestedNamespaces(t *testing.T) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"tools":[{"type":"function","name":"legacy"}],"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"excel","tools":[{"type":"namespace","name":"sheet","tools":[{"type":"function","name":"read"}]}]}]}]}`), &body)
	catalog, err := DeclaredTools(body)
	if err != nil {
		t.Fatal(err)
	}
	good := json.RawMessage(`{"output":[{"type":"function_call","name":"read","namespace":"excel.sheet","call_id":"call_one","arguments":"{}"}]}`)
	if err := catalog.ValidateResponse(good); err != nil {
		t.Fatal("dynamic namespace was dropped", err)
	}
	bad := json.RawMessage(`{"output":[{"type":"function_call","name":"delete","namespace":"excel.sheet","arguments":"{}"}]}`)
	if catalog.ValidateResponse(bad) == nil {
		t.Fatal("undeclared tool escaped dynamic catalog")
	}
	body["tool_choice"] = json.RawMessage(`"none"`)
	catalog, err = DeclaredTools(body)
	if err != nil || catalog.ValidateResponse(good) == nil {
		t.Fatal("none did not disable dynamic tools")
	}
}

func TestDynamicToolCatalogUpdateIsOrderedAndBounded(t *testing.T) {
	var body map[string]json.RawMessage
	_ = json.Unmarshal([]byte(`{"tools":[{"type":"function","name":"edit"}],"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"edit"}]}]}`), &body)
	catalog, err := DeclaredTools(body)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.ValidateResponse(json.RawMessage(`{"output":[{"type":"custom_tool_call","name":"edit","input":"raw"}]}`)) != nil {
		t.Fatal("new tool kind not used")
	}
	if catalog.ValidateResponse(json.RawMessage(`{"output":[{"type":"function_call","name":"edit","arguments":"{}"}]}`)) == nil {
		t.Fatal("old kind retained after catalog update")
	}
	body["input"] = json.RawMessage(`[{"type":"additional_tools","tools":[{"type":"function","name":"same"},{"type":"custom","name":"same"}]}]`)
	if _, err := DeclaredTools(body); err == nil {
		t.Fatal("ambiguous update accepted")
	}
}

func TestAgentCipherEnvelopeAndMalformedNestedContent(t *testing.T) {
	for _, input := range []string{`[{"type":"agent_message","encrypted_content":"opaque","author":"worker"}]`, `[{"type":"agent_message","text":"visible","author":"worker"}]`} {
		body := map[string]json.RawMessage{"input": json.RawMessage(input)}
		if err := PrepareHistory(body, false); err != nil {
			t.Fatal("valid top-level agent envelope rejected", err)
		}
	}
	for _, kind := range []string{"agent_message", "encrypted_content"} {
		part, _ := json.Marshal(map[string]string{"type": kind, "encrypted_content": "opaque"})
		body := map[string]json.RawMessage{"input": json.RawMessage(`[{"type":"message","role":"assistant","content":[` + string(part) + `]}]`)}
		if PrepareHistory(body, false) == nil {
			t.Fatal("malformed nested cipher was forwarded")
		}
	}
}
