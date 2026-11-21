package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
	"unicode"
)

const (
	maxQuotaReplyBytes = 128 << 10
	maxQuotaGroups     = 32
	maxQuotaBuckets    = 128
	maxQuotaMetrics    = 32
	maxQuotaText       = 256
)

var (
	ErrQuotaUnavailable = errors.New("upstream quota lookup is unavailable")
	ErrInvalidQuota     = errors.New("upstream quota response is invalid")
)

// Quota reads the configured provider's quota endpoint without changing
// upstream state. It returns only the normalized CPA quota schema.
func (c *Client) Quota(ctx context.Context, endpoint, token string) (json.RawMessage, error) {
	if endpoint == "" {
		return nil, ErrQuotaUnavailable
	}
	request, err := newRequest(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrQuotaUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrQuotaUnavailable
	}
	raw, err := ReadJSON(io.LimitReader(response.Body, maxQuotaReplyBytes+1))
	if err != nil || len(raw) > maxQuotaReplyBytes {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrInvalidQuota
	}
	var data quotaSnapshot
	if json.Unmarshal(raw, &data) != nil || !data.normalize() {
		return nil, ErrInvalidQuota
	}
	return json.Marshal(data)
}

type quotaSnapshot struct {
	Subscription       *quotaSubscription `json:"subscription,omitempty"`
	Summary            []*quotaMetric     `json:"summary,omitempty"`
	ServerTimeOffsetMs *int64             `json:"serverTimeOffsetMs,omitempty"`
	AlternateOffsetMs  *int64             `json:"server_time_offset_ms,omitempty"`
	Groups             []*quotaGroup      `json:"groups,omitempty"`
}

type quotaSubscription struct {
	Plan              string `json:"plan,omitempty"`
	TierName          string `json:"tierName,omitempty"`
	TierID            string `json:"tierId,omitempty"`
	AlternateTierName string `json:"tier_name,omitempty"`
	AlternateTierID   string `json:"tier_id,omitempty"`
}

type quotaGroup struct {
	DisplayName          string         `json:"displayName,omitempty"`
	AlternateDisplayName string         `json:"display_name,omitempty"`
	Buckets              []*quotaBucket `json:"buckets,omitempty"`
}

type quotaBucket struct {
	Window                     string   `json:"window,omitempty"`
	RemainingFraction          *float64 `json:"remainingFraction"`
	AlternateRemainingFraction *float64 `json:"remaining_fraction,omitempty"`
	ResetTime                  string   `json:"resetTime,omitempty"`
	AlternateResetTime         string   `json:"reset_time,omitempty"`
	Description                string   `json:"description,omitempty"`
}

type quotaMetric struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Value    *float64 `json:"value"`
	Unit     string   `json:"unit,omitempty"`
	Format   string   `json:"format,omitempty"`
	Currency string   `json:"currency,omitempty"`
}

func (q *quotaSnapshot) normalize() bool {
	if len(q.Groups) > maxQuotaGroups || len(q.Summary) > maxQuotaMetrics {
		return false
	}
	if q.ServerTimeOffsetMs == nil {
		q.ServerTimeOffsetMs = q.AlternateOffsetMs
	}
	q.AlternateOffsetMs = nil
	maxOffset := (24 * time.Hour).Milliseconds()
	if offset := q.ServerTimeOffsetMs; offset != nil && (*offset > maxOffset || *offset < -maxOffset) {
		return false
	}
	if subscription := q.Subscription; subscription != nil {
		subscription.TierName = takeQuotaAlias(subscription.TierName, &subscription.AlternateTierName)
		subscription.TierID = takeQuotaAlias(subscription.TierID, &subscription.AlternateTierID)
		if !validQuotaText(subscription.Plan, subscription.TierName, subscription.TierID) {
			return false
		}
		if subscription.Plan == "" && subscription.TierName == "" && subscription.TierID == "" {
			q.Subscription = nil
		}
	}
	for _, metric := range q.Summary {
		if !metric.valid() {
			return false
		}
	}
	var buckets int
	for _, group := range q.Groups {
		if group == nil || len(group.Buckets) == 0 {
			return false
		}
		group.DisplayName = takeQuotaAlias(group.DisplayName, &group.AlternateDisplayName)
		buckets += len(group.Buckets)
		if buckets > maxQuotaBuckets || !validQuotaText(group.DisplayName) {
			return false
		}
		for _, bucket := range group.Buckets {
			if !bucket.normalize() {
				return false
			}
		}
	}
	return q.Subscription != nil || len(q.Summary) > 0 || buckets > 0
}

func (b *quotaBucket) normalize() bool {
	if b == nil {
		return false
	}
	if b.RemainingFraction == nil {
		b.RemainingFraction = b.AlternateRemainingFraction
	}
	b.AlternateRemainingFraction = nil
	if value := b.RemainingFraction; value == nil || *value < 0 || *value > 1 {
		return false
	}
	b.ResetTime = takeQuotaAlias(b.ResetTime, &b.AlternateResetTime)
	if b.ResetTime != "" {
		if _, err := time.Parse(time.RFC3339, b.ResetTime); err != nil {
			return false
		}
	}
	return validQuotaText(b.Window, b.Description)
}

func (m *quotaMetric) valid() bool {
	if m == nil || strings.TrimSpace(m.Key) == "" || strings.TrimSpace(m.Label) == "" ||
		m.Value == nil || math.Abs(*m.Value) > 1e15 || !validQuotaText(m.Key, m.Label, m.Unit) {
		return false
	}
	if m.Format != "" && m.Format != "number" && m.Format != "currency" {
		return false
	}
	if m.Format == "currency" || m.Currency != "" {
		return len(m.Currency) == 3 && !strings.ContainsFunc(m.Currency, func(r rune) bool {
			return r < 'A' || r > 'Z'
		})
	}
	return true
}

func takeQuotaAlias(value string, alternate *string) string {
	if value == "" {
		value = *alternate
	}
	*alternate = ""
	return value
}

func validQuotaText(values ...string) bool {
	for _, value := range values {
		if len(value) > maxQuotaText || strings.ContainsFunc(value, unicode.IsControl) {
			return false
		}
	}
	return true
}
