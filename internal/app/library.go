package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

const librarySchedulerInterval = 15 * time.Second

type libraryUploadPlan struct {
	fileName string
	kind     medialib.Kind
	destDir  string
	label    string
}

type libraryPlaybackSelectionSnapshot struct {
	overrideDate string
	override     medialib.Override
	planDate     string
	period       medialib.Period
	plan         medialib.PeriodPlan
	planExists   bool
}

type obsFailClosedBudget struct {
	// Cleanup operations are serialized by playbackMu. Charge only time spent
	// in cleanup, so later playback attempts do not consume this reserve.
	remaining time.Duration
	ctx       context.Context
}

type obsFailClosedBudgetContextKey struct{}

type libraryScanFunc func(string, string) (medialib.Library, error)

type libraryProgressScanFunc func(
	context.Context,
	string,
	string,
	func(),
) (medialib.Library, error)

func (s *Service) libraryPlaybackMissing() bool {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	return s.activeLoopPath == ""
}

func (s *Service) ScanLibrary(ctx context.Context) error {
	return s.scanLibraryWithProgress(ctx, medialib.ScanDirsContext, true)
}

// scanLibraryWith performs filesystem enumeration and ffprobe validation
// without holding playbackMu. Only the final, generation-checked snapshot
// publication takes the playback lock, so a slow scan cannot freeze period
// transitions, OBS recovery, or Telegram playback controls.
func (s *Service) scanLibraryWith(
	ctx context.Context,
	scan libraryScanFunc,
	validate bool,
) error {
	return s.scanLibraryWithProgress(
		ctx,
		func(
			_ context.Context,
			loopDir string,
			musicDir string,
			progress func(),
		) (medialib.Library, error) {
			lib, err := scan(loopDir, musicDir)
			if progress != nil {
				progress()
			}
			return lib, err
		},
		validate,
	)
}

func (s *Service) scanLibraryWithProgress(
	ctx context.Context,
	scan libraryProgressScanFunc,
	validate bool,
) error {
	tracker := liveness.WorkerFromContext(ctx)
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.acquireLibraryScan(ctx, tracker); err != nil {
		return err
	}
	defer s.releaseLibraryScan()
	if err := ctx.Err(); err != nil {
		return err
	}
	scanScope := tracker.Scope(liveness.PhaseLibraryScan)
	defer scanScope.Close()
	generation := s.libraryScanGeneration.Add(1)

	if err := os.MkdirAll(s.cfg.LoopMediaDir, 0o755); err != nil {
		return s.publishLibraryScanFailure(ctx, generation, err)
	}
	if err := os.MkdirAll(s.cfg.MusicMediaDir, 0o755); err != nil {
		return s.publishLibraryScanFailure(ctx, generation, err)
	}
	lib, err := scan(
		ctx,
		s.cfg.LoopMediaDir,
		s.cfg.MusicMediaDir,
		func() {
			scanScope.Checkpoint()
		},
	)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	if errors.Is(err, medialib.ErrDirectoryCapacity) {
		return s.publishLibraryScanFailure(ctx, generation, err)
	}
	if libraryScanIsIncomplete(err) {
		return s.publishLibraryScanFailure(ctx, generation, err)
	}

	rejectedCount := libraryScanIssueCount(err)
	seen := libraryPaths(lib)
	var validationCache map[string]libraryValidationEntry
	if validate {
		cacheSnapshot := s.libraryValidationCacheSnapshot()
		var validationIssues []*medialib.Error
		lib, validationCache, seen, validationIssues = s.validateScannedLibrary(
			ctx,
			lib,
			cacheSnapshot,
			func() {
				scanScope.Checkpoint()
			},
		)
		if ctxErr := ctx.Err(); ctxErr != nil {
			// Cancellation is not a new view of the filesystem. Preserve the
			// complete last-known-good snapshot and its diagnostics.
			return ctxErr
		}
		rejectedCount += len(validationIssues)
		if len(validationIssues) > 0 {
			err = errors.Join(err, &medialib.ScanError{Issues: validationIssues})
		}
	}
	return s.publishLibraryScan(
		ctx,
		generation,
		lib,
		validationCache,
		seen,
		rejectedCount,
		err,
		validate,
	)
}

func (s *Service) acquireLibraryScan(
	ctx context.Context,
	tracker *liveness.Worker,
) error {
	s.libraryScanGateOnce.Do(func() {
		s.libraryScanGate = make(chan struct{}, 1)
		s.libraryScanGate <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.libraryScanGate:
		if err := ctx.Err(); err != nil {
			s.releaseLibraryScan()
			return err
		}
		return nil
	default:
	}

	tracker.Advance(liveness.PhaseRetryWait)
	pulse := time.NewTicker(tracker.ProgressInterval())
	defer pulse.Stop()
	for {
		select {
		case <-ctx.Done():
			tracker.Advance(liveness.PhaseCancelWait)
			return ctx.Err()
		case <-pulse.C:
			// Waiting for the current complete scan is an intentional,
			// cancellable resource wait, not evidence that the scan itself
			// progressed.
			tracker.Advance(liveness.PhaseRetryWait)
		case <-s.libraryScanGate:
			if err := ctx.Err(); err != nil {
				s.releaseLibraryScan()
				tracker.Advance(liveness.PhaseCancelWait)
				return err
			}
			tracker.Advance(liveness.PhaseOperation)
			return nil
		}
	}
}

func (s *Service) releaseLibraryScan() {
	s.libraryScanGate <- struct{}{}
}

func (s *Service) ensureLibraryScanned(ctx context.Context) error {
	s.playbackMu.Lock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.playbackMu.Unlock()
		return ctxErr
	}
	published := s.librarySnapshotPublished ||
		len(s.librarySnapshot.Loops) > 0 ||
		len(s.librarySnapshot.Music) > 0
	s.playbackMu.Unlock()
	if published {
		return nil
	}
	return s.ScanLibrary(ctx)
}

func (s *Service) publishLibraryScanFailure(
	ctx context.Context,
	generation uint64,
	err error,
) error {
	if err == nil {
		return nil
	}
	published := false
	s.playbackMu.Lock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.playbackMu.Unlock()
		return ctxErr
	}
	if generation == s.libraryScanGeneration.Load() {
		s.libraryScanErr = s.publicScanDiagnostic(err)
		published = true
	}
	s.playbackMu.Unlock()
	if published {
		s.setLastErr(err)
	}
	return err
}

func (s *Service) publishLibraryScan(
	ctx context.Context,
	generation uint64,
	lib medialib.Library,
	validationCache map[string]libraryValidationEntry,
	seen map[string]struct{},
	rejectedCount int,
	scanErr error,
	replaceValidationCache bool,
) error {
	s.playbackMu.Lock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.playbackMu.Unlock()
		return ctxErr
	}
	if generation != s.libraryScanGeneration.Load() {
		s.playbackMu.Unlock()
		return scanErr
	}

	if replaceValidationCache {
		s.libraryValidationCache = validationCache
	}
	for path := range s.libraryQuarantine {
		if _, ok := seen[path]; !ok {
			delete(s.libraryQuarantine, path)
		}
	}

	filtered := medialib.Library{
		Loops: make([]medialib.Loop, 0, len(lib.Loops)),
		Music: make([]medialib.Music, 0, len(lib.Music)),
	}
	quarantineIssues := make([]*medialib.Error, 0)
	for _, loop := range lib.Loops {
		if reason := s.libraryAssetQuarantineReasonLocked(loop.Path); reason != "" {
			quarantineIssues = append(quarantineIssues, invalidPlayableAssetIssue(
				medialib.KindLoop,
				loop.RelPath,
				errors.New(reason),
			))
			continue
		}
		filtered.Loops = append(filtered.Loops, loop)
	}
	for _, music := range lib.Music {
		if reason := s.libraryAssetQuarantineReasonLocked(music.Path); reason != "" {
			quarantineIssues = append(quarantineIssues, invalidPlayableAssetIssue(
				medialib.KindMusic,
				music.RelPath,
				errors.New(reason),
			))
			continue
		}
		filtered.Music = append(filtered.Music, music)
	}
	if len(quarantineIssues) > 0 {
		scanErr = errors.Join(scanErr, &medialib.ScanError{Issues: quarantineIssues})
		rejectedCount += len(quarantineIssues)
	}

	s.librarySnapshot = filtered
	s.libraryRejectedCount = rejectedCount
	s.librarySnapshotPublished = true
	if scanErr != nil {
		s.libraryScanErr = s.publicScanDiagnostic(scanErr)
	} else {
		s.libraryScanErr = ""
	}
	s.playbackMu.Unlock()

	if scanErr != nil {
		s.setLastErr(scanErr)
		return scanErr
	}
	return nil
}

func libraryPaths(lib medialib.Library) map[string]struct{} {
	seen := make(map[string]struct{}, len(lib.Loops)+len(lib.Music))
	for _, loop := range lib.Loops {
		seen[loop.Path] = struct{}{}
	}
	for _, music := range lib.Music {
		seen[music.Path] = struct{}{}
	}
	return seen
}

func libraryScanIssueCount(err error) int {
	if err == nil {
		return 0
	}
	if scanErr, ok := err.(*medialib.ScanError); ok {
		return len(scanErr.Issues)
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		count := 0
		for _, child := range joined.Unwrap() {
			count += libraryScanIssueCount(child)
		}
		return count
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return libraryScanIssueCount(wrapped.Unwrap())
	}
	return 0
}

func libraryScanIsIncomplete(err error) bool {
	var scanErr *medialib.ScanError
	if !errors.As(err, &scanErr) {
		return false
	}
	for _, issue := range scanErr.Issues {
		if issue != nil &&
			(issue.Code == medialib.ErrorReadDirectory ||
				issue.Code == medialib.ErrorDirectoryCapacity) {
			return true
		}
	}
	return false
}

func (s *Service) librarySchedulerLoop(ctx context.Context) error {
	tracker := liveness.WorkerFromContext(ctx)
	tracker.Advance(liveness.PhaseOperation)
	if err := s.ScanLibrary(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		s.logger.Warn("initial media library scan found issues", "error", s.redactError(err))
	}
	retries := newRecurringRetry(librarySchedulerInterval, libraryRetryMaxDelay, s.retryRandom)
	schedule := newRecurringSchedule(librarySchedulerInterval, false, s.retryClockNow)
	delay := schedule.delay()
	waitPhase := liveness.PhaseScheduledWait
	for {
		if err := s.waitRecurring(ctx, tracker, waitPhase, delay); err != nil {
			return err
		}
		tracker.Advance(liveness.PhaseOperation)
		schedule.beginCycle()
		if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
			delay = schedule.delay()
			waitPhase = liveness.PhaseScheduledWait
			continue
		}
		attempted, err := s.reconcileLibraryPlaybackAttempt(ctx)
		if !attempted {
			delay = schedule.delay()
			waitPhase = liveness.PhaseScheduledWait
			continue
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			s.setLastErr(err)
			failure := retries.failure()
			failure.attempt.Delay = s.capLibraryRetryAtPeriodBoundary(failure.attempt.Delay)
			logRecurringFailure(s.logger, "library playback check failed", s.redactError(err), failure)
			delay = schedule.failureDelay(failure.attempt.Delay)
			waitPhase = liveness.PhaseRetryWait
			continue
		}
		logRecurringRecovery(s.logger, "library playback recovered", retries.recovery())
		delay = schedule.delay()
		waitPhase = liveness.PhaseScheduledWait
	}
}

func (s *Service) capLibraryRetryAtPeriodBoundary(delay time.Duration) time.Duration {
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	untilBoundary := medialib.PeriodInfoAt(now).EndsAt.Sub(now)
	if untilBoundary <= 0 {
		return 0
	}
	if delay > untilBoundary {
		return untilBoundary
	}
	return delay
}

func (s *Service) recoverLibraryPlaybackAfterOBSConnect(ctx context.Context) error {
	// Reconnect from the last complete snapshot instead of forcing a full
	// filesystem/probe pass while obsRecoveryInProgress blocks event handling
	// and scheduled transitions. A fresh process (including one whose first
	// incomplete scan failed) still scans here because it has no published
	// snapshot yet. Imports and explicit /scan requests own later refreshes.
	scanErr := s.ensureLibraryScanned(ctx)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var reconcileErr error
	func() {
		s.playbackMu.Lock()
		defer s.playbackMu.Unlock()
		if ctxErr := ctx.Err(); ctxErr != nil {
			reconcileErr = ctxErr
			return
		}
		reconcileErr = s.reconcileLibraryPlaybackLocked(ctx, true)
	}()

	// Scan issues are retained in status and sampled here after releasing
	// playbackMu so repeated reconnects cannot amplify logs or block playback.
	s.observeLibraryRecoveryScan(scanErr)
	return reconcileErr
}

func (s *Service) handleLibraryOBSEvent(ctx context.Context, event obs.Event) error {
	_, err := s.handleLibraryOBSEventAttempt(ctx, event)
	return err
}

func (s *Service) handleLibraryOBSEventAttempt(ctx context.Context, event obs.Event) (bool, error) {
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	var activePath string
	switch event.InputName {
	case s.cfg.OBSMusicSourceName:
		activePath = s.activeMusicPath
	case s.cfg.OBSLoopSourceName:
		activePath = s.activeLoopPath
	}
	if event.Path != "" && activePath != "" && event.Path != activePath {
		return true, nil
	}
	ctx, cancelFailClosed := s.withOBSFailClosedBudget(ctx)
	defer cancelFailClosed()
	operationCtx, cancelOperation := s.libraryPlaybackOperationContext(ctx)
	defer cancelOperation()
	err := s.reconcileLibrarySourceLocked(operationCtx, event.InputName, false)
	if operationErr := operationCtx.Err(); operationErr != nil {
		err = errors.Join(err, operationErr)
	}
	return true, err
}

func (s *Service) restartLibraryLoopAfterEndedLocked(ctx context.Context) error {
	s.clearActiveLoopLocked()
	return s.restartLibraryLoopLocked(ctx)
}

func (s *Service) restartLibraryLoopLocked(ctx context.Context) error {
	_, err := s.activateScheduledLibraryLoopLocked(ctx, s.now(), false, true)
	return err
}

func (s *Service) activateScheduledLibraryLoopLocked(
	ctx context.Context,
	now time.Time,
	redrawPlan bool,
	alwaysPlay bool,
) (lastInfo medialib.PeriodInfo, resultErr error) {
	ctx, cancelFailClosed := s.withOBSFailClosedBudget(ctx)
	defer cancelFailClosed()
	selectionSnapshot, err := s.capturePlaybackSelectionLocked(ctx, now)
	if err != nil {
		return medialib.PeriodInfoAt(now), err
	}
	operationCtx, cancelOperation := s.libraryPlaybackOperationContext(ctx)
	defer cancelOperation()

	attemptLimit := boundedLibraryCandidateAttempts(len(s.librarySnapshot.Loops))
	lastInfo = medialib.PeriodInfoAt(now)
	var (
		playbackErrors error
		tentativePaths []string
		attemptedOBS   bool
		succeeded      bool
	)
	defer func() {
		if succeeded {
			return
		}
		// An alternate candidate never proved the RPC failure asset-specific.
		// Avoid pinning a transient OBS/source outage into quarantine.
		for _, path := range tentativePaths {
			delete(s.libraryQuarantine, path)
		}
		if attemptedOBS {
			restoreCtx, cancelRestore := s.obsFailClosedContext(ctx)
			err := s.libDB.RestorePlaybackSelection(
				restoreCtx,
				selectionSnapshot.overrideDate,
				selectionSnapshot.override,
				selectionSnapshot.planDate,
				selectionSnapshot.period,
				selectionSnapshot.plan,
				selectionSnapshot.planExists,
			)
			cancelRestore()
			liveness.WorkerFromContext(ctx).Advance(liveness.PhaseOperation)
			if err != nil {
				resultErr = errors.Join(
					resultErr,
					fmt.Errorf("restore library playback selection after OBS failure: %w", err),
				)
			}
		}
	}()

	for attempt := 0; attempt < attemptLimit; attempt++ {
		loop, info, _, err := s.loopForTimeLocked(
			operationCtx,
			now,
			redrawPlan && attempt == 0,
		)
		lastInfo = info
		if err != nil {
			return lastInfo, errors.Join(playbackErrors, err)
		}
		needsPlay := alwaysPlay ||
			s.activeLoopID != loop.ID ||
			s.activeLoopPath != loop.Path ||
			s.activeLoopPeriod != info.Period ||
			!s.activeLoopEndsAt.Equal(info.EndsAt) ||
			!now.Before(s.activeLoopEndsAt)
		if !needsPlay {
			succeeded = true
			return lastInfo, nil
		}
		if err := s.playLibraryLoopLocked(operationCtx, loop, info); err != nil {
			if errors.Is(err, errUnhealthyLibraryAsset) {
				continue
			}
			attemptedOBS = true
			if playbackOperationMustAbort(ctx, operationCtx, err) ||
				errors.Is(err, errOBSPlayCleanupFailed) ||
				s.obs.Status().State != obs.StateConnected {
				return lastInfo, errors.Join(playbackErrors, err)
			}
			playbackErrors = errors.Join(playbackErrors, err)
			s.quarantineLibraryAssetLocked(
				loop.Path,
				fmt.Errorf("OBS rejected media playback: %w", err),
			)
			tentativePaths = append(tentativePaths, loop.Path)
			continue
		}
		succeeded = true
		return lastInfo, nil
	}
	return lastInfo, errors.Join(
		playbackErrors,
		publicError(fmt.Sprintf(
			"在單次安全上限 %d 個候選內，沒有可啟動的目前時段 loop 影片。",
			attemptLimit,
		)),
	)
}

func (s *Service) capturePlaybackSelectionLocked(
	ctx context.Context,
	now time.Time,
) (libraryPlaybackSelectionSnapshot, error) {
	info := medialib.PeriodInfoAt(now)
	snapshot := libraryPlaybackSelectionSnapshot{
		overrideDate: overrideDateKey(now),
		planDate:     periodPlanDate(now, info.Period),
		period:       info.Period,
	}
	override, err := s.libDB.Override(ctx, snapshot.overrideDate)
	if err != nil {
		return libraryPlaybackSelectionSnapshot{}, err
	}
	// Do not resurrect a direct override whose asset was already absent or
	// quarantined before this activation attempt began.
	if override.DirectLoopID != "" {
		if _, ok := s.findLoopByID(override.DirectLoopID); !ok {
			override.DirectLoopID = ""
		}
	}
	snapshot.override = override

	plan, exists, err := s.libDB.PeriodPlan(ctx, snapshot.planDate, info.Period)
	if err != nil {
		return libraryPlaybackSelectionSnapshot{}, err
	}
	if exists {
		loop, healthy := s.findLoopByID(plan.LoopID)
		if !healthy || loop.Period != info.Period {
			exists = false
		}
	}
	// The previous day's early-night override/plan is intentionally expired by
	// loopForTimeLocked. A failed OBS attempt must not revive that stale plan.
	if info.Period == medialib.PeriodNight &&
		now.Hour() < 6 &&
		snapshot.planDate != snapshot.overrideDate {
		previousOverride, err := s.libDB.Override(ctx, snapshot.planDate)
		if err != nil {
			return libraryPlaybackSelectionSnapshot{}, err
		}
		if previousOverride.Theme != "" || previousOverride.DirectLoopID != "" {
			exists = false
		}
	}
	snapshot.plan = plan
	snapshot.planExists = exists
	return snapshot, nil
}

func boundedLibraryCandidateAttempts(count int) int {
	if count <= 0 {
		return 1
	}
	if count > maxLibraryPlaybackCandidates {
		return maxLibraryPlaybackCandidates
	}
	return count
}

func (s *Service) libraryPlaybackOperationContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	timeout := s.obsPlaybackTimeout
	if timeout <= 0 {
		timeout = defaultOBSPlaybackOperationTimeout
	}
	return context.WithTimeout(ctx, timeout)
}

func playbackOperationMustAbort(
	parent context.Context,
	operation context.Context,
	err error,
) bool {
	return parent.Err() != nil ||
		operation.Err() != nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded)
}

func (s *Service) playOBSFileLocked(
	ctx context.Context,
	sourceName string,
	path string,
	options obs.PlaySourceOptions,
) error {
	if budget, ok := ctx.Value(obsFailClosedBudgetContextKey{}).(*obsFailClosedBudget); ok && budget.remaining <= 0 {
		return context.DeadlineExceeded
	}
	playCtx, cancelPlay := s.libraryPlaybackOperationContext(ctx)
	err := s.obs.PlaySourceFile(playCtx, sourceName, path, options)
	cancelPlay()
	tracker := liveness.WorkerFromContext(ctx)
	tracker.Advance(liveness.PhaseOperation)
	if err == nil {
		return nil
	}
	if !errors.Is(err, obs.ErrSourceMayBeMutated) {
		return err
	}

	// The OBS request may have applied local_file before its response or a
	// later mute/restart request failed. Stop the source using an independent,
	// bounded cleanup context so an expired activation deadline cannot leave
	// an uncommitted candidate playing.
	cleanupCtx, cancelCleanup := s.obsFailClosedStopContext(ctx)
	stopErr := s.obs.StopSource(cleanupCtx, sourceName)
	cancelCleanup()
	tracker.Advance(liveness.PhaseOperation)
	s.clearMediaProgressLocked(sourceName)
	switch sourceName {
	case s.cfg.OBSLoopSourceName:
		s.clearActiveLoopLocked()
	case s.cfg.OBSMusicSourceName:
		s.clearActiveMusicLocked()
	}
	if stopErr != nil {
		return errors.Join(
			err,
			fmt.Errorf("%w: stop %s: %v", errOBSPlayCleanupFailed, sourceName, stopErr),
		)
	}
	return err
}

func (s *Service) obsFailClosedContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	cleanupTimeout := s.obsCleanupTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = defaultOBSFailClosedBudgetTimeout
	}
	if budget, ok := ctx.Value(obsFailClosedBudgetContextKey{}).(*obsFailClosedBudget); ok {
		started := time.Now()
		cleanupCtx, cancel := context.WithTimeout(budget.ctx, budget.remaining)
		var once sync.Once
		return cleanupCtx, func() {
			once.Do(func() {
				cancel()
				budget.remaining -= time.Since(started)
			})
		}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
}

func (s *Service) obsFailClosedStopContext(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	budgetCtx, cancelBudget := s.obsFailClosedContext(ctx)
	actionTimeout := defaultOBSFailClosedActionTimeout
	if s.obsCleanupTimeout > 0 {
		actionTimeout = s.obsCleanupTimeout / 2
		if actionTimeout <= 0 {
			actionTimeout = time.Millisecond
		}
	}
	actionCtx, cancelAction := context.WithTimeout(budgetCtx, actionTimeout)
	return actionCtx, func() {
		cancelAction()
		cancelBudget()
	}
}

func (s *Service) withOBSFailClosedBudget(
	ctx context.Context,
) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Value(obsFailClosedBudgetContextKey{}).(*obsFailClosedBudget); ok {
		return ctx, func() {}
	}
	cleanupTimeout := s.obsCleanupTimeout
	if cleanupTimeout <= 0 {
		cleanupTimeout = defaultOBSFailClosedBudgetTimeout
	}
	cleanupCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	budget := &obsFailClosedBudget{remaining: cleanupTimeout, ctx: cleanupCtx}
	budgetCtx := context.WithValue(ctx, obsFailClosedBudgetContextKey{}, budget)
	return budgetCtx, cancel
}

func (s *Service) playLibraryLoopLocked(ctx context.Context, loop medialib.Loop, info medialib.PeriodInfo) error {
	if err := s.ensurePlayableLibraryAssetLocked(ctx, medialib.KindLoop, loop.Path); err != nil {
		return err
	}
	looping := true
	mute := true
	if err := s.playOBSFileLocked(ctx, s.cfg.OBSLoopSourceName, loop.Path, obs.PlaySourceOptions{
		Restart:         true,
		Looping:         &looping,
		Mute:            &mute,
		CenterSceneItem: true,
	}); err != nil {
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSLoopSourceName, loop.Path)
	s.activeLoopID = loop.ID
	s.activeLoopPath = loop.Path
	s.activeLoopTheme = loop.Theme
	s.activeLoopPeriod = info.Period
	s.activeLoopEndsAt = info.EndsAt
	return nil
}

func (s *Service) clearActiveLoopLocked() {
	s.activeLoopID = ""
	s.activeLoopPath = ""
	s.activeLoopTheme = ""
	s.activeLoopPeriod = ""
	s.activeLoopEndsAt = time.Time{}
}

func (s *Service) clearActiveMusicLocked() {
	s.activeMusicID = ""
	s.activeMusicPath = ""
}

func (s *Service) reconcileLibraryPlayback(ctx context.Context) error {
	_, err := s.reconcileLibraryPlaybackAttempt(ctx)
	return err
}

func (s *Service) reconcileLibraryPlaybackAttempt(ctx context.Context) (bool, error) {
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, ctxErr
	}
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	return true, s.reconcileLibraryPlaybackLocked(ctx, false)
}

func (s *Service) reconcileLibraryPlaybackLocked(ctx context.Context, recovering bool) error {
	ctx, cancelFailClosed := s.withOBSFailClosedBudget(ctx)
	defer cancelFailClosed()
	operationCtx, cancelOperation := s.libraryPlaybackOperationContext(ctx)
	defer cancelOperation()
	previousLoopID := s.activeLoopID
	previousLoopPath := s.activeLoopPath
	previousMusicID := s.activeMusicID
	previousMusicPath := s.activeMusicPath
	reconcileErr := s.ensureLibraryPlaybackLocked(operationCtx, false)
	if operationErr := operationCtx.Err(); operationErr != nil {
		return errors.Join(reconcileErr, operationErr)
	}

	if previousLoopID == s.activeLoopID && previousLoopPath == s.activeLoopPath && s.activeLoopPath != "" {
		reconcileErr = errors.Join(
			reconcileErr,
			s.reconcileLibrarySourceLocked(operationCtx, s.cfg.OBSLoopSourceName, recovering),
		)
	}
	if operationErr := operationCtx.Err(); operationErr != nil {
		return errors.Join(reconcileErr, operationErr)
	}
	if reconcileErr != nil && s.obs.Status().State != obs.StateConnected {
		return reconcileErr
	}
	if previousMusicID == s.activeMusicID && previousMusicPath == s.activeMusicPath && s.activeMusicPath != "" {
		reconcileErr = errors.Join(
			reconcileErr,
			s.reconcileLibrarySourceLocked(operationCtx, s.cfg.OBSMusicSourceName, recovering),
		)
	}
	if operationErr := operationCtx.Err(); operationErr != nil {
		return errors.Join(reconcileErr, operationErr)
	}
	return reconcileErr
}

func (s *Service) reconcileLibrarySourceLocked(ctx context.Context, inputName string, recovering bool) error {
	if inputName != s.cfg.OBSLoopSourceName && inputName != s.cfg.OBSMusicSourceName {
		return nil
	}

	expectedPath := s.activeLoopPath
	if inputName == s.cfg.OBSMusicSourceName {
		expectedPath = s.activeMusicPath
	}
	inspection, err := s.inspectMediaInputLocked(ctx, inputName, expectedPath)
	if err != nil {
		return err
	}
	if inspection.PathMismatch || inspection.Stalled {
		if inspection.PathMismatch && inspection.Settling && !recovering {
			return nil
		}
		if inspection.Stalled && expectedPath != "" {
			return s.failoverStalledLibrarySourceLocked(ctx, inputName, expectedPath)
		}
		if inputName == s.cfg.OBSLoopSourceName {
			return s.restartLibraryLoopLocked(ctx)
		}
		return s.replayActiveMusicLocked(ctx)
	}
	if inspection.Status.State == obs.MediaStateError && expectedPath != "" {
		s.quarantineLibraryAssetLocked(
			expectedPath,
			errors.New("OBS reported a media playback error"),
		)
	}
	switch inspection.Status.State {
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
		return nil
	case obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		if inspection.Status.State != obs.MediaStateError && inspection.Settling && !recovering {
			return nil
		}
	case obs.MediaStateNone, obs.MediaStatePaused:
	default:
		return fmt.Errorf("OBS media source %s returned unknown state %q", inputName, inspection.Status.State)
	}

	if expectedPath == "" {
		return nil
	}

	if inputName == s.cfg.OBSLoopSourceName {
		if inspection.Status.State == obs.MediaStateStopped ||
			inspection.Status.State == obs.MediaStateEnded ||
			inspection.Status.State == obs.MediaStateError {
			return s.restartLibraryLoopAfterEndedLocked(ctx)
		}
		return s.restartLibraryLoopLocked(ctx)
	}
	if !recovering && (inspection.Status.State == obs.MediaStateStopped || inspection.Status.State == obs.MediaStateEnded || inspection.Status.State == obs.MediaStateError) {
		s.clearActiveMusicLocked()
		return s.playNextMusicLocked(ctx, true)
	}
	return s.replayActiveMusicLocked(ctx)
}

func (s *Service) failoverStalledLibrarySourceLocked(
	ctx context.Context,
	inputName string,
	expectedPath string,
) error {
	if err := s.obs.StopSource(ctx, inputName); err != nil {
		return fmt.Errorf("stop stalled OBS media source %s: %w", inputName, err)
	}
	s.quarantineLibraryAssetLocked(
		expectedPath,
		errors.New("OBS media progress stalled"),
	)
	s.clearMediaProgressLocked(inputName)
	if inputName == s.cfg.OBSLoopSourceName {
		s.clearActiveLoopLocked()
		return s.restartLibraryLoopLocked(ctx)
	}
	s.clearActiveMusicLocked()
	return s.playNextMusicLocked(ctx, false)
}

func (s *Service) ensureLibraryPlayback(ctx context.Context, forceLoop bool) error {
	if err := s.ensureLibraryScanned(ctx); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return s.ensureLibraryPlaybackLocked(ctx, forceLoop)
}

func (s *Service) ensureLibraryPlaybackLocked(ctx context.Context, forceLoop bool) error {
	if s.libDB == nil {
		return errors.New("library state store is not configured")
	}
	ctx, cancelFailClosed := s.withOBSFailClosedBudget(ctx)
	defer cancelFailClosed()
	now := s.now()
	info, err := s.activateScheduledLibraryLoopLocked(ctx, now, forceLoop, forceLoop)
	var loopErr error
	if err != nil {
		loopErr = s.failClosedExpiredLoopLocked(ctx, now, info, err)
		if ctx.Err() != nil ||
			errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(err, errOBSPlayCleanupFailed) ||
			s.obs.Status().State != obs.StateConnected {
			return loopErr
		}
	}

	var persistenceErr error
	if s.pendingLastMusicID != "" {
		persistenceErr = s.persistLastMusicLocked(ctx)
	}

	if len(s.librarySnapshot.Music) == 0 {
		return errors.Join(loopErr, persistenceErr, s.playNextMusicLocked(ctx, false))
	}
	if s.activeMusicID != "" && s.activeMusicPath != "" {
		music, ok := s.findMusicByID(s.activeMusicID)
		if !ok || music.Path != s.activeMusicPath {
			if err := s.obs.StopSource(ctx, s.cfg.OBSMusicSourceName); err != nil {
				return errors.Join(
					loopErr,
					persistenceErr,
					fmt.Errorf("stop music removed from library snapshot: %w", err),
				)
			}
			s.clearMediaProgressLocked(s.cfg.OBSMusicSourceName)
			s.clearActiveMusicLocked()
		}
	}
	if s.activeMusicID == "" || s.activeMusicPath == "" {
		return errors.Join(loopErr, persistenceErr, s.playNextMusicLocked(ctx, false))
	}
	return errors.Join(loopErr, persistenceErr)
}

// failClosedExpiredLoopLocked prevents an already-ended schedule slot from
// leaking into a new period when selection or OBS playback fails. A healthy
// loop that still belongs to the current slot is left running through
// transient state-store or force-redraw failures.
func (s *Service) failClosedExpiredLoopLocked(
	ctx context.Context,
	now time.Time,
	info medialib.PeriodInfo,
	cause error,
) error {
	if s.activeLoopPath == "" &&
		errors.Is(cause, obs.ErrSourceMayBeMutated) &&
		!errors.Is(cause, errOBSPlayCleanupFailed) {
		// playOBSFileLocked already stopped this possibly-mutated source and
		// cleared its in-memory state. Avoid spending the shared cleanup budget
		// on a redundant second StopSource request.
		return cause
	}
	if s.activeLoopPath != "" &&
		s.activeLoopPeriod == info.Period &&
		now.Before(s.activeLoopEndsAt) {
		if active, healthy := s.findLoopByID(s.activeLoopID); healthy &&
			active.Path == s.activeLoopPath {
			return cause
		}
	}

	cleanupCtx, cancelCleanup := s.obsFailClosedStopContext(ctx)
	stopErr := s.obs.StopSource(cleanupCtx, s.cfg.OBSLoopSourceName)
	cancelCleanup()
	liveness.WorkerFromContext(ctx).Advance(liveness.PhaseOperation)
	if stopErr != nil {
		return errors.Join(
			cause,
			fmt.Errorf("stop expired library loop after transition failure: %w", stopErr),
		)
	}
	s.clearMediaProgressLocked(s.cfg.OBSLoopSourceName)
	s.clearActiveLoopLocked()
	return cause
}

func (s *Service) playNextMusic(ctx context.Context, force bool) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return s.playNextMusicLocked(ctx, force)
}

func (s *Service) playNextMusicLocked(ctx context.Context, force bool) error {
	if s.libDB == nil {
		return nil
	}
	ctx, cancelFailClosed := s.withOBSFailClosedBudget(ctx)
	defer cancelFailClosed()
	if len(s.librarySnapshot.Music) == 0 {
		if err := s.obs.StopSource(ctx, s.cfg.OBSMusicSourceName); err != nil {
			return fmt.Errorf("stop stale OBS music for empty library: %w", err)
		}
		s.clearMediaProgressLocked(s.cfg.OBSMusicSourceName)
		s.clearActiveMusicLocked()
		if force {
			return publicError("媒體庫沒有可播放的音樂。")
		}
		return nil
	}
	previousID := s.activeMusicID
	if previousID == "" {
		if s.pendingLastMusicID != "" {
			previousID = s.pendingLastMusicID
		} else {
			stored, err := s.libDB.LastMusicID(ctx)
			if err != nil {
				return err
			}
			previousID = stored
		}
	}
	operationCtx, cancelOperation := s.libraryPlaybackOperationContext(ctx)
	defer cancelOperation()
	attemptLimit := boundedLibraryCandidateAttempts(len(s.librarySnapshot.Music))
	var (
		playbackErrors error
		tentativePaths []string
		succeeded      bool
	)
	defer func() {
		if succeeded {
			return
		}
		for _, path := range tentativePaths {
			delete(s.libraryQuarantine, path)
		}
	}()
	for attempt := 0; attempt < attemptLimit; attempt++ {
		music, err := s.playableLibraryLocked().ChooseMusic(previousID, s.rng)
		if err != nil {
			return errors.Join(playbackErrors, err)
		}
		if err := s.playMusicAssetLocked(operationCtx, music); err != nil {
			if errors.Is(err, errUnhealthyLibraryAsset) {
				continue
			}
			if playbackOperationMustAbort(ctx, operationCtx, err) ||
				errors.Is(err, errOBSPlayCleanupFailed) ||
				s.obs.Status().State != obs.StateConnected {
				return errors.Join(playbackErrors, err)
			}
			playbackErrors = errors.Join(playbackErrors, err)
			s.quarantineLibraryAssetLocked(
				music.Path,
				fmt.Errorf("OBS rejected media playback: %w", err),
			)
			tentativePaths = append(tentativePaths, music.Path)
			continue
		}
		succeeded = true
		s.pendingLastMusicID = music.ID
		return s.persistLastMusicLocked(ctx)
	}
	return errors.Join(
		playbackErrors,
		publicError(fmt.Sprintf(
			"在單次安全上限 %d 個候選內，沒有可啟動的音樂。",
			attemptLimit,
		)),
	)
}

func (s *Service) replayActiveMusicLocked(ctx context.Context) error {
	if s.activeMusicID != "" && s.activeMusicPath != "" {
		if music, ok := s.findMusicByID(s.activeMusicID); ok && music.Path == s.activeMusicPath {
			if err := s.playMusicAssetLocked(ctx, music); err != nil {
				s.clearActiveMusicLocked()
				if errors.Is(err, errUnhealthyLibraryAsset) {
					return s.playNextMusicLocked(ctx, false)
				}
				return err
			}
			return nil
		}
	}
	s.clearActiveMusicLocked()
	return s.playNextMusicLocked(ctx, false)
}

func (s *Service) playMusicAssetLocked(ctx context.Context, music medialib.Music) error {
	if err := s.ensurePlayableLibraryAssetLocked(ctx, medialib.KindMusic, music.Path); err != nil {
		return err
	}
	looping := false
	mute := false
	if err := s.playOBSFileLocked(ctx, s.cfg.OBSMusicSourceName, music.Path, obs.PlaySourceOptions{
		Restart:         true,
		Looping:         &looping,
		Mute:            &mute,
		CenterSceneItem: false,
	}); err != nil {
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSMusicSourceName, music.Path)
	s.activeMusicID = music.ID
	s.activeMusicPath = music.Path
	return nil
}

func (s *Service) persistLastMusicLocked(ctx context.Context) error {
	if s.pendingLastMusicID == "" {
		return nil
	}
	if err := s.libDB.SetLastMusicID(ctx, s.pendingLastMusicID); err != nil {
		return fmt.Errorf("persist last library music %s: %w", s.pendingLastMusicID, err)
	}
	s.pendingLastMusicID = ""
	return nil
}

func (s *Service) loopForTimeLocked(ctx context.Context, t time.Time, force bool) (medialib.Loop, medialib.PeriodInfo, string, error) {
	info := medialib.PeriodInfoAt(t)
	date := periodPlanDate(t, info.Period)
	overrideDate := overrideDateKey(t)
	override, err := s.libDB.Override(ctx, overrideDate)
	if err != nil {
		return medialib.Loop{}, info, "", err
	}
	expiredPreviousOverride := false
	if info.Period == medialib.PeriodNight && t.Hour() < 6 {
		if previousOverride, err := s.libDB.Override(ctx, date); err != nil {
			return medialib.Loop{}, info, "", err
		} else if previousOverride.Theme != "" || previousOverride.DirectLoopID != "" {
			expiredPreviousOverride = true
			if err := s.libDB.ClearOverrideAndPeriodPlan(ctx, date, info.Period); err != nil {
				return medialib.Loop{}, info, "", err
			}
		}
	}

	if override.DirectLoopID != "" {
		if loop, ok := s.findLoopByID(override.DirectLoopID); ok {
			return loop, info, "指定影片", nil
		}
		_ = s.libDB.ClearDirectLoopOverride(ctx, overrideDate)
	}

	if force {
		_ = s.libDB.ClearPeriodPlan(ctx, date, info.Period)
	}
	if plan, ok, err := s.libDB.PeriodPlan(ctx, date, info.Period); err != nil {
		return medialib.Loop{}, info, "", err
	} else if ok && !force && !expiredPreviousOverride && s.canReusePeriodPlan(info.Period, override.Theme, plan.Theme) {
		if loop, found := s.findLoopByID(plan.LoopID); found {
			return loop, info, "", nil
		}
		_ = s.libDB.ClearPeriodPlan(ctx, date, info.Period)
	}

	preferredTheme := override.Theme
	loop, reason, err := s.chooseLoop(info.Period, preferredTheme)
	if err != nil {
		return medialib.Loop{}, info, "", err
	}
	plan := medialib.PeriodPlan{
		Date:   date,
		Period: info.Period,
		Theme:  loop.Theme,
		LoopID: loop.ID,
	}
	if err := s.libDB.SavePeriodPlan(ctx, plan); err != nil {
		return medialib.Loop{}, info, "", err
	}
	return loop, info, reason, nil
}

func (s *Service) canReusePeriodPlan(period medialib.Period, overrideTheme string, planTheme string) bool {
	if overrideTheme == "" || overrideTheme == planTheme {
		return true
	}
	for _, loop := range s.playableLibraryLocked().Loops {
		if loop.Period == period && loop.Theme == overrideTheme {
			return false
		}
	}
	return true
}

func (s *Service) previewLoopLocked(ctx context.Context) (medialib.Loop, medialib.PeriodInfo, string, error) {
	nextAt := medialib.PeriodInfoAt(s.now()).EndsAt
	return s.loopForTimeLocked(ctx, nextAt, false)
}

func (s *Service) chooseLoop(period medialib.Period, preferredTheme string) (medialib.Loop, string, error) {
	playable := s.playableLibraryLocked()
	if preferredTheme != "" {
		if loop, err := playable.ChooseLoop(period, preferredTheme, s.rng); err == nil {
			return loop, "", nil
		}
	}
	theme, err := playable.ChooseTheme(period, s.rng)
	if err == nil {
		loop, err := playable.ChooseLoop(period, theme, s.rng)
		return loop, fallbackReason(preferredTheme, theme), err
	}
	if len(playable.Loops) == 0 {
		return medialib.Loop{}, "", publicError("媒體庫沒有可播放的 loop 影片。")
	}
	return medialib.Loop{}, "", publicError(fmt.Sprintf(
		"媒體庫沒有可播放的%s時段 loop 影片；為避免播放錯誤時段，已停止切換。",
		periodLabel(period),
	))
}

func fallbackReason(preferredTheme string, actualTheme string) string {
	if preferredTheme == "" || preferredTheme == actualTheme {
		return ""
	}
	return fmt.Sprintf("指定主題 %s 沒有此時段素材，已退回 %s", preferredTheme, actualTheme)
}

func (s *Service) findLoopByID(id string) (medialib.Loop, bool) {
	for _, loop := range s.librarySnapshot.Loops {
		if loop.ID == id && s.libraryAssetQuarantineReasonLocked(loop.Path) == "" {
			return loop, true
		}
	}
	return medialib.Loop{}, false
}

func (s *Service) findMusicByID(id string) (medialib.Music, bool) {
	for _, music := range s.librarySnapshot.Music {
		if music.ID == id && s.libraryAssetQuarantineReasonLocked(music.Path) == "" {
			return music, true
		}
	}
	return medialib.Music{}, false
}

func periodPlanDate(t time.Time, period medialib.Period) string {
	if period == medialib.PeriodNight && t.Hour() < 6 {
		t = t.AddDate(0, 0, -1)
	}
	return t.Format("2006-01-02")
}

func overrideDateKey(t time.Time) string {
	return t.Format("2006-01-02")
}

func (s *Service) ImportLibraryUpload(ctx context.Context, req UploadRequest) (string, error) {
	if strings.TrimSpace(req.LocalPath) == "" {
		err := errors.New("local media path is required")
		s.setLastErr(err)
		return "", err
	}
	if !filepath.IsAbs(req.LocalPath) {
		err := fmt.Errorf("local media path must be absolute: %s", req.LocalPath)
		s.setLastErr(err)
		return "", err
	}
	plan, err := s.planLibraryUpload(req.FileName)
	if err != nil {
		s.setLastErr(err)
		return "", err
	}

	if err := os.MkdirAll(plan.destDir, 0o755); err != nil {
		s.setLastErr(err)
		return "", err
	}
	destPath := filepath.Join(plan.destDir, plan.fileName)
	if err := s.storeLibraryUpload(ctx, plan.kind, destPath, req.LocalPath); err != nil {
		s.setLastErr(err)
		return "", err
	}

	if err := s.ScanLibrary(ctx); err != nil {
		s.playbackMu.Lock()
		scanWarning := s.libraryScanErr
		s.playbackMu.Unlock()
		if scanWarning == "" {
			// A canceled or superseded scan may not have published a retained
			// warning. Keep the successful import response useful without ever
			// copying raw scanner, ffprobe, path, or operating-system details
			// into the Telegram group.
			scanWarning = s.publicScanDiagnostic(err)
		}
		return fmt.Sprintf("已匯入素材：%s（掃描提醒：%s）", plan.label, scanWarning), nil
	}
	return fmt.Sprintf("已匯入素材：%s", plan.label), nil
}

func (s *Service) planLibraryUpload(rawFileName string) (libraryUploadPlan, error) {
	fileName := CleanFileName(rawFileName)
	if fileName == "" {
		return libraryUploadPlan{}, publicError("檔名不可為空，請使用新版素材命名規則。")
	}
	if parsed, err := medialib.ParseLoopFilename(fileName); err == nil {
		return libraryUploadPlan{
			fileName: fileName,
			kind:     medialib.KindLoop,
			destDir:  s.cfg.LoopMediaDir,
			label:    fmt.Sprintf("loop %s/%s", parsed.Period, parsed.Theme),
		}, nil
	}
	if parsed, err := medialib.ParseMusicFilename(fileName); err == nil {
		return libraryUploadPlan{
			fileName: fileName,
			kind:     medialib.KindMusic,
			destDir:  s.cfg.MusicMediaDir,
			label:    fmt.Sprintf("music %s", parsed.Track),
		}, nil
	}
	return libraryUploadPlan{}, publicError("檔名不符合素材規則。loop 請用 loop_<period>_<theme>_<variant>，音樂請用 music_<track>。")
}

func (s *Service) preflightLibraryUpload(ctx context.Context, fileName string, declaredSize int64) error {
	return s.preflightLibraryUploadWithLimit(ctx, fileName, declaredSize, medialib.MaxDirectoryEntries)
}

func (s *Service) preflightLibraryUploadWithLimit(
	ctx context.Context,
	fileName string,
	declaredSize int64,
	limit int,
) error {
	plan, err := s.planLibraryUpload(fileName)
	if err != nil {
		return err
	}

	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	destPath := filepath.Join(plan.destDir, plan.fileName)
	if _, err := os.Stat(destPath); err == nil {
		return publicError("媒體庫已有同名素材，請換一個 variant 或 track 名稱。")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureLibraryImportCapacityWithLimit(plan.destDir, limit); err != nil {
		return err
	}
	sharedFilesystem, err := pathsShareFilesystem(s.cfg.TelegramBotAPIDir, plan.destDir)
	if err != nil {
		return err
	}
	if sharedFilesystem {
		combinedIncoming, err := requiredAvailableBytes(uint64(declaredSize), uint64(declaredSize))
		if err != nil {
			return err
		}
		return s.ensureStorageHeadroomBytes(plan.destDir, combinedIncoming)
	}
	if err := s.ensureStorageHeadroom(s.cfg.TelegramBotAPIDir, declaredSize); err != nil {
		return err
	}
	return s.ensureStorageHeadroom(plan.destDir, declaredSize)
}

func (s *Service) storeLibraryUpload(ctx context.Context, kind medialib.Kind, destPath, sourcePath string) error {
	return s.storeLibraryUploadWithLimit(
		ctx,
		kind,
		destPath,
		sourcePath,
		medialib.MaxDirectoryEntries,
	)
}

func (s *Service) storeLibraryUploadWithLimit(
	ctx context.Context,
	kind medialib.Kind,
	destPath string,
	sourcePath string,
	limit int,
) error {
	source, err := openLocalBotAPIFile(s.cfg.TelegramBotAPIDir, sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	return s.storeOpenedLibraryUploadWithLimit(ctx, kind, destPath, source, limit)
}

func (s *Service) storeOpenedLibraryUploadWithLimit(
	ctx context.Context,
	kind medialib.Kind,
	destPath string,
	source *os.File,
	limit int,
) error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(destPath); err == nil {
		return publicError("媒體庫已有同名素材，請換一個 variant 或 track 名稱。")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureLibraryImportCapacityWithLimit(filepath.Dir(destPath), limit); err != nil {
		return err
	}
	if kind != medialib.KindLoop && kind != medialib.KindMusic {
		return fmt.Errorf("unsupported media library kind: %s", kind)
	}
	validator := func(validationCtx context.Context, stagingPath string) error {
		probeCtx, cancelProbe := context.WithTimeout(validationCtx, uploadProbeTimeout)
		defer cancelProbe()
		meta, err := s.media.Probe(probeCtx, stagingPath)
		if err != nil {
			return err
		}
		switch kind {
		case medialib.KindLoop:
			return s.media.ValidateVideo(meta, s.cfg.MaxVideoSizeBytes, s.cfg.MaxVideoDurationSeconds)
		case medialib.KindMusic:
			return s.media.ValidateAudio(meta, s.cfg.MaxVideoSizeBytes, s.cfg.MaxVideoDurationSeconds)
		default:
			return fmt.Errorf("unsupported media library kind: %s", kind)
		}
	}
	_, err := copyOpenedFileAtomic(
		ctx,
		destPath,
		source,
		s.cfg.MaxVideoSizeBytes,
		func(actualSize int64) error {
			return s.ensureStorageHeadroom(filepath.Dir(destPath), actualSize)
		},
		validator,
	)
	if err != nil {
		return err
	}
	return nil
}

func ensureLibraryImportCapacityWithLimit(destDir string, limit int) error {
	if limit <= 0 {
		return errors.New("media library directory limit must be positive")
	}
	additionalEntries := 1
	stagingDir := filepath.Join(destDir, libraryStagingDirName)
	if info, err := os.Lstat(stagingDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("library staging path is not an owned directory: %s", stagingDir)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		additionalEntries++
	} else {
		return err
	}

	handle, err := os.Open(destDir)
	if err != nil {
		return err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if additionalEntries > limit || len(entries) > limit-additionalEntries {
		return publicError(fmt.Sprintf(
			"媒體庫目錄已達 %d 個項目上限，請先移除不需要的素材後再重試。",
			limit,
		))
	}
	return nil
}
