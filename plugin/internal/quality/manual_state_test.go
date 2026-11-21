package quality

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func seedManualPlan(t *testing.T, s *Service, enabled, blocked bool) accountState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := cloneConfig(s.defaults)
	cfg.Enabled = enabled
	cfg.Models = []string{"scheduled-model"}
	account := accountState{AuthID: "account-A", Config: cfg, Blocked: blocked, Revision: 7}
	if enabled {
		account.NextDue = s.now().UTC().Add(6 * time.Hour)
	}
	s.state.Accounts["A"] = account
	s.state.Results = append(s.state.Results, Result{ID: "prior-result", AuthIndex: "A",
		RequestedModel: "scheduled-model", Status: "request_error", ErrorCode: "bps_usage_policy_blocked"})
	if err := writeState(s.path, s.state); err != nil {
		t.Fatal(err)
	}
	return account
}

func TestManualRunOnLocallyBlockedAccountKeepsNormalAdmissionGates(t *testing.T) {
	now, _ := testClock()
	entered, release := make(chan Check, 1), make(chan struct{})
	var calls atomic.Int32
	s := testService(t, func(ctx context.Context, check Check) Result {
		calls.Add(1)
		entered <- check
		select {
		case <-release:
		case <-ctx.Done():
		}
		return Result{Status: "request_error", ErrorCode: "bps_usage_policy_blocked"}
	}, Options{Now: now, Tick: time.Hour})
	before := seedManualPlan(t, s, false, true)
	request := RunRequest{Models: []string{"manual-first", "manual-second"}}
	if _, err := s.Run("A", "another-account", request); !errors.Is(err, ErrInvalid) {
		t.Fatal("manual run accepted a changed credential identity")
	}
	if _, err := s.Run("A", "account-A", request); err != nil {
		t.Fatal("an explicit manual run remained blocked by historical quality state", err)
	}
	check := <-entered
	if !check.Manual || check.AuthID != "account-A" || check.Model != "manual-first" {
		t.Fatal("manual authority lost its account/model scope")
	}
	view := s.Snapshot("A")
	if !view.Blocked || view.Config.Enabled || !view.NextDue.IsZero() || !view.ReservedUntil.After(now()) {
		t.Fatal("manual admission changed the periodic plan or skipped its durable lease")
	}
	if _, err := s.Run("A", "account-A", request); !errors.Is(err, ErrBusy) {
		t.Fatal("duplicate click bypassed account admission")
	}
	if _, err := s.Run("B", "account-B", RunRequest{Models: []string{"first"}}); !errors.Is(err, ErrBusy) {
		t.Fatal("manual run bypassed the global slot limit")
	}
	close(release)
	waitQuality(t, func() bool { return !s.Snapshot("A").Running })
	view = s.Snapshot("A")
	if calls.Load() != 1 || !view.Blocked || !reflect.DeepEqual(view.Config, before.Config) ||
		!view.NextDue.Equal(before.NextDue) || len(view.Results) != 3 || !view.Results[0].Manual || view.Results[2].ID != "prior-result" ||
		view.Results[0].Status != "skipped" || view.Results[0].RequestedModel != "manual-second" || view.Results[1].ErrorCode != "bps_usage_policy_blocked" {
		t.Fatal("policy refusal replayed, changed the periodic plan, or discarded prior history")
	}
	if _, err := s.Run("A", "account-A", request); !errors.Is(err, ErrCooldown) {
		t.Fatal("manual restart bypassed the normal cooldown")
	}
	s.runDue()
	if calls.Load() != 1 {
		t.Fatal("blocked periodic plan ran after a manual request")
	}
}

func TestManualOutcomesNeverChangeSavedPeriodicState(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, blocked := range []bool{false, true} {
			for _, outcome := range []Result{
				{Status: "pass", Answer: "21"},
				{Status: "answer_fail", Answer: "29"},
				{Status: "request_error", ErrorCode: "bps_usage_policy_blocked"},
				{Status: "timeout", ErrorCode: "bps_quality_timeout"},
			} {
				now, _ := testClock()
				s := testService(t, func(_ context.Context, check Check) Result {
					if !check.Manual {
						t.Error("explicit run was not marked manual")
					}
					return outcome
				}, Options{Now: now, Tick: time.Hour})
				before := seedManualPlan(t, s, enabled, blocked)
				if _, err := s.Run("A", "account-A", RunRequest{Models: []string{"manual-model"}}); err != nil {
					t.Fatal(err)
				}
				waitQuality(t, func() bool { return !s.Snapshot("A").Running })
				view := s.Snapshot("A")
				if !reflect.DeepEqual(view.Config, before.Config) || view.Blocked != before.Blocked || !view.NextDue.Equal(before.NextDue) {
					t.Fatalf("manual %s changed scheduled state (enabled=%v blocked=%v)", outcome.Status, enabled, blocked)
				}
				saved, err := readState(s.path)
				account := saved.Accounts["A"]
				if err != nil || !reflect.DeepEqual(account.Config, before.Config) || account.Blocked != before.Blocked ||
					!account.NextDue.Equal(before.NextDue) || account.Revision != before.Revision || !view.Results[0].Manual {
					t.Fatal("persisted manual result altered periodic configuration or omitted provenance")
				}
				s.Close()
			}
		}
	}
}

func TestBlockedManualReservationSurvivesRestartWithoutReplay(t *testing.T) {
	now, advance := testClock()
	entered := make(chan struct{})
	var calls atomic.Int32
	s := testService(t, func(ctx context.Context, _ Check) Result {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return Result{Status: "request_error"}
	}, Options{Now: now, Tick: time.Hour})
	seedManualPlan(t, s, false, true)
	request := RunRequest{Models: []string{"first"}}
	if _, err := s.Run("A", "account-A", request); err != nil {
		t.Fatal(err)
	}
	<-entered
	s.Close()
	advance(31 * time.Second)
	restarted, err := New(s.path, func(context.Context, Check) Result {
		calls.Add(1)
		return Result{Status: "pass", Answer: "21"}
	}, Options{Defaults: testDefaults(), Now: now, Tick: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	restarted.Start(context.Background())
	defer restarted.Close()
	if _, err := restarted.Run("A", "account-A", request); !errors.Is(err, ErrBusy) || calls.Load() != 1 {
		t.Fatal("restart repeated a potentially in-flight manual request")
	}
	advance(time.Minute)
	if _, err := restarted.Run("A", "account-A", request); err != nil {
		t.Fatal("later explicit manual run stayed locked after the reserved deadline", err)
	}
	waitQuality(t, func() bool { return !restarted.Snapshot("A").Running })
	view := restarted.Snapshot("A")
	if calls.Load() != 2 || !view.Blocked || view.Config.Enabled || !view.NextDue.IsZero() {
		t.Fatal("manual replayed or modified the disabled blocked plan")
	}
}
