package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
)

const oauthWindow = 20 * time.Second

type refreshAttempt struct {
	key    [32]byte
	next   [32]byte
	done   chan struct{}
	tokens oauthflow.Tokens
	err    *rpcError
}

func (r *Runtime) startLogin(raw json.RawMessage) (any, error) {
	var request struct{ Provider string }
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	if request.Provider != provider {
		return nil, failure(http.StatusBadRequest, "bps_oauth_provider_mismatch", "the requested provider is unavailable")
	}
	cfg, ok := r.begin()
	if !ok {
		return nil, failure(http.StatusServiceUnavailable, "bps_plugin_unavailable", "BPS plugin is unavailable")
	}
	defer r.tasks.Done()
	if cfg.OAuth == nil {
		return nil, failure(http.StatusServiceUnavailable, "bps_oauth_not_configured", "configure an OAuth issuer and registered client first")
	}
	login, err := r.oauth.Begin(*cfg.OAuth)
	if err != nil {
		return nil, failure(http.StatusServiceUnavailable, "bps_oauth_start_failed", "unable to start an OAuth session")
	}
	return map[string]any{"Provider": provider, "URL": login.URL, "State": login.State, "ExpiresAt": login.ExpiresAt}, nil
}

type callbackRecord struct {
	Code  string `json:"code"`
	State string `json:"state"`
	Error string `json:"error"`
}

func (r *Runtime) pollLogin(raw json.RawMessage) (any, error) {
	var request struct {
		Provider, State string
		Host            struct{ AuthDir string }
	}
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	status := func(kind, message string) (any, error) {
		return map[string]string{"Status": kind, "Message": message}, nil
	}
	cfg, ok := r.begin()
	if !ok {
		return status("error", "BPS plugin is unavailable")
	}
	defer r.tasks.Done()
	if request.Provider != provider || cfg.OAuth == nil || request.Host.AuthDir == "" || !safeOAuthState(request.State) {
		return status("error", "OAuth session is unavailable")
	}
	if _, busy := r.polls.LoadOrStore(request.State, struct{}{}); busy {
		return status("pending", "")
	}
	defer r.polls.Delete(request.State)
	if !r.oauth.Pending(*cfg.OAuth, request.State) {
		return status("error", "OAuth session expired or its configuration changed, start again")
	}

	record, err := takeCPACallback(request.Host.AuthDir, request.State)
	if errors.Is(err, os.ErrNotExist) {
		return status("pending", "")
	}
	if err != nil || record.State != request.State || (record.Error == "" && record.Code == "") {
		return status("error", "OAuth authorization did not complete")
	}
	callback, _ := url.Parse(cfg.OAuth.RedirectURI)
	query := callback.Query()
	query.Set("state", request.State)
	if record.Error != "" {
		query.Set("error", "access_denied")
	} else {
		query.Set("code", record.Code)
	}
	callback.RawQuery = query.Encode()
	ctx, cancel := context.WithTimeout(r.ctx, oauthWindow)
	defer cancel()
	tokens, err := r.oauth.Complete(ctx, *cfg.OAuth, request.State, callback.String())
	if errors.Is(err, oauthflow.ErrAuthorizationDenied) {
		return status("error", "OAuth authorization was denied")
	}
	if err != nil {
		return status("error", "OAuth code exchange failed or the session expired")
	}
	idHash := sha256.Sum256([]byte(request.State))
	id := "cpa-bps-" + hex.EncodeToString(idHash[:12]) + ".json"
	auth := credential{
		Type: provider, UpstreamOrigin: endpointOrigin(cfg.ResponsesURL), Label: "BPS OAuth",
		Grant: &managedGrant{ClientID: cfg.OAuth.ClientID, TokenURL: cfg.OAuth.TokenURL, Tokens: tokens},
	}
	data, err := auth.data(id, id, nil)
	if err != nil {
		return status("error", "could not prepare the OAuth credential")
	}
	return map[string]any{"Status": "success", "Auth": data}, nil
}

func safeOAuthState(state string) bool {
	if len(state) < 32 || len(state) > 128 {
		return false
	}
	for _, char := range state {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-') {
			return false
		}
	}
	return true
}

func takeCPACallback(authDir, state string) (callbackRecord, error) {
	var record callbackRecord
	root, err := os.OpenRoot(authDir)
	if err != nil {
		return record, err
	}
	defer root.Close()
	name := ".oauth-" + provider + "-" + state + ".oauth"
	file, err := root.Open(name)
	if err != nil {
		return record, err
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, 16<<10+1))
	file.Close()
	if removeErr := root.Remove(name); removeErr != nil {
		return record, removeErr
	}
	if readErr != nil {
		return record, readErr
	}
	if len(raw) > 16<<10 || json.Unmarshal(raw, &record) != nil {
		return record, errors.New("invalid host OAuth callback")
	}
	return record, nil
}

func refreshSchedule(tokens oauthflow.Tokens) (time.Time, int) {
	if tokens.RefreshToken == "" {
		return tokens.ExpiresAt, 0
	}
	lead := max(time.Second, min(time.Minute, time.Until(tokens.ExpiresAt)/4)) / time.Second
	next := tokens.ExpiresAt.Add(-lead * time.Second)
	if tokens.EarliestRefreshAt.After(next) {
		next = tokens.EarliestRefreshAt
	}
	return next, int(lead)
}

func (r *Runtime) refreshAuth(raw json.RawMessage) (any, error) {
	var request struct {
		AuthID, AuthProvider string
		StorageJSON          []byte
		Metadata             map[string]any
	}
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	auth, valid := credentialFromStorage(request.StorageJSON)
	if request.AuthProvider != provider || request.AuthID == "" || !valid || auth.Disabled {
		return nil, failure(http.StatusUnauthorized, "bps_credential_unavailable", "a configured credential is required")
	}
	cfg, ok := r.begin()
	if !ok {
		return nil, failure(http.StatusServiceUnavailable, "bps_plugin_unavailable", "BPS plugin is unavailable")
	}
	defer r.tasks.Done()
	if !auth.allows(cfg.ResponsesURL) {
		return nil, failure(http.StatusForbidden, "bps_credential_origin_mismatch", "the credential is scoped to a different Responses origin")
	}
	if auth.Grant != nil {
		if !auth.Grant.matches(cfg.OAuth) || auth.Grant.RefreshToken == "" {
			return nil, failure(http.StatusUnauthorized, "bps_oauth_reconnect_required", "reconnect this credential through its OAuth issuer")
		}
		if !time.Now().Before(auth.Grant.EarliestRefreshAt) {
			tokens, refreshErr := r.rotateGrant(request.AuthID, *cfg.OAuth, auth.Grant.Tokens)
			if refreshErr != nil {
				return nil, refreshErr
			}
			auth.Grant.Tokens = tokens
		}
	}
	data, err := auth.data(request.AuthID, "", request.Metadata)
	if err != nil {
		return nil, err
	}
	next := data["NextRefreshAfter"]
	if auth.Grant == nil {
		next = time.Now().Add(24 * time.Hour)
	}
	return map[string]any{"Auth": data, "NextRefreshAfter": next}, nil
}

func (r *Runtime) rotateGrant(id string, cfg oauthflow.Config, old oauthflow.Tokens) (oauthflow.Tokens, *rpcError) {
	key := grantRevision(cfg, old)
	r.mu.Lock()
	current := r.refreshes[id]
	if current != nil {
		if current.key == key {
			r.mu.Unlock()
			select {
			case <-current.done:
				return current.tokens, current.err
			case <-r.ctx.Done():
				return oauthflow.Tokens{}, failure(499, "connection_lifecycle", "OAuth refresh was canceled")
			}
		}
		select {
		case <-current.done:
			if current.err != nil || current.next != key {
				r.mu.Unlock()
				return oauthflow.Tokens{}, failure(http.StatusConflict, "bps_oauth_stale_grant", "reload the current OAuth credential before refreshing")
			}
		default:
			r.mu.Unlock()
			return oauthflow.Tokens{}, failure(http.StatusConflict, "bps_oauth_refresh_in_progress", "another OAuth refresh is in progress")
		}
	}
	attempt := &refreshAttempt{key: key, done: make(chan struct{})}
	r.refreshes[id] = attempt
	r.mu.Unlock()

	// A polling or management caller may retry, but one stored grant gets one exchange.
	ctx, cancel := context.WithTimeout(r.ctx, oauthWindow)
	defer cancel()
	tokens, err := r.oauth.Refresh(ctx, cfg, old)
	switch {
	case errors.Is(err, oauthflow.ErrInvalidGrant):
		attempt.err = failure(http.StatusUnauthorized, "bps_oauth_reconnect_required", "OAuth refresh did not complete, reconnect this credential")
	case err != nil:
		attempt.err = failure(http.StatusBadGateway, "bps_oauth_refresh_uncertain", "OAuth refresh outcome is uncertain, reconnect this credential")
	default:
		attempt.tokens = tokens
		attempt.next = grantRevision(cfg, tokens)
	}
	close(attempt.done)
	return attempt.tokens, attempt.err
}

func grantRevision(cfg oauthflow.Config, tokens oauthflow.Tokens) [32]byte {
	encoded, _ := json.Marshal([]string{
		cfg.TokenURL, cfg.ClientID, tokens.RefreshToken, tokens.AccessToken,
		tokens.ExpiresAt.UTC().Format(time.RFC3339Nano),
	})
	return sha256.Sum256(encoded)
}
