package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/journalstore"
	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	retryloop "github.com/tiwb/tg-obs-bot/internal/retry"
	"github.com/tiwb/tg-obs-bot/internal/secret"
	"github.com/tiwb/tg-obs-bot/internal/singleton"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

type Service struct {
	cfg           config.Config
	logger        *slog.Logger
	instanceLock  *singleton.Lock
	journalStore  telegramJournalPruner
	libDB         *medialib.StateStore
	media         *media.Manager
	obs           obsController
	bot           telegramMessenger
	now           func() time.Time
	mediaClockNow func() time.Time
	rng           *rand.Rand
	diskUsage     func(string) (media.DiskUsage, error)

	mu                       sync.Mutex
	playbackMu               sync.Mutex
	storageMu                sync.Mutex
	lastErr                  string
	librarySnapshot          medialib.Library
	libraryScanErr           string
	libraryRejectedCount     int
	librarySnapshotPublished bool
	libraryScanGeneration    atomic.Uint64
	libraryScanGateOnce      sync.Once
	libraryScanGate          chan struct{}
	libraryValidationCache   map[string]libraryValidationEntry
	libraryQuarantine        map[string]libraryQuarantineEntry
	activeLoopID             string
	activeLoopPath           string
	activeLoopTheme          string
	activeLoopPeriod         medialib.Period
	activeLoopEndsAt         time.Time
	activeMusicID            string
	activeMusicPath          string
	pendingLastMusicID       string
	mediaProgressByInput     map[string]mediaProgress
	obsRecoveryInProgress    atomic.Bool
	obsPlaybackTimeout       time.Duration
	obsCleanupTimeout        time.Duration
	shutdown                 []func() error
	workerStopGrace          time.Duration
	maintenanceInterval      time.Duration
	maintenanceFn            func(context.Context) maintenanceCycleResult
	retryRandom              func() uint64
	retrySleep               func(context.Context, time.Duration) error
	retryNow                 func() time.Time
	libraryScanLogMu         sync.Mutex
	libraryScanLog           *eventErrorSampler
	livenessRegistry         *liveness.Registry
	livenessReporter         requiredLivenessReporter
}

type obsController interface {
	Connect(context.Context) error
	Close() error
	Events() <-chan obs.Event
	Probe(context.Context) error
	GetMediaInputStatus(context.Context, string) (obs.MediaInputStatus, error)
	GetInputSettings(context.Context, string) (obs.InputSettings, error)
	PlaySourceFile(context.Context, string, string, obs.PlaySourceOptions) error
	StopSource(context.Context, string) error
	Status() obs.Status
}

type telegramMessenger interface {
	Run(context.Context) error
}

type telegramJournalPruner interface {
	PruneTelegramUpdateJournal(context.Context) (done int64, dead int64, err error)
}

const (
	obsEndedEventSettleGrace           = 2 * time.Second
	obsConnectAttemptTimeout           = 15 * time.Second
	defaultOBSPlaybackOperationTimeout = 40 * time.Second
	defaultOBSFailClosedBudgetTimeout  = 8 * time.Second
	defaultOBSFailClosedActionTimeout  = 3 * time.Second
	maxLibraryPlaybackCandidates       = 8
	uploadProbeTimeout                 = 2 * time.Minute
	mediaProgressGrace                 = 2 * time.Minute
	mediaProgressHardGrace             = 2 * mediaProgressGrace
	mediaCursorEpsilonMillis           = 1.0
	mediaCursorConfirmationMin         = 100 * time.Millisecond
	mediaCursorConfirmationMax         = 750 * time.Millisecond
	mediaCursorFallbackGap             = 251 * time.Millisecond
	defaultWorkerStopGrace             = telegram.RecommendedParentDrainGrace
	defaultMaintenancePeriod           = 10 * time.Minute
	maintenanceDeferredMax             = time.Minute
	libraryStatePruneBatchSize         = 256
)

var (
	ErrRequiredWorkerStopped         = errors.New("required service worker stopped unexpectedly")
	ErrRequiredInfrastructureStopped = errors.New("required service infrastructure stopped unexpectedly")
	ErrWorkerShutdownStuck           = errors.New("service workers did not stop after cancellation")
	ErrInfrastructureShutdownStuck   = errors.New("service infrastructure did not stop after cancellation")
	errOBSPlayCleanupFailed          = errors.New("failed to fail-close a possibly modified OBS source")
)

type requiredLivenessReporter interface {
	Start(context.Context) (<-chan error, error)
	Close() error
}

type newOptions struct {
	livenessSink liveness.FrameSink
}

// Option applies one optional process-level dependency while preserving the
// existing New call shape for direct and unit-level callers.
type Option func(*newOptions) error

// WithLivenessSink transfers ownership of the prevalidated supervisor sink to
// the service. A nil sink keeps direct, unsupervised operation unchanged.
func WithLivenessSink(sink liveness.FrameSink) Option {
	return func(options *newOptions) error {
		if sink == nil {
			return nil
		}
		if options.livenessSink != nil {
			return errors.New("liveness sink already configured")
		}
		options.livenessSink = sink
		return nil
	}
}

type mediaProgress struct {
	Path                     string
	State                    obs.MediaState
	CursorMillis             float64
	HasCursor                bool
	ObservedAt               time.Time
	GenerationStartedAt      time.Time
	LastProgressAt           time.Time
	LastDefinitiveProgressAt time.Time
}

type maintenanceCycleResult struct {
	deferred bool
	err      error
}

type mediaInspection struct {
	Status       obs.MediaInputStatus
	PathMismatch bool
	Stalled      bool
	Settling     bool
}

type UploadRequest struct {
	LocalPath string
	FileName  string
}

type publicError string

func (e publicError) Error() string {
	return string(e)
}

func (e publicError) PublicMessage() string {
	return string(e)
}

func New(cfg config.Config, logger *slog.Logger, options ...Option) (*Service, error) {
	var settings newOptions
	releaseUnownedSink := true
	defer func() {
		if releaseUnownedSink && settings.livenessSink != nil {
			_ = settings.livenessSink.Close()
		}
	}()
	for _, option := range options {
		if option == nil {
			continue
		}
		if err := option(&settings); err != nil {
			return nil, fmt.Errorf("apply app option: %w", err)
		}
	}

	// DataDir is a configuration default, not an independent runtime path.
	// Preserve the private default database directory without making deployments
	// with an explicit database path depend on an otherwise-unused base.
	if pathUsesBase(cfg.DatabasePath, cfg.DataDir) {
		if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("create data directory: %w", err)
		}
		if err := os.Chmod(cfg.DataDir, 0o700); err != nil {
			return nil, fmt.Errorf("secure data directory: %w", err)
		}
	}

	registry := liveness.NewRegistry(liveness.Options{})
	var reporter requiredLivenessReporter
	if settings.livenessSink != nil {
		configured, err := liveness.NewReporter(
			registry,
			settings.livenessSink,
			logger.With("component", "liveness"),
			liveness.DefaultReportInterval,
		)
		if err != nil {
			return nil, fmt.Errorf("configure liveness reporter: %w", err)
		}
		reporter = configured
	}

	instanceLock, err := singleton.Acquire(cfg.DatabasePath, cfg.TelegramBotToken)
	if err != nil {
		return nil, fmt.Errorf("acquire backend singleton: %w", err)
	}
	releaseUnownedLock := true
	defer func() {
		if releaseUnownedLock {
			_ = instanceLock.Close()
		}
	}()
	// Open SQLite through the exact canonical location that defines the lock
	// identity. Relative paths and symlink aliases therefore cannot split the
	// runtime lock from the database it protects.
	cfg.DatabasePath = instanceLock.DatabasePath()

	store, err := journalstore.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	manager := media.NewManager(cfg.FFProbePath)
	if err := os.MkdirAll(cfg.LoopMediaDir, 0o755); err != nil {
		_ = store.Close()
		return nil, err
	}
	if err := os.MkdirAll(cfg.MusicMediaDir, 0o755); err != nil {
		_ = store.Close()
		return nil, err
	}
	libDB, err := medialib.OpenState(context.Background(), cfg.DatabasePath)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	obsClient, err := obs.NewClient(obs.Options{
		URL:         cfg.OBSURL(),
		Password:    cfg.OBSPassword,
		EventBuffer: 16,
		Logger:      logger.With("component", "obs"),
	})
	if err != nil {
		_ = libDB.Close()
		_ = store.Close()
		return nil, err
	}

	service := &Service{
		cfg:                 cfg,
		logger:              logger,
		instanceLock:        instanceLock,
		journalStore:        store,
		libDB:               libDB,
		media:               manager,
		obs:                 obsClient,
		now:                 time.Now,
		mediaClockNow:       time.Now,
		rng:                 rand.New(rand.NewSource(time.Now().UnixNano())),
		diskUsage:           media.DiskUsageForPath,
		workerStopGrace:     defaultWorkerStopGrace,
		maintenanceInterval: defaultMaintenancePeriod,
		livenessRegistry:    registry,
		livenessReporter:    reporter,
		shutdown:            []func() error{instanceLock.Close, obsClient.Close, store.Close},
	}
	releaseUnownedLock = false
	releaseUnownedSink = false
	if reporter != nil {
		service.shutdown = append(service.shutdown, reporter.Close)
	}
	service.shutdown = append(service.shutdown, libDB.Close)

	bot, err := telegram.New(telegram.Config{
		Token:              cfg.TelegramBotToken,
		APIBaseURL:         cfg.TelegramAPIBaseURL,
		AllowedChatID:      cfg.AllowedChatID,
		MaxUploadSizeBytes: cfg.MaxVideoSizeBytes,
	}, service.telegramHooks(), logger.With("component", "telegram"), telegram.WithUpdateJournal(store))
	if err != nil {
		service.Close()
		return nil, err
	}
	service.bot = bot
	return service, nil
}

func pathUsesBase(path, base string) bool {
	path = strings.TrimSpace(path)
	base = strings.TrimSpace(base)
	if path == "" || base == "" {
		return false
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	absoluteBase, err := filepath.Abs(base)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(absoluteBase, absolutePath)
	if err != nil {
		return false
	}
	return relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func (s *Service) Close() {
	for i := len(s.shutdown) - 1; i >= 0; i-- {
		if err := s.shutdown[i](); err != nil {
			s.logger.Warn("shutdown error", "error", s.redactError(err))
		}
	}
}

func (s *Service) Run(parentCtx context.Context) error {
	s.logger.Info("tg-obs-bot starting", "database", s.redactString(s.cfg.DatabasePath), "media_dir", s.redactString(s.cfg.MediaDir))
	runCtx, cancel := context.WithCancel(parentCtx)

	workers, err := s.prepareRequiredWorkers()
	if err != nil {
		cancel()
		return fmt.Errorf("prepare required workers: %w", err)
	}

	if err := s.recoverStartupState(runCtx); err != nil {
		cancel()
		return err
	}

	workerResults := make(chan serviceWorkerResult, len(workers))
	pendingWorkers := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		pendingWorkers[worker.name] = struct{}{}
		go func(worker serviceWorker) {
			worker.tracker.Advance(liveness.PhaseStarting)
			workerCtx := liveness.WithWorker(runCtx, worker.tracker)
			workerResults <- serviceWorkerResult{name: worker.name, err: worker.run(workerCtx)}
		}(worker)
	}

	var reporterResults <-chan error
	reporterPending := false
	if s.livenessReporter != nil {
		reporterResults, err = s.livenessReporter.Start(runCtx)
		if err != nil {
			cancel()
			runErr := requiredInfrastructureError(err)
			return errors.Join(
				runErr,
				s.waitForRuntime(
					workerResults,
					pendingWorkers,
					nil,
					false,
				),
			)
		}
		reporterPending = true
	}

	var runErr error
run:
	for {
		select {
		case <-parentCtx.Done():
			runErr = parentCtx.Err()
			break run
		case result := <-workerResults:
			delete(pendingWorkers, result.name)
			if err := parentCtx.Err(); err != nil {
				runErr = err
				if !onlyContextTermination(result.err) {
					runErr = errors.Join(runErr, requiredWorkerError(result))
				}
			} else {
				runErr = requiredWorkerError(result)
			}
			break run
		case result, ok := <-reporterResults:
			reporterPending = false
			if !ok {
				result = nil
			}
			if err := parentCtx.Err(); err != nil {
				runErr = err
				if !onlyContextTermination(result) {
					runErr = errors.Join(runErr, requiredInfrastructureError(result))
				}
			} else {
				runErr = requiredInfrastructureError(result)
			}
			break run
		}
	}

	cancel()
	return errors.Join(
		runErr,
		s.waitForRuntime(
			workerResults,
			pendingWorkers,
			reporterResults,
			reporterPending,
		),
	)
}

type serviceWorker struct {
	name    string
	id      liveness.WorkerID
	owner   liveness.Owner
	run     func(context.Context) error
	tracker *liveness.Worker
}

type serviceWorkerResult struct {
	name string
	err  error
}

func (s *Service) prepareRequiredWorkers() ([]serviceWorker, error) {
	registry := s.livenessRegistry
	if registry == nil {
		registry = liveness.NewRegistry(liveness.Options{})
		s.livenessRegistry = registry
	}

	workers := []serviceWorker{
		{
			name:  "telegram",
			id:    liveness.WorkerTelegram,
			owner: liveness.OwnerTelegram,
			run:   s.bot.Run,
		},
		{
			name:  "obs-reconnect",
			id:    liveness.WorkerOBSReconnect,
			owner: liveness.OwnerOBSReconnect,
			run:   s.obsReconnectLoop,
		},
		{
			name:  "obs-events",
			id:    liveness.WorkerOBSEvents,
			owner: liveness.OwnerOBSEvents,
			run:   s.obsEventLoop,
		},
		{
			name:  "maintenance",
			id:    liveness.WorkerMaintenance,
			owner: liveness.OwnerMaintenance,
			run:   s.maintenanceLoop,
		},
	}
	playbackWorker := serviceWorker{
		name:  "library-scheduler",
		id:    liveness.WorkerPlayback,
		owner: liveness.OwnerLibraryScheduler,
		run:   s.librarySchedulerLoop,
	}
	workers = append(workers, playbackWorker)

	for index := range workers {
		tracker, err := registry.Bind(workers[index].id, workers[index].owner)
		if err != nil {
			return nil, err
		}
		workers[index].tracker = tracker
	}
	if err := registry.Seal(); err != nil {
		return nil, err
	}
	return workers, nil
}

func requiredWorkerError(result serviceWorkerResult) error {
	stopped := fmt.Errorf("%w: %s", ErrRequiredWorkerStopped, result.name)
	if result.err == nil {
		return stopped
	}
	return errors.Join(stopped, fmt.Errorf("%s worker: %w", result.name, result.err))
}

func requiredInfrastructureError(err error) error {
	stopped := fmt.Errorf("%w: liveness-reporter", ErrRequiredInfrastructureStopped)
	if err == nil {
		return stopped
	}
	return errors.Join(stopped, fmt.Errorf("liveness reporter: %w", err))
}

func (s *Service) waitForRuntime(
	results <-chan serviceWorkerResult,
	pending map[string]struct{},
	reporterResults <-chan error,
	reporterPending bool,
) error {
	if len(pending) == 0 && !reporterPending {
		return nil
	}
	if !reporterPending {
		reporterResults = nil
	}
	grace := s.workerStopGrace
	if grace <= 0 {
		grace = defaultWorkerStopGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()

	var drainErr error
	for len(pending) > 0 || reporterPending {
		select {
		case result := <-results:
			delete(pending, result.name)
			if !onlyContextTermination(result.err) {
				drainErr = errors.Join(drainErr, requiredWorkerError(result))
			}
		case result, ok := <-reporterResults:
			reporterPending = false
			reporterResults = nil
			if !ok {
				result = nil
			}
			if !onlyContextTermination(result) {
				drainErr = errors.Join(drainErr, requiredInfrastructureError(result))
			}
		case <-timer.C:
			var stuckErr error
			if len(pending) > 0 {
				names := make([]string, 0, len(pending))
				for name := range pending {
					names = append(names, name)
				}
				sort.Strings(names)
				stuckErr = fmt.Errorf(
					"%w: grace=%s pending=%s",
					ErrWorkerShutdownStuck,
					grace,
					strings.Join(names, ","),
				)
			}
			if reporterPending {
				stuckErr = errors.Join(
					stuckErr,
					fmt.Errorf(
						"%w: grace=%s pending=liveness-reporter",
						ErrInfrastructureShutdownStuck,
						grace,
					),
				)
			}
			return errors.Join(drainErr, stuckErr)
		}
	}
	return drainErr
}

func onlyContextTermination(err error) bool {
	if err == nil {
		return true
	}
	if err == context.Canceled || err == context.DeadlineExceeded {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !onlyContextTermination(cause) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		cause := wrapped.Unwrap()
		return cause != nil && onlyContextTermination(cause)
	}
	return false
}

func (s *Service) maintenanceLoop(ctx context.Context) error {
	tracker := liveness.WorkerFromContext(ctx)
	interval := s.maintenanceInterval
	if interval <= 0 {
		interval = defaultMaintenancePeriod
	}
	retries := newRecurringRetry(
		interval,
		maxRetryDelay(interval, maintenanceRetryMaxDelay),
		s.retryRandom,
	)
	schedule := newRecurringSchedule(interval, false, s.retryClockNow)
	deferredSampler := retryloop.NewSampler(recurringLogEvery)
	delay := schedule.delay()
	waitPhase := liveness.PhaseScheduledWait

	for {
		if err := s.waitRecurring(ctx, tracker, waitPhase, delay); err != nil {
			return err
		}
		tracker.Advance(liveness.PhaseOperation)
		schedule.beginCycle()
		var result maintenanceCycleResult
		if s.maintenanceFn != nil {
			result = s.maintenanceFn(ctx)
		} else {
			result = s.performMaintenanceCycle(ctx)
		}
		if result.err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			s.setLastErr(result.err)
			failure := retries.failure()
			logRecurringFailure(s.logger, "periodic maintenance failed", s.redactError(result.err), failure)
			delay = schedule.failureDelay(failure.attempt.Delay)
			waitPhase = liveness.PhaseRetryWait
			continue
		}
		if result.deferred {
			delay = schedule.retryDelay(maintenanceDeferredDelay(interval))
			logMaintenanceDeferred(s.logger, deferredSampler.Failure())
			waitPhase = liveness.PhaseScheduledWait
			continue
		}
		logMaintenanceDeferredRecovery(s.logger, deferredSampler.Recovery())
		logRecurringRecovery(s.logger, "periodic maintenance recovered", retries.recovery())
		delay = schedule.delay()
		waitPhase = liveness.PhaseScheduledWait
	}
}

func (s *Service) recoverPlaybackAfterOBSConnect(ctx context.Context) error {
	return s.recoverLibraryPlaybackAfterOBSConnect(ctx)
}

func (s *Service) inspectMediaInputLocked(ctx context.Context, inputName string, expectedPath string) (mediaInspection, error) {
	status, err := s.obs.GetMediaInputStatus(ctx, inputName)
	if err != nil {
		return mediaInspection{}, fmt.Errorf("get OBS media status for %s: %w", inputName, err)
	}
	if err := validateMediaState(status.State); err != nil {
		return mediaInspection{}, fmt.Errorf("OBS media source %s: %w", inputName, err)
	}

	inspection := mediaInspection{
		Status:   status,
		Settling: s.mediaGenerationSettlingLocked(inputName, expectedPath),
	}
	settings, err := s.obs.GetInputSettings(ctx, inputName)
	if err != nil {
		return mediaInspection{}, fmt.Errorf("get OBS input settings for %s: %w", inputName, err)
	}
	if !sameMediaPath(settings.LocalFile, expectedPath) {
		inspection.PathMismatch = true
		return inspection, nil
	}
	switch status.State {
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
		inspection.Stalled = s.mediaInputStalledLocked(inputName, expectedPath, status)
	}
	if inspection.Stalled &&
		status.State == obs.MediaStatePlaying &&
		status.CursorMilliseconds != nil {
		confirmed, confirmErr := s.confirmStalledMediaInputLocked(
			ctx,
			inputName,
			expectedPath,
			status,
		)
		if confirmErr != nil {
			return mediaInspection{}, confirmErr
		}
		return confirmed, nil
	}
	return inspection, nil
}

func (s *Service) confirmStalledMediaInputLocked(
	ctx context.Context,
	inputName string,
	expectedPath string,
	initialStatus obs.MediaInputStatus,
) (mediaInspection, error) {
	timer := time.NewTimer(mediaCursorConfirmationDelay(initialStatus))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return mediaInspection{}, ctx.Err()
	case <-timer.C:
	}

	status, err := s.obs.GetMediaInputStatus(ctx, inputName)
	if err != nil {
		return mediaInspection{}, fmt.Errorf("confirm OBS media progress for %s: %w", inputName, err)
	}
	if err := validateMediaState(status.State); err != nil {
		return mediaInspection{}, fmt.Errorf("OBS media source %s: %w", inputName, err)
	}
	inspection := mediaInspection{
		Status:   status,
		Settling: s.mediaGenerationSettlingLocked(inputName, expectedPath),
	}
	settings, err := s.obs.GetInputSettings(ctx, inputName)
	if err != nil {
		return mediaInspection{}, fmt.Errorf("confirm OBS input settings for %s: %w", inputName, err)
	}
	if !sameMediaPath(settings.LocalFile, expectedPath) {
		inspection.PathMismatch = true
		return inspection, nil
	}
	switch status.State {
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
		inspection.Stalled = s.mediaInputStalledLocked(inputName, expectedPath, status)
	}
	return inspection, nil
}

func mediaCursorConfirmationDelay(status obs.MediaInputStatus) time.Duration {
	if status.DurationMilliseconds == nil {
		return mediaCursorFallbackGap
	}
	durationMillis := *status.DurationMilliseconds
	if durationMillis <= 0 ||
		math.IsNaN(durationMillis) ||
		math.IsInf(durationMillis, 0) {
		return mediaCursorFallbackGap
	}

	minMillis := float64(mediaCursorConfirmationMin) / float64(time.Millisecond)
	maxMillis := float64(mediaCursorConfirmationMax) / float64(time.Millisecond)
	halfMillis := durationMillis / 2
	delayMillis := halfMillis
	switch {
	case halfMillis < minMillis:
		// Add whole loop durations until the sample is practical while
		// preserving a half-loop phase offset. This cannot land on the same
		// phase for any positive reported duration.
		cycles := math.Ceil((minMillis - halfMillis) / durationMillis)
		delayMillis = cycles*durationMillis + halfMillis
	case halfMillis > maxMillis:
		// Here maxMillis is strictly less than one full duration, so it also
		// cannot be a whole-loop phase.
		delayMillis = maxMillis
	}
	delay := time.Duration(delayMillis * float64(time.Millisecond))
	if delay <= 0 {
		return mediaCursorFallbackGap
	}
	return delay
}

func validateMediaState(state obs.MediaState) error {
	switch state {
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering,
		obs.MediaStateNone, obs.MediaStatePaused, obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		return nil
	default:
		return fmt.Errorf("returned unknown state %q", state)
	}
}

func sameMediaPath(actual string, expected string) bool {
	if actual == "" || expected == "" {
		return false
	}
	return filepath.Clean(actual) == filepath.Clean(expected)
}

func (s *Service) mediaInputStalledLocked(inputName string, path string, status obs.MediaInputStatus) bool {
	now := s.nowUTC()
	if s.mediaProgressByInput == nil {
		s.mediaProgressByInput = make(map[string]mediaProgress)
	}
	previous, ok := s.mediaProgressByInput[inputName]
	resetProgress := !ok || previous.Path != path || previous.LastProgressAt.IsZero() || now.Before(previous.LastProgressAt)
	progressed := resetProgress || previous.State != status.State
	definitiveProgress := resetProgress || previous.LastDefinitiveProgressAt.IsZero() || now.Before(previous.LastDefinitiveProgressAt)

	hasCursor := status.CursorMilliseconds != nil
	cursorMillis := 0.0
	if hasCursor {
		cursorMillis = *status.CursorMilliseconds
	}
	if ok && previous.HasCursor != hasCursor {
		progressed = true
		if hasCursor {
			definitiveProgress = true
		}
	}
	if ok && previous.HasCursor && hasCursor {
		delta := cursorMillis - previous.CursorMillis
		if delta < 0 {
			delta = -delta
		}
		if delta >= mediaCursorEpsilonMillis {
			progressed = true
			definitiveProgress = true
		}
	}
	lastProgressAt := previous.LastProgressAt
	if progressed || lastProgressAt.IsZero() {
		lastProgressAt = now
	}
	lastDefinitiveProgressAt := previous.LastDefinitiveProgressAt
	if definitiveProgress || lastDefinitiveProgressAt.IsZero() {
		lastDefinitiveProgressAt = now
	}
	generationStartedAt := previous.GenerationStartedAt
	if ok && previous.Path != path {
		generationStartedAt = time.Time{}
	}
	s.mediaProgressByInput[inputName] = mediaProgress{
		Path:                     path,
		State:                    status.State,
		CursorMillis:             cursorMillis,
		HasCursor:                hasCursor,
		ObservedAt:               now,
		GenerationStartedAt:      generationStartedAt,
		LastProgressAt:           lastProgressAt,
		LastDefinitiveProgressAt: lastDefinitiveProgressAt,
	}
	return now.Sub(lastProgressAt) >= mediaProgressGrace ||
		now.Sub(lastDefinitiveProgressAt) >= mediaProgressHardGrace
}

func (s *Service) resetMediaProgressLocked(inputName string, path string) {
	if s.mediaProgressByInput == nil {
		s.mediaProgressByInput = make(map[string]mediaProgress)
	}
	now := s.nowUTC()
	s.mediaProgressByInput[inputName] = mediaProgress{
		Path:                     path,
		ObservedAt:               now,
		GenerationStartedAt:      now,
		LastProgressAt:           now,
		LastDefinitiveProgressAt: now,
	}
}

func (s *Service) mediaGenerationSettlingLocked(inputName string, path string) bool {
	progress, ok := s.mediaProgressByInput[inputName]
	if !ok || progress.Path != path || progress.GenerationStartedAt.IsZero() {
		return false
	}
	now := s.nowUTC()
	if now.Before(progress.GenerationStartedAt) {
		// A wall-clock rollback invalidates the old generation grace window;
		// otherwise a same-period rollback could suppress terminal or
		// path-mismatch recovery for minutes or hours.
		return false
	}
	return now.Before(progress.GenerationStartedAt.Add(obsEndedEventSettleGrace))
}

func (s *Service) clearMediaProgressLocked(inputName string) {
	delete(s.mediaProgressByInput, inputName)
}

func (s *Service) NowText(ctx context.Context) (string, error) {
	return s.LibraryNowText(ctx)
}

func (s *Service) StatusText(ctx context.Context, obsConnected bool) (string, error) {
	return s.LibraryStatusText(ctx, obsConnected)
}

func (s *Service) telegramHooks() telegram.Hooks {
	return telegram.Hooks{
		PreflightUpload: func(ctx context.Context, upload telegram.Upload) error {
			err := s.preflightUpload(ctx, upload.FileName, upload.SizeBytes)
			if err != nil {
				s.setLastErr(err)
			}
			return err
		},
		ImportUpload: func(ctx context.Context, upload telegram.Upload) (string, error) {
			return s.ImportLibraryUpload(ctx, UploadRequest{
				LocalPath: upload.LocalPath,
				FileName:  upload.FileName,
			})
		},
		LibraryPage: s.LibraryPage,
		Scan:        s.ScanLibraryText,
		Preview:     s.PreviewText,
		SetTheme:    s.SetThemeText,
		SelectLoop:  s.SelectLoopText,
		SkipLoop:    s.SkipLoopText,
		SkipMusic:   s.SkipMusicText,
		Now:         s.NowText,
		Status: func(ctx context.Context) (string, error) {
			return s.StatusText(ctx, s.obs.Status().State == obs.StateConnected)
		},
	}
}

func (s *Service) obsReconnectLoop(ctx context.Context) error {
	tracker := liveness.WorkerFromContext(ctx)
	retries := newOBSRetryState(s.retryRandom)
	schedule := newRecurringSchedule(obsReconnectInterval, true, s.retryClockNow)
	delay := schedule.delay()
	waitPhase := liveness.PhaseScheduledWait
	for {
		if delay > 0 {
			if err := s.waitRecurring(ctx, tracker, waitPhase, delay); err != nil {
				return err
			}
		} else if err := ctx.Err(); err != nil {
			tracker.Advance(liveness.PhaseCancelWait)
			return err
		}
		tracker.Advance(liveness.PhaseOperation)
		schedule.beginCycle()
		result := s.maintainOBSConnection(ctx)
		delay = s.observeOBSResult(retries, result)
		if result.failure == obsRetryNone || result.err == nil {
			delay = schedule.delay()
			waitPhase = liveness.PhaseScheduledWait
		} else {
			delay = schedule.failureDelay(delay)
			waitPhase = liveness.PhaseRetryWait
		}
	}
}

func (s *Service) maintainOBSConnection(ctx context.Context) obsMaintenanceResult {
	result := obsMaintenanceResult{}
	switch s.obs.Status().State {
	case obs.StateDisconnected:
		if !s.obsRecoveryInProgress.CompareAndSwap(false, true) {
			return result
		}
		defer s.obsRecoveryInProgress.Store(false)

		connectCtx, cancelConnect := context.WithTimeout(ctx, obsConnectAttemptTimeout)
		err := s.obs.Connect(connectCtx)
		cancelConnect()
		if err != nil {
			if ctx.Err() != nil {
				return result
			}
			s.setLastErr(err)
			result.failure = obsRetryConnect
			result.err = err
			return result
		}
		result.connected = true
		result.recoverConnect = true
		result.recoverProbe = true
		if err := s.recoverPlaybackAfterOBSConnect(ctx); err != nil {
			if ctx.Err() != nil {
				return result
			}
			s.setLastErr(err)
			result.failure = obsRetryResume
			result.err = err
			return result
		}
		result.recoverResume = true
	case obs.StateConnected:
		if s.obsRecoveryInProgress.Load() {
			return result
		}
		if err := s.obs.Probe(ctx); err != nil {
			if ctx.Err() != nil {
				return result
			}
			s.setLastErr(err)
			result.failure = obsRetryProbe
			result.err = err
			return result
		}
		result.recoverProbe = true
		if s.libraryPlaybackMissing() {
			if !s.obsRecoveryInProgress.CompareAndSwap(false, true) {
				return result
			}
			defer s.obsRecoveryInProgress.Store(false)
			if s.obs.Status().State != obs.StateConnected || !s.libraryPlaybackMissing() {
				return result
			}
			if err := s.recoverPlaybackAfterOBSConnect(ctx); err != nil {
				if ctx.Err() != nil {
					return result
				}
				s.setLastErr(err)
				result.failure = obsRetryResume
				result.err = err
				return result
			}
			result.recoverResume = true
		}
	}
	return result
}

func (s *Service) obsEventLoop(ctx context.Context) error {
	tracker := liveness.WorkerFromContext(ctx)
	events := s.obs.Events()
	libraryFailures := newEventErrorSampler()
	progress := time.NewTicker(tracker.ProgressInterval())
	defer progress.Stop()
	for {
		tracker.Advance(liveness.PhaseEventWait)
		select {
		case <-ctx.Done():
			tracker.Advance(liveness.PhaseCancelWait)
			return ctx.Err()
		case <-progress.C:
			continue
		case event, ok := <-events:
			tracker.Advance(liveness.PhaseOperation)
			if !ok {
				if err := ctx.Err(); err != nil {
					tracker.Advance(liveness.PhaseCancelWait)
					return err
				}
				return errors.New("OBS event stream closed")
			}
			if event.Type != obs.EventMediaEnded {
				continue
			}
			if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
				continue
			}
			attempted, err := s.handleLibraryOBSEventAttempt(ctx, event)
			if !attempted {
				continue
			}
			if err != nil {
				logEventFailure(
					s.logger,
					"advance library playback after OBS event failed",
					s.redactError(err),
					libraryFailures.failure(),
				)
			} else {
				libraryFailures.success()
			}
		}
	}
}

func (s *Service) recoverStartupState(ctx context.Context) error {
	return s.sweepStaleLibraryImportTemps(ctx)
}

func (s *Service) performMaintenance(ctx context.Context) error {
	return s.performMaintenanceCycle(ctx).err
}

func (s *Service) performMaintenanceCycle(ctx context.Context) maintenanceCycleResult {
	_, _, journalErr := s.journalStore.PruneTelegramUpdateJournal(ctx)
	now := time.Now()
	if s.now != nil {
		now = s.now()
	}
	_, libraryStateErr := s.libDB.PruneBefore(
		ctx,
		now.AddDate(0, 0, -2).Format("2006-01-02"),
		libraryStatePruneBatchSize,
	)
	tempAttempted, tempErr := s.trySweepStaleLibraryImportTemps(ctx)
	return maintenanceCycleResult{
		deferred: !tempAttempted,
		err:      errors.Join(journalErr, libraryStateErr, tempErr),
	}
}

func (s *Service) setLastErr(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = s.publicErrorDiagnostic(err)
}

func (s *Service) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Service) redactError(err error) error {
	return secret.RedactError(err, s.cfg.SensitiveValues()...)
}

func (s *Service) redactString(value string) string {
	return secret.RedactString(value, s.cfg.SensitiveValues()...)
}

func (s *Service) publicErrorDiagnostic(err error) string {
	type publicMessenger interface {
		PublicMessage() string
	}
	var publicErr publicMessenger
	if errors.As(err, &publicErr) {
		return secret.RedactString(
			publicErr.PublicMessage(),
			s.cfg.SensitiveValues()...,
		)
	}
	// Retained runtime errors are visible to every authorized group member via
	// /status. Never copy internal error text into that public surface: OS,
	// SQLite, OBS, and Telegram errors may contain absolute or relative paths,
	// filenames with spaces, cache identifiers, or other operational details.
	return "內部操作失敗；詳細資訊請查看服務日誌。"
}

func (s *Service) publicScanDiagnostic(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, medialib.ErrDirectoryCapacity) {
		return fmt.Sprintf(
			"媒體庫目錄超過 %d 個項目的安全上限；詳細資訊請查看服務日誌。",
			medialib.MaxDirectoryEntries,
		)
	}
	if count := libraryScanIssueCount(err); count > 0 {
		return fmt.Sprintf(
			"媒體庫掃描發現 %d 個無效或不可播放項目；詳細資訊請查看服務日誌。",
			count,
		)
	}
	return "媒體庫掃描失敗；詳細資訊請查看服務日誌。"
}

func (s *Service) nowUTC() time.Time {
	if s.mediaClockNow != nil {
		return s.mediaClockNow()
	}
	if s.now != nil {
		return s.now()
	}
	return time.Now()
}

func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

func boolText(value bool) string {
	if value {
		return "已連線"
	}
	return "未連線"
}

func CleanFileName(name string) string {
	name = filepath.Base(strings.TrimSpace(name))
	if name == "." || name == "" {
		return "telegram-video.mp4"
	}
	return name
}
