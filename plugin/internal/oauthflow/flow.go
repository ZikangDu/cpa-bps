package oauthflow

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	loginLifetime = 10 * time.Minute
	maxSessions   = 256
	nonceBytes    = 32
	nonceLength   = 43
)

var (
	ErrState               = errors.New("oauth state is no longer pending")
	ErrInvalidCallback     = errors.New("oauth callback did not match the configured redirect and state")
	ErrAuthorizationDenied = errors.New("oauth authorization did not complete")
	ErrClosed              = errors.New("oauth flow is closed")
	ErrBusy                = errors.New("too many pending oauth authorizations")
)

type Login struct {
	URL       string
	State     string
	ExpiresAt time.Time
}

type session struct {
	config    Config
	verifier  string
	expiresAt time.Time
}

// Flow tracks short-lived, one-time authorization attempts. The caller owns
// durable token storage and serialization of refreshes for a stored account.
type Flow struct {
	mu       sync.Mutex
	sessions map[string]session
	closed   bool
	client   *http.Client
	now      func() time.Time
}

func New() *Flow {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &Flow{
		sessions: make(map[string]session),
		now:      time.Now,
		client: &http.Client{
			Timeout: 15 * time.Second,
			Transport: &http.Transport{
				DialContext:       dialer.DialContext,
				DisableKeepAlives: true,
			},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (f *Flow) Begin(cfg Config) (Login, error) {
	if err := cfg.Validate(); err != nil {
		return Login{}, err
	}
	if _, err := clientSecret(cfg); err != nil {
		return Login{}, err
	}
	state, err := nonce()
	if err != nil {
		return Login{}, err
	}
	verifier, err := nonce()
	if err != nil {
		return Login{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return Login{}, ErrClosed
	}
	now := f.now()
	for state, attempt := range f.sessions {
		if !now.Before(attempt.expiresAt) {
			delete(f.sessions, state)
		}
	}
	if len(f.sessions) >= maxSessions {
		return Login{}, ErrBusy
	}
	expiresAt := now.Add(loginLifetime).UTC()
	cfg.Scopes = slices.Clone(cfg.Scopes)
	f.sessions[state] = session{config: cfg, verifier: verifier, expiresAt: expiresAt}

	auth, _ := url.Parse(cfg.AuthorizationURL)
	q := auth.Query()
	q.Set("response_type", "code")
	q.Set("client_id", cfg.ClientID)
	q.Set("redirect_uri", cfg.RedirectURI)
	q.Set("state", state)
	if len(cfg.Scopes) > 0 {
		q.Set("scope", strings.Join(cfg.Scopes, " "))
	}
	challenge := sha256.Sum256([]byte(verifier))
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(challenge[:]))
	q.Set("code_challenge_method", "S256")
	auth.RawQuery = q.Encode()
	return Login{URL: auth.String(), State: state, ExpiresAt: expiresAt}, nil
}

// Pending reports whether the state remains usable with this exact configuration.
func (f *Flow) Pending(cfg Config, state string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.session(cfg, state)
	return ok
}

func (f *Flow) session(cfg Config, state string) (session, bool) {
	attempt, ok := f.sessions[state]
	if f.closed || !ok {
		return session{}, false
	}
	if !f.now().Before(attempt.expiresAt) {
		delete(f.sessions, state)
		return session{}, false
	}
	return attempt, sameConfig(cfg, attempt.config)
}

// Complete consumes a matching callback exactly once, including provider
// denials and ambiguous token exchanges whose code may already be spent.
func (f *Flow) Complete(ctx context.Context, cfg Config, state, callbackURL string) (Tokens, error) {
	f.mu.Lock()
	attempt, ok := f.session(cfg, state)
	if !ok {
		f.mu.Unlock()
		return Tokens{}, ErrState
	}
	code, denied, err := parseCallback(attempt.config.RedirectURI, state, callbackURL)
	if err != nil {
		f.mu.Unlock()
		return Tokens{}, err
	}
	delete(f.sessions, state)
	f.mu.Unlock()
	if denied {
		return Tokens{}, ErrAuthorizationDenied
	}

	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {attempt.config.RedirectURI},
		"code_verifier": {attempt.verifier},
	}
	return f.exchange(ctx, attempt.config, form)
}

func parseCallback(redirectURI, state, raw string) (code string, denied bool, err error) {
	if len(raw) > 16<<10 {
		return "", false, ErrInvalidCallback
	}
	callback, ok := parseURL(raw)
	redirect, _ := url.Parse(redirectURI)
	if !ok || callback.Scheme != redirect.Scheme ||
		!strings.EqualFold(callback.Host, redirect.Host) ||
		callback.EscapedPath() != redirect.EscapedPath() {
		return "", false, ErrInvalidCallback
	}
	query, err := url.ParseQuery(callback.RawQuery)
	if err != nil {
		return "", false, ErrInvalidCallback
	}
	for name, values := range redirect.Query() {
		if !slices.Equal(values, query[name]) {
			return "", false, ErrInvalidCallback
		}
	}
	states, codes, providerErrors := query["state"], query["code"], query["error"]
	if len(states) != 1 || len(states[0]) != nonceLength ||
		subtle.ConstantTimeCompare([]byte(state), []byte(states[0])) != 1 {
		return "", false, ErrInvalidCallback
	}
	if len(codes) == 0 && len(providerErrors) == 1 && providerErrors[0] != "" {
		return "", true, nil
	}
	if len(providerErrors) != 0 || len(codes) != 1 || codes[0] == "" ||
		len(codes[0]) > 8192 || !printable(codes[0]) {
		return "", false, ErrInvalidCallback
	}
	return codes[0], false, nil
}

func nonce() (string, error) {
	var bytes [nonceBytes]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return "", errors.New("oauth secure random value is unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(bytes[:]), nil
}

// Close invalidates pending sessions. The caller cancels running exchanges
// through the contexts it passed to Complete or Refresh.
func (f *Flow) Close() {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.closed = true
	clear(f.sessions)
	f.mu.Unlock()
	if f.client != nil {
		f.client.CloseIdleConnections()
	}
}

// An explicit proxy keeps the token exchange on the same route as the browser.
func (f *Flow) tokenClient(proxyURL string) (*http.Client, func()) {
	if proxyURL == "" {
		return f.client, func() {}
	}
	proxy, _ := url.Parse(proxyURL) // Config.Validate runs before exchanging tokens.
	transport := f.client.Transport.(*http.Transport).Clone()
	transport.Proxy = http.ProxyURL(proxy)
	client := *f.client
	client.Transport = transport
	return &client, transport.CloseIdleConnections
}
