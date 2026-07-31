package app

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

func TestLibraryForwardClockTransitionSwitchesPeriodAndEndTime(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure morning playback: %v", err)
	}
	if svc.activeLoopPeriod != medialib.PeriodMorning {
		t.Fatalf("active period = %q, want morning", svc.activeLoopPeriod)
	}
	playsBefore := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]

	tick = fixedNow("2026-06-24T11:00:00+08:00")()
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure day playback after forward transition: %v", err)
	}
	if svc.activeLoopPeriod != medialib.PeriodDay {
		t.Fatalf("active period = %q, want day", svc.activeLoopPeriod)
	}
	wantEnd := fixedNow("2026-06-24T17:00:00+08:00")()
	if !svc.activeLoopEndsAt.Equal(wantEnd) {
		t.Fatalf("active end = %s, want %s", svc.activeLoopEndsAt, wantEnd)
	}
	if got := filepath.Base(svc.activeLoopPath); got != "loop_day_study_001.mp4" {
		t.Fatalf("active loop = %q, want day asset", got)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playsBefore+1 {
		t.Fatalf("loop play calls = %d, want %d", got, playsBefore+1)
	}
}

func TestLibraryForwardTransitionWithoutCurrentPeriodMediaStopsExpiredLoop(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure morning playback: %v", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got == "" {
		t.Fatal("morning loop did not start")
	}

	tick = fixedNow("2026-06-24T11:00:00+08:00")()
	err := svc.ensureLibraryPlayback(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "白天時段") {
		t.Fatalf("transition error = %v, want missing day-period failure", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != "" {
		t.Fatalf("expired morning loop still playing after failed day transition: %q", got)
	}
	if svc.activeLoopID != "" ||
		svc.activeLoopPath != "" ||
		svc.activeLoopPeriod != "" ||
		!svc.activeLoopEndsAt.IsZero() {
		t.Fatalf(
			"expired active loop state was not cleared: id=%q path=%q period=%q ends=%s",
			svc.activeLoopID,
			svc.activeLoopPath,
			svc.activeLoopPeriod,
			svc.activeLoopEndsAt,
		)
	}
}

func TestLibraryStartupWithoutCurrentPeriodMediaStopsUntrackedOBSLoop(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	stalePath := "/previous-process/loop_morning_cafe_001.mp4"
	if fakeOBS.sourcePlayed == nil {
		fakeOBS.sourcePlayed = make(map[string]string)
	}
	if fakeOBS.inputFiles == nil {
		fakeOBS.inputFiles = make(map[string]string)
	}
	if fakeOBS.mediaStatuses == nil {
		fakeOBS.mediaStatuses = make(map[string]obs.MediaInputStatus)
	}
	fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName] = stalePath
	fakeOBS.inputFiles[svc.cfg.OBSLoopSourceName] = stalePath
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State: obs.MediaStatePlaying,
	}

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	err := svc.ensureLibraryPlayback(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "白天時段") {
		t.Fatalf("startup error = %v, want missing day-period failure", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != "" {
		t.Fatalf("untracked stale OBS loop still playing: %q", got)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSLoopSourceName]; got != 1 {
		t.Fatalf("loop stop calls = %d, want 1", got)
	}
}

func TestLibraryForwardTransitionPlaybackFailureStopsExpiredLoop(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure morning playback: %v", err)
	}

	playErr := errors.New("OBS rejected day loop")
	fakeOBS.playErr = playErr
	tick = fixedNow("2026-06-24T11:00:00+08:00")()
	err := svc.ensureLibraryPlayback(ctx, false)
	if !errors.Is(err, playErr) {
		t.Fatalf("transition error = %v, want %v", err, playErr)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != "" {
		t.Fatalf("expired morning loop still playing after OBS transition failure: %q", got)
	}
	if svc.activeLoopPath != "" || svc.activeLoopPeriod != "" {
		t.Fatalf(
			"expired active loop state was not cleared: path=%q period=%q",
			svc.activeLoopPath,
			svc.activeLoopPeriod,
		)
	}
}

func TestLibraryFailedForcedRedrawKeepsHealthyCurrentPeriodLoop(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure day playback: %v", err)
	}
	currentPath := svc.activeLoopPath
	playErr := errors.New("OBS redraw rejected")
	fakeOBS.playErr = playErr

	err := svc.ensureLibraryPlayback(ctx, true)
	if !errors.Is(err, playErr) {
		t.Fatalf("forced redraw error = %v, want %v", err, playErr)
	}
	if got := svc.activeLoopPath; got != currentPath {
		t.Fatalf("active loop path = %q, want current-period path %q", got, currentPath)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != currentPath {
		t.Fatalf("OBS current-period loop = %q, want %q", got, currentPath)
	}
}

func TestLibraryExpiredLoopStopFailureRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure morning playback: %v", err)
	}
	morningPath := svc.activeLoopPath

	playErr := errors.New("OBS rejected day loop")
	stopErr := errors.New("OBS rejected stop")
	fakeOBS.playErr = playErr
	fakeOBS.stopErr = stopErr
	tick = fixedNow("2026-06-24T11:00:00+08:00")()

	for attempt := 1; attempt <= 2; attempt++ {
		err := svc.ensureLibraryPlayback(ctx, false)
		if !errors.Is(err, playErr) || !errors.Is(err, stopErr) {
			t.Fatalf("attempt %d error = %v, want joined play and stop errors", attempt, err)
		}
		if got := fakeOBS.sourceStopCalls[svc.cfg.OBSLoopSourceName]; got != attempt {
			t.Fatalf("attempt %d stop calls = %d, want %d", attempt, got, attempt)
		}
		if got := svc.activeLoopPath; got != morningPath {
			t.Fatalf(
				"attempt %d active loop path = %q, want retryable expired path %q",
				attempt,
				got,
				morningPath,
			)
		}
	}
}

func TestEarlyNightClearsPreviousOverrideAndPlanWhenTodayOverrideExists(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_study_001.mp4")
	at := fixedNow("2026-06-24T00:30:00+08:00")()
	svc.now = func() time.Time { return at }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if len(svc.librarySnapshot.Loops) != 2 {
		t.Fatalf("night loop count = %d, want 2", len(svc.librarySnapshot.Loops))
	}
	stale := svc.librarySnapshot.Loops[0]
	today := svc.librarySnapshot.Loops[1]
	const previousDate = "2026-06-23"
	if err := svc.libDB.SetThemeOverride(ctx, previousDate, stale.Theme); err != nil {
		t.Fatalf("SetThemeOverride(previous): %v", err)
	}
	if err := svc.libDB.SavePeriodPlan(ctx, medialib.PeriodPlan{
		Date:   previousDate,
		Period: medialib.PeriodNight,
		Theme:  stale.Theme,
		LoopID: stale.ID,
	}); err != nil {
		t.Fatalf("SavePeriodPlan(previous): %v", err)
	}
	if err := svc.libDB.SetDirectLoopOverride(ctx, "2026-06-24", today.ID); err != nil {
		t.Fatalf("SetDirectLoopOverride(today): %v", err)
	}

	loop, _, _, err := svc.loopForTimeLocked(ctx, at, false)
	if err != nil {
		t.Fatalf("loopForTimeLocked: %v", err)
	}
	if loop.ID != today.ID {
		t.Fatalf("selected loop = %q, want today's direct override %q", loop.ID, today.ID)
	}
	expired, err := svc.libDB.Override(ctx, previousDate)
	if err != nil {
		t.Fatalf("Override(previous): %v", err)
	}
	if expired.Theme != "" || expired.DirectLoopID != "" {
		t.Fatalf("previous override survived midnight expiry: %+v", expired)
	}
	if plan, found, err := svc.libDB.PeriodPlan(ctx, previousDate, medialib.PeriodNight); err != nil {
		t.Fatalf("PeriodPlan(previous): %v", err)
	} else if found {
		t.Fatalf("previous override-derived plan survived expiry: %+v", plan)
	}
	current, err := svc.libDB.Override(ctx, "2026-06-24")
	if err != nil {
		t.Fatalf("Override(today): %v", err)
	}
	if current.DirectLoopID != today.ID {
		t.Fatalf("today override changed during previous expiry: %+v", current)
	}
}

func TestLibraryPeriodPlanSurvivesStateStoreRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "library.db")
	first, _ := newLibraryTestServiceAtDBPath(t, dbPath)
	for _, name := range []string{
		"loop_day_cafe_001.mp4",
		"loop_day_cafe_002.mp4",
	} {
		writeLibraryFile(t, first.cfg.LoopMediaDir, name)
	}
	first.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := first.ScanLibrary(ctx); err != nil {
		t.Fatalf("first scan: %v", err)
	}
	if err := first.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("first playback: %v", err)
	}
	firstID := first.activeLoopID
	if firstID == "" {
		t.Fatal("first service did not persist an active loop")
	}
	if err := first.libDB.Close(); err != nil {
		t.Fatalf("close first state store: %v", err)
	}

	second, _ := newLibraryTestServiceAtDBPath(t, dbPath)
	for _, name := range []string{
		"loop_day_cafe_001.mp4",
		"loop_day_cafe_002.mp4",
	} {
		writeLibraryFile(t, second.cfg.LoopMediaDir, name)
	}
	second.now = fixedNow("2026-06-24T12:30:00+08:00")
	if err := second.ScanLibrary(ctx); err != nil {
		t.Fatalf("second scan: %v", err)
	}
	if err := second.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("second playback: %v", err)
	}
	if second.activeLoopID != firstID {
		t.Fatalf("restarted loop ID = %q, want persisted %q", second.activeLoopID, firstID)
	}
}
