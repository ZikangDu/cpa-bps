package oauthflow

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const maxTokenResponse = 1 << 20

var (
	ErrInvalidGrant        = errors.New("oauth issuer rejected the grant")
	ErrExchange            = errors.New("oauth token exchange did not complete")
	ErrInvalidResponse     = errors.New("oauth issuer returned an invalid token response")
	ErrMissingRefreshToken = errors.New("oauth credential has no refresh token")
	ErrClientSecret        = errors.New("oauth client secret is unavailable")
)

type Tokens struct {
	AccessToken       string    `json:"access_token"`
	RefreshToken      string    `json:"refresh_token,omitempty"`
	IDToken           string    `json:"id_token,omitempty"`
	Scope             string    `json:"scope,omitempty"`
	ExpiresAt         time.Time `json:"expires_at"`
	EarliestRefreshAt time.Time `json:"earliest_refresh_at,omitzero"`
}

type tokenResponse struct {
	AccessToken       string          `json:"access_token"`
	RefreshToken      string          `json:"refresh_token"`
	IDToken           string          `json:"id_token"`
	Scope             string          `json:"scope"`
	TokenType         string          `json:"token_type"`
	ExpiresIn         json.RawMessage `json:"expires_in"`
	EarliestRefreshAt json.RawMessage `json:"earliest_refresh_at"`
	Error             json.RawMessage `json:"error"`
}

// Refresh submits one exchange. The caller reconciles ambiguous failures and
// must persist a returned replacement refresh token before using it again.
func (f *Flow) Refresh(ctx context.Context, cfg Config, old Tokens) (Tokens, error) {
	if err := cfg.Validate(); err != nil {
		return Tokens{}, err
	}
	if !validOpaqueToken(old.RefreshToken) {
		return Tokens{}, ErrMissingRefreshToken
	}
	f.mu.Lock()
	closed := f.closed
	f.mu.Unlock()
	if closed {
		return Tokens{}, ErrClosed
	}

	next, err := f.exchange(ctx, cfg, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {old.RefreshToken},
	})
	if err != nil {
		return Tokens{}, err
	}
	if next.RefreshToken == "" {
		next.RefreshToken = old.RefreshToken
	}
	if next.IDToken == "" {
		next.IDToken = old.IDToken
	}
	if next.Scope == "" {
		next.Scope = old.Scope
	}
	if next.EarliestRefreshAt.IsZero() {
		next.EarliestRefreshAt = old.EarliestRefreshAt
	}
	return next, nil
}

func (f *Flow) exchange(ctx context.Context, cfg Config, form url.Values) (Tokens, error) {
	secret, err := clientSecret(cfg)
	if err != nil {
		return Tokens{}, err
	}
	if secret == "" {
		form.Set("client_id", cfg.ClientID)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return Tokens{}, ErrExchange
	}
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("Accept", "application/json")
	if secret != "" {
		request.SetBasicAuth(url.QueryEscape(cfg.ClientID), url.QueryEscape(secret))
	}

	started := f.now()
	client, closeClient := f.tokenClient(cfg.ProxyURL)
	defer closeClient()
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return Tokens{}, ctx.Err()
		}
		return Tokens{}, ErrExchange
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxTokenResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return Tokens{}, ctx.Err()
		}
		return Tokens{}, ErrExchange
	}
	var data tokenResponse
	if len(raw) > maxTokenResponse || json.Unmarshal(raw, &data) != nil {
		if response.StatusCode == http.StatusOK {
			return Tokens{}, ErrInvalidResponse
		}
		return Tokens{}, ErrExchange
	}
	if response.StatusCode != http.StatusOK {
		if (response.StatusCode == http.StatusBadRequest || response.StatusCode == http.StatusUnauthorized) &&
			errorCode(data.Error) == "invalid_grant" {
			return Tokens{}, ErrInvalidGrant
		}
		return Tokens{}, ErrExchange
	}
	lifetime, ok := duration(data.ExpiresIn)
	earliest, validEarliest := refreshTime(data.EarliestRefreshAt)
	hasError := len(data.Error) != 0 && string(data.Error) != "null" && string(data.Error) != `""`
	if !ok || !validEarliest || hasError || !validBearer(data.AccessToken) || !strings.EqualFold(data.TokenType, "Bearer") ||
		(data.RefreshToken != "" && !validOpaqueToken(data.RefreshToken)) ||
		(data.IDToken != "" && !validBearer(data.IDToken)) || !printable(data.Scope) ||
		!f.now().Before(started.Add(lifetime)) {
		return Tokens{}, ErrInvalidResponse
	}
	return Tokens{
		AccessToken:       data.AccessToken,
		RefreshToken:      data.RefreshToken,
		IDToken:           data.IDToken,
		Scope:             data.Scope,
		ExpiresAt:         started.Add(lifetime).UTC(),
		EarliestRefreshAt: earliest,
	}, nil
}

// Only stable OAuth classification leaves this parser. Provider descriptions
// and other arbitrary fields are never used in diagnostic messages.
func errorCode(raw json.RawMessage) string {
	var code string
	if json.Unmarshal(raw, &code) != nil {
		var detail struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(raw, &detail) != nil {
			return ""
		}
		code = detail.Code
	}
	if code == "invalid_grant" {
		return code
	}
	return ""
}

func refreshTime(raw json.RawMessage) (time.Time, bool) {
	if len(raw) == 0 {
		return time.Time{}, true
	}
	value := string(raw)
	if value[0] == '"' {
		if json.Unmarshal(raw, &value) != nil {
			return time.Time{}, false
		}
		if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
			return stamp.UTC(), stamp.After(time.Unix(0, 0))
		}
	}
	// The reference contract supplies Unix seconds. Values too large for a
	// four-digit year are rejected instead of guessing a different unit.
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 || seconds > 253402300799 {
		return time.Time{}, false
	}
	return time.Unix(seconds, 0).UTC(), true
}

func clientSecret(cfg Config) (string, error) {
	if cfg.ClientSecretEnv == "" {
		return "", nil
	}
	value, present := os.LookupEnv(cfg.ClientSecretEnv)
	if !present || strings.TrimSpace(value) == "" || !printable(value) {
		return "", ErrClientSecret
	}
	return value, nil
}

func duration(raw json.RawMessage) (time.Duration, bool) {
	value := string(raw)
	if len(value) == 0 {
		return 0, false
	}
	if value[0] == '"' && json.Unmarshal(raw, &value) != nil {
		return 0, false
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

func validOpaqueToken(value string) bool {
	return value != "" && printable(value)
}

func validBearer(value string) bool {
	core := strings.TrimRight(value, "=")
	if core == "" {
		return false
	}
	for _, r := range core {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("-._~+/", r)) {
			return false
		}
	}
	return true
}
