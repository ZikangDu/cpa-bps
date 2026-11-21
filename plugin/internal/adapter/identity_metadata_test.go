package adapter

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestStoredBPSOAuthPopulatesEmailWithoutReplacingLabelOrToken(t *testing.T) {
	_, auth, _ := quotaFixture(t)
	claims, _ := json.Marshal(map[string]string{"email": "display@example.com"})
	auth.Label = "BPS OAuth"
	auth.Grant.IDToken = "e30." + base64.RawURLEncoding.EncodeToString(claims) + ".fixture"
	data, err := auth.data("fixture", "fixture.json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if data["Label"] != "BPS OAuth" || data["Metadata"].(map[string]any)["email"] != "display@example.com" || data["Attributes"].(map[string]string)["email"] != "display@example.com" {
		t.Fatal("display metadata mismatch")
	}
	var stored credential
	_ = json.Unmarshal(data["StorageJSON"].([]byte), &stored)
	if stored.Grant.AccessToken != auth.Grant.AccessToken || stored.Grant.IDToken != auth.Grant.IDToken {
		t.Fatal("display extraction changed OAuth grant")
	}
	// Existing credentials receive the same derived metadata when loaded.
	raw, _ := json.Marshal(map[string]any{"Provider": provider, "FileName": "fixture.json", "RawJSON": data["StorageJSON"]})
	parsed, err := parseAuth(raw)
	if err != nil {
		t.Fatal(err)
	}
	metadata := parsed.(map[string]any)["Auth"].(map[string]any)["Metadata"].(map[string]any)
	serialized, _ := json.Marshal(metadata)
	if metadata["email"] != "display@example.com" || strings.Contains(string(serialized), auth.Grant.AccessToken) {
		t.Fatal("parse metadata omitted email or leaked token")
	}
}

func TestMigratedCredentialKeepsOriginalAuthIDAndDisplayEmail(t *testing.T) {
	_, auth, _ := quotaFixture(t)
	auth.AuthID = "original-stable-auth-id"
	auth.Email = "existing@example.com"
	auth.Label = "BPS OAuth"
	raw, _ := json.Marshal(auth)
	req, _ := json.Marshal(map[string]any{"Provider": provider, "FileName": "original-file.json", "RawJSON": raw})
	value, err := parseAuth(req)
	if err != nil {
		t.Fatal(err)
	}
	got := value.(map[string]any)["Auth"].(map[string]any)
	if got["ID"] != auth.AuthID || got["Metadata"].(map[string]any)["email"] != auth.Email {
		t.Fatal("migration lost account identity or email")
	}
	rotated := auth
	rotated.Grant.IDToken = ""
	rotated.Grant.AccessToken = "new-access-without-email"
	next, err := rotated.data(auth.AuthID, "", got["Metadata"].(map[string]any))
	if err != nil {
		t.Fatal(err)
	}
	var stored credential
	_ = json.Unmarshal(next["StorageJSON"].([]byte), &stored)
	if stored.AuthID != auth.AuthID || stored.Email != auth.Email || stored.Grant.AccessToken != rotated.Grant.AccessToken {
		t.Fatal("token rotation lost imported identity")
	}
}

func TestImportedMetadataSurvivesRefreshSerialization(t *testing.T) {
	_, auth, _ := quotaFixture(t)
	raw, _ := json.Marshal(auth)
	var stored map[string]json.RawMessage
	_ = json.Unmarshal(raw, &stored)
	stored["note"] = json.RawMessage(`"operator note"`)
	stored["priority"] = json.RawMessage(`7`)
	stored["custom_metadata"] = json.RawMessage(`{"fixture":true}`)
	stored["access_token"] = json.RawMessage(`"stale-secret"`)
	raw, _ = json.Marshal(stored)
	parsed, ok := credentialFromStorage(raw)
	if !ok {
		t.Fatal("migrated credential invalid")
	}
	data, err := parsed.data("original-id", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	saved := data["StorageJSON"].([]byte)
	var result map[string]json.RawMessage
	_ = json.Unmarshal(saved, &result)
	if string(result["note"]) != `"operator note"` || string(result["priority"]) != "7" || len(result["custom_metadata"]) == 0 || len(result["access_token"]) > 0 {
		t.Fatal("metadata lost or stale token retained")
	}
	if data["Metadata"].(map[string]any)["note"] != "operator note" {
		t.Fatal("operator note unavailable to host")
	}
}
