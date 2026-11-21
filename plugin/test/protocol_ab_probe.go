// Command protocol_ab_probe compares two prepared requests using one in-memory
// credential and the normal upstream transport. It never prints input or tokens.
// Run with --self-test to check evidence handling without any network access.
package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cpa-bps-reconstruction/internal/bpsprotocol"
	"cpa-bps-reconstruction/internal/upstream"
)

const endpoint = "https://bps.openai.com/basispoints/api/responses"

type input struct {
	Token        string          `json:"token"`
	OriginalBody json.RawMessage `json:"original_body"`
	Source       json.RawMessage `json:"source"`
	Scope        string          `json:"scope"`
	EvidencePath string          `json:"evidence_path"`
}

type doer interface {
	Do(context.Context, string, string, []byte, bool) (*http.Response, error)
}

type observation struct {
	Variant           string         `json:"variant"`
	Position          int            `json:"position"`
	BodySHA256        string         `json:"input_body_sha256"`
	HTTP              int            `json:"http,omitempty"`
	ElapsedMS         int64          `json:"elapsed_ms"`
	Passed            bool           `json:"passed"`
	Failure           string         `json:"failure,omitempty"`
	Error             map[string]any `json:"error,omitempty"`
	Usage             map[string]any `json:"usage,omitempty"`
	Model             string         `json:"model,omitempty"`
	InstructionsBytes *int           `json:"instructions_bytes,omitempty"`
	NativeToolsCount  *int           `json:"native_tools_count,omitempty"`
	Identifiers       map[string]any `json:"identifiers_sha256,omitempty"`
}

type evidence struct {
	StartedAt     string        `json:"started_at"`
	FinishedAt    string        `json:"finished_at,omitempty"`
	Endpoint      string        `json:"endpoint"`
	RequestBudget int           `json:"request_budget"`
	Deadline      int           `json:"request_deadline_seconds"`
	SourceSHA256  string        `json:"source_sha256"`
	Requests      []observation `json:"requests"`
	Passed        bool          `json:"passed"`
	Failure       string        `json:"failure,omitempty"`
}

var safeName = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,127}$`)

func digest(raw []byte) string {
	h := sha256.Sum256(raw)
	return hex.EncodeToString(h[:])
}

func object(raw []byte) (map[string]any, error) {
	var value map[string]any
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := d.Decode(&value); err != nil || value == nil {
		return nil, errors.New("invalid_object")
	}
	if d.Decode(new(any)) != io.EOF {
		return nil, errors.New("trailing_json")
	}
	return value, nil
}

// Preserve all numeric usage counters and their nesting, but no string values.
func numericUsage(source map[string]any) map[string]any {
	result := make(map[string]any)
	for key, value := range source {
		if !safeName.MatchString(key) {
			continue
		}
		switch typed := value.(type) {
		case json.Number:
			if number, err := typed.Float64(); err == nil && number >= 0 {
				result[key] = typed
			}
		case map[string]any:
			result[key] = numericUsage(typed)
		}
	}
	return result
}

func sanitizedError(raw []byte) map[string]any {
	body, err := object(raw)
	if err != nil {
		return nil
	}
	if nested, ok := body["error"].(map[string]any); ok {
		body = nested
	}
	allowed := map[string]bool{
		"rate_limit_exceeded": true, "rate_limit_error": true, "insufficient_quota": true,
		"invalid_api_key": true, "authentication_error": true, "permission_error": true,
		"access_denied": true, "model_not_found": true, "invalid_request_error": true,
		"server_error": true, "internal_server_error": true, "service_unavailable": true,
		"invalid_request": true, "context_length_exceeded": true,
	}
	result := make(map[string]any)
	for _, field := range []string{"code", "type"} {
		if value, ok := body[field].(string); ok && allowed[value] {
			result[field] = value
		}
	}
	return result
}

func responseText(response map[string]any) string {
	var text strings.Builder
	items, _ := response["output"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "message" {
			continue
		}
		parts, _ := item["content"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if part["type"] == "output_text" {
				value, _ := part["text"].(string)
				text.WriteString(value)
			}
		}
	}
	return text.String()
}

func perform(client doer, token string, body []byte, variant string, position int) (result observation) {
	start := time.Now()
	result = observation{Variant: variant, Position: position, BodySHA256: digest(body)}
	defer func() { result.ElapsedMS = time.Since(start).Milliseconds() }()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	response, err := client.Do(ctx, endpoint, token, body, true)
	if err != nil {
		result.Failure = "transport_error"
		if ctx.Err() != nil {
			result.Failure = "request_deadline"
		}
		return result
	}
	defer response.Body.Close()
	result.HTTP = response.StatusCode
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		result.Failure, result.Error = "http_rejected", sanitizedError(raw)
		return result
	}
	mediaType, _, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if mediaType != "text/event-stream" {
		result.Failure = "unexpected_content_type"
		return result
	}
	raw, err := upstream.ReadEvents(ctx, response.Body, nil)
	if err != nil {
		result.Failure = "stream_failed_or_incomplete"
		if ctx.Err() != nil {
			result.Failure = "request_deadline"
		}
		return result
	}
	terminal, err := object(raw)
	if err != nil {
		result.Failure = "invalid_terminal_json"
		return result
	}
	if usage, ok := terminal["usage"].(map[string]any); ok {
		result.Usage = numericUsage(usage)
	}
	if model, ok := terminal["model"].(string); ok && safeName.MatchString(model) {
		result.Model = model
	}
	if instructions, ok := terminal["instructions"].(string); ok {
		size := len(instructions)
		result.InstructionsBytes = &size
	}
	if tools, ok := terminal["tools"].([]any); ok {
		size := len(tools)
		result.NativeToolsCount = &size
	}
	result.Identifiers = make(map[string]any)
	for _, location := range []string{"response", "metadata"} {
		container := terminal
		if location == "metadata" {
			container, _ = terminal["metadata"].(map[string]any)
		}
		for _, key := range []string{"prompt_cache_key", "task_id", "turn_id"} {
			if value, ok := container[key].(string); ok && value != "" {
				result.Identifiers[location+"."+key] = digest([]byte(value))
			}
		}
	}
	if terminal["status"] != "completed" {
		result.Failure = "terminal_not_completed"
		return result
	}
	if strings.TrimSpace(responseText(terminal)) != "Mira" {
		result.Failure = "expected_answer_mismatch"
		return result
	}
	result.Passed = true
	return result
}

func save(path string, result evidence) error {
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".protocol-ab-")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(append(data, '\n')); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func run() int {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, (16<<20)+1))
	if err != nil || len(raw) > 16<<20 {
		fmt.Println(`{"failure":"invalid_stdin"}`)
		return 2
	}
	var cfg input
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF || cfg.Token == "" ||
		strings.ContainsFunc(cfg.Token, func(r rune) bool { return r <= ' ' || r >= 127 }) ||
		cfg.Scope == "" || cfg.EvidencePath == "" {
		fmt.Println(`{"failure":"invalid_stdin"}`)
		return 2
	}
	if _, err := os.Lstat(cfg.EvidencePath); !os.IsNotExist(err) {
		fmt.Println(`{"failure":"evidence_path_must_be_new"}`)
		return 2
	}
	original, err := object(cfg.OriginalBody)
	source, sourceErr := object(cfg.Source)
	if err != nil || sourceErr != nil || original["model"] != source["model"] || source["model"] == nil {
		fmt.Println(`{"failure":"invalid_or_mismatched_models"}`)
		return 2
	}
	original["stream"] = true
	originalRaw, err := json.Marshal(original)
	if err != nil {
		fmt.Println(`{"failure":"invalid_original_body"}`)
		return 2
	}
	rebuiltRaw, _, err := bpsprotocol.PrepareWithTextTools(cfg.Source, cfg.Scope)
	if err != nil {
		fmt.Println(`{"failure":"reconstruction_prepare_failed"}`)
		return 2
	}
	result := evidence{StartedAt: time.Now().UTC().Format(time.RFC3339), Endpoint: endpoint,
		RequestBudget: 6, Deadline: 90, SourceSHA256: digest(cfg.Source), Requests: []observation{}}
	if save(cfg.EvidencePath, result) != nil {
		fmt.Println(`{"failure":"evidence_write_failed"}`)
		return 2
	}
	client := upstream.New()
	for _, group := range []struct {
		name string
		body []byte
	}{{"original_prepared", originalRaw}, {"reconstructed_prepared", rebuiltRaw}} {
		for position := 1; position <= 3; position++ {
			row := perform(client, cfg.Token, group.body, group.name, position)
			result.Requests = append(result.Requests, row)
			result.FinishedAt = time.Now().UTC().Format(time.RFC3339)
			result.Passed = len(result.Requests) == 6 && row.Passed
			if !row.Passed {
				result.Failure = row.Failure
			}
			if save(cfg.EvidencePath, result) != nil {
				fmt.Println(`{"failure":"evidence_write_failed"}`)
				return 2
			}
			data, _ := json.Marshal(row)
			fmt.Println(string(data))
			if !row.Passed {
				return 1
			}
		}
	}
	return 0
}

type fixtureClient struct{ status int }

func (f fixtureClient) Do(_ context.Context, _ string, _ string, _ []byte, _ bool) (*http.Response, error) {
	body := `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"SECRET_MUST_NOT_LEAK"}}`
	contentType := "application/json"
	if f.status == 200 {
		body = `data: {"type":"response.completed","response":{"status":"completed","model":"fixture-model","output":[{"type":"message","content":[{"type":"output_text","text":"Mira"}]}],"usage":{"input_tokens":19752,"input_tokens_details":{"cached_tokens":10162,"cache_write_tokens":19,"secret":"SECRET_MUST_NOT_LEAK"}},"instructions":"SECRET_MUST_NOT_LEAK","tools":[],"prompt_cache_key":"SECRET_MUST_NOT_LEAK","metadata":{"task_id":"SECRET_MUST_NOT_LEAK"}}}` + "\n\n"
		contentType = "text/event-stream"
	}
	return &http.Response{StatusCode: f.status, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body))}, nil
}

func selfTest() int {
	for _, status := range []int{200, 403, 429} {
		row := perform(fixtureClient{status}, "SYNTHETIC_TOKEN", []byte(`{"fixture":true}`), "fixture", 1)
		data, err := json.Marshal(row)
		if err != nil || bytes.Contains(data, []byte("SECRET_MUST_NOT_LEAK")) || row.Passed != (status == 200) || row.HTTP != status {
			fmt.Println(`{"self_test":"failed"}`)
			return 1
		}
	}
	dir, err := os.MkdirTemp("", "protocol-ab-selftest-")
	if err != nil {
		return 1
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "evidence.json")
	if save(path, evidence{RequestBudget: 6}) != nil {
		return 1
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		return 1
	}
	fmt.Println(`{"self_test":"passed","network_requests":0,"fixture_cases":3,"evidence_mode":"0600"}`)
	return 0
}

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--self-test" {
		os.Exit(selfTest())
	}
	if len(os.Args) != 1 {
		fmt.Println(`{"usage":"protocol_ab_probe < private-input.json; --self-test performs no network calls"}`)
		os.Exit(2)
	}
	os.Exit(run())
}
