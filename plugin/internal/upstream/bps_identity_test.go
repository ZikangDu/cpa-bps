package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
)

func TestBPSEmailReadsOnlyBoundedDisplayClaims(t *testing.T) {
	token := func(claims any) string {
		raw, _ := json.Marshal(claims)
		return "e30." + base64.RawURLEncoding.EncodeToString(raw) + ".fixture"
	}
	for _, claim := range []any{map[string]any{"email": "fixture@example.com"}, map[string]any{"https://api.openai.com/profile": map[string]string{"email": "fixture@example.com"}}} {
		if got := BPSEmail(token(claim)); got != "fixture@example.com" {
			t.Fatal("email omitted", got)
		}
	}
	for _, email := range []string{"", "no-at", "x@@example.com", "<script>@example.com", "bad\n@example.com", strings.Repeat("x", 255) + "@example.com"} {
		if BPSEmail(token(map[string]any{"email": email})) != "" {
			t.Fatal("invalid display claim accepted")
		}
	}
	for _, raw := range []string{"invalid", "a.!.b", "a." + strings.Repeat("e", 129<<10) + ".b"} {
		if BPSEmail(raw) != "" {
			t.Fatal("invalid token accepted")
		}
	}
}
