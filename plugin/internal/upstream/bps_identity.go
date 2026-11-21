package upstream

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"unicode"
)

// BPSEmail reads display-only claims from a locally stored OAuth grant. It is
// never an authentication decision and never causes a network request.
func BPSEmail(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 128<<10 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Email   string `json:"email"`
		Profile struct {
			Email string `json:"email"`
		} `json:"https://api.openai.com/profile"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	for _, value := range []string{claims.Email, claims.Profile.Email} {
		if email := BPSDisplayEmail(value); email != "" {
			return email
		}
	}
	return ""
}

// BPSDisplayEmail validates an existing display value during credential import.
func BPSDisplayEmail(value string) string {
	email := strings.TrimSpace(value)
	if len(email) > 254 || strings.Count(email, "@") != 1 || strings.HasPrefix(email, "@") || strings.HasSuffix(email, "@") || strings.ContainsAny(email, "<>\"\\") {
		return ""
	}
	if strings.ContainsFunc(email, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return ""
	}
	return email
}
