package library

import (
	"context"
	"path/filepath"
	"testing"
)

func TestStateStoreClearOverrideAndPeriodPlanIsAtomicAndScoped(t *testing.T) {
	ctx := context.Background()
	store, err := OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer store.Close()

	const date = "2026-07-31"
	if err := store.SetDirectLoopOverride(ctx, date, "loop-direct"); err != nil {
		t.Fatalf("SetDirectLoopOverride: %v", err)
	}
	for _, plan := range []PeriodPlan{
		{Date: date, Period: PeriodMorning, Theme: "morning", LoopID: "loop-morning"},
		{Date: date, Period: PeriodDay, Theme: "day", LoopID: "loop-day"},
		{Date: "2026-08-01", Period: PeriodMorning, Theme: "tomorrow", LoopID: "loop-tomorrow"},
	} {
		if err := store.SavePeriodPlan(ctx, plan); err != nil {
			t.Fatalf("SavePeriodPlan(%+v): %v", plan, err)
		}
	}

	if err := store.ClearOverrideAndPeriodPlan(ctx, date, PeriodMorning); err != nil {
		t.Fatalf("ClearOverrideAndPeriodPlan: %v", err)
	}

	var overrideCount int
	if err := store.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM library_overrides WHERE date_key = ?`, date,
	).Scan(&overrideCount); err != nil {
		t.Fatalf("count override: %v", err)
	}
	if overrideCount != 0 {
		t.Fatalf("override count = %d, want 0", overrideCount)
	}
	if _, ok, err := store.PeriodPlan(ctx, date, PeriodMorning); err != nil || ok {
		t.Fatalf("cleared period plan exists=%v, err=%v", ok, err)
	}
	if plan, ok, err := store.PeriodPlan(ctx, date, PeriodDay); err != nil || !ok || plan.LoopID != "loop-day" {
		t.Fatalf("other period plan = %+v, exists=%v, err=%v", plan, ok, err)
	}
	if plan, ok, err := store.PeriodPlan(ctx, "2026-08-01", PeriodMorning); err != nil || !ok || plan.LoopID != "loop-tomorrow" {
		t.Fatalf("other date plan = %+v, exists=%v, err=%v", plan, ok, err)
	}
}

func TestStateStoreClearOverrideAndPeriodPlanRollsBackWhenPlanDeleteFails(t *testing.T) {
	ctx := context.Background()
	store, err := OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer store.Close()

	const date = "2026-07-31"
	if err := store.SetThemeOverride(ctx, date, "focus"); err != nil {
		t.Fatalf("SetThemeOverride: %v", err)
	}
	if err := store.SavePeriodPlan(ctx, PeriodPlan{
		Date:   date,
		Period: PeriodDay,
		Theme:  "focus",
		LoopID: "loop-day",
	}); err != nil {
		t.Fatalf("SavePeriodPlan: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_selected_period_plan_delete
BEFORE DELETE ON library_period_plans
WHEN OLD.date_key = '2026-07-31' AND OLD.period = 'day'
BEGIN
	SELECT RAISE(ABORT, 'forced period plan delete failure');
END;
`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	if err := store.ClearOverrideAndPeriodPlan(ctx, date, PeriodDay); err == nil {
		t.Fatal("expected period-plan delete failure")
	}

	override, err := store.Override(ctx, date)
	if err != nil {
		t.Fatalf("Override after rollback: %v", err)
	}
	if override.Theme != "focus" {
		t.Fatalf("override after rollback = %+v, want theme focus", override)
	}
	plan, ok, err := store.PeriodPlan(ctx, date, PeriodDay)
	if err != nil {
		t.Fatalf("PeriodPlan after rollback: %v", err)
	}
	if !ok || plan.LoopID != "loop-day" {
		t.Fatalf("plan after rollback = %+v, exists=%v", plan, ok)
	}
}

func TestStateStorePruneBeforeIsBoundedAndPreservesCutoff(t *testing.T) {
	ctx := context.Background()
	store, err := OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer store.Close()

	for _, date := range []string{"2026-07-01", "2026-07-02", "2026-07-03", "2026-07-04"} {
		if err := store.SetThemeOverride(ctx, date, "theme-"+date); err != nil {
			t.Fatalf("SetThemeOverride(%s): %v", date, err)
		}
		if err := store.SavePeriodPlan(ctx, PeriodPlan{
			Date:   date,
			Period: PeriodMorning,
			Theme:  "theme-" + date,
			LoopID: "loop-" + date,
		}); err != nil {
			t.Fatalf("SavePeriodPlan(%s): %v", date, err)
		}
	}

	result, err := store.PruneBefore(ctx, "2026-07-04", 2)
	if err != nil {
		t.Fatalf("PruneBefore first batch: %v", err)
	}
	if result != (PruneResult{Overrides: 2, PeriodPlans: 2}) {
		t.Fatalf("first result = %#v", result)
	}
	if override, err := store.Override(ctx, "2026-07-04"); err != nil || override.Theme == "" {
		t.Fatalf("cutoff override = %#v, err=%v", override, err)
	}
	if _, ok, err := store.PeriodPlan(ctx, "2026-07-04", PeriodMorning); err != nil || !ok {
		t.Fatalf("cutoff plan exists=%v, err=%v", ok, err)
	}

	result, err = store.PruneBefore(ctx, "2026-07-04", 2)
	if err != nil {
		t.Fatalf("PruneBefore second batch: %v", err)
	}
	if result != (PruneResult{Overrides: 1, PeriodPlans: 1}) {
		t.Fatalf("second result = %#v", result)
	}
	result, err = store.PruneBefore(ctx, "2026-07-04", 2)
	if err != nil {
		t.Fatalf("PruneBefore converged batch: %v", err)
	}
	if result != (PruneResult{}) {
		t.Fatalf("converged result = %#v", result)
	}
}

func TestStateStorePruneBeforeValidatesArguments(t *testing.T) {
	ctx := context.Background()
	store, err := OpenState(ctx, filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("OpenState: %v", err)
	}
	defer store.Close()

	if _, err := store.PruneBefore(ctx, "", 1); err == nil {
		t.Fatal("expected empty cutoff rejection")
	}
	if _, err := store.PruneBefore(ctx, "2026-07-01", 0); err == nil {
		t.Fatal("expected invalid limit rejection")
	}
}
