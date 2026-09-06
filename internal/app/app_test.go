package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/journalstore"
	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
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

func TestNewSecuresDataDirBeforeStateAccess(t *testing.T) {
	for _, preexisting := range []bool{false, true} {
		name := "new"
		if preexisting {
			name = "preexisting"
		}
		t.Run(name, func(t *testing.T) {
			isolateSingletonUserDirectory(t)
			root := t.TempDir()
			dataDir := filepath.Join(root, "data")
			if preexisting {
				if err := os.Mkdir(dataDir, 0o755); err != nil {
					t.Fatalf("create permissive data dir: %v", err)
				}
				if err := os.Chmod(dataDir, 0o755); err != nil {
					t.Fatalf("make data dir permissive: %v", err)
				}
			}

			databasePath := filepath.Join(dataDir, "queue.db")
			token := "123456789:data-dir-permission-" + name
			lock, err := singleton.Acquire(databasePath, token)
			if err != nil {
				t.Fatalf("hold backend lock: %v", err)
			}
			defer lock.Close()

			service, err := New(config.Config{
				TelegramBotToken: token,
				DataDir:          dataDir,
				DatabasePath:     databasePath,
			}, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if service != nil {
				service.Close()
				t.Fatal("New returned a service while the backend lock was held")
			}
			if !errors.Is(err, singleton.ErrAlreadyRunning) {
				t.Fatalf("New error = %v, want singleton contention", err)
			}
			if strings.Contains(err.Error(), token) {
				t.Fatalf("initialization error leaked Telegram token: %v", err)
			}

			info, err := os.Stat(dataDir)
			if err != nil {
				t.Fatalf("stat data dir: %v", err)
			}
			if got := info.Mode().Perm(); got != 0o700 {
				t.Fatalf("data dir mode = %04o, want 0700", got)
			}
		})
	}
}

func TestNewWithExplicitRuntimePathsDoesNotTouchUnusedBaseDirectories(t *testing.T) {
	isolateSingletonUserDirectory(t)
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime", "library.db")
	const token = "123456789:explicit-runtime-paths"
	unusedDataDir := filepath.Join(os.DevNull, "unused-data-base")
	unusedMediaDir := filepath.Join(os.DevNull, "unused-media-base")
	loopDir := filepath.Join(root, "library", "loops")
	musicDir := filepath.Join(root, "library", "music")
	service, err := New(config.Config{
		TelegramBotToken: token,
		// Use a deterministic, non-network transport error after runtime paths
		// have been initialized.
		TelegramAPIBaseURL: "file:///nonexistent",
		TelegramBotAPIDir:  filepath.Join(root, "telegram-bot-api"),
		AllowedChatID:      1,
		OBSHost:            "127.0.0.1",
		OBSPort:            4455,
		OBSLoopSourceName:  "Loop",
		OBSMusicSourceName: "Music",
		DataDir:            unusedDataDir,
		MediaDir:           unusedMediaDir,
		LoopMediaDir:       loopDir,
		MusicMediaDir:      musicDir,
		DatabasePath:       databasePath,
		MaxVideoSizeBytes:  1024 * 1024,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if service != nil {
		service.Close()
		t.Fatal("New returned a service after the intentional Telegram transport failure")
	}
	if err == nil || !strings.Contains(err.Error(), "create telegram bot") {
		t.Fatalf("New error = %v, want intentional late Telegram initialization failure", err)
	}

	if _, err := os.Stat(unusedDataDir); err == nil {
		t.Fatalf("unused DATA_DIR was created: %s", unusedDataDir)
	}
	if _, err := os.Stat(unusedMediaDir); err == nil {
		t.Fatalf("unused MEDIA_DIR was created: %s", unusedMediaDir)
	}
	for label, path := range map[string]string{
		"database":      databasePath,
		"loop library":  loopDir,
		"music library": musicDir,
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("%s explicit path was not initialized: %v", label, err)
		}
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

func TestLibraryScanCheckpointsCachedAssets(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	if err := svc.ScanLibrary(context.Background()); err != nil {
		t.Fatalf("warm validation cache: %v", err)
	}

	registry := liveness.NewRegistry(liveness.Options{})
	worker, err := registry.Bind(liveness.WorkerTelegram, liveness.OwnerTelegram)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	worker.Advance(liveness.PhaseOperation)
	ctx := liveness.WithWorker(context.Background(), worker)
	before := worker.Snapshot().Sequence

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("cached ScanLibrary: %v", err)
	}
	snapshot := worker.Snapshot()
	if snapshot.Phase != liveness.PhaseOperation {
		t.Fatalf("scan phase = %s, want operation after scope restore", snapshot.Phase)
	}
	if snapshot.Sequence <= before+2 {
		t.Fatalf(
			"cached scan sequence = %d, want completed-work checkpoints beyond scope entry/exit from %d",
			snapshot.Sequence,
			before,
		)
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

func TestLibraryReconnectReplaysActiveSourcesAfterOBSReset(t *testing.T) {
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
	loopCount := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	musicCount := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
	if loopPath == "" || musicPath == "" {
		t.Fatalf("expected initial loop and music playback, loop=%q music=%q", loopPath, musicPath)
	}

	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateNone}
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateNone}
	if err := svc.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback: %v", err)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSLoopSourceName]; got != loopPath {
		t.Fatalf("recovered loop path = %q, want %q", got, loopPath)
	}
	if got := fakeOBS.sourcePlayed[svc.cfg.OBSMusicSourceName]; got != musicPath {
		t.Fatalf("recovered music path = %q, want %q", got, musicPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCount+1 {
		t.Fatalf("loop play count = %d, want %d", got, loopCount+1)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCount+1 {
		t.Fatalf("music play count = %d, want %d", got, musicCount+1)
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

func TestLibraryLoopEndedEventIgnoresStalePath(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	stalePath := svc.activeLoopPath
	if stalePath == "" {
		t.Fatal("expected initial loop path")
	}
	var nextLoopID string
	for _, loop := range svc.librarySnapshot.Loops {
		if loop.Path != stalePath {
			nextLoopID = loop.ID
			break
		}
	}
	if nextLoopID == "" {
		t.Fatal("expected a second loop")
	}
	if _, err := svc.SelectLoopText(ctx, nextLoopID); err != nil {
		t.Fatalf("select next loop: %v", err)
	}
	currentPath := svc.activeLoopPath
	if currentPath == "" || currentPath == stalePath {
		t.Fatalf("current loop path = %q, want different from stale %q", currentPath, stalePath)
	}
	playCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]

	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{
		Type:      obs.EventMediaEnded,
		InputName: svc.cfg.OBSLoopSourceName,
		Path:      stalePath,
	}); err != nil {
		t.Fatalf("handle stale loop ended event: %v", err)
	}
	if svc.activeLoopPath != currentPath {
		t.Fatalf("active loop path = %q, want unchanged %q", svc.activeLoopPath, currentPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playCalls {
		t.Fatalf("loop play calls = %d, want unchanged %d", got, playCalls)
	}
}

func TestLibraryActiveLoopPeriodTracksClockRollbackForDirectOverride(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	loopID := svc.librarySnapshot.Loops[0].ID
	if err := svc.libDB.SetDirectLoopOverride(ctx, overrideDateKey(tick), loopID); err != nil {
		t.Fatalf("set direct override: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure playback: %v", err)
	}
	if got := svc.activeLoopPeriod; got != medialib.PeriodDay {
		t.Fatalf("active loop period after ensure = %q, want scheduler period %q", got, medialib.PeriodDay)
	}

	playsBeforeRollback := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
	tick = fixedNow("2026-06-24T08:00:00+08:00")()
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensure after clock rollback: %v", err)
	}
	if got := svc.activeLoopPeriod; got != medialib.PeriodMorning {
		t.Fatalf("active loop period after rollback = %q, want %q", got, medialib.PeriodMorning)
	}
	wantEndsAt := fixedNow("2026-06-24T11:00:00+08:00")()
	if !svc.activeLoopEndsAt.Equal(wantEndsAt) {
		t.Fatalf("active loop end = %s, want %s", svc.activeLoopEndsAt, wantEndsAt)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playsBeforeRollback+1 {
		t.Fatalf("loop play calls = %d, want %d after clock rollback", got, playsBeforeRollback+1)
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

func TestLibraryReconciliationOpeningSourceFailsClosedWhenNoAlternate(t *testing.T) {
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
	if err := svc.reconcileLibraryPlayback(ctx); err == nil {
		t.Fatal("stalled reconcile succeeded without a healthy alternate")
	}

	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != loopCalls {
		t.Fatalf("stalled loop replay calls = %d, want unchanged %d", got, loopCalls)
	}
	if got := fakeOBS.sourceStopCalls[svc.cfg.OBSLoopSourceName]; got != 1 {
		t.Fatalf("stalled loop stop calls = %d, want 1", got)
	}
	if svc.activeLoopPath != "" {
		t.Fatalf("stalled loop remained active without alternate: %q", svc.activeLoopPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("progressing music play calls = %d, want unchanged %d", got, musicCalls)
	}
}

func TestShortLoopAtSameSampledCursorPhaseUsesNonHarmonicConfirmation(t *testing.T) {
	svc, fakeOBS := newLibraryTestService(t)
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }
	path := filepath.Join(svc.cfg.LoopMediaDir, "loop_day_cafe_short.mp4")
	cursor := 1000.0
	advancedCursor := 1250.0
	duration := 5000.0
	status := obs.MediaInputStatus{
		State:                obs.MediaStatePlaying,
		DurationMilliseconds: &duration,
		CursorMilliseconds:   &cursor,
	}

	svc.resetMediaProgressLocked(svc.cfg.OBSLoopSourceName, path)
	if stalled := svc.mediaInputStalledLocked(svc.cfg.OBSLoopSourceName, path, status); stalled {
		t.Fatal("initial loop observation was declared stalled")
	}
	tick = tick.Add(mediaProgressHardGrace + 5*time.Second)
	fakeOBS.inputFiles = map[string]string{svc.cfg.OBSLoopSourceName: path}
	fakeOBS.mediaStatusSequence = map[string][]obs.MediaInputStatus{
		svc.cfg.OBSLoopSourceName: {
			status,
			{
				State:                obs.MediaStatePlaying,
				DurationMilliseconds: &duration,
				CursorMilliseconds:   &advancedCursor,
			},
		},
	}
	inspection, err := svc.inspectMediaInputLocked(
		context.Background(),
		svc.cfg.OBSLoopSourceName,
		path,
	)
	if err != nil {
		t.Fatalf("inspect short loop: %v", err)
	}
	if inspection.Stalled {
		t.Fatal("advancing short loop was declared stalled after confirmation sample")
	}
	if got := fakeOBS.mediaStatusCalls[svc.cfg.OBSLoopSourceName]; got != 2 {
		t.Fatalf("media status calls = %d, want initial plus confirmation sample", got)
	}
}

func TestFrozenShortLoopAtHarmonicSamplePhaseIsDeclaredStalled(t *testing.T) {
	svc, fakeOBS := newLibraryTestService(t)
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }
	path := filepath.Join(svc.cfg.LoopMediaDir, "loop_day_cafe_frozen.mp4")
	cursor := 1000.0
	duration := 5000.0
	status := obs.MediaInputStatus{
		State:                obs.MediaStatePlaying,
		DurationMilliseconds: &duration,
		CursorMilliseconds:   &cursor,
	}
	svc.resetMediaProgressLocked(svc.cfg.OBSLoopSourceName, path)
	if stalled := svc.mediaInputStalledLocked(svc.cfg.OBSLoopSourceName, path, status); stalled {
		t.Fatal("initial loop observation was declared stalled")
	}
	tick = tick.Add(mediaProgressHardGrace + 5*time.Second)
	fakeOBS.inputFiles = map[string]string{svc.cfg.OBSLoopSourceName: path}
	fakeOBS.mediaStatusSequence = map[string][]obs.MediaInputStatus{
		svc.cfg.OBSLoopSourceName: {status, status},
	}

	inspection, err := svc.inspectMediaInputLocked(
		context.Background(),
		svc.cfg.OBSLoopSourceName,
		path,
	)
	if err != nil {
		t.Fatalf("inspect frozen short loop: %v", err)
	}
	if !inspection.Stalled {
		t.Fatal("phase-locked frozen short loop was not declared stalled")
	}
}

func TestMediaCursorConfirmationDelayAvoidsReportedLoopHarmonics(t *testing.T) {
	for _, durationMillis := range []float64{1, 125, 250, 500, 1500, 5000} {
		durationMillis := durationMillis
		t.Run(fmt.Sprintf("%.3fms", durationMillis), func(t *testing.T) {
			status := obs.MediaInputStatus{
				DurationMilliseconds: &durationMillis,
			}
			delayMillis := float64(mediaCursorConfirmationDelay(status)) /
				float64(time.Millisecond)
			phase := math.Mod(delayMillis, durationMillis)
			distance := math.Min(phase, durationMillis-phase)
			if distance < 0.000_001 {
				t.Fatalf(
					"confirmation delay %.9fms is harmonic with %.9fms loop",
					delayMillis,
					durationMillis,
				)
			}
			if delay := mediaCursorConfirmationDelay(status); delay < mediaCursorConfirmationMin ||
				delay > mediaCursorConfirmationMax {
				t.Fatalf(
					"confirmation delay = %s, want bounded in [%s,%s]",
					delay,
					mediaCursorConfirmationMin,
					mediaCursorConfirmationMax,
				)
			}
		})
	}
}

func TestFrozenLongLoopCursorStillStalls(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	tick := fixedNow("2026-06-24T12:00:00+08:00")()
	svc.now = func() time.Time { return tick }
	path := filepath.Join(svc.cfg.LoopMediaDir, "loop_day_cafe_long.mp4")
	cursor := 1000.0
	duration := float64((10 * time.Minute) / time.Millisecond)
	status := obs.MediaInputStatus{
		State:                obs.MediaStatePlaying,
		DurationMilliseconds: &duration,
		CursorMilliseconds:   &cursor,
	}

	svc.resetMediaProgressLocked(svc.cfg.OBSLoopSourceName, path)
	if stalled := svc.mediaInputStalledLocked(svc.cfg.OBSLoopSourceName, path, status); stalled {
		t.Fatal("initial loop observation was declared stalled")
	}
	tick = tick.Add(mediaProgressHardGrace + time.Second)
	if stalled := svc.mediaInputStalledLocked(svc.cfg.OBSLoopSourceName, path, status); !stalled {
		t.Fatal("unchanged long-loop cursor was not declared stalled")
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

func TestLibraryLoopEndedEventClearsActiveStateWhenReplayFails(t *testing.T) {
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
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	fakeOBS.playErr = errors.New("obs unavailable")
	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{Type: obs.EventMediaEnded, InputName: svc.cfg.OBSLoopSourceName}); err == nil {
		t.Fatalf("expected loop replay failure")
	}
	if svc.activeLoopID != "" || svc.activeLoopPath != "" || !svc.activeLoopEndsAt.IsZero() {
		t.Fatalf("active loop state should be cleared after failed ended-event replay: id=%q path=%q ends=%s", svc.activeLoopID, svc.activeLoopPath, svc.activeLoopEndsAt)
	}

	fakeOBS.playErr = nil
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("retry library playback: %v", err)
	}
	if svc.activeLoopID == "" || svc.activeLoopPath == "" {
		t.Fatalf("expected retry to restore active loop state")
	}
}

func TestLibraryMusicEndedEventClearsActiveStateWhenReplayFails(t *testing.T) {
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
	tick = tick.Add(obsEndedEventSettleGrace + time.Millisecond)
	fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{State: obs.MediaStateEnded}
	fakeOBS.playErr = errors.New("obs unavailable")
	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{Type: obs.EventMediaEnded, InputName: svc.cfg.OBSMusicSourceName}); err == nil {
		t.Fatalf("expected music replay failure")
	}
	if svc.activeMusicID != "" || svc.activeMusicPath != "" {
		t.Fatalf("active music state should be cleared after failed ended-event replay: id=%q path=%q", svc.activeMusicID, svc.activeMusicPath)
	}

	fakeOBS.playErr = nil
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("retry library playback: %v", err)
	}
	if svc.activeMusicID == "" || svc.activeMusicPath == "" {
		t.Fatalf("expected retry to restore active music state")
	}
}

func TestLibraryMusicEndedEventIgnoresStalePath(t *testing.T) {
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
	stalePath := svc.activeMusicPath
	if stalePath == "" {
		t.Fatal("expected initial music path")
	}
	if _, err := svc.SkipMusicText(ctx); err != nil {
		t.Fatalf("skip music: %v", err)
	}
	currentPath := svc.activeMusicPath
	if currentPath == "" || currentPath == stalePath {
		t.Fatalf("current music path = %q, want different from stale %q", currentPath, stalePath)
	}
	playCount := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]

	if err := svc.handleLibraryOBSEvent(ctx, obs.Event{
		Type:      obs.EventMediaEnded,
		InputName: svc.cfg.OBSMusicSourceName,
		Path:      stalePath,
	}); err != nil {
		t.Fatalf("handle stale music ended event: %v", err)
	}
	if svc.activeMusicPath != currentPath {
		t.Fatalf("active music path = %q, want unchanged %q", svc.activeMusicPath, currentPath)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != playCount {
		t.Fatalf("music play count = %d, want unchanged %d", got, playCount)
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

func TestLibraryMissingThemeOverrideReusesFallbackPlan(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	if err := svc.libDB.SetThemeOverride(ctx, "2026-06-24", "study"); err != nil {
		t.Fatalf("set theme override: %v", err)
	}
	fallback := svc.librarySnapshot.Loops[0]
	if err := svc.libDB.SavePeriodPlan(ctx, medialib.PeriodPlan{
		Date:   "2026-06-24",
		Period: medialib.PeriodDay,
		Theme:  fallback.Theme,
		LoopID: fallback.ID,
	}); err != nil {
		t.Fatalf("save fallback plan: %v", err)
	}

	svc.playbackMu.Lock()
	loop, _, reason, err := svc.loopForTimeLocked(ctx, svc.now(), false)
	svc.playbackMu.Unlock()
	if err != nil {
		t.Fatalf("loop for time: %v", err)
	}
	if loop.ID != fallback.ID {
		t.Fatalf("loop id = %s, want persisted fallback %s", loop.ID, fallback.ID)
	}
	if reason != "" {
		t.Fatalf("reason = %q, want empty when reusing fallback plan", reason)
	}
}

func TestLibraryMissingCurrentPeriodFailsClosed(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS := newLibraryTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}
	err := svc.ensureLibraryPlayback(ctx, false)
	if err == nil || !strings.Contains(err.Error(), "白天時段") {
		t.Fatalf("ensure playback error = %v, want missing current-period failure", err)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != 0 {
		t.Fatalf("loop play calls = %d, want no cross-period fallback", got)
	}
	if svc.activeLoopPath != "" {
		t.Fatalf("active loop = %q, want empty after fail-closed selection", svc.activeLoopPath)
	}
}

func TestLibraryThemeAndSelectPropagatePlaybackErrors(t *testing.T) {
	ctx := context.Background()
	setup := func(t *testing.T) (*Service, *fakeOBS, string) {
		t.Helper()
		svc, fakeOBS := newLibraryTestService(t)
		writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
		writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
		svc.now = fixedNow("2026-06-24T12:00:00+08:00")
		if err := svc.ScanLibrary(ctx); err != nil {
			t.Fatalf("scan library: %v", err)
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
		fakeOBS.playErr = errors.New("obs unavailable")
		return svc, fakeOBS, cafeID
	}

	tests := []struct {
		name string
		run  func(*testing.T, *Service, string) (string, error)
	}{
		{
			name: "set theme",
			run: func(t *testing.T, svc *Service, _ string) (string, error) {
				return svc.SetThemeText(ctx, "study")
			},
		},
		{
			name: "clear theme",
			run: func(t *testing.T, svc *Service, _ string) (string, error) {
				return svc.SetThemeText(ctx, "random")
			},
		},
		{
			name: "select loop",
			run: func(t *testing.T, svc *Service, cafeID string) (string, error) {
				return svc.SelectLoopText(ctx, cafeID)
			},
		},
		{
			name: "clear selected loop",
			run: func(t *testing.T, svc *Service, cafeID string) (string, error) {
				if err := svc.libDB.SetDirectLoopOverride(ctx, overrideDateKey(svc.now()), cafeID); err != nil {
					t.Fatalf("set direct override: %v", err)
				}
				return svc.SelectLoopText(ctx, "clear")
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _, cafeID := setup(t)
			text, err := tt.run(t, svc, cafeID)
			if err == nil {
				t.Fatalf("expected playback error, text=%q", text)
			}
			if got := svc.lastError(); !strings.Contains(got, "單次安全上限") ||
				strings.Contains(got, "obs unavailable") {
				t.Fatalf("last error = %q, want safe bounded-candidate diagnostic", got)
			}
		})
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

func TestLibraryMusicSkipReportsMissingMusic(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)

	_, err := svc.SkipMusicText(ctx)
	if err == nil {
		t.Fatal("expected missing music error")
	}
	if !strings.Contains(err.Error(), "媒體庫沒有可播放的音樂") {
		t.Fatalf("err = %v, want missing music message", err)
	}
}

func TestLibraryImportCopiesValidAssetsAndRejectsDuplicates(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_001.mp4")

	text, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_001.mp4",
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
	}); err == nil {
		t.Fatalf("expected duplicate import rejection")
	}
}

func TestLibraryImportScanWarningDoesNotExposeInternalPathsOrErrors(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	probePath := filepath.Join(t.TempDir(), "ffprobe")
	probeBody := []byte(`#!/bin/sh
path=
for arg do
  path=$arg
done
case "$path" in
  *"private spaced"*)
    printf 'open %s: permission denied' "$path" >&2
    exit 2
    ;;
esac
printf '{"format":{"duration":"10"},"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}'
`)
	if err := osWriteFile(probePath, probeBody, 0o700); err != nil {
		t.Fatalf("write selective ffprobe: %v", err)
	}
	svc.media = media.NewManager(probePath)

	const privateName = "loop_morning_private spaced_999.mp4"
	privatePath := writeLibraryFile(t, svc.cfg.LoopMediaDir, privateName)
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_001.mp4")

	text, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_001.mp4",
	})
	if err != nil {
		t.Fatalf("import with pre-existing scan issue: %v", err)
	}
	for _, want := range []string{
		"已匯入素材",
		"掃描提醒：媒體庫掃描發現 1 個無效或不可播放項目；詳細資訊請查看服務日誌。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("import text = %q, want %q", text, want)
		}
	}
	for _, leaked := range []string{
		privateName,
		filepath.ToSlash(filepath.Join("loops", privateName)),
		privatePath,
		filepath.Dir(privatePath),
		"permission denied",
		"ffprobe failed",
	} {
		if strings.Contains(text, leaked) {
			t.Fatalf("import text leaked %q: %q", leaked, text)
		}
	}
	if dest := filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_001.mp4"); !fileExists(dest) {
		t.Fatalf("successful import was not published at %s", dest)
	}
}

func TestLibraryImportRejectsMusicWhenProbeFails(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.media = media.NewManager(fakeFailingFFProbe(t))
	source := writeBotAPIFile(t, svc, "music_bad.mp3")

	_, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "music_bad.mp3",
	})
	if err == nil {
		t.Fatal("expected music import probe failure")
	}
	if !strings.Contains(err.Error(), "ffprobe failed") {
		t.Fatalf("err = %v, want ffprobe failed", err)
	}
	dest := filepath.Join(svc.cfg.MusicMediaDir, "music_bad.mp3")
	if fileExists(dest) {
		t.Fatalf("expected failed music import to remove %s", dest)
	}
}

func TestRunReturnsWhenBotStopsUnexpectedly(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newRuntimeTestService(t)

	err := svc.Run(ctx)

	if !errors.Is(err, ErrRequiredWorkerStopped) || !strings.Contains(err.Error(), "telegram") {
		t.Fatalf("err = %v, want unexpected telegram worker stop", err)
	}
	if errors.Is(err, ErrWorkerShutdownStuck) {
		t.Fatalf("cooperative sibling shutdown reported stuck: %v", err)
	}
}

func TestRunFailsFastWhenLivenessReporterReturns(t *testing.T) {
	svc, _, fakeBot := newRuntimeTestService(t)
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
	svc, _, fakeBot := newRuntimeTestService(t)
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
	svc, _, _ := newRuntimeTestService(t)
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
	svc, _, fakeBot := newRuntimeTestService(t)
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

func TestRequiredWorkerBindingsUseLibraryScheduler(t *testing.T) {
	svc, _, _ := newRuntimeTestService(t)
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
	if got := seen[liveness.WorkerPlayback]; got != "library-scheduler" {
		t.Fatalf("playback implementation = %q, want library-scheduler", got)
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
	if playback.ID != liveness.WorkerPlayback || playback.Owner != liveness.OwnerLibraryScheduler {
		t.Fatalf("playback snapshot = %+v, want fixed library scheduler owner", playback)
	}
}

func TestRunFailsBeforeStartingWorkersOnDuplicateLivenessOwner(t *testing.T) {
	svc, _, fakeBot := newRuntimeTestService(t)
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
	svc, fakeOBS, fakeBot := newRuntimeTestService(t)
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
	svc, fakeOBS, fakeBot := newRuntimeTestService(t)
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
	svc, fakeOBS, fakeBot := newRuntimeTestService(t)
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
	svc, fakeOBS, fakeBot := newRuntimeTestService(t)
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
	svc, _, fakeBot := newRuntimeTestService(t)
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

	svc, _, fakeBot := newRuntimeTestService(t)
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
	svc, _, _ := newRuntimeTestServiceAtDBPath(t, dbPath)
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

func TestPeriodicMaintenancePrunesOnlySafeLibraryStateAndPreservesLegacyVideos(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	svc, _, _ := newRuntimeTestServiceAtDBPath(t, dbPath)
	svc.now = fixedNow("2026-07-31T12:00:00Z")

	if err := svc.libDB.SetThemeOverride(ctx, "2026-07-28", "old"); err != nil {
		t.Fatalf("save old override: %v", err)
	}
	if err := svc.libDB.SetThemeOverride(ctx, "2026-07-29", "cutoff"); err != nil {
		t.Fatalf("save cutoff override: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open legacy video fixture database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
CREATE TABLE videos (
	id INTEGER PRIMARY KEY,
	telegram_unique_id TEXT NOT NULL,
	queue_position INTEGER NOT NULL
);
INSERT INTO videos (id, telegram_unique_id, queue_position)
VALUES (7, 'legacy-unique', 19);
`); err != nil {
		t.Fatalf("insert legacy video row: %v", err)
	}

	if err := svc.performMaintenance(ctx); err != nil {
		t.Fatalf("perform maintenance: %v", err)
	}
	oldOverride, err := svc.libDB.Override(ctx, "2026-07-28")
	if err != nil {
		t.Fatalf("load old override: %v", err)
	}
	if oldOverride.Theme != "" {
		t.Fatalf("old override theme = %q, want pruned", oldOverride.Theme)
	}
	cutoffOverride, err := svc.libDB.Override(ctx, "2026-07-29")
	if err != nil {
		t.Fatalf("load cutoff override: %v", err)
	}
	if cutoffOverride.Theme != "cutoff" {
		t.Fatalf("cutoff override theme = %q, want preserved", cutoffOverride.Theme)
	}
	var videoRows int
	if err := db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM videos WHERE telegram_unique_id = 'legacy-unique'`,
	).Scan(&videoRows); err != nil {
		t.Fatalf("count legacy video rows: %v", err)
	}
	if videoRows != 1 {
		t.Fatalf("legacy video rows = %d, want preserved", videoRows)
	}
}

func TestRunParentCancellationPropagatesLateTelegramStuckSentinel(t *testing.T) {
	svc, _, fakeBot := newRuntimeTestService(t)
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

func newRuntimeTestService(t *testing.T) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	svc, fakeOBS := newLibraryTestService(t)
	fakeBot, ok := svc.bot.(*fakeBot)
	if !ok {
		t.Fatalf("test bot type = %T, want *fakeBot", svc.bot)
	}
	return svc, fakeOBS, fakeBot
}

func newRuntimeTestServiceAtDBPath(t *testing.T, dbPath string) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	svc, fakeOBS := newLibraryTestServiceAtDBPath(t, dbPath)
	fakeBot, ok := svc.bot.(*fakeBot)
	if !ok {
		t.Fatalf("test bot type = %T, want *fakeBot", svc.bot)
	}
	return svc, fakeOBS, fakeBot
}

func newLibraryTestService(t *testing.T) (*Service, *fakeOBS) {
	t.Helper()
	return newLibraryTestServiceAtDBPath(t, filepath.Join(t.TempDir(), "library.db"))
}

func newLibraryTestServiceAtDBPath(t *testing.T, dbPath string) (*Service, *fakeOBS) {
	t.Helper()
	ctx := context.Background()
	store, err := journalstore.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open journal store: %v", err)
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
		MediaDir:                mediaDir,
		LoopMediaDir:            filepath.Join(mediaDir, "loops"),
		MusicMediaDir:           filepath.Join(mediaDir, "music"),
		TelegramBotAPIDir:       botAPIDir,
		OBSLoopSourceName:       "loop_source",
		OBSMusicSourceName:      "music_source",
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
	manager := media.NewManager(fakeFFProbe(t, 30))
	fakeOBS := &fakeOBS{
		state:        obs.StateConnected,
		sourcePlayed: make(map[string]string),
	}
	return &Service{
		cfg:          cfg,
		logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		journalStore: store,
		libDB:        libDB,
		media:        manager,
		obs:          fakeOBS,
		bot:          &fakeBot{},
		now:          time.Now,
		rng:          rand.New(rand.NewSource(1)),
	}, fakeOBS
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
	body := []byte("#!/bin/sh\nprintf '{\"format\":{\"duration\":\"" + formatTestInt(duration) + "\"},\"streams\":[{\"codec_type\":\"video\"},{\"codec_type\":\"audio\"}]}'\n")
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

func fileExists(path string) bool {
	_, err := osStat(path)
	return err == nil
}

var (
	osWriteFile = os.WriteFile
	osStat      = os.Stat
)

type fakeOBS struct {
	state                      obs.State
	events                     <-chan obs.Event
	lastPlayed                 string
	lastSource                 string
	sourcePlayed               map[string]string
	sourcePlayCalls            map[string]int
	mediaStatuses              map[string]obs.MediaInputStatus
	mediaStatusSequence        map[string][]obs.MediaInputStatus
	mediaStatusErrs            map[string]error
	mediaStatusCalls           map[string]int
	mediaStatusWaitForContext  bool
	inputFiles                 map[string]string
	inputSettingsErr           map[string]error
	inputSettingCall           map[string]int
	connectErr                 error
	connectNotify              chan struct{}
	connectCalls               int
	probeErr                   error
	probeCalls                 int
	playErr                    error
	playErrByPath              map[string]error
	playErrAfterMutation       error
	playErrAfterMutationByPath map[string]error
	playAttemptCalls           map[string]int
	playWaitForContext         bool
	playDeadlineObserved       bool
	playStarted                chan struct{}
	playRelease                <-chan struct{}
	stopErr                    error
	stopWaitForContext         bool
	stopDeadlineObserved       bool
	sourceStopCalls            map[string]int
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
func (f *fakeOBS) GetMediaInputStatus(ctx context.Context, inputName string) (obs.MediaInputStatus, error) {
	if f.mediaStatusCalls == nil {
		f.mediaStatusCalls = make(map[string]int)
	}
	f.mediaStatusCalls[inputName]++
	if f.mediaStatusWaitForContext {
		<-ctx.Done()
		return obs.MediaInputStatus{}, ctx.Err()
	}
	if err := f.mediaStatusErrs[inputName]; err != nil {
		return obs.MediaInputStatus{}, err
	}
	if sequence := f.mediaStatusSequence[inputName]; len(sequence) > 0 {
		status := sequence[0]
		f.mediaStatusSequence[inputName] = sequence[1:]
		return status, nil
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
func (f *fakeOBS) PlaySourceFile(ctx context.Context, sourceName string, path string, _ obs.PlaySourceOptions) error {
	if f.playAttemptCalls == nil {
		f.playAttemptCalls = make(map[string]int)
	}
	f.playAttemptCalls[sourceName]++
	if _, ok := ctx.Deadline(); ok {
		f.playDeadlineObserved = true
	}
	if f.playStarted != nil {
		select {
		case f.playStarted <- struct{}{}:
		default:
		}
	}
	if f.playRelease != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-f.playRelease:
		}
	}
	if f.playWaitForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	if err := f.playErrByPath[path]; err != nil {
		return err
	}
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
	if err := f.playErrAfterMutationByPath[path]; err != nil {
		return fmt.Errorf("%w: %w", obs.ErrSourceMayBeMutated, err)
	}
	if f.playErrAfterMutation != nil {
		return fmt.Errorf("%w: %w", obs.ErrSourceMayBeMutated, f.playErrAfterMutation)
	}
	return nil
}
func (f *fakeOBS) StopSource(ctx context.Context, sourceName string) error {
	if f.sourceStopCalls == nil {
		f.sourceStopCalls = make(map[string]int)
	}
	f.sourceStopCalls[sourceName]++
	if _, ok := ctx.Deadline(); ok {
		f.stopDeadlineObserved = true
	}
	if f.stopWaitForContext {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.stopErr != nil {
		return f.stopErr
	}
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
	run func(context.Context) error
}

func (f *fakeBot) Run(ctx context.Context) error {
	if f.run != nil {
		return f.run(ctx)
	}
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
