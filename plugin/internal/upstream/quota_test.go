package upstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientQuotaNormalizesOnlyKnownReadOnlyFields(t *testing.T) {
	var lookup atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lookup.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/quota" ||
			r.Header.Get("Authorization") != "Bearer test" || r.Header.Get("Accept") != "application/json" {
			t.Error("incorrect quota request")
		}
		payload, err := io.ReadAll(r.Body)
		if err != nil || len(payload) != 0 {
			t.Error("expected empty quota GET body")
		}
		_, _ = io.WriteString(w, `{
			"subscription":{"plan":"fixture","tier_name":"Test tier","tier_id":"fixture-id","private":"discard"},
			"summary":[{"key":"credit","label":"Credits","value":0,"format":"currency","currency":"USD","private":"discard"}],
			"server_time_offset_ms":0,
			"groups":[{"display_name":"Requests","buckets":[{"window":"daily","remaining_fraction":0,"reset_time":"2030-01-01T00:00:00Z","private":"discard"}]}],
			"private":"discard"
		}`)
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	result, err := client.Quota(context.Background(), server.URL+"/quota", "test")
	if err != nil {
		t.Fatal(err)
	}
	expected := `{"subscription":{"plan":"fixture","tierName":"Test tier","tierId":"fixture-id"},"summary":[{"key":"credit","label":"Credits","value":0,"format":"currency","currency":"USD"}],"serverTimeOffsetMs":0,"groups":[{"displayName":"Requests","buckets":[{"window":"daily","remainingFraction":0,"resetTime":"2030-01-01T00:00:00Z"}]}]}`
	if lookup.Load() != 1 || !bytes.Equal(result, []byte(expected)) {
		t.Fatalf("incorrect normalized quota result: %s", result)
	}
}

func TestClientQuotaRejectsMalformedOrUnsupportedQuota(t *testing.T) {
	var redirected atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusFound)
	})
	mux.HandleFunc("/redirected", func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	})
	responses := map[string]string{
		"/unknown":            `{"unexpected":1}`,
		"/null":               `null`,
		"/missing-fraction":   `{"groups":[{"buckets":[{"window":"daily"}]}]}`,
		"/invalid-fraction":   `{"groups":[{"buckets":[{"remainingFraction":1.1}]}]}`,
		"/invalid-time":       `{"groups":[{"buckets":[{"remainingFraction":0.5,"resetTime":"soon"}]}]}`,
		"/invalid-value":      `{"summary":[{"key":"credits","label":"Credits"}]}`,
		"/invalid-currency":   `{"summary":[{"key":"credits","label":"Credits","value":12,"format":"currency","currency":"invalid"}]}`,
		"/oversized-response": `{"subscription":{"plan":"fixture"}}` + strings.Repeat(" ", maxQuotaReplyBytes),
	}
	for path, body := range responses {
		mux.HandleFunc(path, func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, body)
		})
	}
	server := httptest.NewServer(mux)
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	for path := range responses {
		result, err := client.Quota(context.Background(), server.URL+path, "test")
		if result != nil || !errors.Is(err, ErrInvalidQuota) {
			t.Errorf("%s got %q and %v, wanted invalid quota", path, result, err)
		}
	}
	if _, err := client.Quota(context.Background(), server.URL+"/redirect", "test"); !errors.Is(err, ErrQuotaUnavailable) || redirected.Load() != 0 {
		t.Fatalf("quota request followed a redirect or returned wrong error: %v", err)
	}
}

func TestClientQuotaKeepsCanonicalZeroWhenAliasAlsoPresent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"groups":[{"buckets":[{"remainingFraction":0,"remaining_fraction":1}]}]}`)
	}))
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	quota, err := client.Quota(context.Background(), server.URL, "test")
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Groups []struct{ Buckets []map[string]float64 }
	}
	if err := json.Unmarshal(quota, &result); err != nil {
		t.Fatal(err)
	}
	bucket := result.Groups[0].Buckets[0]
	if fraction, present := bucket["remainingFraction"]; !present || fraction != 0 || len(bucket) != 1 {
		t.Fatalf("canonical zero quota was not preserved: %v", bucket)
	}
}
