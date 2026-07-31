package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

func TestStalledLoopFailsOverToHealthySamePeriodAsset(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensureLibraryPlayback: %v", err)
	}
	stalledPath := svc.activeLoopPath
	wantPath := otherLoopPath(t, svc.librarySnapshot.Loops, stalledPath)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateOpening,
	}
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("seed opening observation: %v", err)
	}

	tick = tick.Add(mediaProgressGrace + time.Second)
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("stalled loop failover: %v", err)
	}
	if svc.activeLoopPath != wantPath {
		t.Fatalf("active loop = %q, want alternate %q", svc.activeLoopPath, wantPath)
	}
	if reason := svc.libraryAssetQuarantineReasonLocked(stalledPath); !strings.Contains(reason, "stalled") {
		t.Fatalf("stalled loop quarantine reason = %q", reason)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSLoopSourceName]; got != 1 {
		t.Fatalf("stalled loop stop calls = %d, want 1", got)
	}
}

func TestStalledMusicFailsOverToHealthyAlternate(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensureLibraryPlayback: %v", err)
	}
	stalledPath := svc.activeMusicPath
	wantPath := otherMusicPath(t, svc.librarySnapshot.Music, stalledPath)
	loopCursor := 100.0
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateOpening,
	}
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State:              obs.MediaStatePlaying,
		CursorMilliseconds: &loopCursor,
	}
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("seed opening observation: %v", err)
	}

	tick = tick.Add(mediaProgressGrace + time.Second)
	loopCursor = 1000
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("stalled music failover: %v", err)
	}
	if svc.activeMusicPath != wantPath {
		t.Fatalf("active music = %q, want alternate %q", svc.activeMusicPath, wantPath)
	}
	if reason := svc.libraryAssetQuarantineReasonLocked(stalledPath); !strings.Contains(reason, "stalled") {
		t.Fatalf("stalled music quarantine reason = %q", reason)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSMusicSourceName]; got != 1 {
		t.Fatalf("stalled music stop calls = %d, want 1", got)
	}
}

func TestLastMusicPendingValuePreventsImmediateRepeat(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "library.db")
	svc, fakeOBS := newLibraryTestServiceAtDBPath(t, dbPath)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}

	// Persist beta so normal no-repeat selection deterministically starts alpha.
	var beta medialib.Music
	for _, music := range svc.librarySnapshot.Music {
		if music.Track == "beta" {
			beta = music
		}
	}
	if beta.ID == "" {
		t.Fatal("beta fixture missing")
	}
	if err := svc.libDB.SetLastMusicID(ctx, beta.ID); err != nil {
		t.Fatalf("seed last music: %v", err)
	}
	db := openLastMusicFailureDB(t, ctx, dbPath)
	defer db.Close()

	if err := svc.ensureLibraryPlayback(ctx, false); err == nil {
		t.Fatal("initial playback swallowed injected persistence failure")
	}
	firstID := svc.activeMusicID
	if firstID == "" || svc.pendingLastMusicID != firstID || firstID == beta.ID {
		t.Fatalf(
			"initial active=%q pending=%q seeded beta=%q",
			firstID,
			svc.pendingLastMusicID,
			beta.ID,
		)
	}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateEnded,
	}
	tick = tick.Add(obsEndedEventSettleGrace + time.Second)

	err := svc.reconcileLibraryPlayback(ctx)
	if err == nil || !strings.Contains(err.Error(), "last library music") {
		t.Fatalf("reconcile error = %v, want observable persistence failure", err)
	}
	if svc.activeMusicID == firstID {
		t.Fatalf("ended track %q repeated while its persistence was pending", firstID)
	}
	if svc.activeMusicID != beta.ID {
		t.Fatalf("next music = %q, want alternate beta %q", svc.activeMusicID, beta.ID)
	}
}

func TestPersistenceFailureDoesNotMaskOBSLoopFailover(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "library.db")
	svc, fakeOBS := newLibraryTestServiceAtDBPath(t, dbPath)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	db := openLastMusicFailureDB(t, ctx, dbPath)
	defer db.Close()
	if err := svc.ensureLibraryPlayback(ctx, false); err == nil {
		t.Fatal("initial playback swallowed injected persistence failure")
	}
	badPath := svc.activeLoopPath
	wantPath := otherLoopPath(t, svc.librarySnapshot.Loops, badPath)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateError,
	}

	err := svc.reconcileLibraryPlayback(ctx)
	if err == nil || !strings.Contains(err.Error(), "last library music") {
		t.Fatalf("reconcile error = %v, want persistence failure joined", err)
	}
	if svc.activeLoopPath != wantPath {
		t.Fatalf("active loop = %q, want failover %q", svc.activeLoopPath, wantPath)
	}
	if reason := svc.libraryAssetQuarantineReasonLocked(badPath); reason == "" {
		t.Fatalf("errored loop %q was not quarantined", badPath)
	}
}

func TestEmptyMusicLibraryStopsStaleOBSInputAfterReconnect(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	stalePath := "/previous-process/music_stale.mp3"
	fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName] = stalePath
	fakeOBS.inputFiles = map[string]string{svc.cfg.OBSMusicSourceName: stalePath}
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMusicSourceName: {State: obs.MediaStatePlaying},
	}

	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recoverLibraryPlaybackAfterOBSConnect: %v", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != "" {
		t.Fatalf("stale OBS music still playing: %q", got)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSMusicSourceName]; got != 1 {
		t.Fatalf("music stop calls = %d, want 1", got)
	}
	if svc.activeMusicPath != "" {
		t.Fatalf("active music path = %q, want empty", svc.activeMusicPath)
	}
}

func TestLoopPlayRPCFailureFallsBackAndQuarantinesRejectedAsset(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	bad := svc.librarySnapshot.Loops[0]
	wantPath := otherLoopPath(t, svc.librarySnapshot.Loops, bad.Path)
	if err := svc.libDB.SavePeriodPlan(ctx, medialib.PeriodPlan{
		Date:   "2026-06-24",
		Period: medialib.PeriodDay,
		Theme:  bad.Theme,
		LoopID: bad.ID,
	}); err != nil {
		t.Fatalf("SavePeriodPlan: %v", err)
	}
	rpcErr := errors.New("asset-specific OBS rejection")
	fakeOBS.playErrByPath = map[string]error{bad.Path: rpcErr}

	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensureLibraryPlayback with alternate: %v", err)
	}
	if svc.activeLoopPath != wantPath {
		t.Fatalf("active loop = %q, want alternate %q", svc.activeLoopPath, wantPath)
	}
	if reason := svc.libraryAssetQuarantineReasonLocked(bad.Path); !strings.Contains(reason, rpcErr.Error()) {
		t.Fatalf("rejected loop quarantine reason = %q", reason)
	}
}

func TestCommonLoopPlayRPCFailureDoesNotQuarantineWholePeriod(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	fakeOBS.playErr = errors.New("source-wide OBS outage")

	if err := svc.ensureLibraryPlayback(ctx, false); err == nil {
		t.Fatal("source-wide OBS failure was swallowed")
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("quarantine count = %d, want rollback after every candidate failed", got)
	}
}

func TestCommonLoopPlayFailureIsBoundedAndRestoresDurableSelection(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	for index := 0; index < maxLibraryPlaybackCandidates+4; index++ {
		writeLibraryFile(
			t,
			svc.cfg.LoopMediaDir,
			fmt.Sprintf("loop_day_theme%02d_v.mp4", index),
		)
	}
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	originalLoop := svc.librarySnapshot.Loops[0]
	date := "2026-06-24"
	if err := svc.libDB.SetDirectLoopOverride(ctx, date, originalLoop.ID); err != nil {
		t.Fatalf("set direct override: %v", err)
	}
	originalPlan := medialib.PeriodPlan{
		Date:   date,
		Period: medialib.PeriodDay,
		Theme:  originalLoop.Theme,
		LoopID: originalLoop.ID,
	}
	if err := svc.libDB.SavePeriodPlan(ctx, originalPlan); err != nil {
		t.Fatalf("save original plan: %v", err)
	}
	fakeOBS.playErr = errors.New("source-wide OBS outage")

	if err := svc.ensureLibraryPlayback(ctx, true); err == nil {
		t.Fatal("source-wide OBS failure was swallowed")
	}
	if got := fakeOBS.playAttemptCalls[svc.cfg.OBSLoopSourceName]; got != maxLibraryPlaybackCandidates {
		t.Fatalf("loop attempts = %d, want bounded budget %d", got, maxLibraryPlaybackCandidates)
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("quarantine count = %d, want rollback after bounded common failure", got)
	}
	override, err := svc.libDB.Override(ctx, date)
	if err != nil {
		t.Fatalf("read restored override: %v", err)
	}
	if override.DirectLoopID != originalLoop.ID {
		t.Fatalf("direct override = %q, want restored %q", override.DirectLoopID, originalLoop.ID)
	}
	plan, ok, err := svc.libDB.PeriodPlan(ctx, date, medialib.PeriodDay)
	if err != nil || !ok {
		t.Fatalf("restored plan ok=%v err=%v", ok, err)
	}
	if plan.Theme != originalPlan.Theme || plan.LoopID != originalPlan.LoopID {
		t.Fatalf("restored plan = %#v, want %#v", plan, originalPlan)
	}
}

func TestCommonLoopPlayFailureRestoresCrossPeriodDirectOverride(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	var morningID string
	for _, loop := range svc.librarySnapshot.Loops {
		if loop.Period == medialib.PeriodMorning {
			morningID = loop.ID
		}
	}
	if morningID == "" {
		t.Fatal("missing cross-period direct loop")
	}
	date := "2026-06-24"
	if err := svc.libDB.SetDirectLoopOverride(ctx, date, morningID); err != nil {
		t.Fatalf("set direct override: %v", err)
	}
	fakeOBS.playErr = errors.New("source-wide OBS outage")

	if err := svc.ensureLibraryPlayback(ctx, true); err == nil {
		t.Fatal("source-wide OBS failure was swallowed")
	}
	override, err := svc.libDB.Override(ctx, date)
	if err != nil {
		t.Fatalf("read restored override: %v", err)
	}
	if override.DirectLoopID != morningID {
		t.Fatalf("cross-period direct override = %q, want restored %q", override.DirectLoopID, morningID)
	}
}

func TestSchedulerRollbackCannotOverwriteNewerOperatorOverride(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	date := "2026-06-24"
	if err := svc.libDB.SetThemeOverride(ctx, date, "cafe"); err != nil {
		t.Fatalf("set initial theme: %v", err)
	}
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	fakeOBS.playStarted = started
	fakeOBS.playRelease = release
	fakeOBS.playErr = errors.New("source-wide OBS outage")

	schedulerDone := make(chan error, 1)
	go func() {
		schedulerDone <- svc.ensureLibraryPlayback(ctx, true)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("scheduler playback attempt did not start")
	}

	operatorDone := make(chan error, 1)
	go func() {
		_, err := svc.SetThemeText(ctx, "study")
		operatorDone <- err
	}()
	select {
	case err := <-operatorDone:
		t.Fatalf("operator mutation bypassed playback serialization: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	beforeRelease, err := svc.libDB.Override(ctx, date)
	if err != nil {
		t.Fatalf("read override while scheduler blocked: %v", err)
	}
	if beforeRelease.Theme != "cafe" {
		t.Fatalf("operator wrote theme %q before acquiring playback lock", beforeRelease.Theme)
	}

	close(release)
	if err := <-schedulerDone; err == nil {
		t.Fatal("scheduler common failure was swallowed")
	}
	if err := <-operatorDone; err == nil {
		t.Fatal("operator playback failure was swallowed")
	}
	finalOverride, err := svc.libDB.Override(ctx, date)
	if err != nil {
		t.Fatalf("read final override: %v", err)
	}
	if finalOverride.Theme != "study" {
		t.Fatalf("final override = %q, want newer operator intent study", finalOverride.Theme)
	}
}

func TestOperatorOverrideDateIsResolvedAfterPlaybackLockWait(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_study_001.mp4")
	tick := fixedNow("2026-06-24T23:59:59+08:00")()
	svc.now = func() time.Time { return tick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}

	svc.playbackMu.Lock()
	commandDone := make(chan error, 1)
	go func() {
		_, err := svc.SetThemeText(ctx, "study")
		commandDone <- err
	}()
	select {
	case err := <-commandDone:
		svc.playbackMu.Unlock()
		t.Fatalf("theme command did not wait for playback serialization: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	tick = fixedNow("2026-06-25T00:00:01+08:00")()
	svc.playbackMu.Unlock()
	if err := <-commandDone; err != nil {
		t.Fatalf("theme command after midnight: %v", err)
	}

	previous, err := svc.libDB.Override(ctx, "2026-06-24")
	if err != nil {
		t.Fatalf("read previous-day override: %v", err)
	}
	current, err := svc.libDB.Override(ctx, "2026-06-25")
	if err != nil {
		t.Fatalf("read current-day override: %v", err)
	}
	if previous.Theme != "" {
		t.Fatalf("previous-day theme = %q, want untouched", previous.Theme)
	}
	if current.Theme != "study" {
		t.Fatalf("current-day theme = %q, want study", current.Theme)
	}
}

func TestMusicPartialPlayMutationFailsClosedAndRollsBackTentativeQuarantine(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	fakeOBS.playErrAfterMutation = errors.New("mute/restart RPC failed after local_file changed")

	svc.playbackMu.Lock()
	err := svc.playNextMusicLocked(ctx, false)
	svc.playbackMu.Unlock()
	if err == nil {
		t.Fatal("partial OBS mutation failure was swallowed")
	}
	if got := fakeOBS.playAttemptCalls[svc.cfg.OBSMusicSourceName]; got != 2 {
		t.Fatalf("music attempts = %d, want both bounded candidates", got)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSMusicSourceName]; got != 2 {
		t.Fatalf("music fail-closed stop calls = %d, want 2", got)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != "" {
		t.Fatalf("uncommitted OBS music candidate still playing: %q", got)
	}
	if svc.activeMusicPath != "" {
		t.Fatalf("active music path = %q, want empty after fail-closed cleanup", svc.activeMusicPath)
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("quarantine count = %d, want rollback after common partial failures", got)
	}
}

func TestPartialMutationCleanupIsBoundedBelowWorkerDrainBudget(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	svc.obsCleanupTimeout = 20 * time.Millisecond
	fakeOBS.playErrAfterMutation = errors.New("post-mutation failure")
	fakeOBS.stopWaitForContext = true

	started := time.Now()
	svc.playbackMu.Lock()
	err := svc.playNextMusicLocked(ctx, false)
	svc.playbackMu.Unlock()
	elapsed := time.Since(started)
	if !errors.Is(err, errOBSPlayCleanupFailed) {
		t.Fatalf("cleanup error = %v, want fail-closed cleanup sentinel", err)
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("cleanup returned after %s, want bounded well below drain budget", elapsed)
	}
	if !fakeOBS.stopDeadlineObserved {
		t.Fatal("fail-closed StopSource did not receive an independent deadline")
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSMusicSourceName]; got != 1 {
		t.Fatalf("cleanup stop calls = %d, want one", got)
	}
}

func TestOBSPlaybackOperationHasOverallDeadline(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	svc.obsPlaybackTimeout = 200 * time.Millisecond
	fakeOBS.playWaitForContext = true

	started := time.Now()
	err := svc.ensureLibraryPlayback(ctx, false)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("playback error = %v, want bounded deadline", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("bounded playback returned after %s, want under 2s", elapsed)
	}
	if !fakeOBS.playDeadlineObserved {
		t.Fatal("OBS PlaySourceFile did not receive a deadline")
	}
	if got := fakeOBS.playAttemptCalls[svc.cfg.OBSLoopSourceName]; got != 1 {
		t.Fatalf("loop attempts = %d, want timeout to abort candidate loop", got)
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("timeout quarantine count = %d, want no asset quarantine", got)
	}
}

func TestReconcileInspectionSharesOverallPlaybackDeadline(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial playback: %v", err)
	}
	svc.obsPlaybackTimeout = 100 * time.Millisecond
	fakeOBS.mediaStatusWaitForContext = true

	started := time.Now()
	err := svc.reconcileLibraryPlayback(ctx)
	elapsed := time.Since(started)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reconcile error = %v, want shared operation deadline", err)
	}
	if elapsed > time.Second {
		t.Fatalf("reconcile returned after %s, want bounded under 1s", elapsed)
	}
}

func TestTransitionTimeoutUsesIndependentFailClosedStopContext(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial morning playback: %v", err)
	}
	tick = fixedNow("2026-06-24T11:00:00+08:00")()
	svc.obsPlaybackTimeout = 100 * time.Millisecond
	fakeOBS.playWaitForContext = true

	err := svc.reconcileLibraryPlayback(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transition error = %v, want playback deadline", err)
	}
	if !fakeOBS.stopDeadlineObserved {
		t.Fatal("expired-loop fail-closed stop did not receive an independent live deadline")
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != "" {
		t.Fatalf("expired morning loop still playing after timeout: %q", got)
	}
	if svc.activeLoopPath != "" {
		t.Fatalf("active loop = %q, want cleared after fail-closed stop", svc.activeLoopPath)
	}
}

func TestTransitionPartialMutationSharesOneFailClosedBudget(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial morning playback: %v", err)
	}
	tick = fixedNow("2026-06-24T11:00:00+08:00")()
	svc.obsCleanupTimeout = 80 * time.Millisecond
	fakeOBS.playErrAfterMutation = errors.New("partial transition mutation")
	fakeOBS.stopWaitForContext = true

	started := time.Now()
	err := svc.reconcileLibraryPlayback(ctx)
	elapsed := time.Since(started)
	if !errors.Is(err, errOBSPlayCleanupFailed) {
		t.Fatalf("transition error = %v, want cleanup failure sentinel", err)
	}
	if elapsed > 250*time.Millisecond {
		t.Fatalf("stacked fail-closed work took %s, want one shared 80ms budget", elapsed)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSLoopSourceName]; got != 2 {
		t.Fatalf("loop stop attempts = %d, want cleanup plus fail-closed retry", got)
	}
}

func TestCanceledReconcileWaiterDoesNotStartFailClosedWorkAfterLock(t *testing.T) {
	svc, fakeOBS := newLibraryTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		attempted bool
		err       error
	}
	started := make(chan struct{})
	resultCh := make(chan result, 1)
	svc.playbackMu.Lock()
	go func() {
		close(started)
		attempted, err := svc.reconcileLibraryPlaybackAttempt(ctx)
		resultCh <- result{attempted: attempted, err: err}
	}()
	<-started
	cancel()
	svc.playbackMu.Unlock()

	select {
	case got := <-resultCh:
		if got.attempted {
			t.Fatal("canceled reconcile waiter reported an OBS attempt")
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled reconcile error = %v, want context canceled", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled reconcile waiter did not return after playback lock release")
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSLoopSourceName]; got != 0 {
		t.Fatalf("canceled reconcile started %d fail-closed stop calls, want none", got)
	}
	if got := fakeOBS.playAttemptCalls[svc.cfg.OBSLoopSourceName]; got != 0 {
		t.Fatalf("canceled reconcile started %d play calls, want none", got)
	}
}

func TestCanceledOBSEventWaiterIsNotReportedAsAttempted(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type result struct {
		attempted bool
		err       error
	}
	started := make(chan struct{})
	resultCh := make(chan result, 1)
	svc.playbackMu.Lock()
	go func() {
		close(started)
		attempted, err := svc.handleLibraryOBSEventAttempt(ctx, obs.Event{
			Type:      obs.EventMediaEnded,
			InputName: svc.cfg.OBSLoopSourceName,
		})
		resultCh <- result{attempted: attempted, err: err}
	}()
	<-started
	cancel()
	svc.playbackMu.Unlock()

	select {
	case got := <-resultCh:
		if got.attempted {
			t.Fatal("canceled OBS event waiter reported an attempt")
		}
		if !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled OBS event error = %v, want context canceled", got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled OBS event waiter did not return after playback lock release")
	}
}

func TestCanceledMusicWaiterDoesNotStartOBSWorkAfterLock(t *testing.T) {
	svc, fakeOBS := newLibraryTestService(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	started := make(chan struct{})
	resultCh := make(chan error, 1)
	svc.playbackMu.Lock()
	go func() {
		close(started)
		resultCh <- svc.playNextMusic(ctx, true)
	}()
	<-started
	cancel()
	svc.playbackMu.Unlock()

	select {
	case err := <-resultCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled music error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled music waiter did not return after playback lock release")
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSMusicSourceName]; got != 0 {
		t.Fatalf("canceled music waiter started %d stop calls, want none", got)
	}
	if got := fakeOBS.playAttemptCalls[svc.cfg.OBSMusicSourceName]; got != 0 {
		t.Fatalf("canceled music waiter started %d play calls, want none", got)
	}
}

func TestPlaybackTimeoutRestoresPlanWithIndependentContext(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}

	const seed = int64(7)
	themes := []string{"cafe", "study"}
	selectedTheme := themes[rand.New(rand.NewSource(seed)).Intn(len(themes))]
	originalTheme := themes[0]
	if originalTheme == selectedTheme {
		originalTheme = themes[1]
	}
	var originalLoop medialib.Loop
	for _, loop := range svc.librarySnapshot.Loops {
		if loop.Theme == originalTheme {
			originalLoop = loop
		}
	}
	if originalLoop.ID == "" {
		t.Fatalf("missing original plan loop for theme %s", originalTheme)
	}
	originalPlan := medialib.PeriodPlan{
		Date:   "2026-06-24",
		Period: medialib.PeriodDay,
		Theme:  originalLoop.Theme,
		LoopID: originalLoop.ID,
	}
	if err := svc.libDB.SavePeriodPlan(ctx, originalPlan); err != nil {
		t.Fatalf("save original plan: %v", err)
	}
	svc.rng = rand.New(rand.NewSource(seed))
	fakeOBS.playWaitForContext = true
	outerCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
	defer cancel()

	svc.playbackMu.Lock()
	err := svc.ensureLibraryPlaybackLocked(outerCtx, true)
	svc.playbackMu.Unlock()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("playback error = %v, want deadline", err)
	}
	restored, ok, err := svc.libDB.PeriodPlan(ctx, originalPlan.Date, originalPlan.Period)
	if err != nil || !ok {
		t.Fatalf("restored plan ok=%v err=%v", ok, err)
	}
	if restored.Theme != originalPlan.Theme || restored.LoopID != originalPlan.LoopID {
		t.Fatalf("restored plan = %#v, want %#v", restored, originalPlan)
	}
}

func TestPlaybackValidationDeadlineDoesNotQuarantineAsset(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	loop := svc.librarySnapshot.Loops[0]
	delete(svc.libraryValidationCache, loop.Path)
	blockingProbe := filepath.Join(t.TempDir(), "ffprobe")
	if err := osWriteFile(blockingProbe, []byte("#!/bin/sh\nexec sleep 10\n"), 0o700); err != nil {
		t.Fatalf("write blocking ffprobe: %v", err)
	}
	svc.media = media.NewManager(blockingProbe)
	svc.obsPlaybackTimeout = 100 * time.Millisecond

	err := svc.ensureLibraryPlayback(ctx, true)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("playback error = %v, want validation deadline", err)
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("quarantine count = %d, want cancellation-neutral validation", got)
	}
}

func TestSamePeriodClockRollbackDoesNotExtendGenerationSettleGrace(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	tick := fixedNow("2026-06-24T12:30:00+08:00")()
	svc.now = func() time.Time { return tick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial playback: %v", err)
	}
	playsBefore := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateStopped,
	}
	tick = fixedNow("2026-06-24T12:00:00+08:00")()

	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("reconcile after same-period rollback: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playsBefore+1 {
		t.Fatalf("loop play calls = %d, want immediate terminal-state recovery %d", got, playsBefore+1)
	}
}

func TestSamePeriodWallClockForwardJumpDoesNotCreateFalseMediaStall(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	wallTick := fixedNow("2026-06-24T12:00:00+08:00")()
	mediaTick := time.Now()
	svc.now = func() time.Time { return wallTick }
	svc.mediaClockNow = func() time.Time { return mediaTick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial playback: %v", err)
	}
	playsBefore := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	activePath := svc.activeLoopPath
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateOpening,
	}
	wallTick = fixedNow("2026-06-24T16:00:00+08:00")()
	mediaTick = mediaTick.Add(time.Second)

	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("reconcile after wall-clock jump: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playsBefore {
		t.Fatalf("loop play calls = %d, want unchanged %d", got, playsBefore)
	}
	if svc.activeLoopPath != activePath {
		t.Fatalf("active loop changed from %q to %q after false stall", activePath, svc.activeLoopPath)
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("quarantine count = %d, want no wall-clock false positive", got)
	}
}

func TestReconnectUsesPublishedSnapshotWithoutWaitingForBlockedRescan(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	tick := fixedNow("2026-06-24T10:59:59+08:00")()
	svc.now = func() time.Time { return tick }
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial morning playback: %v", err)
	}
	published := svc.librarySnapshot

	blockedStarted := make(chan struct{})
	releaseBlocked := make(chan struct{})
	blockedDone := make(chan error, 1)
	go func() {
		blockedDone <- svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
			close(blockedStarted)
			<-releaseBlocked
			return published, nil
		}, false)
	}()
	<-blockedStarted
	tick = fixedNow("2026-06-24T11:00:00+08:00")()

	recoveryDone := make(chan error, 1)
	go func() {
		recoveryDone <- svc.recoverLibraryPlaybackAfterOBSConnect(ctx)
	}()
	select {
	case err := <-recoveryDone:
		if err != nil {
			close(releaseBlocked)
			t.Fatalf("reconnect recovery: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		close(releaseBlocked)
		t.Fatal("reconnect waited for a rescan despite a published snapshot")
	}
	if svc.activeLoopPeriod != medialib.PeriodDay {
		close(releaseBlocked)
		t.Fatalf("active period = %s, want immediate day transition", svc.activeLoopPeriod)
	}
	close(releaseBlocked)
	if err := <-blockedDone; err != nil {
		t.Fatalf("blocked explicit scan after release: %v", err)
	}
}

func TestReconnectRetriesScanAfterInitialIncompleteFailure(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	readErr := &medialib.ScanError{Issues: []*medialib.Error{{
		Code:  medialib.ErrorReadDirectory,
		Kind:  medialib.KindLoop,
		Field: "directory",
		Err:   errors.New("temporary mount failure"),
	}}}
	if err := svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
		return medialib.Library{}, readErr
	}, false); !errors.Is(err, readErr) {
		t.Fatalf("initial scan error = %v, want temporary read failure", err)
	}
	if svc.librarySnapshotPublished {
		t.Fatal("incomplete scan incorrectly marked a complete snapshot as published")
	}

	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("reconnect did not retry repaired initial scan: %v", err)
	}
	if !svc.librarySnapshotPublished || svc.activeLoopPeriod != medialib.PeriodDay {
		t.Fatalf(
			"recovered snapshot published=%v active period=%s",
			svc.librarySnapshotPublished,
			svc.activeLoopPeriod,
		)
	}
}

func TestRemovedActiveLoopFailsClosedWithinSamePeriod(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	path := writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("initial playback: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove active loop: %v", err)
	}
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("rescan after removal: %v", err)
	}

	if err := svc.ensureLibraryPlayback(ctx, false); err == nil {
		t.Fatal("missing same-period loop did not surface a fail-closed error")
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != "" {
		t.Fatalf("removed loop still playing in OBS: %q", got)
	}
	if svc.activeLoopPath != "" {
		t.Fatalf("removed loop remains active in memory: %q", svc.activeLoopPath)
	}
}

func TestLibraryRetryDelayNeverSleepsAcrossPeriodBoundary(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	svc.now = fixedNow("2026-06-24T10:59:50+08:00")
	if got := svc.capLibraryRetryAtPeriodBoundary(5 * time.Minute); got != 10*time.Second {
		t.Fatalf("boundary-capped retry = %s, want 10s", got)
	}
	if got := svc.capLibraryRetryAtPeriodBoundary(5 * time.Second); got != 5*time.Second {
		t.Fatalf("short retry = %s, want unchanged 5s", got)
	}
}

func openLastMusicFailureDB(t *testing.T, ctx context.Context, dbPath string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open failure-injection database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TRIGGER fail_last_music_insert_scheduler
BEFORE INSERT ON library_kv
WHEN NEW.key = 'last_music_id'
BEGIN
	SELECT RAISE(FAIL, 'injected last-music persistence failure');
END;
`); err != nil {
		db.Close()
		t.Fatalf("create failure trigger: %v", err)
	}
	return db
}
