package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	"github.com/tiwb/tg-obs-bot/internal/queue"
	"github.com/tiwb/tg-obs-bot/internal/singleton"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

func TestNewFailsBeforeSQLiteWhenBackendLockIsHeld(t *testing.T) {
	isolateSingletonUserDirectory(t)
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime", "queue.db")
	const token = "123456789:app-lock-test"
	lock, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("hold backend lock: %v", err)
	}
	defer lock.Close()

	started := time.Now()
	service, err := New(config.Config{
		TelegramBotToken: token,
		DataDir:          filepath.Join(root, "data"),
		DatabasePath:     databasePath,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if service != nil {
		service.Close()
		t.Fatal("New returned a service while the backend lock was held")
	}
	if !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("New error = %v, want singleton contention", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("contended startup waited for %s", elapsed)
	}
	if _, err := os.Stat(databasePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SQLite was opened before singleton fencing: %v", err)
	}
}

func TestNewDirectCallerDoesNotInspectSupervisorFD3(t *testing.T) {
	isolateSingletonUserDirectory(t)
	t.Setenv(liveness.SupervisorMarkerEnv, liveness.SupervisorMarkerValue)
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime", "queue.db")
	const token = "123456789:direct-no-fd3"
	lock, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("hold backend lock: %v", err)
	}
	defer lock.Close()

	service, err := New(config.Config{
		TelegramBotToken: token,
		DataDir:          filepath.Join(root, "data"),
		DatabasePath:     databasePath,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if service != nil {
		service.Close()
		t.Fatal("New returned a service while the backend lock was held")
	}
	if !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("direct New error = %v, want singleton contention without FD3 probing", err)
	}
	if errors.Is(err, liveness.ErrInvalidSupervisorFIFO) {
		t.Fatalf("direct New unexpectedly inspected process descriptor 3: %v", err)
	}
}

func TestNewClosesTransferredLivenessSinkOnInitializationFailure(t *testing.T) {
	isolateSingletonUserDirectory(t)
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime", "queue.db")
	const token = "123456789:liveness-option-cleanup"
	lock, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("hold backend lock: %v", err)
	}
	defer lock.Close()

	sink := &appTestFrameSink{}
	service, err := New(
		config.Config{
			TelegramBotToken: token,
			DataDir:          filepath.Join(root, "data"),
			DatabasePath:     databasePath,
		},
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithLivenessSink(sink),
	)
	if service != nil {
		service.Close()
		t.Fatal("New returned a service while the backend lock was held")
	}
	if !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("New error = %v, want singleton contention", err)
	}
	if !sink.closed {
		t.Fatal("failed New did not close transferred liveness sink")
	}
}

func TestNewFencesTelegramIdentityAcrossDifferentRuntimeRoots(t *testing.T) {
	isolateSingletonUserDirectory(t)
	root := t.TempDir()
	firstDatabase := filepath.Join(root, "first-runtime", "queue.db")
	secondDatabase := filepath.Join(root, "second-runtime", "queue.db")
	const token = "123456789:cross-runtime-secret"
	lock, err := singleton.Acquire(firstDatabase, token)
	if err != nil {
		t.Fatalf("hold first backend resource locks: %v", err)
	}
	defer lock.Close()

	service, err := New(config.Config{
		TelegramBotToken: token,
		DataDir:          filepath.Join(root, "second-data"),
		DatabasePath:     secondDatabase,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if service != nil {
		service.Close()
		t.Fatal("New returned a second service for the same Telegram bot")
	}
	if !errors.Is(err, singleton.ErrAlreadyRunning) {
		t.Fatalf("New error = %v, want Telegram identity contention", err)
	}
	if strings.Contains(err.Error(), token) ||
		strings.Contains(err.Error(), "cross-runtime-secret") {
		t.Fatalf("contention error leaked Telegram token: %v", err)
	}
	if _, err := os.Stat(secondDatabase); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("second SQLite database was opened before bot fencing: %v", err)
	}

	// Bot-lock failure occurs after deterministic database-lock acquisition.
	// A different bot must be able to claim that second database immediately,
	// proving the partial database lock was released.
	replacement, err := singleton.Acquire(secondDatabase, "987654321:independent-bot")
	if err != nil {
		t.Fatalf("partial database lock leaked after bot contention: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close partial-acquisition probe: %v", err)
	}
}

func TestNewReleasesBackendLockAfterEarlyInitializationFailure(t *testing.T) {
	isolateSingletonUserDirectory(t)
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime", "queue.db")
	const token = "123456789:app-init-failure"
	dataPath := filepath.Join(root, "data-is-a-file")
	if err := os.WriteFile(dataPath, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create invalid data path: %v", err)
	}

	service, err := New(config.Config{
		TelegramBotToken: token,
		DataDir:          dataPath,
		DatabasePath:     databasePath,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if service != nil {
		service.Close()
		t.Fatal("New returned a service for an invalid data directory")
	}
	if err == nil {
		t.Fatal("New succeeded with an invalid data directory")
	}
	replacement, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("initialization failure leaked backend lock: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close replacement lock: %v", err)
	}
}

func TestServiceCloseReleasesBackendLock(t *testing.T) {
	isolateSingletonUserDirectory(t)
	databasePath := filepath.Join(t.TempDir(), "queue.db")
	const token = "123456789:service-close"
	lock, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("acquire service lock: %v", err)
	}
	service := &Service{
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		instanceLock: lock,
		shutdown:     []func() error{lock.Close},
	}
	service.Close()

	replacement, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("Service.Close did not release backend lock: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close replacement lock: %v", err)
	}
}

func isolateSingletonUserDirectory(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
}

func TestLibraryScanRecordsBoundariesWithoutSyntheticProgress(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	registry := liveness.NewRegistry(liveness.Options{})
	worker, err := registry.Bind(liveness.WorkerTelegram, liveness.OwnerTelegram)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	worker.Advance(liveness.PhaseOperation)
	ctx := liveness.WithWorker(context.Background(), worker)

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	snapshot := worker.Snapshot()
	if snapshot.Phase != liveness.PhaseOperation || snapshot.Sequence != 3 {
		t.Fatalf("scan snapshot = %+v, want entry plus restored normal phase", snapshot)
	}
}

func TestLibraryPreviewMaterializesNextPeriodPlan(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T10:30:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	first, err := svc.PreviewText(ctx)
	if err != nil {
		t.Fatalf("preview: %v", err)
	}
	second, err := svc.PreviewText(ctx)
	if err != nil {
		t.Fatalf("preview again: %v", err)
	}
	if first != second {
		t.Fatalf("preview should be stable after materializing plan:\nfirst=%s\nsecond=%s", first, second)
	}
	for _, want := range []string{"下一時段預告", "時段：白天", "ID：loop_"} {
		if !strings.Contains(first, want) {
			t.Fatalf("preview text = %q, want %q", first, want)
		}
	}

	plan, ok, err := svc.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay)
	if err != nil {
		t.Fatalf("read plan: %v", err)
	}
	if !ok || plan.LoopID == "" {
		t.Fatalf("expected persisted day plan, got ok=%v plan=%#v", ok, plan)
	}
}

func TestLibraryPlaybackKeepsLoopUntilPeriodEnds(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	firstLoop := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]
	if firstLoop == "" {
		t.Fatalf("expected loop source playback")
	}

	svc.rng = rand.New(rand.NewSource(99))
	svc.now = fixedNow("2026-06-24T16:30:00+08:00")
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback again: %v", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != firstLoop {
		t.Fatalf("loop changed within same period: got %q want %q", got, firstLoop)
	}
}

func TestLibraryLoopEndedEventDoesNotRedrawCurrentPeriodPlan(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_002.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	firstPath := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]
	planBefore, ok, err := svc.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay)
	if err != nil || !ok {
		t.Fatalf("plan before ok=%v err=%v", ok, err)
	}

	svc.rng = rand.New(rand.NewSource(42))
	playsBefore := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{Type: obs.EventMediaEnded, InputName: svc.cfg.OBSLoopSourceName, Path: firstPath}); err != nil {
		t.Fatalf("handle loop ended event: %v", err)
	}
	planAfter, ok, err := svc.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay)
	if err != nil || !ok {
		t.Fatalf("plan after ok=%v err=%v", ok, err)
	}
	if planAfter.LoopID != planBefore.LoopID {
		t.Fatalf("loop ended event redrew plan: before=%s after=%s", planBefore.LoopID, planAfter.LoopID)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != firstPath {
		t.Fatalf("loop ended event restarted %q, want existing plan path %q", got, firstPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playsBefore+1 {
		t.Fatalf("loop play calls = %d, want %d after ended event", got, playsBefore+1)
	}
}

func TestLibraryReconnectReplaysCachedLoopAndMusic(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	loopPath := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]
	musicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]

	fakeOBS.sourcePlayed = make(map[string]string)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateNone}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateNone}
	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover library playback: %v", err)
	}

	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != loopPath {
		t.Fatalf("recovered loop = %q, want cached %q", got, loopPath)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != musicPath {
		t.Fatalf("recovered music = %q, want cached %q", got, musicPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("loop play calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls+1 {
		t.Fatalf("music play calls = %d, want %d", got, musicCalls+1)
	}
}

func TestLibraryReconnectKeepsHealthyCachedSourcesPlaying(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	loopID := svc.activeLoopID
	musicID := svc.activeMusicID
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}

	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover library playback: %v", err)
	}

	if svc.activeLoopID != loopID || svc.activeMusicID != musicID {
		t.Fatalf("healthy reconnect changed active IDs: loop %q -> %q, music %q -> %q", loopID, svc.activeLoopID, musicID, svc.activeMusicID)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls {
		t.Fatalf("healthy reconnect loop calls = %d, want unchanged %d", got, loopCalls)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("healthy reconnect music calls = %d, want unchanged %d", got, musicCalls)
	}
}

func TestLibraryReconnectStartsBothSourcesWithoutCachedState(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover fresh library playback: %v", err)
	}

	if svc.activeLoopID == "" || svc.activeMusicID == "" {
		t.Fatalf("fresh recovery did not establish both active IDs: loop=%q music=%q", svc.activeLoopID, svc.activeMusicID)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != 1 {
		t.Fatalf("fresh recovery loop calls = %d, want 1", got)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != 1 {
		t.Fatalf("fresh recovery music calls = %d, want 1", got)
	}
}

func TestLibraryReconnectRecoversOnlyAffectedSource(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}

	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateStopped}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover stopped loop: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("stopped loop calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("healthy music calls = %d, want unchanged %d", got, musicCalls)
	}

	loopCalls = fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls = fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateError}
	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover errored music: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls {
		t.Fatalf("healthy loop calls = %d, want unchanged %d", got, loopCalls)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls+1 {
		t.Fatalf("errored music calls = %d, want %d", got, musicCalls+1)
	}
}

func TestLibraryReconciliationRecoversLostEndedEvents(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	loopPath := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]
	musicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateStopped}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}

	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("reconcile library playback: %v", err)
	}

	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != loopPath {
		t.Fatalf("recovered loop = %q, want %q", got, loopPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("loop play calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got == "" || got == musicPath {
		t.Fatalf("music after ended state = %q, want a different track from %q", got, musicPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls+1 {
		t.Fatalf("music play calls = %d, want %d", got, musicCalls+1)
	}
}

func TestLibraryTerminalSettlingGuardPreventsDoubleAdvanceAndLoopRestart(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	firstMusicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("first terminal reconciliation: %v", err)
	}
	secondMusicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	if secondMusicPath == "" || secondMusicPath == firstMusicPath {
		t.Fatalf("first terminal reconciliation music = %q, want different from %q", secondMusicPath, firstMusicPath)
	}
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]

	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{
		Type:      obs.EventMediaEnded,
		InputName: svc.cfg.OBSMusicSourceName,
		Path:      firstMusicPath,
	}); err != nil {
		t.Fatalf("stale music ended event: %v", err)
	}
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("immediate stale scheduler reconciliation: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls {
		t.Fatalf("stale terminal restarted loop %d times, want unchanged %d", got, loopCalls)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("stale terminal advanced music %d times, want unchanged %d", got, musicCalls)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != secondMusicPath {
		t.Fatalf("stale terminal changed music to %q, want %q", got, secondMusicPath)
	}

	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("post-grace terminal reconciliation: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("post-grace loop calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls+1 {
		t.Fatalf("post-grace music calls = %d, want %d", got, musicCalls+1)
	}
}

func TestLibrarySettlingGuardDefersTransientPathMismatch(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	firstMusicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("establish next generation: %v", err)
	}
	secondMusicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	if secondMusicPath == "" || secondMusicPath == firstMusicPath {
		t.Fatalf("next generation music = %q, want different from %q", secondMusicPath, firstMusicPath)
	}
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	staleLoopPath := filepath.Join(t.TempDir(), "old-loop.mp4")

	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.inputFiles[svc.cfg.OBSLoopSourceName] = staleLoopPath
	fakeOBS.inputFiles[svc.cfg.OBSMusicSourceName] = firstMusicPath
	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{
		Type:      obs.EventMediaEnded,
		InputName: svc.cfg.OBSMusicSourceName,
		Path:      firstMusicPath,
	}); err != nil {
		t.Fatalf("settling mismatched music event: %v", err)
	}
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("settling mismatched scheduler reconciliation: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls {
		t.Fatalf("settling mismatch restarted loop %d times, want unchanged %d", got, loopCalls)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("settling mismatch replayed music %d times, want unchanged %d", got, musicCalls)
	}

	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("post-grace mismatched reconciliation: %v", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("post-grace loop replay calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls+1 {
		t.Fatalf("post-grace music replay calls = %d, want %d", got, musicCalls+1)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != secondMusicPath {
		t.Fatalf("post-grace mismatch changed music to %q, want replay %q", got, secondMusicPath)
	}
}

func TestLibraryReconciliationTreatsActiveStatesAsHealthy(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}

	for _, state := range []obs.MediaState{obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering} {
		t.Run(string(state), func(t *testing.T) {
			loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
			musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
			fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: state}
			fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: state}

			if err := svc.reconcileLibraryPlayback(ctx); err != nil {
				t.Fatalf("reconcile library playback: %v", err)
			}
			if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls {
				t.Fatalf("loop play calls = %d, want unchanged %d", got, loopCalls)
			}
			if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
				t.Fatalf("music play calls = %d, want unchanged %d", got, musicCalls)
			}
		})
	}
}

func TestLibraryReconciliationReplaysSourceWhenLocalFileMismatches(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.inputFiles[svc.cfg.OBSLoopSourceName] = "/stale/loop.mp4"
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)

	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("reconcile library playback: %v", err)
	}

	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("mismatched loop play calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != svc.activeLoopPath {
		t.Fatalf("replayed loop = %q, want active %q", got, svc.activeLoopPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("matching music play calls = %d, want unchanged %d", got, musicCalls)
	}
}

func TestLibraryReconciliationOpeningSourceStallsAfterGrace(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	loopCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	musicCursor := 100.0
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateOpening}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{
		State:              obs.MediaStatePlaying,
		CursorMilliseconds: &musicCursor,
	}

	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("initial reconcile: %v", err)
	}
	tick = tick.Add(mediaProgressGrace + time.Second)
	musicCursor = 1000
	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("stalled reconcile: %v", err)
	}

	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls+1 {
		t.Fatalf("stalled loop play calls = %d, want %d", got, loopCalls+1)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("progressing music play calls = %d, want unchanged %d", got, musicCalls)
	}
}

func TestLibraryReconciliationResumesPausedMusicWithoutSkipping(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	musicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStatePaused}

	if err := svc.reconcileLibraryPlayback(ctx); err != nil {
		t.Fatalf("reconcile library playback: %v", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != musicPath {
		t.Fatalf("resumed music = %q, want current track %q", got, musicPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls+1 {
		t.Fatalf("music play calls = %d, want %d", got, musicCalls+1)
	}
}

func TestLibraryEndedEventUsesCurrentOBSStateToAvoidStaleAdvance(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	musicPath := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}

	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{
		Type:      obs.EventMediaEnded,
		InputName: svc.cfg.OBSMusicSourceName,
		Path:      musicPath,
	}); err != nil {
		t.Fatalf("handle stale music event: %v", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != musicPath {
		t.Fatalf("music changed after stale event: got %q want %q", got, musicPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("music play calls = %d, want unchanged %d", got, musicCalls)
	}
}

func TestLibraryThemeOverrideAndDirectSelect(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_evening_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_evening_study_001.mp4")
	svc.now = fixedNow("2026-06-24T18:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if text, err := svc.SetThemeText(ctx, "study"); err != nil || !strings.Contains(text, "study") {
		t.Fatalf("set theme text=%q err=%v", text, err)
	}
	if got := filepath.Base(fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]); got != "loop_evening_study_001.mp4" {
		t.Fatalf("theme playback = %q, want study loop", got)
	}

	var cafeID string
	for _, loop := range svc.librarySnapshot.Loops {
		if loop.Theme == "cafe" {
			cafeID = loop.ID
		}
	}
	if cafeID == "" {
		t.Fatal("missing cafe loop id")
	}
	if text, err := svc.SelectLoopText(ctx, cafeID); err != nil || !strings.Contains(text, "loop_evening_cafe_001.mp4") {
		t.Fatalf("select text=%q err=%v", text, err)
	}
	if got := filepath.Base(fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]); got != "loop_evening_cafe_001.mp4" {
		t.Fatalf("direct select playback = %q, want cafe loop", got)
	}
}

func TestLibraryThemeOverrideExpiresAtMidnightDuringNightPeriod(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_study_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T23:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if _, err := svc.SetThemeText(ctx, "study"); err != nil {
		t.Fatalf("set theme: %v", err)
	}
	previous, err := svc.libDB.Override(ctx, "2026-06-24")
	if err != nil || previous.Theme != "study" {
		t.Fatalf("previous override = %#v err=%v, want study", previous, err)
	}

	svc.now = fixedNow("2026-06-25T00:30:00+08:00")
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure after midnight: %v", err)
	}
	cleared, err := svc.libDB.Override(ctx, "2026-06-24")
	if err != nil {
		t.Fatalf("read cleared override: %v", err)
	}
	if cleared.Theme != "" || cleared.DirectLoopID != "" {
		t.Fatalf("previous-day override should be cleared after midnight, got %#v", cleared)
	}
}

func TestLibraryDirectOverrideExpiresAtMidnightDuringNightPeriod(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_study_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_night_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T23:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	var studyID string
	for _, loop := range svc.librarySnapshot.Loops {
		if loop.Theme == "study" {
			studyID = loop.ID
		}
	}
	if studyID == "" {
		t.Fatal("missing study loop id")
	}
	if _, err := svc.SelectLoopText(ctx, studyID); err != nil {
		t.Fatalf("select direct loop: %v", err)
	}
	previous, err := svc.libDB.Override(ctx, "2026-06-24")
	if err != nil || previous.DirectLoopID != studyID {
		t.Fatalf("previous direct override = %#v err=%v, want %s", previous, err, studyID)
	}

	svc.now = fixedNow("2026-06-25T00:30:00+08:00")
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure after midnight: %v", err)
	}
	cleared, err := svc.libDB.Override(ctx, "2026-06-24")
	if err != nil {
		t.Fatalf("read cleared override: %v", err)
	}
	if cleared.Theme != "" || cleared.DirectLoopID != "" {
		t.Fatalf("previous-day direct override should be cleared after midnight, got %#v", cleared)
	}
}

func TestLibraryMusicSkipAvoidsImmediateRepeat(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	first := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	if first == "" {
		t.Fatalf("expected music playback")
	}
	if _, err := svc.SkipMusicText(ctx); err != nil {
		t.Fatalf("skip music: %v", err)
	}
	second := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]
	if second == "" || second == first {
		t.Fatalf("music should switch without immediate repeat: first=%q second=%q", first, second)
	}
}

func TestLibraryImportCopiesValidAssetsAndRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_001.mp4")

	text, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_001.mp4",
		SizeBytes: 5,
	})
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if !strings.Contains(text, "已匯入素材") {
		t.Fatalf("import text = %q", text)
	}
	dest := filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	if !fileExists(dest) {
		t.Fatalf("expected imported file at %s", dest)
	}
	if _, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_001.mp4",
		SizeBytes: 5,
	}); err == nil {
		t.Fatalf("expected duplicate import rejection")
	}
}

func TestRandomFallbackStartsAndNotifiesOnce(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "random_played"})
	played := addPlayedVideo(t, ctx, svc, "history.mp4", true)

	if video, err := svc.advancePlayback(ctx); err != nil || video != nil {
		t.Fatalf("first advance video=%#v err=%v", video, err)
	}
	if fakeOBS.lastPlayed != played.LocalPath {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, played.LocalPath)
	}
	if len(fakeBot.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(fakeBot.messages))
	}

	if video, err := svc.advancePlayback(ctx); err != nil || video != nil {
		t.Fatalf("second advance video=%#v err=%v", video, err)
	}
	if len(fakeBot.messages) != 1 {
		t.Fatalf("messages after rotation = %d, want 1", len(fakeBot.messages))
	}
}

func TestFallbackSettlingGuardPreventsDoubleAdvanceAfterEndedEvent(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "file"})
	firstPath := filepath.Join(t.TempDir(), "fallback-one.mp4")
	secondPath := filepath.Join(t.TempDir(), "fallback-two.mp4")
	writeTestFile(t, firstPath)
	writeTestFile(t, secondPath)
	tick := time.Now().UTC()
	svc.now = func() time.Time { return tick }
	svc.cfg.OBSFallbackFile = firstPath
	if _, err := svc.advancePlayback(ctx); err != nil {
		t.Fatalf("start fallback: %v", err)
	}
	svc.cfg.OBSFallbackFile = secondPath
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}

	video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
		Type: obs.EventMediaEnded,
		Path: firstPath,
		At:   time.Now(),
	})
	if err != nil {
		t.Fatalf("fallback ended event: %v", err)
	}
	if video != nil {
		t.Fatalf("fallback event video = %#v, want nil", video)
	}
	if fakeOBS.lastPlayed != secondPath {
		t.Fatalf("played path = %q, want second fallback %q", fakeOBS.lastPlayed, secondPath)
	}
	if fakeOBS.playFileCalls != 2 {
		t.Fatalf("play calls = %d, want initial and first ended event", fakeOBS.playFileCalls)
	}

	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	fakeOBS.inputFiles[svc.cfg.OBSMediaSourceName] = firstPath
	if _, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{Type: obs.EventMediaEnded, Path: firstPath}); err != nil {
		t.Fatalf("stale fallback event with playing status: %v", err)
	}
	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("immediate fallback watchdog with terminal status: %v", err)
	}
	if fakeOBS.playFileCalls != 2 || fakeOBS.lastPlayed != secondPath {
		t.Fatalf("immediate stale hints replayed fallback: calls/path = %d/%q, want 2/%q", fakeOBS.playFileCalls, fakeOBS.lastPlayed, secondPath)
	}

	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	if _, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{Type: obs.EventMediaEnded, Path: secondPath}); err != nil {
		t.Fatalf("post-grace fallback ended event: %v", err)
	}
	if fakeOBS.playFileCalls != 3 || fakeOBS.lastPlayed != secondPath {
		t.Fatalf("post-grace fallback recovery calls/path = %d/%q, want 3/%q", fakeOBS.playFileCalls, fakeOBS.lastPlayed, secondPath)
	}
}

func TestReadyQueueTakesPriorityAfterFallbackEnds(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "random_played"})
	_ = addPlayedVideo(t, ctx, svc, "history.mp4", true)

	if _, err := svc.advancePlayback(ctx); err != nil {
		t.Fatalf("start fallback: %v", err)
	}
	ready := addReadyVideo(t, ctx, svc, "ready.mp4")

	video, err := svc.advancePlayback(ctx)
	if err != nil {
		t.Fatalf("advance to ready: %v", err)
	}
	if video == nil || video.ID != ready.ID {
		t.Fatalf("video = %#v, want ready id %d", video, ready.ID)
	}
	if fakeOBS.lastPlayed != ready.LocalPath {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, ready.LocalPath)
	}
	if svc.playbackState() != playbackNormal {
		t.Fatalf("playback state = %s, want %s", svc.playbackState(), playbackNormal)
	}
}

func TestCleanupRetentionSkipsCurrentRandomFallback(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:      "random_played",
		RetentionMaxFiles: 1,
		RetentionDays:     0,
	})
	locked := addPlayedVideo(t, ctx, svc, "locked.mp4", true)
	time.Sleep(time.Millisecond)
	_ = addPlayedVideo(t, ctx, svc, "other.mp4", true)
	svc.setPlaybackState(playbackRandom, locked.ID, locked.LocalPath)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := svc.store.Get(ctx, locked.ID); err != nil {
		t.Fatalf("locked fallback row should remain: %v", err)
	}
	if !fileExists(locked.LocalPath) {
		t.Fatalf("locked fallback file should remain")
	}
}

func TestCleanupRetentionDoesNotDeleteLocalBotAPIFile(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:      "random_played",
		RetentionMaxFiles: 1,
		RetentionDays:     0,
	})
	removeCandidate := addPlayedVideo(t, ctx, svc, "old.mp4", true)
	time.Sleep(time.Millisecond)
	keepCandidate := addPlayedVideo(t, ctx, svc, "new.mp4", true)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := svc.store.Get(ctx, removeCandidate.ID); err == nil {
		t.Fatalf("old row should be removed")
	}
	if _, err := svc.store.Get(ctx, keepCandidate.ID); err != nil {
		t.Fatalf("new row should remain: %v", err)
	}
	if !fileExists(removeCandidate.LocalPath) {
		t.Fatalf("local bot api file should remain after retention removes row")
	}
}

func TestCleanupRetentionDeletesLocalBotAPIFileWhenEnabled(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:              "random_played",
		RetentionMaxFiles:         1,
		RetentionDays:             0,
		RetentionDeleteLocalFiles: true,
	})
	removeCandidate := addPlayedVideo(t, ctx, svc, "old.mp4", true)
	time.Sleep(time.Millisecond)
	keepCandidate := addPlayedVideo(t, ctx, svc, "new.mp4", true)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := svc.store.Get(ctx, removeCandidate.ID); err == nil {
		t.Fatalf("old row should be removed")
	}
	if _, err := svc.store.Get(ctx, keepCandidate.ID); err != nil {
		t.Fatalf("new row should remain: %v", err)
	}
	if fileExists(removeCandidate.LocalPath) {
		t.Fatalf("local bot api file should be deleted when retention file deletion is enabled")
	}
	if !fileExists(keepCandidate.LocalPath) {
		t.Fatalf("kept local bot api file should remain")
	}
}

func TestCleanupRetentionKeepsRowWhenOptInFileDeleteFailsAndRetries(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:              "random_played",
		RetentionMaxFiles:         1,
		RetentionDays:             0,
		RetentionDeleteLocalFiles: true,
	})
	removeCandidate := addPlayedVideo(t, ctx, svc, "retry-old.mp4", true)
	_ = addPlayedVideo(t, ctx, svc, "retry-new.mp4", true)
	removeErr := errors.New("simulated remove permission error")
	svc.removeFile = func(string) error {
		return removeErr
	}

	if err := svc.CleanupRetention(ctx); !errors.Is(err, removeErr) {
		t.Fatalf("first cleanup error = %v, want %v", err, removeErr)
	}
	if _, err := svc.store.Get(ctx, removeCandidate.ID); err != nil {
		t.Fatalf("row must remain after file removal failure: %v", err)
	}
	if !fileExists(removeCandidate.LocalPath) {
		t.Fatal("file should remain after simulated removal failure")
	}

	svc.removeFile = media.RemoveFile
	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	if _, err := svc.store.Get(ctx, removeCandidate.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("row after retry error = %v, want sql.ErrNoRows", err)
	}
	if fileExists(removeCandidate.LocalPath) {
		t.Fatal("file should be removed after successful retry")
	}
}

func TestCleanupRetentionDoesNotDeleteSharedLocalPath(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:              "random_played",
		RetentionMaxFiles:         1,
		RetentionDays:             0,
		RetentionDeleteLocalFiles: true,
	})
	sharedPath := filepath.Join(svc.cfg.TelegramBotAPIDir, "shared.mp4")
	writeTestFile(t, sharedPath)
	removeCandidate := addPlayedVideoWithPath(t, ctx, svc, "old-shared.mp4", sharedPath)
	time.Sleep(time.Millisecond)
	keepCandidate := addPlayedVideoWithPath(t, ctx, svc, "new-shared.mp4", sharedPath)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := svc.store.Get(ctx, removeCandidate.ID); err == nil {
		t.Fatalf("old row should be removed")
	}
	if _, err := svc.store.Get(ctx, keepCandidate.ID); err != nil {
		t.Fatalf("new row should remain: %v", err)
	}
	if !fileExists(sharedPath) {
		t.Fatalf("shared local bot api file should remain while another row references it")
	}
}

func TestCleanupRetentionSerializesWithFallbackSelectionAndProtectsSharedActivePath(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:              "random_played",
		RetentionMaxFiles:         1,
		RetentionDays:             0,
		RetentionDeleteLocalFiles: true,
	})
	sharedPath := filepath.Join(svc.cfg.TelegramBotAPIDir, "active-shared.mp4")
	writeTestFile(t, sharedPath)
	active := addPlayedVideoWithPath(t, ctx, svc, "active.mp4", sharedPath)
	duplicate := addPlayedVideoWithPath(t, ctx, svc, "duplicate.mp4", sharedPath)
	removeCandidate := addPlayedVideo(t, ctx, svc, "remove.mp4", true)

	svc.playbackMu.Lock()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- svc.CleanupRetention(ctx)
	}()
	<-started
	select {
	case err := <-done:
		svc.playbackMu.Unlock()
		t.Fatalf("cleanup bypassed playback lock: %v", err)
	default:
	}
	svc.setPlaybackState(playbackRandom, active.ID, active.LocalPath)
	svc.playbackMu.Unlock()

	if err := <-done; err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if _, err := svc.store.Get(ctx, active.ID); err != nil {
		t.Fatalf("active row should remain: %v", err)
	}
	for _, video := range []queue.Video{duplicate, removeCandidate} {
		if _, err := svc.store.Get(ctx, video.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("unprotected row #%d get error = %v, want sql.ErrNoRows", video.ID, err)
		}
	}
	if !fileExists(sharedPath) {
		t.Fatal("active fallback path should remain")
	}
}

func TestCleanupRetentionDuplicateActivePathsDoNotStarveNewerRows(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{
		FallbackMode:              "random_played",
		RetentionMaxFiles:         1,
		RetentionDays:             0,
		RetentionDeleteLocalFiles: true,
	})
	sharedPath := filepath.Join(svc.cfg.TelegramBotAPIDir, "many-active-shared.mp4")
	writeTestFile(t, sharedPath)
	active := addPlayedVideoWithPath(t, ctx, svc, "many-active.mp4", sharedPath)
	for i := 0; i < retentionBatchSize/2+12; i++ {
		_ = addPlayedVideoWithPath(t, ctx, svc, fmt.Sprintf("duplicate-active-%03d.mp4", i), sharedPath)
	}
	svc.setPlaybackState(playbackRandom, active.ID, active.LocalPath)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	count, err := svc.store.TerminalCount(ctx, queue.StatusPlayed)
	if err != nil {
		t.Fatalf("played count: %v", err)
	}
	if count != 1 {
		t.Fatalf("duplicate active paths blocked retention: played count=%d, want 1", count)
	}
	if _, err := svc.store.Get(ctx, active.ID); err != nil {
		t.Fatalf("active row should remain: %v", err)
	}
	if !fileExists(sharedPath) {
		t.Fatal("shared active file must not be deleted with duplicate rows")
	}
}

func TestCleanupRetentionHardCapsEachMaintenanceAndEventuallyBoundsFailedCanceled(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newFallbackTestServiceAtDBPath(t, config.Config{
		RetentionMaxFiles: 10,
		RetentionDays:     0,
	}, dbPath)
	seedTerminalRowsAtDBPath(t, ctx, dbPath, queue.StatusFailed, 300)
	seedTerminalRowsAtDBPath(t, ctx, dbPath, queue.StatusCanceled, 300)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	count, err := svc.store.TerminalCount(ctx, queue.StatusFailed, queue.StatusCanceled)
	if err != nil {
		t.Fatalf("terminal count after first cleanup: %v", err)
	}
	if count != 600-retentionBatchSize {
		t.Fatalf("first cleanup removed %d rows, want hard cap %d", 600-count, retentionBatchSize)
	}
	for i := 0; i < 2; i++ {
		if err := svc.CleanupRetention(ctx); err != nil {
			t.Fatalf("cleanup pass %d: %v", i+2, err)
		}
	}
	count, err = svc.store.TerminalCount(ctx, queue.StatusFailed, queue.StatusCanceled)
	if err != nil {
		t.Fatalf("terminal count after convergence: %v", err)
	}
	if count != 10 {
		t.Fatalf("terminal count after convergence = %d, want 10", count)
	}
}

func TestCleanupRetentionGroupsCannotStarveEachOther(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newFallbackTestServiceAtDBPath(t, config.Config{
		RetentionMaxFiles: 300,
		RetentionDays:     0,
	}, dbPath)
	seedTerminalRowsAtDBPath(t, ctx, dbPath, queue.StatusFailed, 256)
	seedTerminalRowsAtDBPath(t, ctx, dbPath, queue.StatusPlayed, 400)

	if err := svc.CleanupRetention(ctx); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	failedCount, err := svc.store.TerminalCount(ctx, queue.StatusFailed, queue.StatusCanceled)
	if err != nil {
		t.Fatalf("failed count: %v", err)
	}
	if failedCount != 256 {
		t.Fatalf("ineligible failed rows changed: got %d, want 256", failedCount)
	}
	playedCount, err := svc.store.TerminalCount(ctx, queue.StatusPlayed)
	if err != nil {
		t.Fatalf("played count: %v", err)
	}
	if playedCount >= 400 {
		t.Fatalf("older ineligible failed rows starved played cleanup: played count=%d", playedCount)
	}
}

func TestMissingRandomFallbackUsesStaticFile(t *testing.T) {
	ctx := context.Background()
	staticPath := filepath.Join(t.TempDir(), "fallback.mp4")
	writeTestFile(t, staticPath)
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{
		FallbackMode:    "random_played",
		OBSFallbackFile: staticPath,
	})
	_ = addPlayedVideo(t, ctx, svc, "missing.mp4", false)

	if video, err := svc.advancePlayback(ctx); err != nil || video != nil {
		t.Fatalf("advance video=%#v err=%v", video, err)
	}
	if fakeOBS.lastPlayed != staticPath {
		t.Fatalf("played path = %q, want static fallback %q", fakeOBS.lastPlayed, staticPath)
	}
	if svc.playbackState() != playbackFile {
		t.Fatalf("playback state = %s, want %s", svc.playbackState(), playbackFile)
	}
}

func TestRandomFallbackSearchesPastNewestInvalidCandidates(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "random_played"})
	validOlder := addPlayedVideo(t, ctx, svc, "valid-older.mp4", true)
	time.Sleep(time.Millisecond)
	for i := 0; i < 101; i++ {
		_ = addPlayedVideo(t, ctx, svc, fmt.Sprintf("missing-newer-%03d.mp4", i), false)
	}

	video, err := svc.playRandomFallbackLocked(ctx)
	if err != nil {
		t.Fatalf("play random fallback: %v", err)
	}
	if video == nil || video.ID != validOlder.ID {
		t.Fatalf("fallback video = %#v, want older valid id %d", video, validOlder.ID)
	}
	if fakeOBS.lastPlayed != validOlder.LocalPath {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, validOlder.LocalPath)
	}
}

func TestRandomFallbackQuarantinesAtMostOneBoundedBatchPerAttempt(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "random_played"})
	validOlder := addPlayedVideo(t, ctx, svc, "valid-beyond-first-batch.mp4", true)
	for i := 0; i < queue.MaxFallbackCandidates+44; i++ {
		_ = addPlayedVideo(t, ctx, svc, fmt.Sprintf("missing-bounded-%03d.mp4", i), false)
	}

	video, err := svc.playRandomFallbackLocked(ctx)
	if err != nil {
		t.Fatalf("first bounded fallback attempt: %v", err)
	}
	if video != nil {
		t.Fatalf("first bounded fallback attempt = %#v, want nil", video)
	}
	failedCount, err := svc.store.TerminalCount(ctx, queue.StatusFailed)
	if err != nil {
		t.Fatalf("failed count: %v", err)
	}
	if failedCount != queue.MaxFallbackCandidates {
		t.Fatalf("quarantined %d rows, want one bounded batch of %d", failedCount, queue.MaxFallbackCandidates)
	}

	video, err = svc.playRandomFallbackLocked(ctx)
	if err != nil {
		t.Fatalf("second bounded fallback attempt: %v", err)
	}
	if video == nil || video.ID != validOlder.ID {
		t.Fatalf("second fallback = %#v, want older valid id %d", video, validOlder.ID)
	}
	if fakeOBS.lastPlayed != validOlder.LocalPath {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, validOlder.LocalPath)
	}
}

func TestFallbackFileModeAndOffMode(t *testing.T) {
	ctx := context.Background()
	staticPath := filepath.Join(t.TempDir(), "fallback.mp4")
	writeTestFile(t, staticPath)
	fileSvc, fileOBS, _ := newFallbackTestService(t, config.Config{
		FallbackMode:    "file",
		OBSFallbackFile: staticPath,
	})
	if video, err := fileSvc.advancePlayback(ctx); err != nil || video != nil {
		t.Fatalf("file mode advance video=%#v err=%v", video, err)
	}
	if fileOBS.lastPlayed != staticPath {
		t.Fatalf("file mode played path = %q, want %q", fileOBS.lastPlayed, staticPath)
	}

	offSvc, offOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	if video, err := offSvc.advancePlayback(ctx); err != nil || video != nil {
		t.Fatalf("off mode advance video=%#v err=%v", video, err)
	}
	if offOBS.lastPlayed != "" {
		t.Fatalf("off mode should not play fallback, got %q", offOBS.lastPlayed)
	}
}

func TestEnqueueUploadUsesLocalPath(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
	})
	path := writeBotAPIFile(t, svc, "upload.mp4")

	video, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        path,
		TelegramFileID:   "file",
		TelegramUniqueID: "unique",
		FileName:         "upload.mp4",
		SizeBytes:        5,
	})
	if err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}
	if video.LocalPath != path {
		t.Fatalf("local path = %q, want %q", video.LocalPath, path)
	}
	if video.DurationSeconds != 60 {
		t.Fatalf("duration = %d, want 60", video.DurationSeconds)
	}
	if fakeOBS.lastPlayed != path {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, path)
	}
}

func TestEnqueueUploadMarksFailedWhenProbeFails(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
	})
	manager, err := media.NewManager(t.TempDir(), fakeFailingFFProbe(t))
	if err != nil {
		t.Fatalf("new media manager: %v", err)
	}
	svc.media = manager
	path := writeBotAPIFile(t, svc, "bad-probe.mp4")

	_, err = svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        path,
		TelegramFileID:   "file",
		TelegramUniqueID: "unique",
		FileName:         "bad-probe.mp4",
		SizeBytes:        5,
	})
	if err == nil {
		t.Fatalf("enqueue upload should fail")
	}
	assertFailedUploadVisible(t, ctx, svc, "bad-probe.mp4", "ffprobe failed")
}

func TestEnqueueUploadMarksFailedWhenValidateFails(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 30,
	})
	path := writeBotAPIFile(t, svc, "too-long.mp4")

	_, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        path,
		TelegramFileID:   "file",
		TelegramUniqueID: "unique",
		FileName:         "too-long.mp4",
		SizeBytes:        5,
	})
	if err == nil {
		t.Fatalf("enqueue upload should fail")
	}
	assertFailedUploadVisible(t, ctx, svc, "too-long.mp4", "video exceeds max duration")
}

func TestUploadFailureFinalizationIgnoresCallerCancellationAndCannotOverwriteCancel(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{})
	path := writeBotAPIFile(t, svc, "finalize.mp4")
	downloading, err := svc.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   "finalize",
		TelegramUniqueID: "finalize",
		FileName:         "finalize.mp4",
		LocalPath:        path,
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	svc.markUploadFailed(canceledCtx, downloading.ID, errors.New("caller canceled"))
	failed, err := svc.store.Get(ctx, downloading.ID)
	if err != nil {
		t.Fatalf("get finalized upload: %v", err)
	}
	if failed.Status != queue.StatusFailed || failed.FinishedAt == nil {
		t.Fatalf("finalized upload = %#v, want failed with finished_at", failed)
	}

	canceledUpload, err := svc.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   "already-canceled",
		TelegramUniqueID: "already-canceled",
		FileName:         "already-canceled.mp4",
		LocalPath:        path,
	})
	if err != nil {
		t.Fatalf("add canceled upload: %v", err)
	}
	if err := svc.store.Cancel(ctx, canceledUpload.ID); err != nil {
		t.Fatalf("cancel upload: %v", err)
	}
	svc.markUploadFailed(ctx, canceledUpload.ID, errors.New("late probe failure"))
	storedCanceled, err := svc.store.Get(ctx, canceledUpload.ID)
	if err != nil {
		t.Fatalf("get canceled upload: %v", err)
	}
	if storedCanceled.Status != queue.StatusCanceled || storedCanceled.Error != "" {
		t.Fatalf("late failure overwrote canceled row: %#v", storedCanceled)
	}
}

func TestEnqueueUploadFinalizesDownloadingWhenSQLiteMarkReadyFails(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newLocalUploadTestServiceAtDBPath(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
	}, dbPath)
	path := writeBotAPIFile(t, svc, "sqlite-full.mp4")

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite trigger connection: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TRIGGER fail_mark_ready
BEFORE UPDATE OF status ON videos
WHEN OLD.status = 'downloading' AND NEW.status = 'ready'
BEGIN
	SELECT RAISE(ABORT, 'database or disk is full');
END;
`); err != nil {
		_ = db.Close()
		t.Fatalf("create mark-ready failure trigger: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close sqlite trigger connection: %v", err)
	}

	_, err = svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        path,
		TelegramFileID:   "sqlite-full",
		TelegramUniqueID: "sqlite-full",
		FileName:         "sqlite-full.mp4",
		SizeBytes:        5,
	})
	if err == nil || !strings.Contains(err.Error(), "database or disk is full") {
		t.Fatalf("enqueue error = %v, want simulated SQLITE_FULL", err)
	}
	assertFailedUploadVisible(t, ctx, svc, "sqlite-full.mp4", "constraint failed: database or disk is full")
}

func TestPeriodicMaintenanceFailsStaleDownloadingRows(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newFallbackTestServiceAtDBPath(t, config.Config{}, dbPath)
	video, err := svc.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   "stale-periodic",
		TelegramUniqueID: "stale-periodic",
		FileName:         "stale-periodic.mp4",
		LocalPath:        filepath.Join(svc.cfg.TelegramBotAPIDir, "stale-periodic.mp4"),
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open sqlite maintenance connection: %v", err)
	}
	old := time.Now().UTC().Add(-staleDownloadingAge - time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `UPDATE videos SET created_at = ?, updated_at = ? WHERE id = ?`, old, old, video.ID); err != nil {
		_ = db.Close()
		t.Fatalf("age downloading row: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close maintenance connection: %v", err)
	}

	if err := svc.performMaintenance(ctx); err != nil {
		t.Fatalf("perform maintenance: %v", err)
	}
	stored, err := svc.store.Get(ctx, video.ID)
	if err != nil {
		t.Fatalf("get stale row: %v", err)
	}
	if stored.Status != queue.StatusFailed || !strings.Contains(stored.Error, "periodic recovery") {
		t.Fatalf("stale row = %#v, want periodic failed transition", stored)
	}
}

func TestEnqueueUploadRejectsPathOutsideLocalBotAPIDir(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
	})
	path := filepath.Join(t.TempDir(), "outside.mp4")
	writeTestFile(t, path)

	_, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        path,
		TelegramFileID:   "file",
		TelegramUniqueID: "unique",
		FileName:         "outside.mp4",
		SizeBytes:        5,
	})
	if err == nil || !strings.Contains(err.Error(), "outside TELEGRAM_BOT_API_DIR") {
		t.Fatalf("err = %v, want outside bot api dir error", err)
	}
	if length, lengthErr := svc.store.QueueLength(ctx); lengthErr != nil {
		t.Fatalf("queue length: %v", lengthErr)
	} else if length != 0 {
		t.Fatalf("queue length = %d, want 0", length)
	}
}

func TestAdvancePlaybackSkipsStoredPathOutsideLocalBotAPIDir(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	outsidePath := filepath.Join(t.TempDir(), "outside-ready.mp4")
	writeTestFile(t, outsidePath)
	video, err := svc.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   "outside",
		TelegramUniqueID: "outside",
		FileName:         "outside-ready.mp4",
		LocalPath:        outsidePath,
	})
	if err != nil {
		t.Fatalf("add outside downloading: %v", err)
	}
	invalidReady, err := svc.store.MarkReady(ctx, video.ID, outsidePath, 100, 60)
	if err != nil {
		t.Fatalf("mark outside ready: %v", err)
	}
	validReady := addReadyVideo(t, ctx, svc, "valid-ready.mp4")

	playing, err := svc.advancePlayback(ctx)
	if err != nil {
		t.Fatalf("advance playback: %v", err)
	}
	if playing == nil || playing.ID != validReady.ID {
		t.Fatalf("playing = %#v, want valid id %d", playing, validReady.ID)
	}
	if fakeOBS.lastPlayed != validReady.LocalPath {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, validReady.LocalPath)
	}
	storedInvalid, err := svc.store.Get(ctx, invalidReady.ID)
	if err != nil {
		t.Fatalf("get invalid ready: %v", err)
	}
	if storedInvalid.Status != queue.StatusFailed {
		t.Fatalf("invalid status = %s, want %s", storedInvalid.Status, queue.StatusFailed)
	}
}

func TestAdvancePlaybackDoesNotMarkReadyPlayedWhenOBSPlayFails(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	ready := addReadyVideo(t, ctx, svc, "ready.mp4")
	fakeOBS.playErr = errors.New("obs play failed")

	video, err := svc.advancePlayback(ctx)
	if err == nil {
		t.Fatalf("advance playback should fail")
	}
	if video != nil {
		t.Fatalf("video = %#v, want nil", video)
	}
	if got := svc.lastError(); !strings.Contains(got, "obs play failed") {
		t.Fatalf("last error = %q, want OBS error", got)
	}
	stored, err := svc.store.Get(ctx, ready.ID)
	if err != nil {
		t.Fatalf("get ready video: %v", err)
	}
	if stored.Status != queue.StatusReady {
		t.Fatalf("status = %s, want %s", stored.Status, queue.StatusReady)
	}
	if current, err := svc.store.Current(ctx); err != nil {
		t.Fatalf("current: %v", err)
	} else if current != nil {
		t.Fatalf("current = %#v, want nil", current)
	}
	statusText, err := svc.StatusText(ctx, true)
	if err != nil {
		t.Fatalf("status text: %v", err)
	}
	for _, want := range []string{"Ready：1", "Played：0", "Last error：obs play failed"} {
		if !strings.Contains(statusText, want) {
			t.Fatalf("status text = %q, want %q", statusText, want)
		}
	}
	if svc.playbackState() != playbackIdle {
		t.Fatalf("playback state = %s, want %s", svc.playbackState(), playbackIdle)
	}
}

func TestAdvancePlaybackMarkPlayingFailureLeavesRecoverableIdleGeneration(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, fakeOBS, fakeBot := newLocalUploadTestServiceAtDBPath(
		t,
		config.Config{FallbackMode: "off"},
		dbPath,
	)
	firstReady := addReadyVideo(t, ctx, svc, "first.mp4")
	firstPlaying, err := svc.store.MarkPlaying(ctx, firstReady.ID)
	if err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	second := addReadyVideo(t, ctx, svc, "second.mp4")
	third := addReadyVideo(t, ctx, svc, "third.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	svc.resetMediaProgressLocked(svc.cfg.OBSMediaSourceName, firstPlaying.LocalPath)
	fakeOBS.lastPlayed = firstPlaying.LocalPath
	fakeOBS.inputFiles = map[string]string{svc.cfg.OBSMediaSourceName: firstPlaying.LocalPath}
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePlaying},
	}
	dropFailure := installMarkPlayingFailureTrigger(t, dbPath, second.ID)

	video, err := svc.advancePlayback(ctx)
	if err == nil || !strings.Contains(err.Error(), "phase9 transient mark playing") {
		t.Fatalf("advance error = %v, want transient MarkPlaying failure", err)
	}
	if video != nil {
		t.Fatalf("advance video = %#v, want nil", video)
	}
	assertQueuePlaybackIdle(t, svc)
	if fakeOBS.stopCurrentCalls != 1 {
		t.Fatalf("StopCurrent calls = %d, want 1", fakeOBS.stopCurrentCalls)
	}
	assertBoundedStopContext(t, fakeOBS)
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("play calls after failed persistence = %d, want 1", fakeOBS.playFileCalls)
	}
	if current, currentErr := svc.store.Current(ctx); currentErr != nil {
		t.Fatalf("current after failed persistence: %v", currentErr)
	} else if current != nil {
		t.Fatalf("current after failed persistence = %#v, want nil", current)
	}
	assertStoredStatus(t, ctx, svc, firstPlaying.ID, queue.StatusPlayed)
	assertStoredStatus(t, ctx, svc, second.ID, queue.StatusReady)
	assertStoredStatus(t, ctx, svc, third.ID, queue.StatusReady)

	// Both generation guards must reject the stale first generation while the
	// durable queue has no current row.
	for _, guard := range []struct {
		name string
		id   int64
		path string
	}{
		{name: "id", id: firstPlaying.ID},
		{name: "path", path: firstPlaying.LocalPath},
	} {
		svc.playbackMu.Lock()
		staleVideo, staleErr := svc.advancePlaybackLockedAfter(ctx, guard.id, guard.path)
		svc.playbackMu.Unlock()
		if staleErr != nil || staleVideo != nil {
			t.Fatalf("%s guard result video=%#v err=%v, want no-op", guard.name, staleVideo, staleErr)
		}
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("stale guards replayed media: calls = %d, want 1", fakeOBS.playFileCalls)
	}
	assertStoredStatus(t, ctx, svc, second.ID, queue.StatusReady)
	assertStoredStatus(t, ctx, svc, third.ID, queue.StatusReady)

	dropFailure()
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog recovery: %v", err)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after watchdog recovery: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current after watchdog = %#v, want second id %d", current, second.ID)
	}
	if fakeOBS.playFileCalls != 2 || fakeOBS.lastPlayed != second.LocalPath {
		t.Fatalf(
			"watchdog recovery calls/path = %d/%q, want 2/%q",
			fakeOBS.playFileCalls,
			fakeOBS.lastPlayed,
			second.LocalPath,
		)
	}
	if svc.playbackState() != playbackNormal {
		t.Fatalf("playback state after recovery = %s, want %s", svc.playbackState(), playbackNormal)
	}
	if progress := svc.mediaProgressByInput[svc.cfg.OBSMediaSourceName]; progress.Path != second.LocalPath {
		t.Fatalf("media progress after recovery = %#v, want second path", progress)
	}
	assertStoredStatus(t, ctx, svc, firstPlaying.ID, queue.StatusPlayed)
	assertStoredStatus(t, ctx, svc, second.ID, queue.StatusPlaying)
	assertStoredStatus(t, ctx, svc, third.ID, queue.StatusReady)
	if len(fakeBot.messages) != 1 {
		t.Fatalf("recovery notifications = %d, want 1", len(fakeBot.messages))
	}

	// A delayed first-generation hint must not consume the third row after the
	// second generation is durably playing.
	svc.playbackMu.Lock()
	staleVideo, staleErr := svc.advancePlaybackLockedAfter(ctx, firstPlaying.ID, firstPlaying.LocalPath)
	svc.playbackMu.Unlock()
	if staleErr != nil || staleVideo != nil {
		t.Fatalf("post-recovery stale guard video=%#v err=%v, want no-op", staleVideo, staleErr)
	}
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("healthy normal watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 2 {
		t.Fatalf("healthy normal playback restarted: calls = %d, want 2", fakeOBS.playFileCalls)
	}
	assertStoredStatus(t, ctx, svc, second.ID, queue.StatusPlaying)
	assertStoredStatus(t, ctx, svc, third.ID, queue.StatusReady)
}

func TestAdvancePlaybackMarkPlayingAndStopErrorsPreservePrimaryFailure(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, fakeOBS, _ := newLocalUploadTestServiceAtDBPath(
		t,
		config.Config{FallbackMode: "off"},
		dbPath,
	)
	first := addReadyVideo(t, ctx, svc, "first.mp4")
	if _, err := svc.store.MarkPlaying(ctx, first.ID); err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	second := addReadyVideo(t, ctx, svc, "second.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	svc.resetMediaProgressLocked(svc.cfg.OBSMediaSourceName, first.LocalPath)
	dropFailure := installMarkPlayingFailureTrigger(t, dbPath, second.ID)
	defer dropFailure()
	stopErr := errors.New("stop cleanup failed")
	fakeOBS.stopErr = stopErr

	video, err := svc.advancePlayback(ctx)
	if video != nil {
		t.Fatalf("advance video = %#v, want nil", video)
	}
	if err == nil || !strings.Contains(err.Error(), "phase9 transient mark playing") {
		t.Fatalf("advance error = %v, want primary MarkPlaying failure", err)
	}
	if !errors.Is(err, stopErr) {
		t.Fatalf("advance error = %v, want joined StopCurrent failure", err)
	}
	if got := svc.lastError(); !strings.Contains(got, "phase9 transient mark playing") ||
		!strings.Contains(got, stopErr.Error()) {
		t.Fatalf("last error = %q, want primary and cleanup failures", got)
	}
	assertBoundedStopContext(t, fakeOBS)
	assertQueuePlaybackIdle(t, svc)
	if current, currentErr := svc.store.Current(ctx); currentErr != nil {
		t.Fatalf("current after cleanup failure: %v", currentErr)
	} else if current != nil {
		t.Fatalf("current after cleanup failure = %#v, want nil", current)
	}
	assertStoredStatus(t, ctx, svc, second.ID, queue.StatusReady)
}

func TestAdvancePlaybackNextReadyErrorLeavesIdleAndRetryable(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, fakeOBS, _ := newLocalUploadTestServiceAtDBPath(
		t,
		config.Config{FallbackMode: "off"},
		dbPath,
	)
	first := addReadyVideo(t, ctx, svc, "first.mp4")
	if _, err := svc.store.MarkPlaying(ctx, first.ID); err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	second := addReadyVideo(t, ctx, svc, "second.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	svc.resetMediaProgressLocked(svc.cfg.OBSMediaSourceName, first.LocalPath)
	execQueueSQL(t, dbPath, `UPDATE videos SET created_at = ? WHERE id = ?`, "not-a-time", second.ID)

	video, err := svc.advancePlayback(ctx)
	if video != nil {
		t.Fatalf("advance video = %#v, want nil", video)
	}
	if err == nil || !strings.Contains(err.Error(), "parse videos.created_at") {
		t.Fatalf("advance error = %v, want NextReady decode failure", err)
	}
	assertQueuePlaybackIdle(t, svc)
	if fakeOBS.playFileCalls != 0 {
		t.Fatalf("NextReady error reached OBS: play calls = %d", fakeOBS.playFileCalls)
	}
	assertRawStoredStatus(t, dbPath, first.ID, queue.StatusPlayed)
	assertRawStoredStatus(t, dbPath, second.ID, queue.StatusReady)

	execQueueSQL(t, dbPath, `UPDATE videos SET created_at = ? WHERE id = ?`, formatQueueTime(second.CreatedAt), second.ID)
	if err := svc.playIfIdle(ctx); err != nil {
		t.Fatalf("playIfIdle retry: %v", err)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after retry: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current after retry = %#v, want second id %d", current, second.ID)
	}
	if fakeOBS.playFileCalls != 1 || svc.playbackState() != playbackNormal {
		t.Fatalf(
			"retry calls/state = %d/%s, want 1/%s",
			fakeOBS.playFileCalls,
			svc.playbackState(),
			playbackNormal,
		)
	}
}

func TestAdvancePlaybackFallbackErrorLeavesIdleAndWatchdogRetries(t *testing.T) {
	ctx := context.Background()
	fallbackPath := filepath.Join(t.TempDir(), "fallback.mp4")
	writeTestFile(t, fallbackPath)
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{
		FallbackMode:    "file",
		OBSFallbackFile: fallbackPath,
	})
	first := addReadyVideo(t, ctx, svc, "first.mp4")
	if _, err := svc.store.MarkPlaying(ctx, first.ID); err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	svc.setPlaybackState(playbackNormal, 0, "")
	svc.resetMediaProgressLocked(svc.cfg.OBSMediaSourceName, first.LocalPath)
	playErr := errors.New("fallback play failed")
	fakeOBS.playErr = playErr

	video, err := svc.advancePlayback(ctx)
	if video != nil || !errors.Is(err, playErr) {
		t.Fatalf("fallback failure video=%#v err=%v, want %v", video, err, playErr)
	}
	assertQueuePlaybackIdle(t, svc)
	assertStoredStatus(t, ctx, svc, first.ID, queue.StatusPlayed)
	if current, currentErr := svc.store.Current(ctx); currentErr != nil {
		t.Fatalf("current after fallback error: %v", currentErr)
	} else if current != nil {
		t.Fatalf("current after fallback error = %#v, want nil", current)
	}

	fakeOBS.playErr = nil
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog fallback retry: %v", err)
	}
	if svc.playbackState() != playbackFile || svc.currentPlaybackPath() != fallbackPath {
		t.Fatalf(
			"fallback retry state/path = %s/%q, want %s/%q",
			svc.playbackState(),
			svc.currentPlaybackPath(),
			playbackFile,
			fallbackPath,
		)
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("fallback retry play calls = %d, want 1", fakeOBS.playFileCalls)
	}
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("healthy fallback watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 1 || svc.playbackState() != playbackFile {
		t.Fatalf(
			"healthy fallback changed: calls/state = %d/%s",
			fakeOBS.playFileCalls,
			svc.playbackState(),
		)
	}
}

func TestMissingCurrentSelfHealingStartsReadyQueueWithoutDoubleConsumption(t *testing.T) {
	for _, test := range []struct {
		name    string
		recover func(context.Context, *Service) error
	}{
		{name: "play_if_idle", recover: func(ctx context.Context, svc *Service) error {
			return svc.playIfIdle(ctx)
		}},
		{name: "watchdog", recover: func(ctx context.Context, svc *Service) error {
			return svc.checkPlaybackWatchdog(ctx)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
			first := addReadyVideo(t, ctx, svc, "first.mp4")
			second := addReadyVideo(t, ctx, svc, "second.mp4")
			svc.setPlaybackState(playbackNormal, 0, "")
			svc.resetMediaProgressLocked(svc.cfg.OBSMediaSourceName, "/stale/finished.mp4")

			if err := test.recover(ctx, svc); err != nil {
				t.Fatalf("self-healing recovery: %v", err)
			}
			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current after self-heal: %v", err)
			}
			if current == nil || current.ID != first.ID {
				t.Fatalf("current after self-heal = %#v, want first id %d", current, first.ID)
			}
			if fakeOBS.playFileCalls != 1 || fakeOBS.lastPlayed != first.LocalPath {
				t.Fatalf(
					"self-heal calls/path = %d/%q, want 1/%q",
					fakeOBS.playFileCalls,
					fakeOBS.lastPlayed,
					first.LocalPath,
				)
			}
			if progress := svc.mediaProgressByInput[svc.cfg.OBSMediaSourceName]; progress.Path != first.LocalPath {
				t.Fatalf("self-healed media progress = %#v, want first path", progress)
			}
			if err := test.recover(ctx, svc); err != nil {
				t.Fatalf("healthy normal recovery: %v", err)
			}
			if fakeOBS.playFileCalls != 1 {
				t.Fatalf("healthy normal state replayed current: calls = %d", fakeOBS.playFileCalls)
			}
			assertStoredStatus(t, ctx, svc, first.ID, queue.StatusPlaying)
			assertStoredStatus(t, ctx, svc, second.ID, queue.StatusReady)
		})
	}
}

func TestMissingCurrentRecoveryPreservesHealthyFallbackGenerations(t *testing.T) {
	for _, test := range []struct {
		name string
		mode string
		kind playbackKind
	}{
		{name: "random", mode: "random_played", kind: playbackRandom},
		{name: "file", mode: "file", kind: playbackFile},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			fallbackPath := filepath.Join(t.TempDir(), test.name+".mp4")
			writeTestFile(t, fallbackPath)
			cfg := config.Config{FallbackMode: test.mode}
			if test.mode == "file" {
				cfg.OBSFallbackFile = fallbackPath
			}
			svc, fakeOBS, _ := newLocalUploadTestService(t, cfg)
			if test.mode == "random_played" {
				played := addPlayedVideo(t, ctx, svc, "history.mp4", true)
				fallbackPath = played.LocalPath
			}
			if _, err := svc.advancePlayback(ctx); err != nil {
				t.Fatalf("start fallback: %v", err)
			}
			if svc.playbackState() != test.kind || svc.currentPlaybackPath() != fallbackPath {
				t.Fatalf(
					"initial state/path = %s/%q, want %s/%q",
					svc.playbackState(),
					svc.currentPlaybackPath(),
					test.kind,
					fallbackPath,
				)
			}
			if err := svc.playIfIdle(ctx); err != nil {
				t.Fatalf("playIfIdle with healthy fallback: %v", err)
			}
			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("watchdog with healthy fallback: %v", err)
			}
			if fakeOBS.playFileCalls != 1 ||
				svc.playbackState() != test.kind ||
				svc.currentPlaybackPath() != fallbackPath {
				t.Fatalf(
					"healthy fallback changed: calls/state/path = %d/%s/%q",
					fakeOBS.playFileCalls,
					svc.playbackState(),
					svc.currentPlaybackPath(),
				)
			}
		})
	}
}

func TestStatusTextRedactsLastErrorSecrets(t *testing.T) {
	const (
		token    = "123456:ABCdefghi_jklmnop"
		password = "obs-secret-password"
		apiHash  = "telegram-api-hash"
	)
	t.Setenv("TELEGRAM_API_HASH", apiHash)
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		FallbackMode:      "off",
		TelegramBotToken:  token,
		OBSPassword:       password,
		RetentionMaxFiles: 100,
	})

	svc.setLastErr(errors.New(`Post "http://127.0.0.1:8081/bot123456:ABCdefghi_jklmnop/getMe": obs-secret-password telegram-api-hash`))

	statusText, err := svc.StatusText(ctx, true)
	if err != nil {
		t.Fatalf("status text: %v", err)
	}
	for _, leaked := range []string{token, password, apiHash} {
		if strings.Contains(statusText, leaked) {
			t.Fatalf("status text leaked %q: %q", leaked, statusText)
		}
	}
	if !strings.Contains(statusText, "<redacted>") {
		t.Fatalf("status text = %q, want redacted marker", statusText)
	}
}

func TestRecoveredPlaybackLogRedactsPathSecrets(t *testing.T) {
	const token = "123456:ABCdefghi_jklmnop"
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "bot"+token)
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		FallbackMode:      "off",
		TelegramBotToken:  token,
		TelegramBotAPIDir: root,
	})
	var logs bytes.Buffer
	svc.logger = slog.New(slog.NewTextHandler(&logs, nil))
	ready := addReadyVideo(t, ctx, svc, "current.mp4")
	if _, err := svc.store.MarkPlaying(ctx, ready.ID); err != nil {
		t.Fatalf("mark playing: %v", err)
	}

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback: %v", err)
	}

	got := logs.String()
	if strings.Contains(got, token) {
		t.Fatalf("log leaked token in path: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("log = %q, want redacted marker", got)
	}
}

func TestMaintainOBSConnectionProbesEveryPlaybackState(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	states := []playbackKind{
		playbackIdle,
		playbackNormal,
		playbackRandom,
		playbackFile,
	}

	for _, state := range states {
		svc.setPlaybackState(state, 0, "")
		svc.maintainOBSConnection(ctx)
	}

	if fakeOBS.probeCalls != len(states) {
		t.Fatalf("probe calls = %d, want %d for all playback states", fakeOBS.probeCalls, len(states))
	}
	if fakeOBS.connectCalls != 0 {
		t.Fatalf("healthy probes triggered %d reconnects, want 0", fakeOBS.connectCalls)
	}
}

func TestMaintainOBSConnectionRecordsProbeFailureWithoutPlaybackMutation(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.setPlaybackState(playbackFile, 0, "/existing/fallback.mp4")
	fakeOBS.probeErr = context.DeadlineExceeded

	svc.maintainOBSConnection(ctx)

	if fakeOBS.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", fakeOBS.probeCalls)
	}
	if fakeOBS.lastPlayed != "" {
		t.Fatalf("probe failure should not mutate playback, played %q", fakeOBS.lastPlayed)
	}
	if got := svc.lastError(); !strings.Contains(got, context.DeadlineExceeded.Error()) {
		t.Fatalf("last error = %q, want probe deadline", got)
	}
}

func TestMaintainOBSConnectionDoesNotReconnectAfterSemanticProbeFailure(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.setPlaybackState(playbackFile, 0, "/existing/fallback.mp4")
	fakeOBS.probeErr = &obs.RequestError{
		RequestType: "GetVersion",
		Code:        500,
		Comment:     "request rejected",
	}

	svc.maintainOBSConnection(ctx)

	if fakeOBS.probeCalls != 1 {
		t.Fatalf("probe calls = %d, want 1", fakeOBS.probeCalls)
	}
	if fakeOBS.connectCalls != 0 {
		t.Fatalf("semantic probe failure triggered %d reconnects, want 0", fakeOBS.connectCalls)
	}
	if fakeOBS.state != obs.StateConnected {
		t.Fatalf("state = %s, want %s", fakeOBS.state, obs.StateConnected)
	}
}

func TestMaintainOBSConnectionGatesWatchdogUntilRecoveryCompletes(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	ready := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	next := addReadyVideo(t, ctx, svc, "next.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.state = obs.StateDisconnected
	fakeOBS.connectNotify = make(chan struct{})
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}

	svc.playbackMu.Lock()
	locked := true
	defer func() {
		if locked {
			svc.playbackMu.Unlock()
		}
	}()
	recoveryDone := make(chan struct{})
	go func() {
		svc.maintainOBSConnection(ctx)
		close(recoveryDone)
	}()
	select {
	case <-fakeOBS.connectNotify:
	case <-time.After(time.Second):
		t.Fatal("OBS connect did not publish connected state")
	}
	if !svc.obsRecoveryInProgress.Load() {
		t.Fatal("recovery gate is not active after OBS connected")
	}

	watchdogDone := make(chan error, 1)
	go func() {
		watchdogDone <- svc.checkPlaybackWatchdog(ctx)
	}()
	select {
	case err := <-watchdogDone:
		if err != nil {
			t.Fatalf("watchdog while recovery pending: %v", err)
		}
	case <-time.After(time.Second):
		svc.playbackMu.Unlock()
		locked = false
		<-recoveryDone
		t.Fatal("watchdog blocked on playback lock instead of honoring recovery gate")
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current while recovery pending: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current while recovery pending = %#v, want id %d", current, playing.ID)
	}

	svc.playbackMu.Unlock()
	locked = false
	select {
	case <-recoveryDone:
	case <-time.After(time.Second):
		t.Fatal("OBS recovery did not finish after playback lock released")
	}
	if svc.obsRecoveryInProgress.Load() {
		t.Fatal("recovery gate remained active after recovery finished")
	}
	current, err = svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after recovery: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current after recovery = %#v, want replayed id %d", current, playing.ID)
	}
	storedNext, err := svc.store.Get(ctx, next.ID)
	if err != nil {
		t.Fatalf("get next: %v", err)
	}
	if storedNext.Status != queue.StatusReady {
		t.Fatalf("next status = %s, want %s", storedNext.Status, queue.StatusReady)
	}
	if fakeOBS.playFileCalls != 1 || fakeOBS.lastPlayed != playing.LocalPath {
		t.Fatalf("recovery play calls/path = %d/%q, want 1/%q", fakeOBS.playFileCalls, fakeOBS.lastPlayed, playing.LocalPath)
	}
}

func TestRecoverPlaybackAfterOBSConnectKeepsMatchingActiveCurrentPosition(t *testing.T) {
	for _, state := range []obs.MediaState{
		obs.MediaStatePlaying,
		obs.MediaStateOpening,
		obs.MediaStateBuffering,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
			ready := addReadyVideo(t, ctx, svc, "current.mp4")
			playing, err := svc.store.MarkPlaying(ctx, ready.ID)
			if err != nil {
				t.Fatalf("mark playing: %v", err)
			}
			svc.setPlaybackState(playbackNormal, 0, "")
			cursor := 100.0
			fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
				svc.cfg.OBSMediaSourceName: {
					State:              state,
					CursorMilliseconds: &cursor,
				},
			}
			fakeOBS.inputFiles = map[string]string{
				svc.cfg.OBSMediaSourceName: playing.LocalPath,
			}

			if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
				t.Fatalf("recover healthy playback: %v", err)
			}
			if fakeOBS.playFileCalls != 0 {
				t.Fatalf("healthy reconnect replayed current %d times, want 0", fakeOBS.playFileCalls)
			}
			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current: %v", err)
			}
			if current == nil || current.StartedAt == nil || playing.StartedAt == nil {
				t.Fatalf("current timestamps unavailable: before=%#v after=%#v", playing, current)
			}
			if !current.StartedAt.Equal(*playing.StartedAt) {
				t.Fatalf("healthy reconnect changed started_at from %s to %s", playing.StartedAt, current.StartedAt)
			}
		})
	}
}

func TestRecoverPlaybackAfterOBSConnectReplaysUnhealthyCurrentWithoutAdvancing(t *testing.T) {
	tests := []struct {
		name         string
		state        obs.MediaState
		pathMismatch bool
	}{
		{name: "none", state: obs.MediaStateNone},
		{name: "paused", state: obs.MediaStatePaused},
		{name: "stopped", state: obs.MediaStateStopped},
		{name: "ended", state: obs.MediaStateEnded},
		{name: "error", state: obs.MediaStateError},
		{name: "path_mismatch", state: obs.MediaStatePlaying, pathMismatch: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "queue.db")
			svc, fakeOBS, _ := newFallbackTestServiceAtDBPath(t, config.Config{FallbackMode: "off"}, dbPath)
			ready := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
			playing, err := svc.store.MarkPlaying(ctx, ready.ID)
			if err != nil {
				t.Fatalf("mark playing: %v", err)
			}
			next := addReadyVideo(t, ctx, svc, "next.mp4")
			oldStarted := time.Now().UTC().Add(-2 * time.Hour)
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatalf("open raw db: %v", err)
			}
			if _, err := db.ExecContext(ctx, `
UPDATE videos SET started_at = ?, updated_at = ? WHERE id = ?
`, formatQueueTime(oldStarted), formatQueueTime(oldStarted), playing.ID); err != nil {
				_ = db.Close()
				t.Fatalf("age current started_at: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close raw db: %v", err)
			}

			svc.setPlaybackState(playbackNormal, 0, "")
			actualPath := playing.LocalPath
			if tt.pathMismatch {
				actualPath = next.LocalPath
				svc.playbackMu.Lock()
				svc.resetMediaProgressLocked(svc.cfg.OBSMediaSourceName, playing.LocalPath)
				svc.playbackMu.Unlock()
			}
			fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
				svc.cfg.OBSMediaSourceName: {State: tt.state},
			}
			fakeOBS.inputFiles = map[string]string{
				svc.cfg.OBSMediaSourceName: actualPath,
			}

			if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
				t.Fatalf("recover unhealthy playback: %v", err)
			}
			if fakeOBS.playFileCalls != 1 {
				t.Fatalf("play calls = %d, want 1", fakeOBS.playFileCalls)
			}
			if fakeOBS.lastPlayed != playing.LocalPath {
				t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, playing.LocalPath)
			}
			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current: %v", err)
			}
			if current == nil || current.ID != playing.ID || current.StartedAt == nil {
				t.Fatalf("current = %#v, want replayed id %d", current, playing.ID)
			}
			if !current.StartedAt.After(oldStarted) {
				t.Fatalf("replayed started_at = %s, want after %s", current.StartedAt, oldStarted)
			}
			storedNext, err := svc.store.Get(ctx, next.ID)
			if err != nil {
				t.Fatalf("get next: %v", err)
			}
			if storedNext.Status != queue.StatusReady {
				t.Fatalf("next status = %s, want %s", storedNext.Status, queue.StatusReady)
			}
		})
	}
}

func TestRecoverPlaybackAfterOBSConnectFreshProcessReplaysCurrentDespiteHealthyOBS(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	ready := addReadyVideo(t, ctx, svc, "current.mp4")
	playing, err := svc.store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePlaying},
	}

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover fresh process playback: %v", err)
	}

	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("fresh process play calls = %d, want 1", fakeOBS.playFileCalls)
	}
	if fakeOBS.lastPlayed != playing.LocalPath {
		t.Fatalf("played path = %q, want %q", fakeOBS.lastPlayed, playing.LocalPath)
	}
}

func TestRecoverPlaybackAfterOBSConnectFreshProcessReplaysCurrentDespiteStaleEndedSource(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	ready := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	next := addReadyVideo(t, ctx, svc, "next.mp4")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: next.LocalPath,
	}

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover fresh process playback: %v", err)
	}

	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current = %#v, want original id %d", current, playing.ID)
	}
	if fakeOBS.playFileCalls != 1 || fakeOBS.lastPlayed != playing.LocalPath {
		t.Fatalf("replay calls/path = %d/%q, want 1/%q", fakeOBS.playFileCalls, fakeOBS.lastPlayed, playing.LocalPath)
	}
}

func TestRecoverPlaybackAfterOBSConnectReplaysCurrent(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	ready := addReadyVideo(t, ctx, svc, "current.mp4")
	playing, err := svc.store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback: %v", err)
	}
	if fakeOBS.lastPlayed != playing.LocalPath {
		t.Fatalf("played path = %q, want current path %q", fakeOBS.lastPlayed, playing.LocalPath)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current = %#v, want playing id %d", current, playing.ID)
	}
}

func TestRecoverPlaybackAfterOBSConnectFailureLeavesPlaybackIdleForRetry(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	ready := addReadyVideo(t, ctx, svc, "current.mp4")
	playing, err := svc.store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	fakeOBS.playErr = errors.New("obs replay failed")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePaused},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}

	err = svc.recoverPlaybackAfterOBSConnect(ctx)

	if err == nil {
		t.Fatal("expected recover playback to fail")
	}
	if svc.playbackState() != playbackIdle {
		t.Fatalf("playback state = %s, want %s", svc.playbackState(), playbackIdle)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current = %#v, want playing id %d", current, playing.ID)
	}
	if got := svc.lastError(); !strings.Contains(got, "obs replay failed") {
		t.Fatalf("last error = %q, want replay failure", got)
	}
}

func TestRecoverPlaybackAfterOBSConnectRefreshesWatchdogDeadline(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newFallbackTestServiceAtDBPath(t, config.Config{FallbackMode: "off"}, dbPath)
	ready := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 30)
	playing, err := svc.store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	oldStarted := playing.StartedAt.Add(-2 * time.Hour)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
UPDATE videos SET started_at = ?, updated_at = ? WHERE id = ?
`, formatQueueTime(oldStarted), formatQueueTime(oldStarted), playing.ID); err != nil {
		t.Fatalf("age current started_at: %v", err)
	}
	_ = addReadyVideo(t, ctx, svc, "next.mp4")

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback: %v", err)
	}
	restarted, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after recover: %v", err)
	}
	if restarted == nil || restarted.StartedAt == nil {
		t.Fatalf("current after recover = %#v", restarted)
	}
	if !restarted.StartedAt.After(oldStarted) {
		t.Fatalf("started_at = %s, want after old %s", restarted.StartedAt, oldStarted)
	}
	svc.now = func() time.Time {
		return restarted.StartedAt.Add(5 * time.Second)
	}

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog: %v", err)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current = %#v, want recovered id %d", current, playing.ID)
	}
}

func TestRecoverPlaybackAfterOBSConnectRestartsFallbackWhenNoCurrent(t *testing.T) {
	ctx := context.Background()
	staticPath := filepath.Join(t.TempDir(), "fallback.mp4")
	writeTestFile(t, staticPath)
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{
		FallbackMode:    "file",
		OBSFallbackFile: staticPath,
	})
	svc.setPlaybackState(playbackFile, 0, staticPath)
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateStopped},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: staticPath,
	}

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback: %v", err)
	}
	if fakeOBS.lastPlayed != staticPath {
		t.Fatalf("played path = %q, want fallback path %q", fakeOBS.lastPlayed, staticPath)
	}
	if svc.playbackState() != playbackFile {
		t.Fatalf("playback state = %s, want %s", svc.playbackState(), playbackFile)
	}
}

func TestRecoverPlaybackAfterOBSConnectKeepsHealthyFallbackPlaying(t *testing.T) {
	ctx := context.Background()
	staticPath := filepath.Join(t.TempDir(), "fallback.mp4")
	writeTestFile(t, staticPath)
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{
		FallbackMode:    "file",
		OBSFallbackFile: staticPath,
	})
	svc.setPlaybackState(playbackFile, 0, staticPath)
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePlaying},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: staticPath,
	}

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover healthy fallback: %v", err)
	}

	if fakeOBS.playFileCalls != 0 {
		t.Fatalf("healthy fallback replayed %d times, want 0", fakeOBS.playFileCalls)
	}
	if svc.playbackState() != playbackFile {
		t.Fatalf("playback state = %s, want %s", svc.playbackState(), playbackFile)
	}
}

func TestRecoverPlaybackAfterOBSConnectSkipsMissingCurrent(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	missing := addReadyVideo(t, ctx, svc, "missing-current.mp4")
	playing, err := svc.store.MarkPlaying(ctx, missing.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	if err := osRemove(playing.LocalPath); err != nil {
		t.Fatalf("remove current file: %v", err)
	}
	next := addReadyVideo(t, ctx, svc, "next.mp4")

	if err := svc.recoverPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback: %v", err)
	}
	storedMissing, err := svc.store.Get(ctx, playing.ID)
	if err != nil {
		t.Fatalf("get missing current: %v", err)
	}
	if storedMissing.Status != queue.StatusFailed {
		t.Fatalf("missing current status = %s, want %s", storedMissing.Status, queue.StatusFailed)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != next.ID {
		t.Fatalf("current = %#v, want next id %d", current, next.ID)
	}
	if fakeOBS.lastPlayed != next.LocalPath {
		t.Fatalf("played path = %q, want next path %q", fakeOBS.lastPlayed, next.LocalPath)
	}
}

func TestPlaybackWatchdogAdvancesExpiredCurrent(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, fakeBot := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 30)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	next := addReadyVideo(t, ctx, svc, "next.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePlaying},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}
	svc.now = func() time.Time {
		return playing.StartedAt.Add(30*time.Second + playbackWatchdogGrace + time.Second)
	}

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog: %v", err)
	}
	storedCurrent, err := svc.store.Get(ctx, playing.ID)
	if err != nil {
		t.Fatalf("get expired current: %v", err)
	}
	if storedCurrent.Status != queue.StatusPlayed {
		t.Fatalf("expired status = %s, want %s", storedCurrent.Status, queue.StatusPlayed)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != next.ID {
		t.Fatalf("current = %#v, want next id %d", current, next.ID)
	}
	if fakeOBS.lastPlayed != next.LocalPath {
		t.Fatalf("played path = %q, want next path %q", fakeOBS.lastPlayed, next.LocalPath)
	}
	if len(fakeBot.messages) != 1 {
		t.Fatalf("messages = %d, want 1", len(fakeBot.messages))
	}
}

func TestEarlyStaleOBSEndedEventDoesNotSkipCurrentAfterWatchdogAdvance(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	firstReady := addReadyVideoWithDuration(t, ctx, svc, "first.mp4", 30)
	firstPlaying, err := svc.store.MarkPlaying(ctx, firstReady.ID)
	if err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	second := addReadyVideo(t, ctx, svc, "second.mp4")
	third := addReadyVideo(t, ctx, svc, "third.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePlaying},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: firstPlaying.LocalPath,
	}
	svc.now = func() time.Time {
		return firstPlaying.StartedAt.Add(30*time.Second + playbackWatchdogGrace + time.Second)
	}
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog: %v", err)
	}
	currentAfterWatchdog, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after watchdog: %v", err)
	}
	if currentAfterWatchdog == nil || currentAfterWatchdog.StartedAt == nil {
		t.Fatalf("current after watchdog = %#v", currentAfterWatchdog)
	}
	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}

	video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
		Type: obs.EventMediaEnded,
		Path: second.LocalPath,
		At:   currentAfterWatchdog.StartedAt.Add(obsEndedEventSettleGrace + time.Second),
	})
	if err != nil {
		t.Fatalf("stale OBS event: %v", err)
	}
	if video != nil {
		t.Fatalf("stale OBS event advanced to %#v, want nil", video)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current = %#v, want second id %d", current, second.ID)
	}
	storedThird, err := svc.store.Get(ctx, third.ID)
	if err != nil {
		t.Fatalf("get third: %v", err)
	}
	if storedThird.Status != queue.StatusReady {
		t.Fatalf("third status = %s, want %s", storedThird.Status, queue.StatusReady)
	}
	if got := fakeOBS.mediaStatusCalls[svc.cfg.OBSMediaSourceName]; got != 1 {
		t.Fatalf("known-duration guard queried stale OBS status %d times, want watchdog query only", got)
	}
}

func TestQueueEndedEventReconcilesTerminalOBSStateWithEmptyMetadata(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	next := addReadyVideo(t, ctx, svc, "next.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}
	tick := playing.StartedAt.Add(obsEndedEventSettleGrace + time.Millisecond)
	svc.now = func() time.Time { return tick }

	video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
		Type: obs.EventMediaEnded,
		At:   tick,
	})
	if err != nil {
		t.Fatalf("reconcile ended event: %v", err)
	}
	if video == nil || video.ID != next.ID {
		t.Fatalf("advanced video = %#v, want next id %d", video, next.ID)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != next.ID {
		t.Fatalf("current = %#v, want next id %d", current, next.ID)
	}
	if fakeOBS.mediaStatusCalls[svc.cfg.OBSMediaSourceName] != 1 {
		t.Fatalf("media status calls = %d, want 1", fakeOBS.mediaStatusCalls[svc.cfg.OBSMediaSourceName])
	}
}

func TestDelayedSamePathEndedEventDoesNotSkipUnknownDurationCurrent(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	firstReady := addReadyVideoWithDuration(t, ctx, svc, "first.mp4", 0)
	firstPlaying, err := svc.store.MarkPlaying(ctx, firstReady.ID)
	if err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	secondDownloading, err := svc.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   "second",
		TelegramUniqueID: "second",
		FileName:         "second.mp4",
		LocalPath:        firstPlaying.LocalPath,
	})
	if err != nil {
		t.Fatalf("add second downloading: %v", err)
	}
	second, err := svc.store.MarkReady(ctx, secondDownloading.ID, firstPlaying.LocalPath, 100, 0)
	if err != nil {
		t.Fatalf("mark second ready: %v", err)
	}
	third := addReadyVideo(t, ctx, svc, "third.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: firstPlaying.LocalPath,
	}
	tick := firstPlaying.StartedAt.Add(obsEndedEventSettleGrace + time.Millisecond)
	svc.now = func() time.Time { return tick }

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog advance: %v", err)
	}
	currentAfterWatchdog, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after watchdog: %v", err)
	}
	if currentAfterWatchdog == nil || currentAfterWatchdog.ID != second.ID {
		t.Fatalf("current after watchdog = %#v, want second id %d", currentAfterWatchdog, second.ID)
	}
	if currentAfterWatchdog.StartedAt == nil {
		t.Fatalf("second started_at unavailable: %#v", currentAfterWatchdog)
	}
	// PlayFile has succeeded, but OBS can briefly continue reporting the
	// previous generation's terminal state.
	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	earlyEventAt := currentAfterWatchdog.StartedAt.Add(time.Millisecond)
	tick = earlyEventAt
	generationStartedAt := svc.mediaProgressByInput[svc.cfg.OBSMediaSourceName].GenerationStartedAt

	for _, delayed := range []struct {
		name string
		path string
		at   time.Time
	}{
		{name: "same_path", path: firstPlaying.LocalPath, at: earlyEventAt},
		{name: "empty_path"},
		{name: "stale_path", path: third.LocalPath, at: currentAfterWatchdog.StartedAt.Add(obsEndedEventSettleGrace + time.Second)},
	} {
		t.Run(delayed.name, func(t *testing.T) {
			video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
				Type: obs.EventMediaEnded,
				Path: delayed.path,
				At:   delayed.at,
			})
			if err != nil {
				t.Fatalf("delayed OBS event: %v", err)
			}
			if video != nil {
				t.Fatalf("delayed OBS event advanced to %#v, want nil", video)
			}
		})
	}

	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current = %#v, want second id %d", current, second.ID)
	}
	storedThird, err := svc.store.Get(ctx, third.ID)
	if err != nil {
		t.Fatalf("get third: %v", err)
	}
	if storedThird.Status != queue.StatusReady {
		t.Fatalf("third status = %s, want %s", storedThird.Status, queue.StatusReady)
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("play calls = %d, want watchdog advance only", fakeOBS.playFileCalls)
	}
	if got := fakeOBS.mediaStatusCalls[svc.cfg.OBSMediaSourceName]; got != 1 {
		t.Fatalf("guarded events queried stale OBS status %d times, want watchdog query only", got)
	}

	settledFrom := generationStartedAt
	if currentAfterWatchdog.StartedAt.After(settledFrom) {
		settledFrom = *currentAfterWatchdog.StartedAt
	}
	tick = settledFrom.Add(obsEndedEventSettleGrace + time.Millisecond)
	video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
		Type: obs.EventMediaEnded,
		Path: firstPlaying.LocalPath,
		At:   tick,
	})
	if err != nil {
		t.Fatalf("legitimate terminal event: %v", err)
	}
	if video == nil || video.ID != third.ID {
		t.Fatalf("legitimate terminal event advanced to %#v, want third id %d", video, third.ID)
	}
	current, err = svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after legitimate event: %v", err)
	}
	if current == nil || current.ID != third.ID {
		t.Fatalf("current after legitimate event = %#v, want third id %d", current, third.ID)
	}
	if fakeOBS.playFileCalls != 2 {
		t.Fatalf("play calls = %d, want watchdog and legitimate event advances", fakeOBS.playFileCalls)
	}
	if got := fakeOBS.mediaStatusCalls[svc.cfg.OBSMediaSourceName]; got != 2 {
		t.Fatalf("media status calls = %d, want watchdog plus legitimate event query", got)
	}
}

func TestWatchdogSettlingGuardDoesNotSkipNewSamePathCurrentAfterEndedEvent(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, fakeOBS, _ := newFallbackTestServiceAtDBPath(t, config.Config{FallbackMode: "off"}, dbPath)
	firstReady := addReadyVideoWithDuration(t, ctx, svc, "first.mp4", 0)
	firstPlaying, err := svc.store.MarkPlaying(ctx, firstReady.ID)
	if err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	secondDownloading, err := svc.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   "second",
		TelegramUniqueID: "second",
		FileName:         "second.mp4",
		LocalPath:        firstPlaying.LocalPath,
	})
	if err != nil {
		t.Fatalf("add second downloading: %v", err)
	}
	second, err := svc.store.MarkReady(ctx, secondDownloading.ID, firstPlaying.LocalPath, 100, 0)
	if err != nil {
		t.Fatalf("mark second ready: %v", err)
	}
	third := addReadyVideo(t, ctx, svc, "third.mp4")

	oldStarted := time.Now().UTC().Add(-time.Hour)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
UPDATE videos SET started_at = ?, updated_at = ? WHERE id = ?
`, formatQueueTime(oldStarted), formatQueueTime(oldStarted), firstPlaying.ID); err != nil {
		_ = db.Close()
		t.Fatalf("age first started_at: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	tick := time.Now().UTC()
	svc.now = func() time.Time { return tick }
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: firstPlaying.LocalPath,
	}
	video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
		Type: obs.EventMediaEnded,
		Path: firstPlaying.LocalPath,
		At:   tick,
	})
	if err != nil {
		t.Fatalf("legitimate first ended event: %v", err)
	}
	if video == nil || video.ID != second.ID {
		t.Fatalf("first ended event advanced to %#v, want second id %d", video, second.ID)
	}
	currentSecond, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current second: %v", err)
	}
	if currentSecond == nil || currentSecond.ID != second.ID || currentSecond.StartedAt == nil {
		t.Fatalf("current second = %#v, want id %d with started_at", currentSecond, second.ID)
	}

	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	tick = currentSecond.StartedAt.Add(time.Millisecond)
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("immediate watchdog: %v", err)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after immediate watchdog: %v", err)
	}
	if current == nil || current.ID != second.ID {
		t.Fatalf("current after immediate watchdog = %#v, want second id %d", current, second.ID)
	}
	storedThird, err := svc.store.Get(ctx, third.ID)
	if err != nil {
		t.Fatalf("get third: %v", err)
	}
	if storedThird.Status != queue.StatusReady {
		t.Fatalf("third status = %s, want %s", storedThird.Status, queue.StatusReady)
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("play calls = %d, want first event advance only", fakeOBS.playFileCalls)
	}

	generationStartedAt := svc.mediaProgressByInput[svc.cfg.OBSMediaSourceName].GenerationStartedAt
	settledFrom := generationStartedAt
	if currentSecond.StartedAt.After(settledFrom) {
		settledFrom = *currentSecond.StartedAt
	}
	tick = settledFrom.Add(obsEndedEventSettleGrace + time.Millisecond)
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("post-grace watchdog: %v", err)
	}
	current, err = svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current after post-grace watchdog: %v", err)
	}
	if current == nil || current.ID != third.ID {
		t.Fatalf("current after post-grace watchdog = %#v, want third id %d", current, third.ID)
	}
	if fakeOBS.playFileCalls != 2 {
		t.Fatalf("play calls = %d, want first event and post-grace watchdog advances", fakeOBS.playFileCalls)
	}
}

func TestQueueSettlingGuardDefersTransientPathMismatch(t *testing.T) {
	for _, state := range []obs.MediaState{obs.MediaStatePlaying, obs.MediaStateEnded} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			dbPath := filepath.Join(t.TempDir(), "queue.db")
			svc, fakeOBS, _ := newFallbackTestServiceAtDBPath(t, config.Config{FallbackMode: "off"}, dbPath)
			firstReady := addReadyVideoWithDuration(t, ctx, svc, "first.mp4", 0)
			firstPlaying, err := svc.store.MarkPlaying(ctx, firstReady.ID)
			if err != nil {
				t.Fatalf("mark first playing: %v", err)
			}
			second := addReadyVideoWithDuration(t, ctx, svc, "second.mp4", 0)
			third := addReadyVideo(t, ctx, svc, "third.mp4")

			oldStarted := time.Now().UTC().Add(-time.Hour)
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatalf("open raw db: %v", err)
			}
			if _, err := db.ExecContext(ctx, `
UPDATE videos SET started_at = ?, updated_at = ? WHERE id = ?
`, formatQueueTime(oldStarted), formatQueueTime(oldStarted), firstPlaying.ID); err != nil {
				_ = db.Close()
				t.Fatalf("age first started_at: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close raw db: %v", err)
			}

			tick := time.Now().UTC()
			svc.now = func() time.Time { return tick }
			svc.setPlaybackState(playbackNormal, 0, "")
			fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
				svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
			}
			fakeOBS.inputFiles = map[string]string{
				svc.cfg.OBSMediaSourceName: firstPlaying.LocalPath,
			}
			video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
				Type: obs.EventMediaEnded,
				Path: firstPlaying.LocalPath,
				At:   tick,
			})
			if err != nil {
				t.Fatalf("advance first: %v", err)
			}
			if video == nil || video.ID != second.ID {
				t.Fatalf("first event advanced to %#v, want second id %d", video, second.ID)
			}
			currentSecond, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current second: %v", err)
			}
			if currentSecond == nil || currentSecond.ID != second.ID || currentSecond.StartedAt == nil {
				t.Fatalf("current second = %#v, want id %d with started_at", currentSecond, second.ID)
			}
			generationStartedAt := svc.mediaProgressByInput[svc.cfg.OBSMediaSourceName].GenerationStartedAt

			fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: state}
			fakeOBS.inputFiles[svc.cfg.OBSMediaSourceName] = firstPlaying.LocalPath
			tick = generationStartedAt.Add(time.Millisecond)
			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("settling mismatch watchdog: %v", err)
			}
			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current after settling mismatch: %v", err)
			}
			if current == nil || current.ID != second.ID {
				t.Fatalf("current after settling mismatch = %#v, want second id %d", current, second.ID)
			}
			if fakeOBS.playFileCalls != 1 {
				t.Fatalf("settling mismatch play calls = %d, want initial transition only", fakeOBS.playFileCalls)
			}

			settledFrom := generationStartedAt
			if currentSecond.StartedAt.After(settledFrom) {
				settledFrom = *currentSecond.StartedAt
			}
			tick = settledFrom.Add(obsEndedEventSettleGrace + time.Millisecond)
			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("post-grace mismatch watchdog: %v", err)
			}
			current, err = svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current after post-grace mismatch: %v", err)
			}
			if current == nil || current.ID != second.ID {
				t.Fatalf("current after post-grace mismatch = %#v, want replayed second id %d", current, second.ID)
			}
			storedThird, err := svc.store.Get(ctx, third.ID)
			if err != nil {
				t.Fatalf("get third: %v", err)
			}
			if storedThird.Status != queue.StatusReady {
				t.Fatalf("third status = %s, want %s", storedThird.Status, queue.StatusReady)
			}
			if fakeOBS.playFileCalls != 2 || fakeOBS.lastPlayed != second.LocalPath {
				t.Fatalf("post-grace mismatch replay calls/path = %d/%q, want 2/%q", fakeOBS.playFileCalls, fakeOBS.lastPlayed, second.LocalPath)
			}
		})
	}
}

func TestPlaybackWatchdogIgnoresUnknownDuration(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	_ = addReadyVideo(t, ctx, svc, "next.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStatePlaying},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}
	svc.now = func() time.Time {
		return playing.StartedAt.Add(24 * time.Hour)
	}

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog: %v", err)
	}
	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current = %#v, want original id %d", current, playing.ID)
	}
	if fakeOBS.lastPlayed != "" {
		t.Fatalf("watchdog should not advance unknown duration, played %q", fakeOBS.lastPlayed)
	}
}

func TestPlaybackWatchdogAdvancesMissedTerminalEventWithUnknownDurationExactlyOnce(t *testing.T) {
	for _, state := range []obs.MediaState{
		obs.MediaStateStopped,
		obs.MediaStateEnded,
		obs.MediaStateError,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			svc, fakeOBS, fakeBot := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
			currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
			playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
			if err != nil {
				t.Fatalf("mark playing: %v", err)
			}
			next := addReadyVideo(t, ctx, svc, "next.mp4")
			svc.setPlaybackState(playbackNormal, 0, "")
			fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
				svc.cfg.OBSMediaSourceName: {State: state},
			}
			fakeOBS.inputFiles = map[string]string{
				svc.cfg.OBSMediaSourceName: playing.LocalPath,
			}
			svc.now = func() time.Time {
				return playing.StartedAt.Add(obsEndedEventSettleGrace + time.Millisecond)
			}

			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("first watchdog: %v", err)
			}
			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("second watchdog: %v", err)
			}

			stored, err := svc.store.Get(ctx, playing.ID)
			if err != nil {
				t.Fatalf("get original: %v", err)
			}
			if stored.Status != queue.StatusPlayed {
				t.Fatalf("original status = %s, want %s", stored.Status, queue.StatusPlayed)
			}
			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current: %v", err)
			}
			if current == nil || current.ID != next.ID {
				t.Fatalf("current = %#v, want next id %d", current, next.ID)
			}
			if fakeOBS.playFileCalls != 1 {
				t.Fatalf("play calls = %d, want exactly 1", fakeOBS.playFileCalls)
			}
			if len(fakeBot.messages) != 1 {
				t.Fatalf("notifications = %d, want exactly 1", len(fakeBot.messages))
			}
		})
	}
}

func TestPlaybackWatchdogKeepsHealthyUnknownDurationCurrent(t *testing.T) {
	for _, state := range []obs.MediaState{
		obs.MediaStatePlaying,
		obs.MediaStateOpening,
		obs.MediaStateBuffering,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
			currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
			playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
			if err != nil {
				t.Fatalf("mark playing: %v", err)
			}
			_ = addReadyVideo(t, ctx, svc, "next.mp4")
			svc.setPlaybackState(playbackNormal, 0, "")
			fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
				svc.cfg.OBSMediaSourceName: {State: state},
			}
			fakeOBS.inputFiles = map[string]string{
				svc.cfg.OBSMediaSourceName: playing.LocalPath,
			}

			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("watchdog: %v", err)
			}

			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current: %v", err)
			}
			if current == nil || current.ID != playing.ID {
				t.Fatalf("current = %#v, want original id %d", current, playing.ID)
			}
			if fakeOBS.playFileCalls != 0 {
				t.Fatalf("healthy state replayed %d times, want 0", fakeOBS.playFileCalls)
			}
		})
	}
}

func TestPlaybackWatchdogReplaysPausedOrEmptyCurrentWithoutSkipping(t *testing.T) {
	for _, state := range []obs.MediaState{obs.MediaStatePaused, obs.MediaStateNone} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
			currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
			playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
			if err != nil {
				t.Fatalf("mark playing: %v", err)
			}
			next := addReadyVideo(t, ctx, svc, "next.mp4")
			svc.setPlaybackState(playbackNormal, 0, "")
			fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
				svc.cfg.OBSMediaSourceName: {State: state},
			}
			fakeOBS.inputFiles = map[string]string{
				svc.cfg.OBSMediaSourceName: playing.LocalPath,
			}

			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("first watchdog: %v", err)
			}
			if err := svc.checkPlaybackWatchdog(ctx); err != nil {
				t.Fatalf("second watchdog: %v", err)
			}

			current, err := svc.store.Current(ctx)
			if err != nil {
				t.Fatalf("current: %v", err)
			}
			if current == nil || current.ID != playing.ID {
				t.Fatalf("current = %#v, want original id %d", current, playing.ID)
			}
			storedNext, err := svc.store.Get(ctx, next.ID)
			if err != nil {
				t.Fatalf("get next: %v", err)
			}
			if storedNext.Status != queue.StatusReady {
				t.Fatalf("next status = %s, want %s", storedNext.Status, queue.StatusReady)
			}
			if fakeOBS.playFileCalls != 1 {
				t.Fatalf("replay calls = %d, want exactly 1", fakeOBS.playFileCalls)
			}
			if fakeOBS.lastPlayed != playing.LocalPath {
				t.Fatalf("replayed path = %q, want %q", fakeOBS.lastPlayed, playing.LocalPath)
			}
		})
	}
}

func TestPlaybackWatchdogReplaysExpectedPathInsteadOfAdvancingStaleTerminalSource(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	next := addReadyVideo(t, ctx, svc, "next.mp4")
	svc.setPlaybackState(playbackNormal, 0, "")
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateEnded},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: next.LocalPath,
	}
	svc.now = func() time.Time {
		return playing.StartedAt.Add(obsEndedEventSettleGrace + time.Millisecond)
	}

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("watchdog: %v", err)
	}

	current, err := svc.store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.ID != playing.ID {
		t.Fatalf("current = %#v, want original id %d", current, playing.ID)
	}
	if fakeOBS.playFileCalls != 1 || fakeOBS.lastPlayed != playing.LocalPath {
		t.Fatalf("replay calls/path = %d/%q, want 1/%q", fakeOBS.playFileCalls, fakeOBS.lastPlayed, playing.LocalPath)
	}
}

func TestPlaybackWatchdogFrozenCursorUsesGraceAndProgressReset(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	svc.setPlaybackState(playbackNormal, 0, "")
	tick := playing.StartedAt.Add(time.Minute)
	svc.now = func() time.Time { return tick }
	cursor := 100.0
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {
			State:              obs.MediaStatePlaying,
			CursorMilliseconds: &cursor,
		},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("initial watchdog: %v", err)
	}
	tick = tick.Add(mediaProgressGrace - time.Second)
	cursor = 200
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("progress watchdog: %v", err)
	}
	tick = tick.Add(mediaProgressGrace - time.Second)
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("pre-grace watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 0 {
		t.Fatalf("progress reset still replayed %d times before grace", fakeOBS.playFileCalls)
	}
	tick = tick.Add(2 * time.Second)
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("stalled watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("frozen cursor replay calls = %d, want 1", fakeOBS.playFileCalls)
	}
}

func TestPlaybackWatchdogOpeningToBufferingProgressEventuallyStalls(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	svc.setPlaybackState(playbackNormal, 0, "")
	tick := playing.StartedAt.Add(time.Minute)
	svc.now = func() time.Time { return tick }
	fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
		svc.cfg.OBSMediaSourceName: {State: obs.MediaStateOpening},
	}
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}

	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("opening watchdog: %v", err)
	}
	tick = tick.Add(mediaProgressGrace - time.Second)
	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateBuffering}
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("buffering transition watchdog: %v", err)
	}
	tick = tick.Add(mediaProgressGrace - time.Second)
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("pre-grace buffering watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 0 {
		t.Fatalf("state transition did not reset progress grace; replay calls = %d", fakeOBS.playFileCalls)
	}
	tick = tick.Add(2 * time.Second)
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("stalled buffering watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("stalled buffering replay calls = %d, want 1", fakeOBS.playFileCalls)
	}
}

func TestPlaybackWatchdogOpeningBufferingOscillationHitsHardGrace(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	currentReady := addReadyVideoWithDuration(t, ctx, svc, "current.mp4", 0)
	playing, err := svc.store.MarkPlaying(ctx, currentReady.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	svc.setPlaybackState(playbackNormal, 0, "")
	tick := playing.StartedAt.Add(time.Minute)
	svc.now = func() time.Time { return tick }
	fakeOBS.inputFiles = map[string]string{
		svc.cfg.OBSMediaSourceName: playing.LocalPath,
	}

	for i := 0; i < 4; i++ {
		state := obs.MediaStateOpening
		if i%2 == 1 {
			state = obs.MediaStateBuffering
		}
		fakeOBS.mediaStatuses = map[string]obs.MediaInputStatus{
			svc.cfg.OBSMediaSourceName: {State: state},
		}
		if err := svc.checkPlaybackWatchdog(ctx); err != nil {
			t.Fatalf("oscillation watchdog %d: %v", i, err)
		}
		if fakeOBS.playFileCalls != 0 {
			t.Fatalf("oscillation replayed at tick %d before hard grace", i)
		}
		tick = tick.Add(mediaProgressGrace / 2)
	}

	fakeOBS.mediaStatuses[svc.cfg.OBSMediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateOpening}
	if err := svc.checkPlaybackWatchdog(ctx); err != nil {
		t.Fatalf("hard-grace watchdog: %v", err)
	}
	if fakeOBS.playFileCalls != 1 {
		t.Fatalf("oscillation hard-grace replay calls = %d, want 1", fakeOBS.playFileCalls)
	}
}

func TestRunReturnsWhenBotStopsUnexpectedly(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})

	err := svc.Run(ctx)

	if !errors.Is(err, ErrRequiredWorkerStopped) || !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("err = %v, want unexpected telegram worker stop", err)
	}
	if errors.Is(err, ErrWorkerShutdownStuck) {
		t.Fatalf("cooperative sibling shutdown reported stuck: %v", err)
	}
}

func TestRunFailsFastWhenLivenessReporterReturns(t *testing.T) {
	svc, _, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	botStarted := make(chan struct{})
	botStopped := make(chan struct{})
	fakeBot.run = func(ctx context.Context) error {
		close(botStarted)
		<-ctx.Done()
		close(botStopped)
		return ctx.Err()
	}
	wantErr := errors.New("supervisor pipe broke")
	reporterResult := make(chan error, 1)
	reporterResult <- wantErr
	svc.livenessReporter = &fakeRequiredReporter{
		start: func(context.Context) (<-chan error, error) {
			return reporterResult, nil
		},
	}

	err := svc.Run(context.Background())
	if !errors.Is(err, ErrRequiredInfrastructureStopped) ||
		!errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want required reporter failure %v", err, wantErr)
	}
	if errors.Is(err, ErrWorkerShutdownStuck) ||
		errors.Is(err, ErrInfrastructureShutdownStuck) {
		t.Fatalf("cooperative sibling shutdown reported stuck: %v", err)
	}
	select {
	case <-botStarted:
	default:
		t.Fatal("reporter started before the Telegram worker was launched")
	}
	select {
	case <-botStopped:
	default:
		t.Fatal("reporter failure did not cancel and drain Telegram worker")
	}
}

func TestRunFailsFastWhenInitialLivenessFrameFails(t *testing.T) {
	svc, _, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.workerStopGrace = 100 * time.Millisecond
	botStopped := make(chan struct{})
	fakeBot.run = func(ctx context.Context) error {
		<-ctx.Done()
		close(botStopped)
		return ctx.Err()
	}
	wantErr := errors.New("initial liveness write failed")
	svc.livenessReporter = &fakeRequiredReporter{
		start: func(context.Context) (<-chan error, error) {
			return nil, wantErr
		},
	}

	err := svc.Run(context.Background())
	if !errors.Is(err, ErrRequiredInfrastructureStopped) ||
		!errors.Is(err, wantErr) {
		t.Fatalf("Run error = %v, want synchronous reporter failure %v", err, wantErr)
	}
	if errors.Is(err, ErrWorkerShutdownStuck) {
		t.Fatalf("initial reporter failure did not drain siblings: %v", err)
	}
	select {
	case <-botStopped:
	default:
		t.Fatal("initial reporter failure did not cancel Telegram worker")
	}
}

func TestRunBoundsReporterDrainAfterWorkerFailure(t *testing.T) {
	svc, _, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.workerStopGrace = 20 * time.Millisecond
	neverReturns := make(chan error)
	svc.livenessReporter = &fakeRequiredReporter{
		start: func(context.Context) (<-chan error, error) {
			return neverReturns, nil
		},
	}

	start := time.Now()
	err := svc.Run(context.Background())
	if !errors.Is(err, ErrRequiredWorkerStopped) {
		t.Fatalf("Run error = %v, want worker failure", err)
	}
	if !errors.Is(err, ErrInfrastructureShutdownStuck) {
		t.Fatalf("Run error = %v, want bounded reporter drain failure", err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("reporter drain remained blocked for %s", elapsed)
	}
}

func TestRunCancellationDrainsRequiredReporterWithoutFatalClassification(t *testing.T) {
	svc, _, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	fakeBot.run = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	reporterStarted := make(chan struct{})
	svc.livenessReporter = &fakeRequiredReporter{
		start: func(ctx context.Context) (<-chan error, error) {
			result := make(chan error, 1)
			close(reporterStarted)
			go func() {
				<-ctx.Done()
				result <- ctx.Err()
			}()
			return result, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- svc.Run(ctx)
	}()
	select {
	case <-reporterStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("required reporter did not start")
	}
	cancel()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
		if errors.Is(err, ErrRequiredInfrastructureStopped) ||
			errors.Is(err, ErrInfrastructureShutdownStuck) {
			t.Fatalf("normal reporter cancellation classified fatal: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not drain required reporter")
	}
}

func TestRequiredWorkerBindingsKeepFixedPlaybackWireIDAcrossModes(t *testing.T) {
	tests := []struct {
		name          string
		playerMode    string
		playbackName  string
		playbackOwner liveness.Owner
	}{
		{
			name:          "queue watchdog",
			playerMode:    "queue",
			playbackName:  "playback-watchdog",
			playbackOwner: liveness.OwnerPlaybackWatchdog,
		},
		{
			name:          "library scheduler",
			playerMode:    "library",
			playbackName:  "library-scheduler",
			playbackOwner: liveness.OwnerLibraryScheduler,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, _ := newFallbackTestService(t, config.Config{
				FallbackMode: "off",
				PlayerMode:   tt.playerMode,
			})
			svc.livenessRegistry = liveness.NewRegistry(liveness.Options{})

			workers, err := svc.prepareRequiredWorkers()
			if err != nil {
				t.Fatalf("prepareRequiredWorkers: %v", err)
			}
			if len(workers) != liveness.RequiredWorkerCount {
				t.Fatalf("worker count = %d, want %d", len(workers), liveness.RequiredWorkerCount)
			}

			seen := make(map[liveness.WorkerID]string, len(workers))
			for _, worker := range workers {
				if prior := seen[worker.id]; prior != "" {
					t.Fatalf("logical worker %s owned by both %s and %s", worker.id, prior, worker.name)
				}
				seen[worker.id] = worker.name
			}
			if got := seen[liveness.WorkerPlayback]; got != tt.playbackName {
				t.Fatalf("playback implementation = %q, want %q", got, tt.playbackName)
			}

			snapshots := svc.livenessRegistry.Snapshots()
			for index, id := range liveness.RequiredWorkerIDs() {
				if snapshots[index].ID != id {
					t.Fatalf("snapshot %d ID = %s, want %s", index, snapshots[index].ID, id)
				}
				if snapshots[index].Phase != liveness.PhaseUnknown || snapshots[index].Sequence != 0 {
					t.Fatalf("snapshot %d pre-launch progress = %+v, want no worker-owned progress", index, snapshots[index])
				}
			}
			playback := snapshots[len(snapshots)-1]
			if playback.ID != liveness.WorkerPlayback || playback.Owner != tt.playbackOwner {
				t.Fatalf("playback snapshot = %+v, want fixed ID with owner %s", playback, tt.playbackOwner)
			}
		})
	}
}

func TestRunFailsBeforeStartingWorkersOnDuplicateLivenessOwner(t *testing.T) {
	svc, _, fakeBot := newFallbackTestService(t, config.Config{
		FallbackMode: "off",
		PlayerMode:   "queue",
	})
	registry := liveness.NewRegistry(liveness.Options{})
	if _, err := registry.Bind(liveness.WorkerTelegram, liveness.OwnerTelegram); err != nil {
		t.Fatalf("prime registry: %v", err)
	}
	svc.livenessRegistry = registry
	botStarted := false
	fakeBot.run = func(context.Context) error {
		botStarted = true
		return nil
	}

	err := svc.Run(context.Background())
	if !errors.Is(err, liveness.ErrDuplicateBinding) {
		t.Fatalf("Run error = %v, want %v", err, liveness.ErrDuplicateBinding)
	}
	if botStarted {
		t.Fatal("Telegram worker started before liveness ownership validation")
	}
}

func TestRunKeepsFourWorkerSequencesIndependentWhenTelegramWorkerStalls(t *testing.T) {
	svc, fakeOBS, fakeBot := newFallbackTestService(t, config.Config{
		FallbackMode: "off",
		PlayerMode:   "queue",
	})
	fakeOBS.events = make(chan obs.Event)
	svc.livenessRegistry = liveness.NewRegistry(liveness.Options{
		ProgressInterval: 2 * time.Millisecond,
	})
	telegramStarted := make(chan struct{})
	fakeBot.run = func(ctx context.Context) error {
		close(telegramStarted)
		// Deliberately do not advance the tracker: this required worker is the
		// single stalled owner while the other four loops remain schedulable.
		<-ctx.Done()
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-telegramStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Telegram worker did not start")
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	var before [liveness.RequiredWorkerCount]liveness.Snapshot
	for {
		before = svc.livenessRegistry.Snapshots()
		allStarted := true
		for _, snapshot := range before {
			if snapshot.Sequence == 0 {
				allStarted = false
				break
			}
		}
		if allStarted {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("workers did not all start: %+v", before)
		}
		time.Sleep(time.Millisecond)
	}

	for {
		after := svc.livenessRegistry.Snapshots()
		activeAdvanced := true
		for index, snapshot := range after {
			if snapshot.ID == liveness.WorkerTelegram {
				if snapshot.Sequence != before[index].Sequence {
					t.Fatalf(
						"stalled Telegram sequence advanced from %d to %d",
						before[index].Sequence,
						snapshot.Sequence,
					)
				}
				continue
			}
			if snapshot.Sequence <= before[index].Sequence {
				activeAdvanced = false
			}
		}
		if activeAdvanced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("four active workers did not advance independently: before=%+v after=%+v", before, after)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunReturnsWhenOBSEventStreamCloses(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	events := make(chan obs.Event)
	close(events)
	fakeOBS.events = events
	fakeBot.run = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	err := svc.Run(ctx)

	if !errors.Is(err, ErrRequiredWorkerStopped) {
		t.Fatalf("err = %v, want %v", err, ErrRequiredWorkerStopped)
	}
	for _, want := range []string{"obs-events", "OBS event stream closed"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want %q", err, want)
		}
	}
	if errors.Is(err, ErrWorkerShutdownStuck) {
		t.Fatalf("cooperative sibling shutdown reported stuck: %v", err)
	}
}

func TestRunBoundsWorkerDrainAfterFatalResult(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.workerStopGrace = 20 * time.Millisecond
	events := make(chan obs.Event)
	close(events)
	fakeOBS.events = events

	botStarted := make(chan struct{})
	botDone := make(chan struct{})
	releaseBot := make(chan struct{})
	fakeBot.run = func(context.Context) error {
		close(botStarted)
		defer close(botDone)
		<-releaseBot
		return nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-botStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("telegram worker did not start")
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRequiredWorkerStopped) {
			t.Fatalf("err = %v, want required worker failure", err)
		}
		if !errors.Is(err, ErrWorkerShutdownStuck) {
			t.Fatalf("err = %v, want bounded worker shutdown failure", err)
		}
		if !strings.Contains(err.Error(), "telegram") {
			t.Fatalf("err = %v, want pending telegram worker", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run remained blocked on a worker that ignored cancellation")
	}

	close(releaseBot)
	select {
	case <-botDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("test telegram worker did not drain after release")
	}
}

func TestRunReportsBlockedMaintenanceDuringFatalShutdown(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.workerStopGrace = 50 * time.Millisecond
	svc.maintenanceInterval = time.Millisecond

	events := make(chan obs.Event)
	fakeOBS.events = events
	fakeBot.run = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}

	maintenanceStarted := make(chan struct{})
	releaseMaintenance := make(chan struct{})
	maintenanceDone := make(chan struct{})
	firstMaintenance := true
	svc.maintenanceFn = func(ctx context.Context) maintenanceCycleResult {
		if !firstMaintenance {
			return maintenanceCycleResult{err: ctx.Err()}
		}
		firstMaintenance = false
		close(maintenanceStarted)
		<-releaseMaintenance
		close(maintenanceDone)
		return maintenanceCycleResult{}
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-maintenanceStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("maintenance worker did not start")
	}

	close(events)
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrRequiredWorkerStopped) {
			t.Fatalf("err = %v, want required worker failure", err)
		}
		if !errors.Is(err, ErrWorkerShutdownStuck) {
			t.Fatalf("err = %v, want bounded worker shutdown failure", err)
		}
		if !strings.Contains(err.Error(), "maintenance") {
			t.Fatalf("err = %v, want pending maintenance worker", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run remained blocked behind maintenance")
	}

	close(releaseMaintenance)
	select {
	case <-maintenanceDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("test maintenance worker did not drain after release")
	}
}

func TestRunNormalCancellationDrainsWorkersWithoutFatalError(t *testing.T) {
	svc, _, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.workerStopGrace = 100 * time.Millisecond
	botStarted := make(chan struct{})
	fakeBot.run = func(ctx context.Context) error {
		close(botStarted)
		<-ctx.Done()
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-botStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("telegram worker did not start")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want normal cancellation", err)
		}
		if errors.Is(err, ErrRequiredWorkerStopped) || errors.Is(err, ErrWorkerShutdownStuck) {
			t.Fatalf("normal cancellation was classified fatal: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not drain cooperative workers after cancellation")
	}
}

func TestWorkerDrainBudgetAllowsTelegramJournalFinalization(t *testing.T) {
	if defaultWorkerStopGrace != telegram.RecommendedParentDrainGrace {
		t.Fatalf(
			"default worker grace = %s, want Telegram budget %s",
			defaultWorkerStopGrace,
			telegram.RecommendedParentDrainGrace,
		)
	}
	if defaultWorkerStopGrace <= 9*time.Second {
		t.Fatalf(
			"default worker grace = %s, want more than 5s handler grace + 4s journal timeout",
			defaultWorkerStopGrace,
		)
	}

	svc, _, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	// Scale the production 5s + 4s + 1s composition down while preserving the
	// ordering: handler stop grace, independent journal finalization, cushion.
	svc.workerStopGrace = 150 * time.Millisecond
	botStarted := make(chan struct{})
	handlerGraceElapsed := make(chan struct{})
	journalCommitted := make(chan struct{})
	fakeBot.run = func(ctx context.Context) error {
		close(botStarted)
		<-ctx.Done()
		time.Sleep(50 * time.Millisecond)
		close(handlerGraceElapsed)
		time.Sleep(50 * time.Millisecond)
		close(journalCommitted)
		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-botStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Telegram worker did not start")
	}
	cancel()

	select {
	case <-handlerGraceElapsed:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Telegram handler stop grace did not elapse")
	}
	select {
	case <-journalCommitted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("outer worker drain ended before Telegram journal finalization")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
		if errors.Is(err, ErrWorkerShutdownStuck) {
			t.Fatalf("Telegram journal finalization exhausted worker drain: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not finish after Telegram journal finalization")
	}
}

func TestPeriodicMaintenanceConvergesTelegramJournalInBoundedBatches(t *testing.T) {
	const (
		doneLimit     = 10_000
		deadLimit     = 1_000
		pruneBatch    = 256
		unsafeDoneID  = 50_000
		unsafeDeadID  = 50_001
		oldDoneID     = 15_000
		oldDeadID     = 30_000
		checkpointMax = 60_000
	)
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newFallbackTestServiceAtDBPath(t, config.Config{}, dbPath)
	now := time.Now().UTC()

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open maintenance fixture database: %v", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin maintenance fixture transaction: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO telegram_update_attempts (
	update_id, update_kind, action, chat_id, message_id, actor_id,
	attempt_count, status, last_error, created_at, updated_at, finished_at
) VALUES (?, 'message', '', 0, 0, 0, 1, ?, '', ?, ?, ?)
`)
	if err != nil {
		t.Fatalf("prepare maintenance fixture insert: %v", err)
	}
	recent := now.Add(-time.Hour).Format(time.RFC3339Nano)
	old := now.Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	for id := 1; id <= doneLimit+pruneBatch+5; id++ {
		if _, err := stmt.ExecContext(ctx, id, "done", recent, recent, recent); err != nil {
			t.Fatalf("insert done row %d: %v", id, err)
		}
	}
	for id := 20_001; id <= 20_000+deadLimit+pruneBatch+5; id++ {
		if _, err := stmt.ExecContext(ctx, id, "dead", recent, recent, recent); err != nil {
			t.Fatalf("insert dead row %d: %v", id, err)
		}
	}
	for _, fixture := range []struct {
		id       int
		status   string
		finished string
	}{
		{id: oldDoneID, status: "done", finished: old},
		{id: oldDeadID, status: "dead", finished: old},
		{id: unsafeDoneID, status: "done", finished: old},
		{id: unsafeDeadID, status: "dead", finished: old},
	} {
		if _, err := stmt.ExecContext(
			ctx,
			fixture.id,
			fixture.status,
			fixture.finished,
			fixture.finished,
			fixture.finished,
		); err != nil {
			t.Fatalf("insert special terminal row %d: %v", fixture.id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close maintenance fixture insert: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint
SET next_offset = ?, confirmed_offset = ?
WHERE singleton = 1
`, checkpointMax, unsafeDoneID); err != nil {
		t.Fatalf("set maintenance checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit maintenance fixtures: %v", err)
	}

	countStatus := func(status string) int {
		t.Helper()
		var count int
		if err := db.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM telegram_update_attempts WHERE status = ?`,
			status,
		).Scan(&count); err != nil {
			t.Fatalf("count %s attempts: %v", status, err)
		}
		return count
	}
	doneCount := countStatus("done")
	deadCount := countStatus("dead")
	for pass := 1; ; pass++ {
		if err := svc.performMaintenance(ctx); err != nil {
			t.Fatalf("maintenance pass %d: %v", pass, err)
		}
		nextDone := countStatus("done")
		nextDead := countStatus("dead")
		if removed := doneCount - nextDone; removed < 0 || removed > pruneBatch {
			t.Fatalf("pass %d done removals = %d, want 0..%d", pass, removed, pruneBatch)
		}
		if removed := deadCount - nextDead; removed < 0 || removed > pruneBatch {
			t.Fatalf("pass %d dead removals = %d, want 0..%d", pass, removed, pruneBatch)
		}
		doneCount, deadCount = nextDone, nextDead
		if doneCount == doneLimit+1 && deadCount == deadLimit+1 {
			break
		}
		if pass >= 10 {
			t.Fatalf(
				"maintenance did not converge: done=%d dead=%d",
				doneCount,
				deadCount,
			)
		}
	}

	for _, fixture := range []struct {
		id   int
		want bool
	}{
		{id: oldDoneID, want: false},
		{id: oldDeadID, want: false},
		{id: unsafeDoneID, want: true},
		{id: unsafeDeadID, want: true},
	} {
		var count int
		if err := db.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM telegram_update_attempts WHERE update_id = ?`,
			fixture.id,
		).Scan(&count); err != nil {
			t.Fatalf("inspect update %d: %v", fixture.id, err)
		}
		if got := count == 1; got != fixture.want {
			t.Fatalf("update %d exists = %v, want %v", fixture.id, got, fixture.want)
		}
	}
}

func TestRunParentCancellationPropagatesLateTelegramStuckSentinel(t *testing.T) {
	svc, _, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "off"})
	svc.workerStopGrace = 500 * time.Millisecond
	botStarted := make(chan struct{})
	botSawCancellation := make(chan struct{})
	releaseBot := make(chan struct{})
	fakeBot.run = func(ctx context.Context) error {
		close(botStarted)
		<-ctx.Done()
		close(botSawCancellation)
		<-releaseBot
		return errors.Join(ctx.Err(), telegram.ErrUpdateHandlerStuck)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-botStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("telegram worker did not start")
	}
	cancel()
	select {
	case <-botSawCancellation:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("telegram worker did not observe parent cancellation")
	}
	close(releaseBot)

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want parent cancellation", err)
		}
		if !errors.Is(err, telegram.ErrUpdateHandlerStuck) {
			t.Fatalf("err = %v, want late Telegram stuck sentinel", err)
		}
		if !errors.Is(err, ErrRequiredWorkerStopped) {
			t.Fatalf("err = %v, want required worker failure", err)
		}
		if errors.Is(err, ErrWorkerShutdownStuck) {
			t.Fatalf("released Telegram worker was incorrectly reported pending: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Run did not propagate late Telegram stuck sentinel")
	}
}

func TestRemoveQueuedDoesNotDeleteLocalBotAPIFile(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
	})
	path := writeBotAPIFile(t, svc, "upload.mp4")

	video, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        path,
		TelegramFileID:   "file",
		TelegramUniqueID: "unique",
		FileName:         "upload.mp4",
		SizeBytes:        5,
	})
	if err != nil {
		t.Fatalf("enqueue upload: %v", err)
	}
	if err := svc.store.FinishCurrent(ctx); err != nil {
		t.Fatalf("finish current: %v", err)
	}
	ready := addReadyVideo(t, ctx, svc, "queued.mp4")
	if err := svc.RemoveQueued(ctx, ready.ID); err != nil {
		t.Fatalf("remove queued: %v", err)
	}
	if !fileExists(path) {
		t.Fatalf("local bot api file for played video #%d should remain", video.ID)
	}
	if !fileExists(ready.LocalPath) {
		t.Fatalf("queued local bot api file should remain")
	}
}

func assertFailedUploadVisible(t *testing.T, ctx context.Context, svc *Service, fileName, errText string) {
	t.Helper()
	history, err := svc.store.History(ctx, 10)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 1 {
		t.Fatalf("history length = %d, want 1", len(history))
	}
	video := history[0]
	if video.FileName != fileName {
		t.Fatalf("history file = %q, want %q", video.FileName, fileName)
	}
	if video.Status != queue.StatusFailed {
		t.Fatalf("status = %s, want %s", video.Status, queue.StatusFailed)
	}
	if video.LocalPath == "" {
		t.Fatal("failed upload must retain its Local Bot API path")
	}
	if !strings.Contains(video.Error, errText) {
		t.Fatalf("row error = %q, want %q", video.Error, errText)
	}
	statusText, err := svc.StatusText(ctx, true)
	if err != nil {
		t.Fatalf("status text: %v", err)
	}
	for _, want := range []string{"Ready：0", "Failed：1", "Last error：" + errText} {
		if !strings.Contains(statusText, want) {
			t.Fatalf("status text = %q, want %q", statusText, want)
		}
	}
	historyText, err := svc.HistoryText(ctx)
	if err != nil {
		t.Fatalf("history text: %v", err)
	}
	for _, want := range []string{"[failed]", fileName} {
		if !strings.Contains(historyText, want) {
			t.Fatalf("history text = %q, want %q", historyText, want)
		}
	}
}

func installMarkPlayingFailureTrigger(t *testing.T, dbPath string, videoID int64) func() {
	t.Helper()
	const triggerName = "fail_mark_playing_phase9"
	execQueueSQL(t, dbPath, fmt.Sprintf(`
CREATE TRIGGER %s
BEFORE UPDATE OF status ON videos
WHEN NEW.id = %d AND NEW.status = 'playing'
BEGIN
	SELECT RAISE(FAIL, 'phase9 transient mark playing');
END
`, triggerName, videoID))
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		execQueueSQL(t, dbPath, "DROP TRIGGER "+triggerName)
		dropped = true
	}
	t.Cleanup(drop)
	return drop
}

func execQueueSQL(t *testing.T, dbPath string, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw queue database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("execute raw queue SQL: %v", err)
	}
}

func assertQueuePlaybackIdle(t *testing.T, svc *Service) {
	t.Helper()
	if got := svc.playbackState(); got != playbackIdle {
		t.Fatalf("playback state = %s, want %s", got, playbackIdle)
	}
	if path := svc.currentPlaybackPath(); path != "" {
		t.Fatalf("current playback path = %q, want empty", path)
	}
	if progress, ok := svc.mediaProgressByInput[svc.cfg.OBSMediaSourceName]; ok {
		t.Fatalf("stale media progress remained after durable finish: %#v", progress)
	}
}

func assertBoundedStopContext(t *testing.T, fakeOBS *fakeOBS) {
	t.Helper()
	if fakeOBS.stopCurrentCalls != 1 {
		t.Fatalf("StopCurrent calls = %d, want 1", fakeOBS.stopCurrentCalls)
	}
	if !fakeOBS.stopHadDeadline {
		t.Fatal("StopCurrent cleanup context had no deadline")
	}
	remaining := time.Until(fakeOBS.stopDeadline)
	if remaining <= 0 || remaining > obsPlaybackCleanupTimeout {
		t.Fatalf(
			"StopCurrent cleanup deadline remaining = %s, want within (0, %s]",
			remaining,
			obsPlaybackCleanupTimeout,
		)
	}
}

func assertStoredStatus(t *testing.T, ctx context.Context, svc *Service, videoID int64, want queue.Status) {
	t.Helper()
	video, err := svc.store.Get(ctx, videoID)
	if err != nil {
		t.Fatalf("get video %d: %v", videoID, err)
	}
	if video.Status != want {
		t.Fatalf("video %d status = %s, want %s", videoID, video.Status, want)
	}
}

func assertRawStoredStatus(t *testing.T, dbPath string, videoID int64, want queue.Status) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw queue database: %v", err)
	}
	defer db.Close()
	var got queue.Status
	if err := db.QueryRowContext(
		context.Background(),
		`SELECT status FROM videos WHERE id = ?`,
		videoID,
	).Scan(&got); err != nil {
		t.Fatalf("query video %d status: %v", videoID, err)
	}
	if got != want {
		t.Fatalf("video %d status = %s, want %s", videoID, got, want)
	}
}

func newFallbackTestService(t *testing.T, cfg config.Config) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	return newFallbackTestServiceAtDBPath(t, cfg, filepath.Join(t.TempDir(), "queue.db"))
}

func newFallbackTestServiceAtDBPath(t *testing.T, cfg config.Config, dbPath string) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	ctx := context.Background()
	cfg.DatabasePath = dbPath
	store, err := queue.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	if cfg.FallbackMode == "" {
		cfg.FallbackMode = "random_played"
	}
	if cfg.RetentionMaxFiles == 0 {
		cfg.RetentionMaxFiles = 100
	}
	if cfg.TelegramBotAPIDir == "" {
		cfg.TelegramBotAPIDir = t.TempDir()
	}
	if cfg.OBSMediaSourceName == "" {
		cfg.OBSMediaSourceName = "media"
	}
	if err := os.MkdirAll(cfg.TelegramBotAPIDir, 0o755); err != nil {
		t.Fatalf("create telegram bot api dir: %v", err)
	}
	cfg.AllowedChatID = -100123
	fakeOBS := &fakeOBS{state: obs.StateConnected, mediaSourceName: cfg.OBSMediaSourceName}
	fakeBot := &fakeBot{}
	return &Service{
		cfg:      cfg,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
		obs:      fakeOBS,
		bot:      fakeBot,
		now:      time.Now,
		rng:      rand.New(rand.NewSource(1)),
		playback: playbackIdle,
	}, fakeOBS, fakeBot
}

func newLocalUploadTestService(t *testing.T, cfg config.Config) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	return newLocalUploadTestServiceAtDBPath(t, cfg, filepath.Join(t.TempDir(), "queue.db"))
}

func newLocalUploadTestServiceAtDBPath(t *testing.T, cfg config.Config, dbPath string) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	svc, fakeOBS, fakeBot := newFallbackTestServiceAtDBPath(t, cfg, dbPath)
	manager, err := media.NewManager(t.TempDir(), fakeFFProbe(t, 60))
	if err != nil {
		t.Fatalf("new media manager: %v", err)
	}
	svc.media = manager
	if svc.cfg.MaxQueueLength == 0 {
		svc.cfg.MaxQueueLength = 50
	}
	if svc.cfg.MaxVideoSizeBytes == 0 {
		svc.cfg.MaxVideoSizeBytes = 1024
	}
	return svc, fakeOBS, fakeBot
}

func newLibraryTestService(t *testing.T) (*Service, *fakeOBS) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "library.db")
	store, err := queue.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open queue store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	libDB, err := medialib.OpenState(ctx, dbPath)
	if err != nil {
		t.Fatalf("open library state: %v", err)
	}
	t.Cleanup(func() {
		_ = libDB.Close()
	})
	mediaDir := t.TempDir()
	botAPIDir := t.TempDir()
	cfg := config.Config{
		PlayerMode:              "library",
		MediaDir:                mediaDir,
		LoopMediaDir:            filepath.Join(mediaDir, "loops"),
		MusicMediaDir:           filepath.Join(mediaDir, "music"),
		TelegramBotAPIDir:       botAPIDir,
		OBSLoopSourceName:       "loop_source",
		OBSMusicSourceName:      "music_source",
		OBSMediaSourceName:      "queue_source",
		MaxVideoSizeBytes:       1024 * 1024,
		MaxVideoDurationSeconds: 120,
		AllowedChatID:           -100123,
	}
	if err := os.MkdirAll(cfg.LoopMediaDir, 0o755); err != nil {
		t.Fatalf("create loop dir: %v", err)
	}
	if err := os.MkdirAll(cfg.MusicMediaDir, 0o755); err != nil {
		t.Fatalf("create music dir: %v", err)
	}
	manager, err := media.NewManager(mediaDir, fakeFFProbe(t, 30))
	if err != nil {
		t.Fatalf("new media manager: %v", err)
	}
	fakeOBS := &fakeOBS{
		state:           obs.StateConnected,
		sourcePlayed:    make(map[string]string),
		mediaSourceName: cfg.OBSMediaSourceName,
	}
	return &Service{
		cfg:      cfg,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
		libDB:    libDB,
		media:    manager,
		obs:      fakeOBS,
		bot:      &fakeBot{},
		now:      time.Now,
		rng:      rand.New(rand.NewSource(1)),
		playback: playbackIdle,
	}, fakeOBS
}

func addReadyVideo(t *testing.T, ctx context.Context, svc *Service, name string) queue.Video {
	t.Helper()
	return addReadyVideoWithDuration(t, ctx, svc, name, 60)
}

func addReadyVideoWithDuration(t *testing.T, ctx context.Context, svc *Service, name string, durationSeconds int) queue.Video {
	t.Helper()
	store := svc.store
	path := filepath.Join(svc.cfg.TelegramBotAPIDir, name)
	writeTestFile(t, path)
	video, err := store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   name,
		TelegramUniqueID: name,
		FileName:         name,
		LocalPath:        path,
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	ready, err := store.MarkReady(ctx, video.ID, path, 100, durationSeconds)
	if err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	return ready
}

func addPlayedVideo(t *testing.T, ctx context.Context, svc *Service, name string, createFile bool) queue.Video {
	t.Helper()
	ready := addReadyVideo(t, ctx, svc, name)
	if !createFile {
		if err := osRemove(ready.LocalPath); err != nil {
			t.Fatalf("remove test file: %v", err)
		}
	}
	return markReadyVideoPlayed(t, ctx, svc, ready)
}

func addPlayedVideoWithPath(t *testing.T, ctx context.Context, svc *Service, name string, path string) queue.Video {
	t.Helper()
	store := svc.store
	video, err := store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   name,
		TelegramUniqueID: name,
		FileName:         name,
		LocalPath:        path,
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	ready, err := store.MarkReady(ctx, video.ID, path, 100, 60)
	if err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	return markReadyVideoPlayed(t, ctx, svc, ready)
}

func markReadyVideoPlayed(t *testing.T, ctx context.Context, svc *Service, ready queue.Video) queue.Video {
	t.Helper()
	store := svc.store
	playing, err := store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	if err := store.FinishCurrent(ctx); err != nil {
		t.Fatalf("finish current: %v", err)
	}
	played, err := store.Get(ctx, playing.ID)
	if err != nil {
		t.Fatalf("get played: %v", err)
	}
	return played
}

func seedTerminalRowsAtDBPath(t *testing.T, ctx context.Context, dbPath string, status queue.Status, count int) {
	t.Helper()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open terminal seed database: %v", err)
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin terminal seed: %v", err)
	}
	defer tx.Rollback()
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO videos (
	telegram_file_id, telegram_unique_id, submitter_id, submitter_name, chat_id, message_id,
	file_name, local_path, mime_type, size_bytes, duration_seconds, queue_position, status,
	error, created_at, updated_at, finished_at
) VALUES (?, ?, 0, '', 0, 0, ?, '', 'video/mp4', 100, 60, 0, ?, '', ?, ?, ?)
`)
	if err != nil {
		t.Fatalf("prepare terminal seed: %v", err)
	}
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%s-seed-%06d.mp4", status, i)
		at := base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano)
		if _, err := stmt.ExecContext(ctx, name, name, name, string(status), at, at, at); err != nil {
			_ = stmt.Close()
			t.Fatalf("seed terminal row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close terminal seed statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit terminal seed: %v", err)
	}
}

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create file parent: %v", err)
	}
	if err := osWriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
}

func writeBotAPIFile(t *testing.T, svc *Service, name string) string {
	t.Helper()
	path := filepath.Join(svc.cfg.TelegramBotAPIDir, name)
	writeTestFile(t, path)
	return path
}

func writeLibraryFile(t *testing.T, dir string, name string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeTestFile(t, path)
	return path
}

func fixedNow(raw string) func() time.Time {
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	return func() time.Time {
		return parsed
	}
}

func fakeFFProbe(t *testing.T, duration int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	body := []byte("#!/bin/sh\nprintf '{\"format\":{\"duration\":\"" + formatTestInt(duration) + "\"}}'\n")
	if err := osWriteFile(path, body, 0o700); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	return path
}

func fakeFailingFFProbe(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	body := []byte("#!/bin/sh\nprintf 'probe exploded' >&2\nexit 2\n")
	if err := osWriteFile(path, body, 0o700); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	return path
}

func formatTestInt(value int) string {
	return fmt.Sprintf("%d", value)
}

func formatQueueTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func fileExists(path string) bool {
	_, err := osStat(path)
	return err == nil
}

var (
	osWriteFile = os.WriteFile
	osRemove    = os.Remove
	osStat      = os.Stat
)

type fakeOBS struct {
	state            obs.State
	events           <-chan obs.Event
	lastPlayed       string
	lastSource       string
	sourcePlayed     map[string]string
	sourcePlayCalls  map[string]int
	mediaStatuses    map[string]obs.MediaInputStatus
	mediaStatusErrs  map[string]error
	mediaStatusCalls map[string]int
	inputFiles       map[string]string
	inputSettingsErr map[string]error
	inputSettingCall map[string]int
	mediaSourceName  string
	connectErr       error
	connectNotify    chan struct{}
	connectCalls     int
	probeErr         error
	probeCalls       int
	playFileCalls    int
	playErr          error
	stopCurrentCalls int
	stopErr          error
	stopHadDeadline  bool
	stopDeadline     time.Time
}

func (f *fakeOBS) Connect(context.Context) error {
	f.connectCalls++
	if f.connectErr != nil {
		return f.connectErr
	}
	f.state = obs.StateConnected
	if f.connectNotify != nil {
		f.connectNotify <- struct{}{}
	}
	return nil
}
func (f *fakeOBS) Close() error             { return nil }
func (f *fakeOBS) Events() <-chan obs.Event { return f.events }
func (f *fakeOBS) Probe(context.Context) error {
	f.probeCalls++
	return f.probeErr
}
func (f *fakeOBS) GetMediaInputStatus(_ context.Context, inputName string) (obs.MediaInputStatus, error) {
	if f.mediaStatusCalls == nil {
		f.mediaStatusCalls = make(map[string]int)
	}
	f.mediaStatusCalls[inputName]++
	if err := f.mediaStatusErrs[inputName]; err != nil {
		return obs.MediaInputStatus{}, err
	}
	if status, ok := f.mediaStatuses[inputName]; ok {
		return status, nil
	}
	return obs.MediaInputStatus{State: obs.MediaStatePlaying}, nil
}
func (f *fakeOBS) GetInputSettings(_ context.Context, inputName string) (obs.InputSettings, error) {
	if f.inputSettingCall == nil {
		f.inputSettingCall = make(map[string]int)
	}
	f.inputSettingCall[inputName]++
	if err := f.inputSettingsErr[inputName]; err != nil {
		return obs.InputSettings{}, err
	}
	return obs.InputSettings{
		LocalFile: f.inputFiles[inputName],
		InputKind: "ffmpeg_source",
	}, nil
}
func (f *fakeOBS) PlayFile(_ context.Context, path string) error {
	if f.playErr != nil {
		return f.playErr
	}
	f.playFileCalls++
	if f.inputFiles == nil {
		f.inputFiles = make(map[string]string)
	}
	if f.mediaStatuses == nil {
		f.mediaStatuses = make(map[string]obs.MediaInputStatus)
	}
	f.inputFiles[f.mediaSourceName] = path
	f.mediaStatuses[f.mediaSourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	f.lastPlayed = path
	f.lastSource = ""
	return nil
}
func (f *fakeOBS) PlaySourceFile(_ context.Context, sourceName string, path string, _ obs.PlaySourceOptions) error {
	if f.playErr != nil {
		return f.playErr
	}
	if f.sourcePlayed == nil {
		f.sourcePlayed = make(map[string]string)
	}
	if f.sourcePlayCalls == nil {
		f.sourcePlayCalls = make(map[string]int)
	}
	if f.mediaStatuses == nil {
		f.mediaStatuses = make(map[string]obs.MediaInputStatus)
	}
	if f.inputFiles == nil {
		f.inputFiles = make(map[string]string)
	}
	f.sourcePlayed[sourceName] = path
	f.sourcePlayCalls[sourceName]++
	f.inputFiles[sourceName] = path
	f.mediaStatuses[sourceName] = obs.MediaInputStatus{State: obs.MediaStatePlaying}
	f.lastPlayed = path
	f.lastSource = sourceName
	return nil
}
func (f *fakeOBS) StopCurrent(ctx context.Context) error {
	f.stopCurrentCalls++
	f.stopDeadline, f.stopHadDeadline = ctx.Deadline()
	if f.stopErr != nil {
		return f.stopErr
	}
	f.lastPlayed = ""
	if f.mediaStatuses == nil {
		f.mediaStatuses = make(map[string]obs.MediaInputStatus)
	}
	f.mediaStatuses[f.mediaSourceName] = obs.MediaInputStatus{State: obs.MediaStateStopped}
	return nil
}
func (f *fakeOBS) StopSource(_ context.Context, sourceName string) error {
	if f.sourcePlayed != nil {
		delete(f.sourcePlayed, sourceName)
	}
	if f.mediaStatuses == nil {
		f.mediaStatuses = make(map[string]obs.MediaInputStatus)
	}
	f.mediaStatuses[sourceName] = obs.MediaInputStatus{State: obs.MediaStateStopped}
	if f.lastSource == sourceName {
		f.lastPlayed = ""
	}
	return nil
}
func (f *fakeOBS) Status() obs.Status { return obs.Status{State: f.state} }

type fakeBot struct {
	messages []string
	run      func(context.Context) error
}

func (f *fakeBot) Run(ctx context.Context) error {
	if f.run != nil {
		return f.run(ctx)
	}
	return nil
}
func (f *fakeBot) SendMessage(_ context.Context, _ int64, text string) error {
	f.messages = append(f.messages, text)
	return nil
}

type appTestFrameSink struct {
	closed bool
}

func (sink *appTestFrameSink) WriteFrame([]byte) (bool, error) {
	return true, nil
}

func (sink *appTestFrameSink) Close() error {
	sink.closed = true
	return nil
}

type fakeRequiredReporter struct {
	start  func(context.Context) (<-chan error, error)
	closed bool
}

func (reporter *fakeRequiredReporter) Start(ctx context.Context) (<-chan error, error) {
	return reporter.start(ctx)
}

func (reporter *fakeRequiredReporter) Close() error {
	reporter.closed = true
	return nil
}
