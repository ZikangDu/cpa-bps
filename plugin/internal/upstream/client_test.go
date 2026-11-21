package upstream

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
)

func TestClientRequests(t *testing.T) {
	var redirected atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test" ||
			r.Header.Get("Content-Type") != "application/json" ||
			r.Header.Get("Accept") != "text/event-stream" {
			t.Error("wrong POST headers")
		}
		if payload, err := io.ReadAll(r.Body); err != nil || string(payload) != `{"model":"test"}` {
			t.Error("wrong POST payload")
		}
		http.Redirect(w, r, "/redirected", http.StatusTemporaryRedirect)
	})
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer test" ||
			r.Header.Get("Accept") != "application/json" {
			t.Error("wrong model catalog headers")
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"first"},{"id":"second"}]}`)
	})
	mux.HandleFunc("/invalid", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"data":[{}]}`)
	})
	mux.HandleFunc("/catalog-redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/redirected", http.StatusFound)
	})
	mux.HandleFunc("/redirected", func(w http.ResponseWriter, _ *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	response, err := client.Do(context.Background(), server.URL+"/v1/responses", "test", []byte(`{"model":"test"}`), true)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("POST returned %d and made %d redirected requests", response.StatusCode, redirected.Load())
	}
	models, err := client.Models(context.Background(), server.URL+"/v1/models", "test")
	if err != nil || !slices.Equal(models, []string{"first", "second"}) {
		t.Fatalf("unexpected model catalog %v (error %v)", models, err)
	}
	if _, err := client.Models(context.Background(), server.URL+"/invalid", "test"); !errors.Is(err, ErrModelCatalog) {
		t.Fatalf("invalid catalog error = %v", err)
	}
	if _, err := client.Models(context.Background(), server.URL+"/catalog-redirect", "test"); !errors.Is(err, ErrModelCatalog) || redirected.Load() != 0 {
		t.Fatalf("GET returned error %v and made %d redirected requests", err, redirected.Load())
	}
}

func TestClientRequiresTrustedTLS(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	defer server.Close()
	client := New()
	defer client.CloseIdleConnections()
	response, err := client.Do(context.Background(), server.URL, "test", []byte("{}"), true)
	if err == nil {
		response.Body.Close()
		t.Fatal("accepted a server certificate outside the system trust store")
	}
}

func TestValidateEndpoint(t *testing.T) {
	for _, endpoint := range []string{
		"http://example.test/v1/responses", "http://localhost/v1/responses", "https://user@example.test/v1/responses",
		"https://example.test/v1/responses?", "https://example.test/v1/responses#", "/v1/responses",
	} {
		if !errors.Is(ValidateEndpoint(endpoint), ErrInvalidEndpoint) {
			t.Errorf("accepted invalid endpoint %q", endpoint)
		}
	}
	for _, endpoint := range []string{
		"https://example.test/v1/responses", "http://127.0.0.1:8000/v1/responses", "http://[::1]/v1/responses",
	} {
		if err := ValidateEndpoint(endpoint); err != nil {
			t.Errorf("endpoint %q failed validation: %v", endpoint, err)
		}
	}
}
