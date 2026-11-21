// Package quality runs small, account-scoped answer checks. A passing answer is
// evidence about this one prompt, not proof of model identity or overall ability.
package quality

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"cpa-bps-reconstruction/internal/candy"
)

const (
	MaxModels       = 16
	HistoryPerModel = 20
	MaxAnswerBytes  = 2048
)

var (
	ErrInvalid     = errors.New("quality configuration is invalid")
	ErrUnavailable = errors.New("quality state is unavailable")
	ErrBusy        = errors.New("quality check is already running")
	ErrCooldown    = errors.New("quality check is cooling down")
	ErrBlocked     = errors.New("quality checks are paused after a usage policy rejection")
	ErrClosed      = errors.New("quality service is closed")
)

type Config struct {
	Enabled         bool     `json:"enabled"`
	Models          []string `json:"models"`
	IntervalMinutes int      `json:"interval_minutes"`
	ReasoningEffort string   `json:"reasoning_effort"`
	Prompt          string   `json:"prompt"`
	ExpectedAnswer  string   `json:"expected_answer"`
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
}

type Result struct {
	ID                     string    `json:"id"`
	RunID                  string    `json:"run_id"`
	AuthIndex              string    `json:"auth_index"`
	RequestedModel         string    `json:"requested_model"`
	ActualModel            string    `json:"actual_model,omitempty"`
	ModelMismatch          bool      `json:"model_mismatch"`
	ValidForRequestedModel bool      `json:"valid_for_requested_model"`
	Status                 string    `json:"status"`
	StartedAt              time.Time `json:"started_at"`
	CompletedAt            time.Time `json:"completed_at"`
	ElapsedMS              int64     `json:"elapsed_ms"`
	Answer                 string    `json:"answer,omitempty"`
	Prompt                 string    `json:"prompt"`
	ExtractedAnswer        string    `json:"extracted_answer,omitempty"`
	ExpectedAnswer         string    `json:"expected_answer"`
	ErrorCode              string    `json:"error_code,omitempty"`
	BenchmarkVersion       string    `json:"benchmark_version"`
	ReasoningEffort        string    `json:"reasoning_effort"`
	Usage                  Usage     `json:"usage"`
	Manual                 bool      `json:"manual,omitempty"`
}

type RunRequest struct {
	AuthIndex       string   `json:"auth_index,omitempty"`
	Models          []string `json:"models,omitempty"`
	ReasoningEffort string   `json:"reasoning_effort,omitempty"`
}

type Check struct {
	AuthIndex string
	AuthID    string
	Model     string
	Config    Config
	RunID     string
	// Manual marks an explicit run. It may recheck a historical local warning,
	// but changes neither the saved periodic plan nor upstream authorization.
	Manual bool
}

type Snapshot struct {
	AuthIndex          string    `json:"auth_index"`
	Config             Config    `json:"config"`
	NextDue            time.Time `json:"next_due,omitempty"`
	LastStarted        time.Time `json:"last_started,omitempty"`
	ReservedUntil      time.Time `json:"reserved_until,omitempty"`
	Running            bool      `json:"running"`
	RunID              string    `json:"run_id,omitempty"`
	Blocked            bool      `json:"blocked"`
	CredentialDisabled bool      `json:"credential_disabled"`
	StateError         bool      `json:"state_error"`
	Results            []Result  `json:"results"`
}

var modelPattern = regexp.MustCompile(`^[A-Za-z0-9_.:/-]{1,128}$`)
var secretPattern = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/-]{8,}|\bsk-[A-Za-z0-9_-]{8,}|\beyJ[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]{12,}\.[A-Za-z0-9_-]+`)

func ValidModel(value string) bool { return modelPattern.MatchString(value) }

// BenchmarkVersion also labels unexecuted members of an accepted batch.
func BenchmarkVersion(cfg Config) string {
	if cfg.Prompt == candy.DefaultPrompt && cfg.ExpectedAnswer == candy.DefaultExpectedAnswer {
		return candy.Version
	}
	digest := sha256.Sum256([]byte(cfg.Prompt + "\x00" + cfg.ExpectedAnswer))
	return "custom-" + hex.EncodeToString(digest[:8])
}

func ValidateConfig(c Config) error {
	if c.IntervalMinutes < 1 || c.IntervalMinutes > 10080 || len(c.Models) > MaxModels || c.Enabled && len(c.Models) == 0 ||
		strings.TrimSpace(c.Prompt) == "" || len(c.Prompt) > 8192 || strings.TrimSpace(c.ExpectedAnswer) == "" || len(c.ExpectedAnswer) > 128 ||
		secretPattern.MatchString(c.Prompt) || secretPattern.MatchString(c.ExpectedAnswer) || !candy.Evaluate(c.ExpectedAnswer, c.ExpectedAnswer) {
		return ErrInvalid
	}
	switch c.ReasoningEffort {
	case "", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra":
	default:
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, model := range c.Models {
		if !ValidModel(model) || seen[model] {
			return ErrInvalid
		}
		seen[model] = true
	}
	return nil
}

// SafeText removes credential-looking strings before truncation. Callers also
// replace the exact in-memory credentials before passing upstream answer text.
func SafeText(value string, limit int) string {
	value = secretPattern.ReplaceAllString(value, "[redacted]")
	value = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return -1
		}
		return r
	}, value)
	if len(value) <= limit {
		return value
	}
	value = value[:limit]
	for !utf8.ValidString(value) && len(value) > 0 {
		value = value[:len(value)-1]
	}
	return value + "…"
}
