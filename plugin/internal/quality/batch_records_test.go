package quality

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestAcceptedBatchKeepsOneHonestRecordPerModelAfterPolicyStop(t *testing.T) {
	var calls atomic.Int32
	s := testService(t, func(context.Context, Check) Result {
		calls.Add(1)
		return Result{Status: "request_error", ErrorCode: "bps_usage_policy_blocked"}
	}, Options{})
	models := make([]string, MaxModels)
	for i := range models {
		models[i] = fmt.Sprintf("model-%02d", i)
	}
	id, err := s.Run("A", "account-A", RunRequest{Models: models})
	if err != nil {
		t.Fatal(err)
	}
	waitQuality(t, func() bool { return !s.Snapshot("A").Running })
	view := s.Snapshot("A")
	if calls.Load() != 1 || len(view.Results) != MaxModels {
		t.Fatal("accepted models disappeared or policy refusal caused additional requests")
	}
	seen := map[string]bool{}
	for _, record := range view.Results {
		if seen[record.RequestedModel] || record.RunID != id || !record.Manual {
			t.Fatal("a selected model was duplicated or attributed to a different batch")
		}
		seen[record.RequestedModel] = true
		if record.RequestedModel == models[0] {
			if record.Status != "request_error" || record.ErrorCode != "bps_usage_policy_blocked" {
				t.Fatal("the real failed request was altered")
			}
		} else if record.Status != "skipped" || record.ErrorCode != "bps_quality_skipped_after_policy" ||
			record.Answer != "" || record.ActualModel != "" || record.Usage != (Usage{}) || record.ElapsedMS != 0 ||
			record.ExpectedAnswer != "21" || record.BenchmarkVersion != BenchmarkVersion(s.defaults) {
			t.Fatal("an unexecuted model was reported as a measured answer")
		}
	}
	saved, err := readState(s.path)
	if err != nil || len(saved.Results) != MaxModels {
		t.Fatal("complete model accounting did not survive persistence")
	}
}

func TestCanceledBatchRecordsUnsentModelsWithoutChangingSchedule(t *testing.T) {
	entered := make(chan struct{})
	var calls atomic.Int32
	s := testService(t, func(ctx context.Context, check Check) Result {
		calls.Add(1)
		close(entered)
		<-ctx.Done()
		return Result{Status: "request_error"}
	}, Options{Timeout: time.Minute})
	cfg := testDefaults()
	cfg.Models = []string{"first", "second", "third"}
	if err := s.Put("A", "account-A", cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Run("A", "account-A", RunRequest{}); err != nil {
		t.Fatal(err)
	}
	<-entered
	s.CancelRunning()
	waitQuality(t, func() bool { return !s.Snapshot("A").Running })
	view := s.Snapshot("A")
	if calls.Load() != 1 || len(view.Results) != 3 || view.Config.Enabled {
		t.Fatal("cancellation sent more models or lost batch members")
	}
	for _, result := range view.Results {
		want := "bps_quality_skipped_canceled"
		if result.RequestedModel == "first" {
			want = "bps_quality_canceled"
		}
		if result.Status != "skipped" || result.ErrorCode != want {
			t.Fatal("in-flight cancellation was confused with an unissued request")
		}
	}
}
