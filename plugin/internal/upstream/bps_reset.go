package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

const bpsConsumeResetCreditURL = bpsResetCreditsURL + "/consume"

var (
	ErrBPSResetRejected       = errors.New("BPS reset credit redemption rejected")
	ErrBPSResetOutcomeUnknown = errors.New("BPS reset credit redemption outcome unknown")
)

// ValidBPSRedeemRequestID accepts UUIDs and the panel's documented opaque
// fallback request IDs. This identifier is reused for duplicate submissions.
func ValidBPSRedeemRequestID(id string) bool {
	return len(id) >= 16 && validBPSIdentifier(id, 128)
}

func (c *Client) ConsumeBPSResetCredit(ctx context.Context, token, accountID, requestID string) error {
	return c.consumeBPSResetCredit(ctx, bpsConsumeResetCreditURL, token, accountID, requestID)
}

// The caller must freshly validate applicability and serialize each account.
// No POST is replayed, including after ambiguous transport or server failures.
func (c *Client) consumeBPSResetCredit(ctx context.Context, endpoint, token, accountID, requestID string) error {
	if !validBPSIdentifier(accountID, 256) || !ValidBPSRedeemRequestID(requestID) {
		return ErrInvalidToken
	}
	body, _ := json.Marshal(map[string]string{"redeem_request_id": requestID})
	req, err := newRequest(ctx, http.MethodPost, endpoint, token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.GetBody = nil
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("ChatGPT-Account-ID", accountID)
	response, err := c.http.Do(req)
	if err != nil {
		return ErrBPSResetOutcomeUnknown
	}
	defer response.Body.Close()
	// Discard unrelated account data. Status is sufficient for the existing
	// consume contract, whose successful response is not guaranteed to be JSON.
	_, readErr := io.Copy(io.Discard, io.LimitReader(response.Body, maxQuotaReplyBytes+1))
	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}
	if response.StatusCode >= 500 || readErr != nil {
		return ErrBPSResetOutcomeUnknown
	}
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return ErrBPSReauthorization
	case http.StatusForbidden:
		return ErrBPSQuotaDenied
	default:
		return ErrBPSResetRejected
	}
}
