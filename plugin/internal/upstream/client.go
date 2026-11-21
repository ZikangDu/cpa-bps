package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var (
	ErrInvalidEndpoint = errors.New("upstream must be an HTTPS URL or an explicit HTTP loopback address")
	ErrInvalidToken    = errors.New("upstream Bearer credential is missing or invalid")
	ErrEmptyRequest    = errors.New("upstream request is empty")
	ErrModelCatalog    = errors.New("upstream model catalog is unavailable or invalid")
)

// Client owns a transport shared by requests to the configured Responses API.
// Redirects and ambient proxy settings do not affect credential routing.
type Client struct {
	http *http.Client
}

func New() *Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return &Client{http: &http.Client{
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConns:          32,
			MaxIdleConnsPerHost:   8,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 120 * time.Second,
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Do sends one credentialed POST. The caller owns the returned response body,
// including on non-2xx statuses.
func (c *Client) Do(ctx context.Context, endpoint, token string, payload []byte, stream bool) (*http.Response, error) {
	return c.DoForAccount(ctx, endpoint, token, "", payload, stream)
}

// DoForAccount retains the selected OAuth workspace across BPS operations.
// The caller obtains accountID from its stored grant, never from input text.
func (c *Client) DoForAccount(ctx context.Context, endpoint, token, accountID string, payload []byte, stream bool) (*http.Response, error) {
	return c.doForAccount(ctx, endpoint, token, accountID, payload, stream, false)
}

// DoNativeBPS selects the Excel tool protocol used by the original BPS plugin.
// These fixed protocol selectors do not replace OAuth or account authorization.
func (c *Client) DoNativeBPS(ctx context.Context, token, accountID string, payload []byte) (*http.Response, error) {
	return c.doForAccount(ctx, "https://bps.openai.com/basispoints/api/responses", token, accountID, payload, true, true)
}

func (c *Client) doForAccount(ctx context.Context, endpoint, token, accountID string, payload []byte, stream, nativeBPS bool) (*http.Response, error) {
	if len(payload) == 0 {
		return nil, ErrEmptyRequest
	}
	if accountID != "" && !validBPSIdentifier(accountID, 256) {
		return nil, ErrInvalidToken
	}
	request, err := newRequest(ctx, http.MethodPost, endpoint, token, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	// The transport must never re-send the body after a write might have occurred.
	request.GetBody = nil
	request.Header.Set("Content-Type", "application/json")
	if accountID != "" {
		request.Header.Set("ChatGPT-Account-ID", accountID)
	}
	if nativeBPS {
		request.Header.Set("X-Basispoints-Auth-Mode", "chatgpt")
		request.Header.Set("X-OpenAI-Internal-Basispoints-Client-Agent-Profile", "excel")
		request.Header.Set("X-OpenAI-Internal-Basispoints-Client-Product", "basispoints-excel-plugin")
	}
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	return c.http.Do(request)
}

// Models reads the models from the operator-configured catalog endpoint.
func (c *Client) Models(ctx context.Context, endpoint, token string) ([]string, error) {
	request, err := newRequest(ctx, http.MethodGet, endpoint, token, nil)
	if err != nil {
		return nil, err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, ErrModelCatalog
	}
	data, err := ReadJSON(response.Body)
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &catalog); err != nil || catalog.Data == nil {
		return nil, ErrModelCatalog
	}
	models := make([]string, 0, len(catalog.Data))
	for _, entry := range catalog.Data {
		if entry.ID == "" {
			return nil, ErrModelCatalog
		}
		models = append(models, entry.ID)
	}
	return models, nil
}

func newRequest(ctx context.Context, method, endpoint, token string, body io.Reader) (*http.Request, error) {
	if err := ValidateEndpoint(endpoint); err != nil {
		return nil, err
	}
	if token == "" || strings.ContainsFunc(token, func(r rune) bool { return r <= ' ' || r >= 127 }) {
		return nil, ErrInvalidToken
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Accept-Encoding", "identity")
	return request, nil
}

// ValidateEndpoint permits cleartext only to an explicitly specified loopback
// IP for local integration. Userinfo, query parameters and fragments are rejected.
func ValidateEndpoint(endpoint string) error {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Hostname() == "" || parsed.User != nil || parsed.Opaque != "" ||
		parsed.ForceQuery || parsed.RawQuery != "" || strings.Contains(endpoint, "#") {
		return ErrInvalidEndpoint
	}
	if parsed.Scheme == "https" || parsed.Scheme == "http" && net.ParseIP(parsed.Hostname()).IsLoopback() {
		return nil
	}
	return ErrInvalidEndpoint
}

func (c *Client) CloseIdleConnections() {
	c.http.CloseIdleConnections()
}
