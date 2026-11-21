package upstream

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestBPSModelCatalogKeepsEnabledAccountModels(t *testing.T) {
	raw := []byte(`{
		"allowed":true,
		"model_catalog":{
			"restricted_models":["restricted"],
			"models":[
				{"id":"first","label":" First model ","efforts":[{"value":"low"},{"value":"ultra"},{"value":"low"},{"value":"unknown"}]},
				{"id":"second","model_picker_enabled":true,"policy":{"state":"enabled"}},
				{"id":"restricted"},
				{"id":"picker-hidden","model_picker_enabled":false},
				{"id":"policy-disabled","policy":{"state":"disabled"}},
				{"id":"policy-empty","policy":{"state":"unconfigured"}},
				{"id":"already-excel"},
				{"id":"first","label":"duplicate"},
				{"id":"bad model"},
				{"id":"third","label":"含有中文的模型"},
				null,
				"not a model object"
			]
		}
	}`)
	got, err := parseBPSModels(raw)
	want := []BPSModel{
		{ID: "first", Label: "First model", Efforts: []string{"low", "ultra"}},
		{ID: "second", Label: "second"},
		{ID: "third", Label: "含有中文的模型"},
	}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("catalog = %#v, error %v, want %#v", got, err, want)
	}
}

func TestBPSModelCatalogRequiresAccessDecisionAndCatalog(t *testing.T) {
	for _, input := range []string{
		`{}`, `null`, `{"allowed":true}`, `{"allowed":true,"model_catalog":{}}`,
		`{"allowed":true,"model_catalog":{"models":null}}`,
		`{"allowed":"true","model_catalog":{"models":[]}}`,
		`{"data":[{"id":"not-the-BPS-catalog"}]}`,
	} {
		if _, err := parseBPSModels([]byte(input)); !errors.Is(err, ErrModelCatalog) {
			t.Errorf("unexpected result for %s: %v", input, err)
		}
	}
	for _, input := range []string{
		`{"allowed":false}`,
		`{"allowed":true,"model_catalog":{"models":[]}}`,
		`{"allowed":true,"model_catalog":{"restricted_models":["limited"],"models":[{"id":"limited"}]}}`,
	} {
		got, err := parseBPSModels([]byte(input))
		if err != nil || got == nil || len(got) != 0 {
			t.Errorf("empty catalog %s returned %#v, error %v", input, got, err)
		}
	}
}

type bpsModelTestTransport func(*http.Request) (*http.Response, error)

func (f bpsModelTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestBPSModelsRequestUsesExactAccessEndpoint(t *testing.T) {
	var requests int
	redirect := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		requests++
		if request.Method != http.MethodGet || request.URL.Path != "/basispoints/api/responses/access" ||
			request.URL.RawQuery != "include_models=true" ||
			request.Header.Get("Authorization") != "Bearer fixture-token" ||
			request.Header.Get("ChatGPT-Account-ID") != "fixture-account" {
			t.Error("incorrect BPS model discovery request")
		}
		if redirect {
			w.Header().Set("Location", "https://redirect.example.test/collect")
			w.WriteHeader(http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, `{"allowed":true,"model_catalog":{"models":[{"id":"fixture","label":"Fixture"}],"restricted_models":[]}}`)
	}))
	defer server.Close()
	target, _ := url.Parse(server.URL)
	client := New()
	transport := client.http.Transport.(*http.Transport)
	defer transport.CloseIdleConnections()
	client.http.Transport = bpsModelTestTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Scheme != "https" || request.URL.Host != "bps.openai.com" {
			t.Fatal("credential was routed outside the fixed BPS origin")
		}
		request = request.Clone(request.Context())
		request.URL.Scheme = target.Scheme
		request.URL.Host = target.Host
		return transport.RoundTrip(request)
	})
	got, err := client.BPSModels(context.Background(), "fixture-token", "fixture-account")
	if err != nil || len(got) != 1 || got[0].ID != "fixture" {
		t.Fatalf("BPS catalog = %#v, error %v", got, err)
	}
	redirect = true
	_, err = client.BPSModels(context.Background(), "fixture-token", "fixture-account")
	if !errors.Is(err, ErrModelCatalog) || requests != 2 {
		t.Fatalf("redirect should have been rejected, count %d, error %v", requests, err)
	}
	for _, account := range []string{"invalid\r\nheader", strings.Repeat("x", 257)} {
		if _, err := client.BPSModels(context.Background(), "fixture-token", account); err == nil || requests != 2 {
			t.Fatal("invalid account header caused a request")
		}
	}
}

func TestBPSModelsBoundsResponse(t *testing.T) {
	client := New()
	defer client.CloseIdleConnections()
	client.http.Transport = bpsModelTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader(strings.Repeat(" ", maxBPSCatalogBytes+1))),
		}, nil
	})
	if _, err := client.BPSModels(context.Background(), "fixture", ""); !errors.Is(err, ErrModelCatalog) {
		t.Fatalf("oversized response returned %v", err)
	}
}
