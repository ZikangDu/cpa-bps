package upstream

import "net/http"

// NewWithTransport permits an explicitly trusted transport, such as an in-memory
// test dispatcher. It preserves the no-redirect policy; nil uses normal New.
// Production configuration never supplies this transport implicitly.
func NewWithTransport(transport http.RoundTripper) *Client {
	if transport == nil {
		return New()
	}
	return &Client{http: &http.Client{
		Transport: transport,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}
