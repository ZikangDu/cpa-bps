package main

import (
	"encoding/json"
	"fmt"
	"testing"
)

type fixtureQuotaError struct{ scoped bool }

func (e fixtureQuotaError) Error() string {
	return `{"error":{"code":"bps_quota_exhausted","message":"Account quota exhausted.","reset_at":"2030-01-01T00:00:00Z","retry_after_seconds":604800}}`
}
func (e fixtureQuotaError) Code() string             { return "credential_quota" }
func (e fixtureQuotaError) StatusCode() int          { return 429 }
func (e fixtureQuotaError) RetryAfterSeconds() int   { return 604800 }
func (e fixtureQuotaError) IsCredentialScoped() bool { return e.scoped }

func TestErrorEnvelopePreservesCredentialQuotaContract(t *testing.T) {
	input := fixtureQuotaError{scoped: true}
	detail := rpcErrorFrom(fmt.Errorf("execution failed: %w", input))
	raw, err := json.Marshal(envelope{Error: detail})
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Error struct {
			Code             string `json:"code"`
			Status           int    `json:"http_status"`
			RetryAfter       int    `json:"retry_after_seconds"`
			CredentialScoped bool   `json:"credential_scoped"`
			Message          string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Error.Code != "credential_quota" || wire.Error.Status != 429 || wire.Error.RetryAfter != 604800 || !wire.Error.CredentialScoped {
		t.Fatalf("credential scope or reset duration lost in ABI: %s", raw)
	}
	if wire.Error.Message != "execution failed: "+input.Error() {
		t.Fatalf("safe error details were not preserved: %s", raw)
	}
}

func TestOrdinaryErrorEnvelopeOmitsCredentialScope(t *testing.T) {
	detail := rpcErrorFrom(fixtureQuotaError{})
	raw, err := json.Marshal(detail)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if _, present := fields["credential_scoped"]; present {
		t.Fatalf("false credential scope should be omitted: %s", raw)
	}
}
