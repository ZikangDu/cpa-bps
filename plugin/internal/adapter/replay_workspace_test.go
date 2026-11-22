package adapter

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/oauthflow"
)

func TestNativeReplayFollowsWorkspaceAcrossInPlaceCredentialReplacement(t *testing.T) {

	req := executionRequest{AuthID: "same-imported-auth-id", StorageJSON: replayWorkspaceCredential("workspace-A", "opaque-old"), Metadata: map[string]any{"caller_scope": "caller"}}
	source := []byte(`{"model":"fixture","input":"read fixture","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	_, bridge, err := bpsprotocol.Prepare(source, bpsCallerScope(req))
	if err != nil {
		t.Fatal(err)
	}
	native := []byte(`{"status":"completed","output":[{"type":"function_call","name":"run_officejs","id":"fc_workspace_fixture","call_id":"call_workspace_fixture","arguments":"{\"code\":\"{\\\"name\\\":\\\"lookup\\\",\\\"arguments\\\":{}}\",\"references\":[]}","encrypted_function_args":["WORKSPACE_A_OPAQUE"]}]}`)
	transformed, err := bridge.Transform(native)
	if err != nil {
		t.Fatal(err)
	}
	var response struct{ Output []any }
	if json.Unmarshal(transformed, &response) != nil {
		t.Fatal("invalid transformed fixture")
	}
	history, _ := json.Marshal(map[string]any{"model": "fixture", "input": append(response.Output, map[string]any{"type": "function_call_output", "call_id": "call_workspace_fixture", "output": "done"}), "tools": []any{map[string]any{"type": "function", "name": "lookup"}}})
	prepare := func(request executionRequest) []byte {
		raw, _, err := bpsprotocol.Prepare(history, bpsCallerScope(request))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	if !bytes.Contains(prepare(req), []byte("WORKSPACE_A_OPAQUE")) {
		t.Fatal("same workspace lost native replay")
	}
	refreshed := req
	refreshed.StorageJSON = replayWorkspaceCredential("workspace-A", "opaque-refreshed")
	if bpsCallerScope(req) != bpsCallerScope(refreshed) || !bytes.Contains(prepare(refreshed), []byte("WORKSPACE_A_OPAQUE")) {
		t.Fatal("same workspace token refresh lost continuity")
	}
	replaced := req
	replaced.StorageJSON = replayWorkspaceCredential("workspace-B", "opaque-new-account")
	clean := prepare(replaced)
	if bytes.Contains(clean, []byte("WORKSPACE_A_OPAQUE")) || !bytes.Contains(clean, []byte("lookup")) || !bytes.Contains(clean, []byte("call_workspace_fixture")) {
		t.Fatal("workspace replacement reused opaque state or lost inline history")
	}
	other := req
	other.Metadata = map[string]any{"caller_scope": "another-caller"}
	if bytes.Contains(prepare(other), []byte("WORKSPACE_A_OPAQUE")) {
		t.Fatal("native replay crossed caller partition")
	}
}

func replayWorkspaceCredential(account, token string) []byte {
	claims, _ := json.Marshal(map[string]any{"https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account}})
	idToken := "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	raw, _ := json.Marshal(credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Grant: &managedGrant{ClientID: "fixture-client", TokenURL: "https://auth.openai.com/oauth/token", Tokens: oauthflow.Tokens{AccessToken: token, IDToken: idToken, ExpiresAt: time.Now().Add(time.Hour)}}})
	return raw
}
