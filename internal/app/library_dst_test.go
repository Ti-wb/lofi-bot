package app

import (
	"context"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
)

func TestPeriodPlanAndOverrideDateKeysAcrossNewYorkDST(t *testing.T) {
	loc := mustLoadAppDSTLocation(t, "America/New_York")
	firstFallOneThirty := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC).In(loc)
	secondFallOneThirty := time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC).In(loc)

	tests := []struct {
		name         string
		at           time.Time
		period       medialib.Period
		wantPlan     string
		wantOverride string
	}{
		{
			name:         "spring before skipped hour",
			at:           time.Date(2026, time.March, 8, 1, 30, 0, 0, loc),
			period:       medialib.PeriodNight,
			wantPlan:     "2026-03-07",
			wantOverride: "2026-03-08",
		},
		{
			name:         "spring after skipped hour",
			at:           time.Date(2026, time.March, 8, 3, 30, 0, 0, loc),
			period:       medialib.PeriodNight,
			wantPlan:     "2026-03-07",
			wantOverride: "2026-03-08",
		},
		{
			name:         "fall first repeated hour",
			at:           firstFallOneThirty,
			period:       medialib.PeriodNight,
			wantPlan:     "2026-10-31",
			wantOverride: "2026-11-01",
		},
		{
			name:         "fall second repeated hour",
			at:           secondFallOneThirty,
			period:       medialib.PeriodNight,
			wantPlan:     "2026-10-31",
			wantOverride: "2026-11-01",
		},
		{
			name:         "morning after fall transition",
			at:           time.Date(2026, time.November, 1, 6, 0, 0, 0, loc),
			period:       medialib.PeriodMorning,
			wantPlan:     "2026-11-01",
			wantOverride: "2026-11-01",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := periodPlanDate(tt.at, tt.period); got != tt.wantPlan {
				t.Fatalf("periodPlanDate(%s, %q) = %q, want %q", tt.at, tt.period, got, tt.wantPlan)
			}
			if got := overrideDateKey(tt.at); got != tt.wantOverride {
				t.Fatalf("overrideDateKey(%s) = %q, want %q", tt.at, got, tt.wantOverride)
			}
		})
	}
}

func TestLibraryNightPlanSurvivesRestartAcrossNewYorkDSTDiscontinuities(t *testing.T) {
	loc := mustLoadAppDSTLocation(t, "America/New_York")
	tests := []struct {
		name     string
		before   time.Time
		after    time.Time
		planDate string
	}{
		{
			name:     "spring forward skipped local hour",
			before:   time.Date(2026, time.March, 8, 1, 30, 0, 0, loc),
			after:    time.Date(2026, time.March, 8, 3, 30, 0, 0, loc),
			planDate: "2026-03-07",
		},
		{
			name:     "fall back repeated local hour",
			before:   time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC).In(loc),
			after:    time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC).In(loc),
			planDate: "2026-10-31",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.after.Sub(tt.before); got != time.Hour {
				t.Fatalf("test discontinuity elapsed duration = %s, want 1h", got)
			}

			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "library.db")
			first, _ := newLibraryTestServiceAtDBPath(t, dbPath)
			for _, name := range []string{
				"loop_night_cafe_001.mp4",
				"loop_night_cafe_002.mp4",
			} {
				writeLibraryFile(t, first.cfg.LoopMediaDir, name)
			}
			first.now = func() time.Time { return tt.before }
			if err := first.ScanLibrary(ctx); err != nil {
				t.Fatalf("first scan: %v", err)
			}
			if err := first.ensureLibraryPlayback(ctx, false); err != nil {
				t.Fatalf("first playback: %v", err)
			}
			firstID := first.activeLoopID
			if firstID == "" {
				t.Fatal("first service did not select a loop")
			}
			plan, found, err := first.libDB.PeriodPlan(ctx, tt.planDate, medialib.PeriodNight)
			if err != nil {
				t.Fatalf("read first persisted plan: %v", err)
			}
			if !found || plan.LoopID != firstID {
				t.Fatalf("first persisted plan = %#v found=%v, want loop %q", plan, found, firstID)
			}
			if err := first.libDB.Close(); err != nil {
				t.Fatalf("close first state store: %v", err)
			}

			second, _ := newLibraryTestServiceAtDBPath(t, dbPath)
			for _, name := range []string{
				"loop_night_cafe_001.mp4",
				"loop_night_cafe_002.mp4",
			} {
				writeLibraryFile(t, second.cfg.LoopMediaDir, name)
			}
			second.now = func() time.Time { return tt.after }
			if err := second.ScanLibrary(ctx); err != nil {
				t.Fatalf("second scan: %v", err)
			}
			if err := second.ensureLibraryPlayback(ctx, false); err != nil {
				t.Fatalf("second playback: %v", err)
			}
			if second.activeLoopID != firstID {
				t.Fatalf(
					"restarted loop ID = %q, want persisted %q across %s",
					second.activeLoopID,
					firstID,
					tt.name,
				)
			}
			if got := periodPlanDate(tt.after, medialib.PeriodNight); got != tt.planDate {
				t.Fatalf("post-restart plan date = %q, want %q", got, tt.planDate)
			}
		})
	}
}

func TestLibraryPreviousDayOverrideExpiresAcrossNewYorkDST(t *testing.T) {
	loc := mustLoadAppDSTLocation(t, "America/New_York")
	tests := []struct {
		name     string
		now      time.Time
		planDate string
	}{
		{
			name:     "spring forward",
			now:      time.Date(2026, time.March, 8, 3, 30, 0, 0, loc),
			planDate: "2026-03-07",
		},
		{
			name:     "fall back second repeated hour",
			now:      time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC).In(loc),
			planDate: "2026-10-31",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			svc, _ := newLibraryTestService(t)
			for _, name := range []string{
				"loop_night_study_001.mp4",
				"loop_night_study_002.mp4",
			} {
				writeLibraryFile(t, svc.cfg.LoopMediaDir, name)
			}
			if err := svc.ScanLibrary(ctx); err != nil {
				t.Fatalf("scan library: %v", err)
			}

			loops := append([]medialib.Loop(nil), svc.librarySnapshot.Loops...)
			sort.Slice(loops, func(i, j int) bool {
				return loops[i].RelPath < loops[j].RelPath
			})
			if len(loops) != 2 {
				t.Fatalf("night loop count = %d, want 2", len(loops))
			}

			const seed = int64(73)
			predictor := rand.New(rand.NewSource(seed))
			_ = predictor.Intn(1) // ChooseTheme consumes one draw for the sole theme.
			wantIndex := predictor.Intn(len(loops))
			staleIndex := 1 - wantIndex
			svc.rng = rand.New(rand.NewSource(seed))

			if err := svc.libDB.SetThemeOverride(ctx, tt.planDate, "study"); err != nil {
				t.Fatalf("save previous-day override: %v", err)
			}
			if err := svc.libDB.SavePeriodPlan(ctx, medialib.PeriodPlan{
				Date:   tt.planDate,
				Period: medialib.PeriodNight,
				Theme:  "study",
				LoopID: loops[staleIndex].ID,
			}); err != nil {
				t.Fatalf("save previous-day plan: %v", err)
			}

			svc.now = func() time.Time { return tt.now }
			if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
				t.Fatalf("ensure early-night playback: %v", err)
			}
			if svc.activeLoopID != loops[wantIndex].ID {
				t.Fatalf(
					"active loop after expired override = %q, want fresh draw %q (stale plan %q)",
					svc.activeLoopID,
					loops[wantIndex].ID,
					loops[staleIndex].ID,
				)
			}

			expired, err := svc.libDB.Override(ctx, tt.planDate)
			if err != nil {
				t.Fatalf("read expired override: %v", err)
			}
			if expired.Theme != "" || expired.DirectLoopID != "" {
				t.Fatalf("previous-day override survived DST boundary: %#v", expired)
			}
			plan, found, err := svc.libDB.PeriodPlan(ctx, tt.planDate, medialib.PeriodNight)
			if err != nil {
				t.Fatalf("read replacement plan: %v", err)
			}
			if !found || plan.LoopID != svc.activeLoopID {
				t.Fatalf(
					"replacement plan = %#v found=%v, want active loop %q",
					plan,
					found,
					svc.activeLoopID,
				)
			}
		})
	}
}

func mustLoadAppDSTLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load IANA location %q: %v", name, err)
	}
	return loc
}
