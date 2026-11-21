package adapter

import (
	"encoding/json"
	"testing"
)

func TestQuotaErrorSeparatesHostRoutingCodeAndPublicRecovery(t *testing.T) {
	err := &rpcError{ErrorCode: "credential_quota", PublicCode: "bps_quota_exhausted", Message: "Account quota exhausted.", HTTPStatus: 429,
		RetryAfter: 604800, CredentialScoped: true, ResetAt: "2030-01-01T00:00:00Z"}
	var body struct {
		Error struct {
			Code       string `json:"code"`
			Type       string `json:"type"`
			ResetAt    string `json:"reset_at"`
			RetryAfter int    `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(err.Error()), &body) != nil || body.Error.Code != "bps_quota_exhausted" || body.Error.Type != "rate_limit_error" ||
		body.Error.ResetAt != err.ResetAt || body.Error.RetryAfter != 604800 || err.Code() != "credential_quota" || !err.IsCredentialScoped() {
		t.Fatalf("quota error lost routing/public recovery contract: %s", err.Error())
	}
	if !(&rpcError{ErrorCode: "credential_quota"}).IsCredentialScoped() {
		t.Fatal("host-compatible credential_quota code must identify account scope")
	}
	if failure(429, "bps_rate_limited", "Temporary rate limit.").IsCredentialScoped() {
		t.Fatal("unconfirmed rate limit must not acquire account scope")
	}
}
