package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	bpsUsageURL        = "https://chatgpt.com/backend-api/wham/usage"
	bpsResetCreditsURL = "https://chatgpt.com/backend-api/wham/rate-limit-reset-credits"
)

var (
	ErrBPSReauthorization = errors.New("BPS quota credential requires authorization")
	ErrBPSQuotaDenied     = errors.New("BPS quota query was denied")
)

// BPSQuotaResult is the legacy BPS Manager quota contract. Missing fields remain
// absent. ObservedAt describes a successful server observation, not a JWT hint.
type BPSQuotaResult struct {
	Quota                         json.RawMessage `json:"quota"`
	ObservedAt                    string          `json:"observed_at,omitempty"`
	ResetCardsAvailable           *int            `json:"reset_cards_available,omitempty"`
	ApplicableResetCardsAvailable *int            `json:"applicable_reset_cards_available,omitempty"`
	Warning                       string          `json:"warning,omitempty"`
	Source                        string          `json:"source"`
	SupportsReset                 bool            `json:"supports_reset"`
	Stale                         bool            `json:"stale,omitempty"`
	ResetEligible                 bool            `json:"reset_eligible"`
	ResetUnavailableReason        string          `json:"reset_unavailable_reason,omitempty"`
	CredentialDisabled            bool            `json:"credential_disabled,omitempty"`
}

// BPSProfile reads display/routing hints from a token obtained by OAuth. Parsing
// these claims does not verify a token or establish that it remains authorized.
func BPSProfile(token string) (accountID, plan string) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || len(parts[1]) > 128<<10 {
		return "", ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
			Plan      string `json:"chatgpt_plan_type"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return "", ""
	}
	if validBPSIdentifier(claims.Auth.AccountID, 256) {
		accountID = claims.Auth.AccountID
	}
	if validBPSIdentifier(strings.ToLower(claims.Auth.Plan), 64) {
		plan = strings.ToLower(claims.Auth.Plan)
	}
	return accountID, plan
}

func validBPSIdentifier(value string, maximum int) bool {
	if value == "" || len(value) > maximum {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '_' || char == '-' || char == '.') {
			return false
		}
	}
	return true
}

func (c *Client) BPSQuota(ctx context.Context, token, accountID string) (BPSQuotaResult, error) {
	return c.bpsQuota(ctx, bpsUsageURL, bpsResetCreditsURL, token, accountID)
}

func (c *Client) bpsQuota(ctx context.Context, usageURL, creditsURL, token, accountID string) (BPSQuotaResult, error) {
	// Independent reads share the deadline, so a slow usage response does not
	// prevent a successful credit lookup from being retained (or vice versa).
	type read struct {
		raw []byte
		err error
	}
	usageRead, creditRead := make(chan read, 1), make(chan read, 1)
	go func() { raw, err := c.readBPSQuota(ctx, usageURL, token, accountID); usageRead <- read{raw, err} }()
	go func() { raw, err := c.readBPSQuota(ctx, creditsURL, token, accountID); creditRead <- read{raw, err} }()
	usage, credits := <-usageRead, <-creditRead
	observed := time.Now().UTC()
	var result BPSQuotaResult
	if usage.err == nil {
		result, usage.err = normalizeBPSUsage(usage.raw, observed)
	}
	var cards struct {
		Available *int `json:"available_count"`
	}
	if credits.err == nil && (json.Unmarshal(credits.raw, &cards) != nil || !validResetCardCount(cards.Available)) {
		credits.err = ErrInvalidQuota
	}
	if usage.err != nil {
		if credits.err != nil {
			return BPSQuotaResult{}, usage.err
		}
		result = BPSQuotaResult{Quota: json.RawMessage(`{}`), Source: "reset_credits",
			ObservedAt: observed.Format(time.RFC3339), Warning: "active_quota_unavailable", SupportsReset: true}
	}
	if credits.err == nil {
		result.ResetCardsAvailable = cards.Available
	}
	if result.ResetCardsAvailable == nil {
		result.Warning = "reset_cards_unavailable"
	}
	result.UpdateResetEligibility()
	return result, nil
}

// UpdateResetEligibility keeps ownership separate from permission to redeem.
// In particular, available_count must never imply applicable_available_count.
func (r *BPSQuotaResult) UpdateResetEligibility() {
	r.ResetEligible = false
	switch {
	case r.CredentialDisabled:
		r.ResetUnavailableReason = "bps_credential_disabled"
	case r.Stale:
		r.ResetUnavailableReason = "bps_reset_stale_quota"
	case !r.SupportsReset:
		r.ResetUnavailableReason = "bps_reset_unavailable"
	case r.ResetCardsAvailable != nil && !validResetCardCount(r.ResetCardsAvailable),
		r.ApplicableResetCardsAvailable != nil && !validResetCardCount(r.ApplicableResetCardsAvailable):
		r.ResetUnavailableReason = "bps_reset_inconsistent_credits"
	case r.ResetCardsAvailable == nil || r.ApplicableResetCardsAvailable == nil:
		r.ResetUnavailableReason = "bps_reset_eligibility_unknown"
	case *r.ApplicableResetCardsAvailable > *r.ResetCardsAvailable:
		r.ResetUnavailableReason = "bps_reset_inconsistent_credits"
	case *r.ResetCardsAvailable == 0:
		r.ResetUnavailableReason = "bps_reset_no_credits"
	case *r.ApplicableResetCardsAvailable == 0:
		r.ResetUnavailableReason = "bps_reset_not_applicable"
	default:
		r.ResetEligible = true
		r.ResetUnavailableReason = ""
	}
}

func (c *Client) readBPSQuota(ctx context.Context, endpoint, token, accountID string) ([]byte, error) {
	if accountID != "" && !validBPSIdentifier(accountID, 256) {
		return nil, ErrInvalidToken
	}
	req, err := newRequest(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return nil, err
	}
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-ID", accountID)
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrQuotaUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return nil, ErrBPSReauthorization
	case http.StatusForbidden:
		return nil, ErrBPSQuotaDenied
	case http.StatusOK:
	default:
		return nil, ErrQuotaUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxQuotaReplyBytes+1))
	if err != nil || len(raw) > maxQuotaReplyBytes || !json.Valid(raw) {
		return nil, ErrInvalidQuota
	}
	return raw, nil
}

type bpsUsage struct {
	PlanType  string `json:"plan_type"`
	RateLimit *struct {
		Primary   *bpsQuotaWindow `json:"primary_window"`
		Secondary *bpsQuotaWindow `json:"secondary_window"`
	} `json:"rate_limit"`
	ResetCredits *struct {
		Available  *int `json:"available_count"`
		Applicable *int `json:"applicable_available_count"`
	} `json:"rate_limit_reset_credits"`
}

type bpsQuotaWindow struct {
	UsedPercent        *float64 `json:"used_percent"`
	LimitWindowSeconds *int64   `json:"limit_window_seconds"`
	ResetAt            *int64   `json:"reset_at"`
	ResetAfterSeconds  *int64   `json:"reset_after_seconds"`
}

func normalizeBPSUsage(raw []byte, observed time.Time) (BPSQuotaResult, error) {
	var usage bpsUsage
	if json.Unmarshal(raw, &usage) != nil {
		return BPSQuotaResult{}, ErrInvalidQuota
	}
	snapshot := quotaSnapshot{}
	if usage.PlanType != "" {
		plan := strings.ToLower(usage.PlanType)
		if !validBPSIdentifier(plan, 64) {
			return BPSQuotaResult{}, ErrInvalidQuota
		}
		snapshot.Subscription = &quotaSubscription{Plan: plan}
	}
	buckets := make([]*quotaBucket, 0, 2)
	if limit := usage.RateLimit; limit != nil {
		for _, item := range []struct {
			name   string
			window *bpsQuotaWindow
		}{{"primary", limit.Primary}, {"secondary", limit.Secondary}} {
			bucket, err := normalizeBPSWindow(item.name, item.window, observed)
			if err != nil {
				return BPSQuotaResult{}, err
			}
			if bucket != nil {
				buckets = append(buckets, bucket)
			}
		}
	}
	if len(buckets) > 0 {
		snapshot.Groups = []*quotaGroup{{DisplayName: "BPS account quota", Buckets: buckets}}
	}
	if snapshot.Subscription == nil && len(buckets) == 0 {
		return BPSQuotaResult{}, ErrInvalidQuota
	}
	quota, err := json.Marshal(snapshot)
	if err != nil {
		return BPSQuotaResult{}, err
	}
	result := BPSQuotaResult{Quota: quota, ObservedAt: observed.UTC().Format(time.RFC3339),
		Source: "active_usage", SupportsReset: true}
	if credits := usage.ResetCredits; credits != nil {
		if credits.Available != nil && !validResetCardCount(credits.Available) ||
			credits.Applicable != nil && !validResetCardCount(credits.Applicable) {
			return BPSQuotaResult{}, ErrInvalidQuota
		}
		result.ResetCardsAvailable = credits.Available
		result.ApplicableResetCardsAvailable = credits.Applicable
	}
	result.UpdateResetEligibility()
	return result, nil
}

func normalizeBPSWindow(name string, window *bpsQuotaWindow, observed time.Time) (*quotaBucket, error) {
	if window == nil || window.UsedPercent == nil {
		return nil, nil
	}
	used := *window.UsedPercent
	if used < 0 || used > 100 {
		return nil, ErrInvalidQuota
	}
	fraction := 1 - used/100
	bucket := &quotaBucket{Window: name, RemainingFraction: &fraction}
	if seconds := window.LimitWindowSeconds; seconds != nil {
		if *seconds <= 0 || *seconds > 366*24*3600 {
			return nil, ErrInvalidQuota
		}
		if *seconds%60 == 0 {
			bucket.Window = strconv.FormatInt(*seconds/60, 10) + "m"
		}
	}
	if stamp := window.ResetAt; stamp != nil {
		if *stamp <= 0 || *stamp > 253402300799 {
			return nil, ErrInvalidQuota
		}
		bucket.ResetTime = time.Unix(*stamp, 0).UTC().Format(time.RFC3339)
	} else if after := window.ResetAfterSeconds; after != nil {
		if *after < 0 || *after > 366*24*3600 {
			return nil, ErrInvalidQuota
		}
		bucket.ResetTime = observed.Add(time.Duration(*after) * time.Second).UTC().Format(time.RFC3339)
	}
	return bucket, nil
}

func validResetCardCount(count *int) bool {
	return count != nil && *count >= 0 && *count <= 1_000_000
}
