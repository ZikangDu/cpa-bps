package adapter

import "testing"

func TestResolveTokenScopesGatewayOrigin(t *testing.T) {
	t.Setenv("CPA_BPS_TEST_TOKEN", "fixture")
	credential := []byte(`{"type":"cpa-bps","token_env":"CPA_BPS_TEST_TOKEN","upstream_origin":"https://gateway.example"}`)
	for _, endpoint := range []string{
		"https://gateway.example/v1/responses",
		"https://GATEWAY.example:443/v1/models",
	} {
		token, err := resolveToken(credential, endpoint, nil)
		if err != nil || token != "fixture" {
			t.Fatalf("scoped gateway token unavailable for %s", endpoint)
		}
	}
	for _, endpoint := range []string{
		"https://other.example/v1/responses",
		"https://gateway.example:8443/v1/responses",
		"http://gateway.example/v1/responses",
	} {
		token, err := resolveToken(credential, endpoint, nil)
		if token != "" || err == nil || err.Code() != "bps_credential_origin_mismatch" {
			t.Fatalf("credential was not denied for %s", endpoint)
		}
	}
}

func TestCredentialPinsOriginWithoutPath(t *testing.T) {
	raw := []byte(`{"type":"cpa-bps","token_env":"CPA_BPS_TEST_TOKEN","upstream_origin":"https://gateway.example/v1/responses"}`)
	if _, ok := credentialFromStorage(raw); ok {
		t.Fatal("an endpoint path is not an origin")
	}
	raw = []byte(`{"type":"cpa-bps","token_env":"CPA_BPS_TEST_TOKEN"}`)
	if auth, ok := credentialFromStorage(raw); !ok || !auth.allows("https://gateway.example/v1/responses") {
		t.Fatal("existing env-backed credentials must still work")
	}
}
