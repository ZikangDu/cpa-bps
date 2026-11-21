package upstream

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type accountTransport func(*http.Request) (*http.Response, error)

func (f accountTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestDoForAccountIsScopedAndNeverReplayable(t *testing.T) {
	calls := 0
	c := NewWithTransport(accountTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Header.Get("ChatGPT-Account-ID") != "fixture-account" || r.Header.Get("Authorization") != "Bearer token" || r.GetBody != nil {
			t.Fatal("lost workspace or replay protection")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	response, err := c.DoForAccount(context.Background(), "https://bps.openai.com/basispoints/api/responses", "token", "fixture-account", []byte(`{}`), true)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if _, err := c.DoForAccount(context.Background(), "https://bps.openai.com/basispoints/api/responses", "token", "bad\nheader", []byte(`{}`), false); err != ErrInvalidToken || calls != 1 {
		t.Fatal("invalid workspace reached transport")
	}
}

func TestNativeBPSUsesCapturedProtocolSelectorsOnly(t *testing.T) {
	c := NewWithTransport(accountTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://bps.openai.com/basispoints/api/responses" || r.Header.Get("X-Basispoints-Auth-Mode") != "chatgpt" || r.Header.Get("X-OpenAI-Internal-Basispoints-Client-Agent-Profile") != "excel" || r.Header.Get("X-OpenAI-Internal-Basispoints-Client-Product") != "basispoints-excel-plugin" {
			t.Fatal("native catalog selector drift")
		}
		if r.Header.Get("User-Agent") != "" || r.Header.Get("X-OpenAI-Account-User-ID") != "" {
			t.Fatal("unexpected device/user identity override")
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	}))
	response, err := c.DoNativeBPS(context.Background(), "token", "fixture-account", []byte(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
}
