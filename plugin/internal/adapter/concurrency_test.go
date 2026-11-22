package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func concurrencyRuntime(t *testing.T, path string) *Runtime {
	t.Helper()
	runtime := New(func(method string, request any, response any) error {
		if method != "host.auth.list" {
			t.Fatalf("unexpected host call %s", method)
		}
		parameters := request.(map[string]any)
		if parameters["host_callback_id"] != "fixture-callback" {
			t.Fatal("host callback context was not forwarded")
		}
		return json.Unmarshal([]byte(`{"files":[
			{"id":"fixture-auth","auth_index":"fixture-index","provider":"cpa-bps"},
			{"id":"unrelated-auth","auth_index":"unrelated-index","provider":"other"}
		]}`), response)
	})
	runtime.cfg = config{Enabled: true, ConcurrencyLimit: 1, ConcurrencyPolicyPath: path}
	t.Cleanup(runtime.Close)
	return runtime
}

func concurrencyRequest(method, body string) managementRequest {
	return managementRequest{
		Method: method, Path: "/v0/management/cpa-bps/concurrency",
		Query: url.Values{"auth_index": {"fixture-index"}}, Body: []byte(body),
		HostCallbackID: "fixture-callback",
	}
}

func concurrencySnapshot(t *testing.T, result any, err error) map[string]any {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	reply := result.(map[string]any)
	if reply["StatusCode"] != http.StatusOK {
		t.Fatalf("unexpected management status: %v", reply["StatusCode"])
	}
	var snapshot map[string]any
	if err := json.Unmarshal(reply["Body"].([]byte), &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot["scope"] != "plugin_instance" {
		t.Fatal("Manager's required scope field is missing")
	}
	return snapshot
}

func TestConcurrencyManagementPersistsAndRestoresAccountLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	runtime := concurrencyRuntime(t, path)
	result, err := runtime.manageConcurrency(concurrencyRequest(http.MethodGet, ""))
	got := concurrencySnapshot(t, result, err)
	if got["limit"] != float64(1) || got["active"] != float64(0) || got["waiting"] != float64(0) {
		t.Fatalf("unexpected initial snapshot: %+v", got)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("GET created a policy file")
	}

	result, err = runtime.manageConcurrency(concurrencyRequest(http.MethodPost, `{"limit":3}`))
	got = concurrencySnapshot(t, result, err)
	if got["limit"] != float64(3) {
		t.Fatalf("POST did not report its saved limit: %+v", got)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("policy was not saved with owner-only permissions")
	}
	if limit, err := runtime.accountLimit(runtime.cfg, "fixture-auth"); err != nil || limit != 3 {
		t.Fatal("execution did not read the saved limit")
	}
	restarted := concurrencyRuntime(t, path)
	result, err = restarted.manageConcurrency(concurrencyRequest(http.MethodGet, ""))
	if got := concurrencySnapshot(t, result, err); got["limit"] != float64(3) {
		t.Fatal("saved limit did not survive a restart")
	}
	result, err = restarted.manageConcurrency(concurrencyRequest(http.MethodPost, `{"limit":3}`))
	if got := concurrencySnapshot(t, result, err); got["limit"] != float64(3) {
		t.Fatal("retrying the same save did not converge")
	}
}

func TestConcurrencyManagementValidatesIndexAndLimit(t *testing.T) {
	runtime := concurrencyRuntime(t, filepath.Join(t.TempDir(), "policy.json"))
	for _, body := range []string{`{}`, `{"limit":null}`, `{"limit":-1}`, `{"limit":10001}`, `{"limit":1.5}`, `{"limit":"2"}`, `{"limit":2,"other":3}`, `{"limit":2} {}`} {
		_, err := runtime.manageConcurrency(concurrencyRequest(http.MethodPost, body))
		var problem *rpcError
		if !errors.As(err, &problem) || problem.StatusCode() != 400 {
			t.Fatalf("invalid limit body accepted: %s", body)
		}
	}
	for _, index := range []string{"missing", "unrelated-index"} {
		request := concurrencyRequest(http.MethodGet, "")
		request.Query.Set("auth_index", index)
		_, err := runtime.manageConcurrency(request)
		var problem *rpcError
		if !errors.As(err, &problem) || problem.StatusCode() != 404 {
			t.Fatalf("unexpected credential resolution for %q: %v", index, err)
		}
	}
	request := concurrencyRequest(http.MethodGet, "")
	request.Query.Add("auth_index", "duplicate")
	_, err := runtime.manageConcurrency(request)
	var problem *rpcError
	if !errors.As(err, &problem) || problem.StatusCode() != 400 {
		t.Fatal("duplicate auth_index accepted")
	}
}

func TestCorruptPolicyDoesNotOverwriteSavedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policy.json")
	const invalid = `{"version":1,"limits":{"fixture-auth":-1}}`
	if err := os.WriteFile(path, []byte(invalid), 0600); err != nil {
		t.Fatal(err)
	}
	runtime := concurrencyRuntime(t, path)
	if _, err := runtime.accountLimit(runtime.cfg, "fixture-auth"); err == nil {
		t.Fatal("corrupt policy silently fell back to default admission")
	}
	_, err := runtime.manageConcurrency(concurrencyRequest(http.MethodPost, `{"limit":2}`))
	if err == nil {
		t.Fatal("corrupt policy was overwritten")
	}
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != invalid {
		t.Fatal("original corrupt policy changed")
	}
}

func TestConcurrencySaveFailureLeavesRuntimeLimitIntact(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "parent-file", "policy.json")
	runtime := concurrencyRuntime(t, path)
	_, err := runtime.manageConcurrency(concurrencyRequest(http.MethodGet, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "parent-file"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.manageConcurrency(concurrencyRequest(http.MethodPost, `{"limit":4}`)); err == nil {
		t.Fatal("write failure was reported as saved")
	}
	if got := runtime.queue.Snapshot("fixture-auth", 1); got.Limit != 1 {
		t.Fatal("failed save changed admission capacity")
	}
}

func TestConcurrencyManagementResizesSaturatedQueueAndRejectsStaleDefaults(t *testing.T) {
	r := concurrencyRuntime(t, filepath.Join(t.TempDir(), "policy.json"))
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	first, err := r.queue.Acquire(ctx, "fixture-auth", 1)
	if err != nil {
		t.Fatal(err)
	}
	defer first()
	type admitted struct {
		release func()
		err     error
	}
	results := make(chan admitted, 3)
	waitingCtx, cancelWaiting := context.WithCancel(ctx)
	defer cancelWaiting()
	for range 3 {
		go func() {
			release, err := r.queue.Acquire(waitingCtx, "fixture-auth", 1)
			results <- admitted{release, err}
		}()
	}
	for r.queue.Snapshot("fixture-auth", 1).Waiting != 3 {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		runtime.Gosched()
	}
	result, err := r.manageConcurrency(concurrencyRequest(http.MethodPost, `{"limit":3}`))
	if snapshot := concurrencySnapshot(t, result, err); snapshot["active"] != float64(3) || snapshot["waiting"] != float64(1) {
		t.Fatalf("raising persisted capacity failed: %v", snapshot)
	}
	holders := []func(){first}
	for range 2 {
		select {
		case next := <-results:
			if next.err != nil {
				t.Fatal(next.err)
			}
			holders = append(holders, next.release)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	result, err = r.manageConcurrency(concurrencyRequest(http.MethodPost, `{"limit":1}`))
	if snapshot := concurrencySnapshot(t, result, err); snapshot["active"] != float64(3) || snapshot["waiting"] != float64(1) {
		t.Fatalf("lowering capacity canceled existing work or lost waiter: %v", snapshot)
	}
	cancelWaiting()
	select {
	case next := <-results:
		if !errors.Is(next.err, context.Canceled) || next.release != nil {
			t.Fatal("canceled waiter was admitted")
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	stale := r.cfg
	stale.ConcurrencyLimit = 10000
	if limit, err := r.accountLimit(stale, "fixture-auth"); err != nil || limit != 1 {
		t.Fatalf("stale execution restored old default: %d %v", limit, err)
	}
	for _, release := range holders {
		release()
		release()
	}
	if state := r.queue.Snapshot("fixture-auth", 10000); state.Active != 0 || state.Waiting != 0 || state.Limit != 1 {
		t.Fatalf("resized queue did not drain: %+v", state)
	}
}
