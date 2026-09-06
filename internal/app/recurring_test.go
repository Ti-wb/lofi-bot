package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	retryloop "github.com/tiwb/tg-obs-bot/internal/retry"
)

func TestObserveOBSResultSkippedCycleDoesNotMutateRetryState(t *testing.T) {
	svc, _, _ := newRuntimeTestService(t)
	state := newOBSRetryState(func() uint64 { return 0 })

	first := svc.observeOBSResult(state, obsMaintenanceResult{
		failure: obsRetryConnect,
		err:     errors.New("offline"),
	})
	if first != 4*time.Second {
		t.Fatalf("first retry = %s, want jittered 4s", first)
	}
	if got := state.connect.backoff.Failures(); got != 1 {
		t.Fatalf("connect failures = %d, want 1", got)
	}

	if got := svc.observeOBSResult(state, obsMaintenanceResult{}); got != obsReconnectInterval {
		t.Fatalf("skipped cycle delay = %s, want healthy cadence %s", got, obsReconnectInterval)
	}
	if got := state.connect.backoff.Failures(); got != 1 {
		t.Fatalf("skipped cycle mutated connect failures to %d", got)
	}
	if got := state.probe.backoff.Failures(); got != 0 {
		t.Fatalf("skipped cycle mutated probe failures to %d", got)
	}
	if got := state.resume.backoff.Failures(); got != 0 {
		t.Fatalf("skipped cycle mutated resume failures to %d", got)
	}

	second := svc.observeOBSResult(state, obsMaintenanceResult{
		failure: obsRetryConnect,
		err:     errors.New("still offline"),
	})
	if second != 8*time.Second {
		t.Fatalf("second attempted retry = %s, want jittered 8s", second)
	}
}

func TestConnectedProbeWithoutResumeAttemptPreservesResumeFailures(t *testing.T) {
	svc, _, _ := newRuntimeTestService(t)
	svc.activeLoopPath = "/active-loop.mp4"
	state := newOBSRetryState(func() uint64 { return 0 })
	svc.observeOBSResult(state, obsMaintenanceResult{
		failure: obsRetryResume,
		err:     errors.New("resume failed"),
	})
	if got := state.resume.backoff.Failures(); got != 1 {
		t.Fatalf("resume failures = %d, want 1", got)
	}

	result := svc.maintainOBSConnection(context.Background())
	if !result.recoverProbe {
		t.Fatal("healthy connected cycle did not record a successful probe")
	}
	if result.recoverResume {
		t.Fatal("healthy connected cycle claimed resume recovery without attempting it")
	}
	svc.observeOBSResult(state, result)
	if got := state.resume.backoff.Failures(); got != 1 {
		t.Fatalf("non-attempted resume reset failure sequence to %d", got)
	}
}

func TestLibrarySchedulerAttemptHelpersDistinguishDisconnectedAndRecoverySkips(t *testing.T) {
	ctx := context.Background()
	librarySvc, libraryOBS := newLibraryTestService(t)
	libraryOBS.state = obs.StateDisconnected
	if attempted, err := librarySvc.reconcileLibraryPlaybackAttempt(ctx); err != nil || attempted {
		t.Fatalf("disconnected library attempted=%t error=%v, want clean skip", attempted, err)
	}
	if attempted, err := librarySvc.handleLibraryOBSEventAttempt(ctx, obs.Event{Type: obs.EventMediaEnded}); err != nil || attempted {
		t.Fatalf("disconnected library event attempted=%t error=%v, want clean skip", attempted, err)
	}
	libraryOBS.state = obs.StateConnected
	librarySvc.obsRecoveryInProgress.Store(true)
	if attempted, err := librarySvc.reconcileLibraryPlaybackAttempt(ctx); err != nil || attempted {
		t.Fatalf("recovering library attempted=%t error=%v, want clean skip", attempted, err)
	}
}

func TestMaintenanceRetryCadenceBacksOffAndResetsAfterRecovery(t *testing.T) {
	const interval = 10 * time.Minute
	current := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	svc := &Service{
		logger:              slog.New(slog.NewTextHandler(io.Discard, nil)),
		maintenanceInterval: interval,
		retryRandom:         func() uint64 { return 0 },
		retryNow:            func() time.Time { return current },
	}
	maintenanceCalls := 0
	svc.maintenanceFn = func(context.Context) maintenanceCycleResult {
		maintenanceCalls++
		current = current.Add(2 * time.Minute)
		if maintenanceCalls <= 2 {
			return maintenanceCycleResult{err: errors.New("maintenance failed")}
		}
		return maintenanceCycleResult{}
	}
	var delays []time.Duration
	svc.retrySleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		current = current.Add(delay)
		if len(delays) == 4 {
			return context.Canceled
		}
		return nil
	}

	err := svc.maintenanceLoop(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("maintenanceLoop error = %v, want canceled", err)
	}
	if maintenanceCalls != 3 {
		t.Fatalf("maintenance calls = %d, want 3", maintenanceCalls)
	}
	want := []time.Duration{interval, 8 * time.Minute, 16 * time.Minute, 8 * time.Minute}
	if fmt.Sprint(delays) != fmt.Sprint(want) {
		t.Fatalf("wait delays = %v, want %v", delays, want)
	}
}

func TestMaintenanceDeferralsRetrySoonWithoutMutatingErrorBackoff(t *testing.T) {
	const (
		interval      = 10 * time.Minute
		deferredCount = 9
	)
	current := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	var logs bytes.Buffer
	svc := &Service{
		logger:              slog.New(slog.NewTextHandler(&logs, nil)),
		maintenanceInterval: interval,
		retryRandom:         func() uint64 { return 0 },
		retryNow:            func() time.Time { return current },
	}
	results := []maintenanceCycleResult{{
		deferred: true,
		err:      errors.New("first real failure"),
	}}
	for range deferredCount {
		results = append(results, maintenanceCycleResult{deferred: true})
	}
	results = append(
		results,
		maintenanceCycleResult{
			deferred: true,
			err:      errors.New("second real failure"),
		},
		maintenanceCycleResult{},
	)
	maintenanceCalls := 0
	svc.maintenanceFn = func(context.Context) maintenanceCycleResult {
		result := results[maintenanceCalls]
		maintenanceCalls++
		return result
	}

	var delays []time.Duration
	svc.retrySleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		current = current.Add(delay)
		if len(delays) == len(results)+1 {
			return context.Canceled
		}
		return nil
	}

	err := svc.maintenanceLoop(context.Background())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("maintenanceLoop error = %v, want canceled", err)
	}
	if maintenanceCalls != len(results) {
		t.Fatalf("maintenance calls = %d, want %d", maintenanceCalls, len(results))
	}
	wantDelays := []time.Duration{interval, 8 * time.Minute}
	for range deferredCount {
		wantDelays = append(wantDelays, maintenanceDeferredMax)
	}
	wantDelays = append(wantDelays, 16*time.Minute, interval)
	if fmt.Sprint(delays) != fmt.Sprint(wantDelays) {
		t.Fatalf("wait delays = %v, want %v", delays, wantDelays)
	}

	gotLogs := logs.String()
	if count := strings.Count(gotLogs, `msg="periodic maintenance deferred"`); count != 2 {
		t.Fatalf("deferred warning count = %d, want first/eighth only; logs=%q", count, gotLogs)
	}
	if count := strings.Count(gotLogs, `msg="periodic maintenance contention recovered"`); count != 1 {
		t.Fatalf("deferred recovery count = %d, want one; logs=%q", count, gotLogs)
	}
	if !strings.Contains(gotLogs, "consecutive_deferrals=8") ||
		!strings.Contains(gotLogs, "suppressed=6") ||
		!strings.Contains(gotLogs, "consecutive_deferrals=9") {
		t.Fatalf("deferred samples lack bounded counts: %q", gotLogs)
	}
	if strings.Contains(gotLogs, "path=") {
		t.Fatalf("deferred logs unexpectedly contain a path: %q", gotLogs)
	}
	if got := maintenanceDeferredDelay(30 * time.Second); got != 30*time.Second {
		t.Fatalf("short-interval deferred delay = %s, want normal 30s cadence", got)
	}
}

func TestMaintenanceSkipsBusyStorageWithoutBlockingAndRecoversNextCycle(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newRuntimeTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	svc.now = func() time.Time { return now }
	stagingDir, err := ensureLibraryStagingDir(svc.cfg.LoopMediaDir)
	if err != nil {
		t.Fatalf("create staging dir: %v", err)
	}
	staleTemp := filepath.Join(stagingDir, libraryImportTempPrefix+"maintenance"+libraryImportTempSuffix)
	if err := os.WriteFile(staleTemp, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale temp: %v", err)
	}
	old := now.Add(-staleLibraryImportAge - time.Second)
	if err := os.Chtimes(staleTemp, old, old); err != nil {
		t.Fatalf("age stale temp: %v", err)
	}
	registry := liveness.NewRegistry(liveness.Options{})
	worker, err := registry.Bind(liveness.WorkerMaintenance, liveness.OwnerMaintenance)
	if err != nil {
		t.Fatalf("Bind maintenance: %v", err)
	}
	worker.Advance(liveness.PhaseOperation)
	maintenanceCtx := liveness.WithWorker(ctx, worker)

	svc.storageMu.Lock()
	for attempt := 1; attempt <= 2; attempt++ {
		cycle := make(chan maintenanceCycleResult, 1)
		go func() {
			cycle <- svc.performMaintenanceCycle(maintenanceCtx)
		}()
		select {
		case result := <-cycle:
			if result.err != nil || !result.deferred {
				svc.storageMu.Unlock()
				t.Fatalf("busy maintenance cycle %d = %+v, want clean deferral", attempt, result)
			}
		case <-time.After(250 * time.Millisecond):
			svc.storageMu.Unlock()
			<-cycle
			t.Fatalf("maintenance cycle %d blocked behind busy storage", attempt)
		}
	}
	svc.storageMu.Unlock()
	if !fileExists(staleTemp) {
		t.Fatal("busy maintenance cycle removed a deferred stale temp")
	}

	beforeRecovery := worker.Snapshot().Sequence
	result := svc.performMaintenanceCycle(maintenanceCtx)
	if result.err != nil || result.deferred {
		t.Fatalf("recovered maintenance cycle = %+v, want complete success", result)
	}
	if fileExists(staleTemp) {
		t.Fatal("next maintenance cycle did not resume skipped stale-temp sweep")
	}
	if worker.Snapshot().Sequence <= beforeRecovery {
		t.Fatal("recovered stale-temp sweep did not record an actual item checkpoint")
	}
}

func TestRecurringSchedulePreservesTickerCadenceAndCollapsesMissedTicks(t *testing.T) {
	const interval = 10 * time.Minute
	current := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	schedule := newRecurringSchedule(interval, false, func() time.Time { return current })

	if got := schedule.delay(); got != interval {
		t.Fatalf("initial delay = %s, want %s", got, interval)
	}
	current = current.Add(interval)
	schedule.beginCycle()
	current = current.Add(2 * time.Minute)
	if got := schedule.delay(); got != 8*time.Minute {
		t.Fatalf("delay after two-minute healthy operation = %s, want 8m", got)
	}

	current = current.Add(8 * time.Minute)
	schedule.beginCycle()
	current = current.Add(25 * time.Minute)
	if got := schedule.delay(); got != 0 {
		t.Fatalf("overrun delay = %s, want one immediate missed tick", got)
	}
	schedule.beginCycle()
	if got := schedule.delay(); got != 5*time.Minute {
		t.Fatalf("delay after collapsed missed tick = %s, want next fixed tick in 5m", got)
	}

	if got := schedule.failureDelay(7 * time.Minute); got != 7*time.Minute {
		t.Fatalf("failure delay = %s, want 7m", got)
	}
	current = current.Add(7 * time.Minute)
	schedule.beginCycle()
	current = current.Add(3 * time.Minute)
	if got := schedule.delay(); got != 7*time.Minute {
		t.Fatalf("post-retry healthy delay = %s, want start-anchored 7m", got)
	}
}

func TestEventErrorSamplerRequiresSustainedRecovery(t *testing.T) {
	sampler := newEventErrorSampler()
	if sample := sampler.failure(); sample.Kind != retryloop.SampleFirst {
		t.Fatalf("first failure = %+v", sample)
	}
	for failureNumber := 2; failureNumber <= recurringLogEvery; failureNumber++ {
		sampler.success()
		sample := sampler.failure()
		if failureNumber < recurringLogEvery && sample.Kind != retryloop.SampleNone {
			t.Fatalf("alternating failure %d = %+v, want suppressed", failureNumber, sample)
		}
		if failureNumber == recurringLogEvery && sample.Kind != retryloop.SamplePeriodic {
			t.Fatalf("failure %d = %+v, want periodic", failureNumber, sample)
		}
	}
	for range eventRecoveryThreshold {
		sampler.success()
	}
	if sample := sampler.failure(); sample.Kind != retryloop.SampleFirst {
		t.Fatalf("failure after sustained recovery = %+v, want first", sample)
	}
}

func TestLibraryRecoveryScanWarningsAreSampled(t *testing.T) {
	const token = "123456:ABCdefghi_jklmnop"
	var logs bytes.Buffer
	svc := &Service{
		cfg:    config.Config{TelegramBotToken: token},
		logger: slog.New(slog.NewTextHandler(&logs, nil)),
	}
	scanErr := fmt.Errorf("%s %s", token, strings.Repeat("界", 500))

	for range 16 {
		svc.observeLibraryRecoveryScan(scanErr)
	}
	for range eventRecoveryThreshold {
		svc.observeLibraryRecoveryScan(nil)
	}
	svc.observeLibraryRecoveryScan(scanErr)

	got := logs.String()
	if strings.Contains(got, token) {
		t.Fatalf("library scan logs leaked token: %q", got)
	}
	if count := strings.Count(got, `msg="media library scan found issues during OBS recovery"`); count != 4 {
		t.Fatalf("library scan warning count = %d, want 4", count)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if len(line) > 800 {
			t.Fatalf("library scan log retained an unbounded error: %d bytes", len(line))
		}
	}
}

func TestLibraryRecoveryUnlocksAfterPanic(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.now = func() time.Time {
		panic("clock panic")
	}
	requirePanic(t, func() {
		_ = svc.recoverLibraryPlaybackAfterOBSConnect(ctx)
	})
	requireMutexUnlocked(t, &svc.playbackMu, "playbackMu")
}

func requirePanic(t *testing.T, fn func()) {
	t.Helper()
	didPanic := false
	func() {
		defer func() {
			didPanic = recover() != nil
		}()
		fn()
	}()
	if !didPanic {
		t.Fatal("operation did not panic")
	}
}

func requireMutexUnlocked(t *testing.T, mutex *sync.Mutex, name string) {
	t.Helper()
	if !mutex.TryLock() {
		t.Fatalf("%s remained locked after panic", name)
	}
	mutex.Unlock()
}
