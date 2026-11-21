package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/config"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/service/managerconfig"
	"github.com/seakee/cpa-manager-plus/apps/manager-server/internal/store"
)

func TestProxyBPSPlanEnrichmentUsesConfiguredUpstreamAndExactIndexes(t *testing.T) {
	const source = `{"files":[
		{"provider":"cpa-bps","auth_index":"bps-a","name":"first.json","other":{"number":9007199254740993}},
		{"type":"cpa-bps","auth_index":"bps-b","name":"second.json"},
		{"provider":"codex","auth_index":"codex-a","name":"native.json","plan_type":"plus"},
		{"provider":"cpa-bps","auth_index":"missing","name":"missing.json"}
	],"total":4,"extension":"preserved"}`
	var calls atomic.Int32
	observedAt := time.Now().UTC().Add(-time.Minute).Format(time.RFC3339)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer configured-management-key" {
			t.Error("expected configured management authentication")
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/prefix/v0/management/auth-files":
			if r.Header.Get("Accept-Encoding") != "identity" {
				t.Error("auth-file listing should request an uncompressed response")
			}
			w.Header().Set("ETag", "original")
			w.Header().Set("Content-Length", fmt.Sprint(len(source)))
			_, _ = io.WriteString(w, source)
		case "/prefix" + bpsPlanObservationPath:
			calls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []bpsPlanObservation{
				{AuthIndex: "bps-a", PlanType: "team", ObservedAt: observedAt, Source: "active_usage"},
				{AuthIndex: "bps-b", PlanType: "enterprise", ObservedAt: observedAt, Source: "active_usage"},
				{AuthIndex: "codex-a", PlanType: "free", ObservedAt: observedAt, Source: "active_usage"},
			}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	service := New(managerconfig.New(config.Config{
		CPAUpstreamURL: server.URL + "/prefix",
		ManagementKey:  "configured-management-key",
	}, nil, nil))
	request := httptest.NewRequest(http.MethodGet, "/v0/management/auth-files", nil)
	request.Header.Set("Authorization", "Bearer untrusted-caller-key")
	recorder := httptest.NewRecorder()
	service.ProxyManagement(recorder, request, func(w http.ResponseWriter, status int, err error) {
		t.Errorf("unexpected proxy error: %v", err)
		http.Error(w, err.Error(), status)
	})
	if recorder.Code != http.StatusOK || calls.Load() != 1 {
		t.Fatalf("status %d, observation requests %d", recorder.Code, calls.Load())
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(recorder.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	var files []map[string]json.RawMessage
	_ = json.Unmarshal(document["files"], &files)
	for index, expected := range []string{"team", "enterprise"} {
		if rawFieldString(files[index]["plan_type"]) != expected ||
			rawFieldString(files[index]["plan_type_source"]) != "active_usage" ||
			rawFieldString(files[index]["plan_type_observed_at"]) != observedAt {
			t.Fatalf("BPS observation was not joined to row %d", index)
		}
	}
	if rawFieldString(files[2]["plan_type"]) != "plus" || files[2]["plan_type_source"] != nil ||
		files[3]["plan_type"] != nil || string(files[0]["other"]) != `{"number":9007199254740993}` ||
		string(document["total"]) != "4" || rawFieldString(document["extension"]) != "preserved" {
		t.Fatal("unrelated account or response data changed")
	}
	if recorder.Header().Get("ETag") != "" || recorder.Header().Get("Content-Length") != fmt.Sprint(recorder.Body.Len()) {
		t.Fatal("modified response retained stale entity headers")
	}
}

func TestBPSPlanObservationRejectsStaleOrAmbiguousData(t *testing.T) {
	now := time.Date(2026, time.November, 21, 14, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Minute).Format(time.RFC3339Nano)
	payload, _ := json.Marshal(map[string]any{"accounts": []bpsPlanObservation{
		{AuthIndex: "valid", PlanType: "team", ObservedAt: fresh, Source: "active_usage"},
		{AuthIndex: "stale", PlanType: "team", ObservedAt: now.Add(-6 * time.Minute).Format(time.RFC3339), Source: "active_usage"},
		{AuthIndex: "future", PlanType: "team", ObservedAt: now.Add(time.Second).Format(time.RFC3339), Source: "active_usage"},
		{AuthIndex: "invalid-time", PlanType: "team", ObservedAt: "invalid", Source: "active_usage"},
		{AuthIndex: "unknown-plan", PlanType: "unknown", ObservedAt: fresh, Source: "active_usage"},
		{AuthIndex: "claim", PlanType: "team", ObservedAt: fresh, Source: "token_claims"},
		{AuthIndex: "duplicate", PlanType: "team", ObservedAt: fresh, Source: "active_usage"},
		{AuthIndex: "duplicate", PlanType: "free", ObservedAt: fresh, Source: "active_usage"},
	}})
	got := parseBPSPlanObservations(payload, now)
	if len(got) != 1 || got["valid"].PlanType != "team" {
		t.Fatalf("unexpected observations: %#v", got)
	}
}

func TestBPSPlanEnrichmentFailurePreservesOriginalList(t *testing.T) {
	const raw = `{ "files": [ { "provider":"cpa-bps", "auth_index":"one" } ] }`
	var redirected atomic.Int32
	sink := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer sink.Close()
	for _, status := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusFound, http.StatusOK} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", sink.URL)
				w.WriteHeader(status)
				_, _ = io.WriteString(w, `{"accounts":"malformed"}`)
			}))
			defer server.Close()
			response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header),
				Body: io.NopCloser(strings.NewReader(raw)), ContentLength: int64(len(raw))}
			enrichBPSAuthFilePlans(context.Background(), response, store.Setup{
				CPAUpstreamURL: server.URL, ManagementKey: "fixture",
			})
			body, err := io.ReadAll(response.Body)
			_ = response.Body.Close()
			if err != nil || string(body) != raw || response.ContentLength != int64(len(raw)) {
				t.Fatal("optional observation failure altered the upstream credential list")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("observation request followed a redirect")
	}
}

func TestBPSPlanEnrichmentSkipsUnrelatedAndDuplicateRows(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []bpsPlanObservation{
			{AuthIndex: "shared", PlanType: "team", ObservedAt: time.Now().Add(-time.Second).Format(time.RFC3339), Source: "active_usage"},
		}})
	}))
	defer server.Close()
	for _, raw := range []string{
		`{"files":[{"provider":"codex","auth_index":"native"}]}`,
		`{"files":[{"provider":"cpa-bps","auth_index":"shared"},{"provider":"codex","auth_index":"shared"}]}`,
	} {
		response := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(raw))}
		enrichBPSAuthFilePlans(context.Background(), response, store.Setup{CPAUpstreamURL: server.URL, ManagementKey: "fixture"})
		body, _ := io.ReadAll(response.Body)
		_ = response.Body.Close()
		if string(body) != raw {
			t.Fatal("unrelated or ambiguous account list was enriched")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("expected no observation request for unrelated providers, got %d", calls.Load())
	}
}

func TestBPSObservationHonorsCallerDeadline(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if result := fetchBPSPlanObservations(ctx, store.Setup{CPAUpstreamURL: server.URL, ManagementKey: "fixture"}); result != nil {
		t.Fatal("canceled observation request returned data")
	}
}
