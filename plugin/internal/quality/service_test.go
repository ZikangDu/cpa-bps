package quality

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

func testDefaults() Config {
	return Config{Models: []string{}, IntervalMinutes: 360, ReasoningEffort: "high", Prompt: "Choose the minimum candy count.", ExpectedAnswer: "21"}
}
func waitQuality(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("quality worker did not settle")
		}
		time.Sleep(time.Millisecond)
	}
}
func testClock() (func() time.Time, func(time.Duration)) {
	var value atomic.Int64
	value.Store(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).UnixNano())
	return func() time.Time { return time.Unix(0, value.Load()) }, func(d time.Duration) { value.Add(int64(d)) }
}
func testService(t *testing.T, runner Runner, options Options) *Service {
	t.Helper()
	options.Defaults = testDefaults()
	service, err := New(filepath.Join(t.TempDir(), "quality.json"), runner, options)
	if err != nil {
		t.Fatal(err)
	}
	service.Start(context.Background())
	t.Cleanup(service.Close)
	return service
}

func TestDefaultDisabledPersistenceAndNoStartupReplay(t *testing.T) {
	now, advance := testClock()
	var calls atomic.Int32
	runner := func(_ context.Context, check Check) Result {
		calls.Add(1)
		return Result{Status: "pass", Answer: "21", BenchmarkVersion: "test-v1"}
	}
	s := testService(t, runner, Options{Now: now, Tick: time.Hour})
	s.Start(context.Background())
	if s.Snapshot("A").Config.Enabled || calls.Load() != 0 {
		t.Fatal("startup ran a default plan")
	}
	cfg := testDefaults()
	cfg.Enabled = true
	cfg.Models = []string{"first", "second"}
	cfg.IntervalMinutes = 1
	if err := s.Put("B", "account-B", cfg); err != nil {
		t.Fatal(err)
	}
	advance(24 * time.Hour)
	s.runDue()
	waitQuality(t, func() bool { return len(s.Snapshot("B").Results) == 2 && !s.Snapshot("B").Running })
	view := s.Snapshot("B")
	if calls.Load() != 2 || !view.NextDue.Equal(now().Add(time.Minute)) {
		t.Fatalf("missed periods replayed or completion-based next_due wrong: %d %s", calls.Load(), view.NextDue)
	}
	for _, result := range view.Results {
		if result.AuthIndex != "B" || result.Prompt != cfg.Prompt || result.ExpectedAnswer != "21" {
			t.Fatal("lost account or benchmark snapshot")
		}
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("state permissions: %v %v", info, err)
	}
	s.Close()
	restarted, err := New(s.path, runner, Options{Defaults: testDefaults(), Now: now, Tick: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	restarted.Start(context.Background())
	defer restarted.Close()
	restarted.runDue()
	if calls.Load() != 2 || len(restarted.Snapshot("B").Results) != 2 {
		t.Fatal("restart replayed or lost results")
	}
	if _, err = restarted.Run("B", "account-B", RunRequest{}); !errors.Is(err, ErrCooldown) {
		t.Fatalf("cooldown did not survive restart: %v", err)
	}
}

func TestAccountIsolationGlobalConcurrencyAndClose(t *testing.T) {
	entered := make(chan Check, 4)
	release := make(chan struct{})
	var active, peak atomic.Int32
	s := testService(t, func(ctx context.Context, check Check) Result {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		entered <- check
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Status: "pass"}
	}, Options{GlobalLimit: 1})
	req := RunRequest{Models: []string{"first", "second"}}
	if _, err := s.Run("B", "account-B", req); err != nil {
		t.Fatal(err)
	}
	first := <-entered
	if first.AuthIndex != "B" || first.AuthID != "account-B" {
		t.Fatal("wrong account")
	}
	if _, err := s.Run("B", "account-B", req); !errors.Is(err, ErrBusy) {
		t.Fatalf("duplicate account accepted: %v", err)
	}
	if _, err := s.Run("C", "account-C", req); !errors.Is(err, ErrBusy) {
		t.Fatalf("global busy did not reject instead of queue: %v", err)
	}
	select {
	case other := <-entered:
		t.Fatalf("global limit exceeded: %+v", other)
	case <-time.After(10 * time.Millisecond):
	}
	s.Close()
	if active.Load() != 0 || peak.Load() != 1 {
		t.Fatal("Close did not drain one bounded worker")
	}
	if _, err := s.Run("D", "account-D", req); !errors.Is(err, ErrClosed) {
		t.Fatalf("closed service accepted work: %v", err)
	}
	if len(s.Snapshot("A").Results) != 0 || len(s.Snapshot("C").Results) != 0 {
		t.Fatal("queued or unrelated account recorded another account's result")
	}
}

func TestTimeoutAndPolicyBlockDoNotBecomeAnswerFailures(t *testing.T) {
	now, advance := testClock()
	s := testService(t, func(ctx context.Context, check Check) Result {
		if check.AuthIndex == "timeout" {
			<-ctx.Done()
			return Result{Status: "request_error"}
		}
		return Result{Status: "request_error", ErrorCode: "bps_usage_policy_blocked"}
	}, Options{Timeout: 10 * time.Millisecond, Now: now, Tick: time.Hour})
	if _, err := s.Run("timeout", "timeout-id", RunRequest{Models: []string{"first"}}); err != nil {
		t.Fatal(err)
	}
	waitQuality(t, func() bool { return !s.Snapshot("timeout").Running })
	if result := s.Snapshot("timeout").Results[0]; result.Status != "timeout" {
		t.Fatalf("timeout misclassified: %+v", result)
	}
	cfg := testDefaults()
	cfg.Enabled = true
	cfg.Models = []string{"first", "second"}
	if err := s.Put("blocked", "blocked-id", cfg); err != nil {
		t.Fatal(err)
	}
	advance(time.Duration(cfg.IntervalMinutes) * time.Minute)
	s.runDue()
	waitQuality(t, func() bool { return !s.Snapshot("blocked").Running })
	view := s.Snapshot("blocked")
	if !view.Blocked || view.Config.Enabled || len(view.Results) != 2 || view.Results[1].Status != "request_error" ||
		view.Results[0].Status != "skipped" || view.Results[0].ErrorCode != "bps_quality_skipped_after_policy" || view.Results[0].Manual {
		t.Fatalf("policy block did not stop batch/plan: %+v", view)
	}
	s.runDue()
	if len(s.Snapshot("blocked").Results) != 2 || s.Snapshot("blocked").Running {
		t.Fatal("blocked automatic plan retried")
	}
	if err := s.Put("blocked", "blocked-id", cfg); err != nil || s.Snapshot("blocked").Blocked {
		t.Fatal("explicit resume failed")
	}
}

func TestBoundedHistoryAndSafeState(t *testing.T) {
	now, advance := testClock()
	s := testService(t, func(context.Context, Check) Result {
		return Result{Status: "answer_fail", Answer: "Bearer secret-example-token " + strings.Repeat("糖", 1000), Usage: Usage{InputTokens: -2}}
	}, Options{Now: now})
	for i := 0; i < HistoryPerModel+5; i++ {
		advance(time.Minute)
		if _, err := s.Run("B", "account-B", RunRequest{Models: []string{"first"}}); err != nil {
			t.Fatal(err)
		}
		waitQuality(t, func() bool { return !s.Snapshot("B").Running })
	}
	view := s.Snapshot("B")
	if len(view.Results) != HistoryPerModel {
		t.Fatal("history was not bounded by account and model")
	}
	for _, result := range view.Results {
		if len(result.Answer) > MaxAnswerBytes+3 || !utf8.ValidString(result.Answer) || strings.Contains(result.Answer, "secret-example-token") || result.Usage.InputTokens != 0 {
			t.Fatal("unsafe or invalid result")
		}
	}
	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "secret-example-token") {
		t.Fatal("secret persisted")
	}
	if _, err := readState(s.path); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptStateAndChangedAccountFailClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.json")
	corrupt := []byte(`{"version":1,"accounts":`)
	if err := os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := New(path, func(context.Context, Check) Result { return Result{} }, Options{Defaults: testDefaults()}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("corrupt state accepted: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != string(corrupt) {
		t.Fatal("corrupt state overwritten")
	}
	s := testService(t, func(context.Context, Check) Result { return Result{Status: "pass"} }, Options{})
	if err := s.Put("B", "original-id", testDefaults()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run("B", "replacement-id", RunRequest{Models: []string{"first"}}); !errors.Is(err, ErrInvalid) {
		t.Fatal("account identity changed silently")
	}
}

func TestConfigChangeStopsRemainingModels(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	s := testService(t, func(ctx context.Context, check Check) Result {
		calls.Add(1)
		close(entered)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Status: "pass"}
	}, Options{})
	cfg := testDefaults()
	cfg.Enabled = true
	cfg.Models = []string{"first", "second"}
	if err := s.Put("B", "B-id", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run("B", "B-id", RunRequest{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	cfg.Enabled = false
	if err := s.Put("B", "B-id", cfg); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitQuality(t, func() bool { return !s.Snapshot("B").Running })
	if calls.Load() != 1 || s.Snapshot("B").Config.Enabled {
		t.Fatal("paused plan sent remaining model or resumed itself")
	}
}

func TestManualReservationSurvivesRestartEvenWhenPlanDisabled(t *testing.T) {
	now, advance := testClock()
	entered := make(chan struct{})
	var calls atomic.Int32
	runner := func(ctx context.Context, check Check) Result {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return Result{Status: "request_error"}
	}
	s := testService(t, runner, Options{Now: now, Timeout: 90 * time.Second, Tick: time.Hour})
	if _, err := s.Run("B", "account-B", RunRequest{Models: []string{"first"}}); err != nil {
		t.Fatal(err)
	}
	<-entered
	disk, err := readState(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if disk.Accounts["B"].Config.Enabled || !disk.Accounts["B"].ReservedUntil.Equal(now().Add(90*time.Second)) {
		t.Fatal("disabled manual check has no durable lease")
	}
	s.Close()
	advance(31 * time.Second)
	restarted, err := New(s.path, func(context.Context, Check) Result { calls.Add(1); return Result{Status: "pass"} }, Options{Defaults: testDefaults(), Now: now, Tick: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	restarted.Start(context.Background())
	defer restarted.Close()
	if _, err := restarted.Run("B", "account-B", RunRequest{Models: []string{"first"}}); !errors.Is(err, ErrBusy) {
		t.Fatalf("possible issued request replayed after restart: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatal("reservation allowed a second request")
	}
}

func TestUnsupportedExpectedAnswersRejectedBeforeSpending(t *testing.T) {
	for _, expected := range []string{"二十一", "21颗", "1000000000", "answer", "-21"} {
		cfg := testDefaults()
		cfg.ExpectedAnswer = expected
		if ValidateConfig(cfg) == nil {
			t.Errorf("unsupported reference accepted: %q", expected)
		}
	}
	for _, expected := range []string{"21", "２１", "0", "999999999"} {
		cfg := testDefaults()
		cfg.ExpectedAnswer = expected
		if ValidateConfig(cfg) != nil {
			t.Errorf("valid reference rejected: %q", expected)
		}
	}
}
