package adapter

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
)

func policyRuntime() *Runtime { return &Runtime{transports: newTransportPolicies()} }
func policySelect(t *testing.T, r *Runtime, raw string) (string, [32]byte, bool) {
	t.Helper()
	mode, key, buffer, err := r.selectAutoTransport(executionRequest{AuthID: "account-A", Model: "fixture", StorageJSON: []byte(`{"fixture":"credential"}`)}, config{ResponsesURL: "https://bps.openai.com/basispoints/api/responses"}, []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return mode, key, buffer
}

func TestAutoPolicyUsesRequestStructureNotPromptKeywords(t *testing.T) {
	cases := []struct {
		name, source, want string
		buffer             bool
	}{
		{"plain", `{"model":"fixture","input":"native Excel code tools required"}`, "text", false},
		{"image", `{"model":"fixture","input":[{"role":"user","content":[{"type":"input_image","file_id":"file_x"}]}]}`, "text", false},
		{"structured", `{"input":"q","text":{"format":{"type":"json_schema"}}}`, "text", false},
		{"one function", `{"tools":[{"type":"function","name":"sum"}]}`, "native", true},
		{"code function", `{"tools":[{"type":"function","name":"python","parameters":{"properties":{"code":{"type":"string"}}}}]}`, "native", true},
		{"command", `{"tools":[{"type":"function","name":"exec_command","parameters":{"properties":{"cmd":{"type":"string"}}}}]}`, "native", true},
		{"unrelated cmd", `{"tools":[{"type":"function","name":"record","parameters":{"properties":{"cmd":{"type":"string"}}}}]}`, "native", true},
		{"custom", `{"tools":[{"type":"custom","name":"patch"}]}`, "native", true},
		{"nested namespace", `{"tools":[{"type":"namespace","name":"excel","tools":[{"type":"namespace","name":"sheet","tools":[{"type":"function","name":"read"}]}]}]}`, "native", true},
		{"two functions", `{"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}]}`, "native", true},
		{"no parallel", `{"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"parallel_tool_calls":false}`, "native", true},
		{"one explicit parallel", `{"tools":[{"type":"function","name":"a"}],"parallel_tool_calls":true}`, "native", true},
		{"one chosen function", `{"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"tool_choice":{"type":"function","name":"a"}}`, "native", true},
		{"allowed one", `{"tools":[{"type":"function","name":"a"},{"type":"function","name":"b"}],"tool_choice":{"type":"allowed_tools","tools":[{"type":"function","name":"a"}]}}`, "native", true},
		{"none", `{"tools":[{"type":"custom","name":"patch"}],"tool_choice":"none"}`, "text", true},
		{"result history", `{"input":[{"type":"function_call_output","call_id":"x","output":"5"}]}`, "text", true},
		{"dynamic tools", `{"input":[{"type":"additional_tools","tools":[{"type":"custom","name":"patch"}]}]}`, "native", true},
		{"dynamic overrides kind", `{"tools":[{"type":"custom","name":"patch"}],"input":[{"type":"additional_tools","tools":[{"type":"function","name":"patch"}]}]}`, "native", true},
		{"hosted not client", `{"tools":[{"type":"web_search"}]}`, "text", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, _, buffer := policySelect(t, policyRuntime(), tc.source)
			if mode != tc.want || buffer != tc.buffer {
				t.Fatalf("mode=%s buffer=%v", mode, buffer)
			}
		})
	}
}

func TestAutoPolicyLearnsWithinCapabilityClassAndReevaluatesNewRequests(t *testing.T) {
	r := policyRuntime()
	simple := `{"model":"fixture","prompt_cache_key":"session","tools":[{"type":"function","name":"sum"}]}`
	_, simpleKey, _ := policySelect(t, r, simple)
	r.transports.learn(simpleKey, "text")
	cases := []struct{ name, source string }{
		{"code", `{"model":"fixture","prompt_cache_key":"session","tools":[{"type":"function","name":"python","parameters":{"properties":{"code":{"type":"string"}}}}]}`},
		{"custom", `{"model":"fixture","prompt_cache_key":"session","tools":[{"type":"custom","name":"patch"}]}`},
		{"namespace", `{"model":"fixture","prompt_cache_key":"session","tools":[{"type":"namespace","name":"excel","tools":[{"type":"function","name":"read"}]}]}`},
		{"multiple", `{"model":"fixture","prompt_cache_key":"session","tools":[{"type":"function","name":"sum"},{"type":"function","name":"lookup"}]}`},
	}
	keys := map[[32]byte]bool{simpleKey: true}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mode, key, _ := policySelect(t, r, tc.source)
			if mode != "native" || keys[key] {
				t.Fatal("earlier simple text binding masked new request capability")
			}
			keys[key] = true
			r.transports.learn(key, "text")
			if learned, again, _ := policySelect(t, r, tc.source); learned != "text" || key != again {
				t.Fatal("same-family successful fallback was not retained")
			}
			var body map[string]any
			_ = json.Unmarshal([]byte(tc.source), &body)
			body["tool_choice"] = "none"
			followup, _ := json.Marshal(body)
			if learned, again, _ := policySelect(t, r, string(followup)); learned != "text" || key == again {
				t.Fatal("no-tool turn reused a callable-tool transport binding")
			}
		})
	}
	if mode, _, buffer := policySelect(t, r, `{"model":"fixture","prompt_cache_key":"session","input":"plain"}`); mode != "text" || buffer {
		t.Fatal("plain request unnecessarily buffered")
	}
}

func TestAutoPolicyInfersResultFamilyAfterToolsAreRemoved(t *testing.T) {
	cases := []struct{ name, tools, calls string }{
		{"function", `[{"type":"function","name":"sum"}]`, `[{"type":"function_call","name":"sum","call_id":"c","arguments":"{\"a\":2}"}]`},
		{"command", `[{"type":"function","name":"exec_command","parameters":{"properties":{"cmd":{"type":"string"}}}}]`, `[{"type":"function_call","name":"exec_command","call_id":"c","arguments":"{\"cmd\":\"fixture\"}"}]`},
		{"code", `[{"type":"function","name":"python","parameters":{"properties":{"code":{"type":"string"}}}}]`, `[{"type":"function_call","name":"python","call_id":"c","arguments":"{\"code\":\"fixture\"}"}]`},
		{"custom", `[{"type":"custom","name":"patch"}]`, `[{"type":"custom_tool_call","name":"patch","call_id":"c","input":"fixture"}]`},
		{"namespace", `[{"type":"namespace","name":"excel","tools":[{"type":"function","name":"read"}]}]`, `[{"type":"function_call","name":"read","namespace":"excel","call_id":"c","arguments":"{}"}]`},
		{"multi", `[{"type":"function","name":"a"},{"type":"function","name":"b"}]`, `[{"type":"function_call","name":"a","call_id":"c","arguments":"{}"},{"type":"function_call","name":"b","call_id":"d","arguments":"{}"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := policyRuntime()
			source := `{"model":"fixture","prompt_cache_key":"session","tools":` + tc.tools + `}`
			_, key, _ := policySelect(t, r, source)
			r.transports.learn(key, "text")
			history := []any{map[string]any{"type": "custom_tool_call", "name": "old", "call_id": "old", "input": "old"}, map[string]any{"role": "user", "content": "next request"}}
			var calls []any
			_ = json.Unmarshal([]byte(tc.calls), &calls)
			history = append(history, calls...)
			history = append(history, map[string]any{"type": "function_call_output", "call_id": "c", "output": "5"})
			followup, _ := json.Marshal(map[string]any{"model": "fixture", "prompt_cache_key": "session", "tool_choice": "none", "input": history})
			mode, resultKey, buffer := policySelect(t, r, string(followup))
			if !buffer || mode != "text" || resultKey == key {
				t.Fatalf("history failed to isolate a result-only turn: %s", tc.name)
			}
			var currentTools []any
			_ = json.Unmarshal([]byte(tc.tools), &currentTools)
			withCatalog, _ := json.Marshal(map[string]any{"model": "fixture", "prompt_cache_key": "session", "tool_choice": "none", "tools": currentTools, "input": history})
			if catalogMode, catalogKey, _ := policySelect(t, r, string(withCatalog)); catalogMode != "text" || catalogKey != resultKey {
				t.Fatalf("removing the catalog changed result-only capability: %s", tc.name)
			}
		})
	}
}

func policyCredential(account, signature string) []byte {
	claims := fmt.Sprintf(`{"https://api.openai.com/auth":{"chatgpt_account_id":%q}}`, account)
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + "." + signature
	raw, _ := json.Marshal(credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Grant: &managedGrant{ClientID: oauthflow.BPSClientID, TokenURL: oauthflow.BPSConfig().TokenURL, Tokens: oauthflow.Tokens{AccessToken: token, ExpiresAt: time.Date(2035, 1, 1, 0, 0, 0, 0, time.UTC)}}})
	return raw
}

func TestAutoTransportIdentityStableHistoryAndScopedAccounts(t *testing.T) {
	r := policyRuntime()
	cfg := config{ResponsesURL: "https://bps.openai.com/basispoints/api/responses", BPSToolsVersionID: "v1"}
	req := executionRequest{AuthID: "auth", Model: "fixture", StorageJSON: policyCredential("account-1", "sig-1"), Metadata: map[string]any{"caller_scope": "caller-1"}}
	keyFor := func(q executionRequest, c config, raw string) [32]byte {
		t.Helper()
		_, key, _, err := r.selectAutoTransport(q, c, []byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		return key
	}
	first := `{"model":"fixture","input":"hello"}`
	key := keyFor(req, cfg, first)
	for _, history := range []string{`{"model":"fixture","input":[{"role":"user","content":"hello"},{"role":"assistant","content":"reply"},{"role":"user","content":"follow up"}]}`, `{"model":"fixture","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`} {
		if keyFor(req, cfg, history) != key {
			t.Fatal("history growth changed conversation")
		}
	}
	rotated := req
	rotated.StorageJSON = policyCredential("account-1", "new-signature")
	if keyFor(rotated, cfg, first) != key {
		t.Fatal("same-account token rotation discarded binding")
	}
	replaced := req
	replaced.StorageJSON = policyCredential("account-2", "new-signature")
	if keyFor(replaced, cfg, first) == key {
		t.Fatal("replacement account inherited binding")
	}
	for _, changed := range []executionRequest{
		{AuthID: "different", Model: req.Model, StorageJSON: req.StorageJSON, Metadata: req.Metadata},
		{AuthID: req.AuthID, Model: req.Model, StorageJSON: req.StorageJSON, Metadata: map[string]any{"caller_scope": "different"}},
	} {
		if keyFor(changed, cfg, first) == key {
			t.Fatal("account/caller isolation lost")
		}
	}
	changedCfg := cfg
	changedCfg.BPSToolsVersionID = "v2"
	if keyFor(req, changedCfg, first) == key {
		t.Fatal("catalog version mixed")
	}
	changedCfg = cfg
	changedCfg.ResponsesURL = "https://another.invalid/responses"
	if keyFor(req, changedCfg, first) == key {
		t.Fatal("origin mixed")
	}
	if keyFor(req, cfg, `{"model":"different","input":"hello"}`) == key {
		t.Fatal("model mixed")
	}
	if keyFor(req, cfg, `{"model":"fixture","input":"different"}`) == key {
		t.Fatal("unrelated implicit conversations mixed")
	}
	req.Metadata = map[string]any{"canonical_session_id": "header:stable", "caller_scope": "caller-1"}
	a := keyFor(req, cfg, `{"model":"fixture","prompt_cache_key":"changing-1","input":"first"}`)
	b := keyFor(req, cfg, `{"model":"fixture","prompt_cache_key":"changing-2","input":"next"}`)
	if a != b {
		t.Fatal("canonical header did not override body hint")
	}
}

func TestAutoTransportCacheExpiryCapacityAndConcurrentUse(t *testing.T) {
	p := newTransportPolicies()
	now := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	p.now = func() time.Time { return now }
	key := [32]byte{1}
	p.learn(key, "native")
	now = now.Add(transportPolicyTTL - time.Second)
	if _, ok := p.read(key); !ok {
		t.Fatal("binding expired early")
	}
	now = now.Add(time.Second)
	if _, ok := p.read(key); ok {
		t.Fatal("expired binding retained")
	}
	for i := 0; i < maxTransportPolicies+7; i++ {
		p.learn(sha256.Sum256([]byte(fmt.Sprint(i))), "text")
	}
	if len(p.entries) != maxTransportPolicies {
		t.Fatal("cache exceeded bounded capacity")
	}
	p.learn(key, "invalid")
	if _, ok := p.read(key); ok {
		t.Fatal("invalid mode learned")
	}
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			key := sha256.Sum256([]byte(fmt.Sprint(i)))
			for n := 0; n < 20; n++ {
				p.learn(key, "native")
				if mode, ok := p.read(key); ok && mode != "native" {
					t.Error("invalid concurrent mode")
				}
			}
		}(i)
	}
	wg.Wait()
}

func TestAutoTransportRejectsMalformedRequest(t *testing.T) {
	r := policyRuntime()
	for _, raw := range []string{`null`, `[]`, `{} {}`, `{"tools":`} {
		if _, _, _, err := r.selectAutoTransport(executionRequest{}, config{}, []byte(raw)); err == nil {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestAutoPolicyResultUsesRecentCallInsteadOfUnrelatedCatalogFamily(t *testing.T) {
	r := policyRuntime()
	tools := `[{"type":"function","name":"sum"},{"type":"custom","name":"patch"}]`
	initial := `{"model":"fixture","prompt_cache_key":"session","tools":` + tools + `,"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"sum"}]}}`
	_, key, _ := policySelect(t, r, initial)
	r.transports.learn(key, "text")
	followup := `{"model":"fixture","prompt_cache_key":"session","tools":` + tools + `,"tool_choice":"none","input":[{"role":"user","content":"task"},{"type":"function_call","name":"sum","call_id":"c","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"5"}]}`
	mode, resultKey, _ := policySelect(t, r, followup)
	if mode != "text" || resultKey == key {
		t.Fatal("result-round reused a callable-tool binding")
	}
	var withoutCatalog map[string]any
	_ = json.Unmarshal([]byte(followup), &withoutCatalog)
	delete(withoutCatalog, "tools")
	raw, _ := json.Marshal(withoutCatalog)
	if _, withoutCatalogKey, _ := policySelect(t, r, string(raw)); withoutCatalogKey != resultKey {
		t.Fatal("unused custom declaration changed the result-round family")
	}
}

func TestCallableToolTransportStartsNativeAcrossModelsAndChoices(t *testing.T) {
	models := []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra", "gpt-6-luna", "gpt-6-sol", "gpt-6-astra", "gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra", "future-model"}
	choices := []any{
		nil, "auto", "required",
		map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}},
		map[string]any{"type": "function", "name": "lookup"},
		map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}},
	}
	for _, model := range models {
		for _, choice := range choices {
			body := map[string]any{"model": model, "input": "fixture", "prompt_cache_key": "required-audit", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}, "tool_choice": choice, "parallel_tool_calls": false}
			raw, _ := json.Marshal(body)
			mode, _, buffered := policySelect(t, policyRuntime(), string(raw))
			if mode != "native" || !buffered {
				t.Fatalf("callable tool did not use native transport: model=%s choice=%v mode=%s", model, choice, mode)
			}
		}
	}
}

func TestRequiredAndOptionalTransportBindingsStaySeparate(t *testing.T) {
	r := policyRuntime()
	body := map[string]any{"model": "fixture", "input": "fixture", "prompt_cache_key": "policy-transition", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}}
	selectChoice := func(choice any) (string, [32]byte) {
		t.Helper()
		body["tool_choice"] = choice
		raw, _ := json.Marshal(body)
		mode, key, buffered := policySelect(t, r, string(raw))
		if !buffered {
			t.Fatal("tool request lost buffering")
		}
		return mode, key
	}
	mode, optionalKey := selectChoice("auto")
	if mode != "native" {
		t.Fatal("optional simple function did not start on native transport")
	}
	r.transports.learn(optionalKey, "text")
	mode, requiredKey := selectChoice("required")
	if mode != "native" || requiredKey == optionalKey {
		t.Fatal("optional text success masked the required-call native capability")
	}
	for _, choice := range []any{map[string]any{"type": "function", "name": "lookup"}, map[string]any{"type": "allowed_tools", "mode": "required", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}}} {
		if mode, key := selectChoice(choice); mode != "native" || key != requiredKey {
			t.Fatal("equivalent required policies lost their capability identity")
		}
	}
	// Successful, fully validated fallback can still be learned independently
	// for required calls; this does not relax the replay safety gate.
	r.transports.learn(requiredKey, "text")
	if mode, key := selectChoice("required"); mode != "text" || key != requiredKey {
		t.Fatal("validated required-call fallback was not retained")
	}
	r.transports.learn(requiredKey, "native")
	for _, choice := range []any{nil, "auto", map[string]any{"type": "allowed_tools", "mode": "auto", "tools": []any{map[string]any{"type": "function", "name": "lookup"}}}} {
		if mode, key := selectChoice(choice); mode != "text" || key != optionalKey {
			t.Fatal("required-call binding changed optional/no-tool behavior")
		}
	}
	if mode, key := selectChoice("none"); mode != "text" || key == optionalKey || key == requiredKey {
		t.Fatal("tool_choice none did not retain its independent text default")
	}
}
