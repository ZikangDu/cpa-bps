package adapter

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"cpa-bps-reconstruction/internal/oauthflow"
	"cpa-bps-reconstruction/internal/upstream"
)

func quotaFixture(t *testing.T) (*Runtime, credential, []byte) {
	t.Helper()
	claims := `{"https://api.openai.com/auth":{"chatgpt_account_id":"fixture-account","chatgpt_plan_type":"team"}}`
	token := "e30." + base64.RawURLEncoding.EncodeToString([]byte(claims)) + ".fixture"
	auth := credential{Type: provider, UpstreamOrigin: "https://bps.openai.com", Grant: &managedGrant{
		ClientID: oauthflow.BPSClientID, TokenURL: oauthflow.BPSConfig().TokenURL,
		Tokens: oauthflow.Tokens{AccessToken: token, ExpiresAt: time.Now().Add(time.Hour)},
	}}
	raw, _ := json.Marshal(auth)
	r := New(func(method string, payload any, response any) error {
		req := payload.(map[string]any)
		if req["host_callback_id"] != "fixture-context" {
			t.Fatal("host callback context was lost")
		}
		switch method {
		case "host.auth.list":
			return json.Unmarshal([]byte(`{"files":[{"id":"fixture-id","auth_index":"fixture-index","provider":"cpa-bps"}]}`), response)
		case "host.auth.get":
			if req["auth_index"] != "fixture-index" {
				t.Fatal("unrelated credential was requested")
			}
			envelope, _ := json.Marshal(map[string]json.RawMessage{"json": raw})
			return json.Unmarshal(envelope, response)
		default:
			return errors.New("unexpected host method")
		}
	})
	cfg, err := parseConfig([]byte("enabled: true\noauth_preset: bps\nmodels: [fixture]\n"))
	if err != nil {
		t.Fatal(err)
	}
	r.cfg = cfg
	t.Cleanup(r.Close)
	return r, auth, raw
}

func TestBPSQuotaManagementUsesPanelContract(t *testing.T) {
	r, auth, _ := quotaFixture(t)
	keyRaw, _ := json.Marshal([]string{"fixture-id", auth.Grant.AccessToken, "fixture-account"})
	key := sha256.Sum256(keyRaw)
	done := make(chan struct{})
	close(done)
	r.bpsQuota.entries[key] = &bpsQuotaRead{done: done, expires: time.Now().Add(time.Minute),
		result: upstream.BPSQuotaResult{Quota: json.RawMessage(`{"subscription":{"plan":"team"}}`),
			ObservedAt: time.Now().UTC().Format(time.RFC3339), Source: "active_usage"}}
	reply, err := r.manageBPSQuota(managementRequest{
		Method: http.MethodGet, Query: url.Values{"auth_index": {"fixture-index"}}, HostCallbackID: "fixture-context",
	})
	if err != nil {
		t.Fatal(err)
	}
	response := reply.(map[string]any)
	if response["StatusCode"] != http.StatusOK {
		t.Fatal("quota management request failed")
	}
	var result upstream.BPSQuotaResult
	if json.Unmarshal(response["Body"].([]byte), &result) != nil || result.Source != "active_usage" ||
		result.ObservedAt == "" || result.SupportsReset {
		t.Fatal("quota management omitted required panel fields")
	}
}

func TestBPSQuotaFallbackNeverInventsObservationOrRemaining(t *testing.T) {
	r, auth, raw := quotaFixture(t)
	keyRaw, _ := json.Marshal([]string{"fixture-id", auth.Grant.AccessToken, "fixture-account"})
	key := sha256.Sum256(keyRaw)
	done := make(chan struct{})
	close(done)
	r.bpsQuota.entries[key] = &bpsQuotaRead{done: done, expires: time.Now().Add(time.Minute), err: upstream.ErrQuotaUnavailable}
	got, err := r.loadBPSQuota(context.Background(), r.cfg, "fixture-id", raw, false)
	if err != nil || got.Source != "token_claims" || got.Warning != "active_quota_unavailable" ||
		got.ObservedAt != "" || got.ResetCardsAvailable != nil || string(got.Quota) != `{"subscription":{"plan":"team"}}` {
		t.Fatalf("invalid hint fallback: %+v, %v", got, err)
	}
}

func TestBPSQuotaCacheCoalescesAndKeepsRevisionsSeparate(t *testing.T) {
	cache := newBPSQuotaCache()
	var fetches atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	key := sha256.Sum256([]byte("first-token"))
	fetch := func() (upstream.BPSQuotaResult, error) {
		if fetches.Add(1) == 1 {
			close(started)
		}
		<-release
		return upstream.BPSQuotaResult{Quota: json.RawMessage(`{"subscription":{"plan":"team"}}`), Source: "active_usage"}, nil
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_, err := cache.load(context.Background(), key, false, fetch)
			if err != nil {
				t.Error(err)
			}
		})
	}
	<-started
	close(release)
	wg.Wait()
	if fetches.Load() != 1 {
		t.Fatal("concurrent quota reads were not coalesced")
	}
	next := sha256.Sum256([]byte("rotated-token"))
	_, err := cache.load(context.Background(), next, false, fetch)
	if err != nil || fetches.Load() != 2 {
		t.Fatal("rotated credential reused an older credential quota cache")
	}
}

func TestBPSQuotaManagementErrorsCarryHTTPStatus(t *testing.T) {
	r, _, _ := quotaFixture(t)
	for _, req := range []managementRequest{
		{Method: http.MethodGet},
		{Method: http.MethodGet, Query: url.Values{"auth_index": {"a", "b"}}},
		{Method: http.MethodGet, Query: url.Values{"auth_index": {"fixture-index"}, "refresh": {"bad"}}},
	} {
		reply, err := r.manageBPSQuota(req)
		if err != nil || reply.(map[string]any)["StatusCode"] != http.StatusBadRequest {
			t.Fatal("invalid management request did not receive a structured HTTP 400")
		}
	}
	reply, err := r.manageBPSQuota(managementRequest{Method: http.MethodGet, Query: url.Values{"auth_index": {"other"}}, HostCallbackID: "fixture-context"})
	if err != nil || reply.(map[string]any)["StatusCode"] != http.StatusNotFound {
		t.Fatal("missing credential was not classified as HTTP 404")
	}
}
