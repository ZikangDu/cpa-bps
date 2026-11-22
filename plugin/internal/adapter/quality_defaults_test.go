package adapter

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	"cpa-bps-reconstruction/internal/quality"
)

func TestQualityDefaultsUseThirtyMinutesWithoutStartingSchedule(t *testing.T) {
	defaults := qualityDefaults()
	if defaults.IntervalMinutes != 30 || defaults.Enabled || len(defaults.Models) != 0 || quality.ValidateConfig(defaults) != nil {
		t.Fatal("quality defaults must be valid, thirty minutes and disabled")
	}
	service, err := quality.New(filepath.Join(t.TempDir(), "quality.json"), func(context.Context, quality.Check) quality.Result {
		t.Error("reading defaults must not launch a quality check")
		return quality.Result{}
	}, quality.Options{Defaults: defaults})
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	view := service.Snapshot("new-account")
	if !reflect.DeepEqual(view.Config, defaults) || !view.NextDue.IsZero() || view.Running {
		t.Fatal("new account did not receive disabled thirty-minute defaults")
	}
}

func TestQualityDefaultChangePreservesPersistedIntervalsAndPlans(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quality.json")
	legacy := qualityDefaults()
	legacy.IntervalMinutes = 360
	runner := func(context.Context, quality.Check) quality.Result {
		return quality.Result{Status: "pass", Answer: "21"}
	}
	old, err := quality.New(path, runner, quality.Options{Defaults: legacy})
	if err != nil {
		t.Fatal(err)
	}
	old.Start(t.Context())
	// Manual checks also persist an account record containing the then-current
	// default interval. It cannot be distinguished from an intentionally saved
	// value, so a default change must not rewrite it implicitly.
	if _, err := old.Run("manual-only", "account-manual", quality.RunRequest{Models: []string{"gpt-6.1-sol"}}); err != nil {
		t.Fatal(err)
	}
	custom := legacy
	custom.Enabled = true
	custom.Models = []string{"gpt-6.1-sol", "gpt-6-astra"}
	custom.IntervalMinutes = 75
	if err := old.Put("custom", "account-custom", custom); err != nil {
		t.Fatal(err)
	}
	beforeCustom := old.Snapshot("custom")
	old.Close()
	current, err := quality.New(path, runner, quality.Options{Defaults: qualityDefaults()})
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	manual := current.Snapshot("manual-only")
	if manual.Config.IntervalMinutes != 360 || manual.Config.Enabled || len(manual.Config.Models) != 0 || !manual.NextDue.IsZero() {
		t.Fatal("new defaults rewrote a persisted manual-only account or enabled it")
	}
	afterCustom := current.Snapshot("custom")
	if !reflect.DeepEqual(afterCustom.Config, custom) || !afterCustom.NextDue.Equal(beforeCustom.NextDue) {
		t.Fatal("new defaults overwrote a custom interval, model selection or due date")
	}
	if fresh := current.Snapshot("new-account"); fresh.Config.IntervalMinutes != 30 || fresh.Config.Enabled || !fresh.NextDue.IsZero() {
		t.Fatal("newly observed account did not use the new disabled default")
	}
}
