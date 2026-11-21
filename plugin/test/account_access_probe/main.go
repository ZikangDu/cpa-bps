// account_access_probe makes one authorized request and prints only safe diagnostics.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"time"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/upstream"
)

func main() {
	var input struct {
		Token string `json:"token"`
	}
	decoder := json.NewDecoder(io.LimitReader(os.Stdin, 256<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || input.Token == "" {
		fmt.Println(`{"failure":"invalid_private_input"}`)
		os.Exit(2)
	}
	body, _, err := bpsprotocol.PrepareWithTextTools([]byte(`{"model":"gpt-6.2-sol","input":"Reply only READY.","reasoning":{"effort":"low"},"stream":true}`), "isolated-account-access-diagnostic")
	if err != nil {
		fmt.Println(`{"failure":"preparation_failed"}`)
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := upstream.New()
	defer client.CloseIdleConnections()
	response, err := client.Do(ctx, "https://bps.openai.com/basispoints/api/responses", input.Token, body, true)
	if err != nil {
		fmt.Println(`{"failure":"transport_error"}`)
		os.Exit(1)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		response.Body.Close()
		response.Body = io.NopCloser(bytes.NewReader(raw))
		markers := []string{}
		for _, marker := range []string{"cloudflare", "1010", "challenge", "workspace", "organization", "deactivated", "suspended", "account", "permission", "scope", "expired", "invalid token", "unauthorized", "access denied", "forbidden", "region", "country", "quota", "rate limit", "blocked", "security", "systems", "try again", "policy", "abuse", "unusual", "support", "user", "sign in", "login", "email", "verification", "not allowed", "not authorized", "membership", "subscription"} {
			if strings.Contains(strings.ToLower(string(raw)), marker) {
				markers = append(markers, marker)
			}
		}
		var detail struct {
			Error struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"error"`
		}
		_ = json.Unmarshal(raw, &detail)
		message := strings.ReplaceAll(detail.Error.Message, input.Token, "[redacted]")
		message = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`).ReplaceAllString(message, "[email]")
		message = regexp.MustCompile(`[A-Za-z0-9_./+=:\-]{20,}`).ReplaceAllString(message, "[identifier]")
		if len(message) > 240 || strings.ContainsAny(message, "\r\n<>") || len(strings.Fields(message)) < 3 {
			message = "[omitted]"
		}
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"diagnosis": upstream.DescribeRejection(response), "body_bytes": len(raw), "diagnostic_keywords": markers, "redacted_upstream_message": message})
		os.Exit(1)
	}
	terminal, err := upstream.ReadEvents(ctx, response.Body, nil)
	if err != nil {
		fmt.Println(`{"http_status":200,"failure":"stream_incomplete"}`)
		os.Exit(1)
	}
	var result struct {
		Status string `json:"status"`
		Model  string `json:"model"`
	}
	if json.Unmarshal(terminal, &result) != nil || result.Status != "completed" {
		fmt.Println(`{"http_status":200,"failure":"terminal_not_completed"}`)
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"http_status": 200, "completed": true, "model": result.Model})
}
