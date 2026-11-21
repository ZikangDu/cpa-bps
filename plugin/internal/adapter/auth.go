package adapter

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
)

type credential struct {
	Type           string                     `json:"type"`
	AuthID         string                     `json:"auth_id,omitempty"`
	Email          string                     `json:"email,omitempty"`
	TokenEnv       string                     `json:"token_env,omitempty"`
	UpstreamOrigin string                     `json:"upstream_origin,omitempty"`
	Label          string                     `json:"label,omitempty"`
	Disabled       bool                       `json:"disabled,omitempty"`
	Grant          *managedGrant              `json:"oauth_grant,omitempty"`
	Extra          map[string]json.RawMessage `json:"-"`
}

// Preserve provider metadata across refresh without retaining a second stale
// token source. Credential files are private; only display/routing fields are
// separately exposed to the host below.
func (a *credential) UnmarshalJSON(raw []byte) error {
	type known credential
	var value known
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	var extra map[string]json.RawMessage
	if err := json.Unmarshal(raw, &extra); err != nil {
		return err
	}
	for _, key := range []string{"type", "auth_id", "email", "token_env", "upstream_origin", "label", "disabled", "oauth_grant", "access_token", "refresh_token", "id_token", "expires_at", "expired", "scope", "client_id", "token_url"} {
		delete(extra, key)
	}
	*a = credential(value)
	a.Extra = extra
	return nil
}
func (a credential) MarshalJSON() ([]byte, error) {
	type known credential
	raw, err := json.Marshal(known(a))
	if err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	for key, value := range a.Extra {
		if _, exists := result[key]; !exists {
			result[key] = value
		}
	}
	return json.Marshal(result)
}

type managedGrant struct {
	ClientID string `json:"client_id"`
	TokenURL string `json:"token_url"`
	oauthflow.Tokens
}

func credentialFromStorage(raw []byte) (credential, bool) {
	var auth credential
	if json.Unmarshal(raw, &auth) != nil || auth.Type != provider {
		return credential{}, false
	}
	if len(auth.AuthID) > 1024 || strings.ContainsFunc(auth.AuthID, func(r rune) bool { return r <= ' ' || r == 127 }) {
		return credential{}, false
	}
	if auth.Grant == nil && !validTokenEnv(auth.TokenEnv) {
		return credential{}, false
	}
	if auth.Grant != nil && (auth.TokenEnv != "" || auth.UpstreamOrigin == "" ||
		auth.Grant.ClientID == "" || auth.Grant.TokenURL == "" || auth.Grant.AccessToken == "" ||
		auth.Grant.ExpiresAt.IsZero()) {
		return credential{}, false
	}
	if auth.UpstreamOrigin != "" {
		if upstream.ValidateEndpoint(auth.UpstreamOrigin) != nil {
			return credential{}, false
		}
		origin, _ := url.Parse(auth.UpstreamOrigin)
		if origin.EscapedPath() != "" && origin.EscapedPath() != "/" {
			return credential{}, false
		}
	}
	return auth, true
}

func (a credential) allows(endpoint string) bool {
	return a.UpstreamOrigin == "" || sameOrigin(a.UpstreamOrigin, endpoint)
}

func validTokenEnv(name string) bool {
	if !strings.HasPrefix(name, "CPA_BPS_") || len(name) == len("CPA_BPS_") {
		return false
	}
	return !strings.ContainsFunc(name, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_')
	})
}

func resolveToken(raw []byte, endpoint string, oauth *oauthflow.Config) (string, *rpcError) {
	auth, ok := credentialFromStorage(raw)
	if !ok || auth.Disabled {
		return "", failure(http.StatusUnauthorized, "bps_credential_unavailable", "a configured BPS credential is required")
	}
	if !auth.allows(endpoint) {
		return "", failure(http.StatusForbidden, "bps_credential_origin_mismatch", "the credential is scoped to a different Responses origin")
	}
	if auth.Grant != nil {
		if !auth.Grant.matches(oauth) {
			return "", failure(http.StatusUnauthorized, "bps_oauth_client_mismatch", "reconnect this credential using the configured OAuth client")
		}
		if !time.Now().Before(auth.Grant.ExpiresAt) {
			return "", failure(http.StatusUnauthorized, "bps_oauth_expired", "this OAuth access token has expired")
		}
		return auth.Grant.AccessToken, nil
	}
	token := strings.TrimSpace(os.Getenv(auth.TokenEnv))
	if token == "" {
		return "", failure(http.StatusUnauthorized, "bps_credential_unavailable", "the configured BPS credential is unavailable")
	}
	return token, nil
}

func (g *managedGrant) matches(cfg *oauthflow.Config) bool {
	return cfg != nil && g.ClientID == cfg.ClientID && g.TokenURL == cfg.TokenURL
}

func parseAuth(raw json.RawMessage) (any, error) {
	var request struct {
		Provider string
		FileName string
		RawJSON  []byte
	}
	if err := decode(raw, &request); err != nil {
		return nil, err
	}
	auth, ok := credentialFromStorage(request.RawJSON)
	if !ok || request.Provider != provider {
		return map[string]any{"Handled": false}, nil
	}
	if auth.Label == "" {
		auth.Label = request.FileName
	}
	data, err := auth.data(auth.AuthID, "", nil)
	if err != nil {
		return nil, err
	}
	return map[string]any{"Handled": true, "Auth": data}, nil
}

func (auth credential) data(id, filename string, metadata map[string]any) (map[string]any, error) {
	storage, err := json.Marshal(auth)
	if err != nil {
		return nil, err
	}
	if metadata == nil {
		metadata = make(map[string]any)
	}
	for _, key := range []string{"priority", "weight", "note", "proxy_url", "prefix", "chatgpt_account_id", "user_id", "oauth_model_aliases", "excluded_models"} {
		if _, present := metadata[key]; present {
			continue
		}
		if raw := auth.Extra[key]; len(raw) > 0 {
			var value any
			if json.Unmarshal(raw, &value) == nil {
				metadata[key] = value
			}
		}
	}
	// CPA merges metadata over provider storage when saving the credential.
	delete(metadata, "oauth_grant")
	metadata["type"] = provider
	if auth.TokenEnv != "" {
		metadata["token_env"] = auth.TokenEnv
	}
	if auth.UpstreamOrigin != "" {
		metadata["upstream_origin"] = auth.UpstreamOrigin
	}
	data := map[string]any{
		"Provider":    provider,
		"ID":          id,
		"FileName":    filename,
		"Label":       auth.Label,
		"Disabled":    auth.Disabled,
		"StorageJSON": storage,
		"Metadata":    metadata,
	}
	if auth.Grant != nil {
		if auth.Grant.ClientID == oauthflow.BPSClientID {
			email := upstream.BPSEmail(auth.Grant.IDToken)
			if email == "" {
				email = upstream.BPSEmail(auth.Grant.AccessToken)
			}
			if email == "" {
				email = upstream.BPSDisplayEmail(auth.Email)
			}
			attributes := map[string]string{}
			if email != "" {
				metadata["email"] = email
				attributes["email"] = email
			} else {
				delete(metadata, "email")
			}
			_, plan := upstream.BPSProfile(auth.Grant.AccessToken)
			if plan == "" {
				_, plan = upstream.BPSProfile(auth.Grant.IDToken)
			}
			if plan != "" {
				metadata["plan_type"] = plan
				attributes["plan_type"] = plan
			}
			if len(attributes) > 0 {
				data["Attributes"] = attributes
			}
		}
		metadata["expires_at"] = auth.Grant.ExpiresAt.Format(time.RFC3339)
		next, interval := refreshSchedule(auth.Grant.Tokens)
		data["NextRefreshAfter"] = next
		if interval > 0 {
			metadata["refresh_interval_seconds"] = interval
		} else {
			delete(metadata, "refresh_interval_seconds")
		}
	}
	return data, nil
}
