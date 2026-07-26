package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	"github.com/tiwb/tg-obs-bot/internal/queue"
	"github.com/tiwb/tg-obs-bot/internal/secret"
	"github.com/tiwb/tg-obs-bot/internal/singleton"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

type Service struct {
	cfg          config.Config
	logger       *slog.Logger
	instanceLock *singleton.Lock
	store        *queue.Store
	libDB        *medialib.StateStore
	media        *media.Manager
	obs          obsController
	bot          telegramMessenger
	now          func() time.Time
	rng          *rand.Rand
	removeFile   func(string) error
	diskUsage    func(string) (media.DiskUsage, error)

	mu                    sync.Mutex
	playbackMu            sync.Mutex
	storageMu             sync.Mutex
	lastErr               string
	playback              playbackKind
	randomFallbackID      int64
	randomFallbackPath    string
	randomFallbackNotice  bool
	librarySnapshot       medialib.Library
	libraryScanErr        string
	activeLoopID          string
	activeLoopPath        string
	activeLoopTheme       string
	activeLoopPeriod      medialib.Period
	activeLoopEndsAt      time.Time
	activeMusicID         string
	activeMusicPath       string
	mediaProgressByInput  map[string]mediaProgress
	obsRecoveryInProgress atomic.Bool
	shutdown              []func() error
	workerStopGrace       time.Duration
	maintenanceInterval   time.Duration
	maintenanceFn         func(context.Context) error
}

type obsController interface {
	Connect(context.Context) error
	Close() error
	Events() <-chan obs.Event
	Probe(context.Context) error
	GetMediaInputStatus(context.Context, string) (obs.MediaInputStatus, error)
	GetInputSettings(context.Context, string) (obs.InputSettings, error)
	PlayFile(context.Context, string) error
	PlaySourceFile(context.Context, string, string, obs.PlaySourceOptions) error
	StopCurrent(context.Context) error
	StopSource(context.Context, string) error
	Status() obs.Status
}

type telegramMessenger interface {
	Run(context.Context) error
	SendMessage(context.Context, int64, string) error
}

type playbackKind string

const (
	playbackIdle   playbackKind = "idle"
	playbackNormal playbackKind = "normal_queue"
	playbackRandom playbackKind = "random_played"
	playbackFile   playbackKind = "fallback_file"

	playbackWatchdogInterval = 30 * time.Second
	playbackWatchdogGrace    = 60 * time.Second
	obsEndedEventSettleGrace = 2 * time.Second
	staleDownloadingAge      = 6 * time.Hour
	obsConnectAttemptTimeout = 15 * time.Second
	uploadProbeTimeout       = 2 * time.Minute
	uploadFailureTimeout     = 5 * time.Second
	retentionBatchSize       = 256
	mediaProgressGrace       = 2 * time.Minute
	mediaProgressHardGrace   = 2 * mediaProgressGrace
	mediaCursorEpsilonMillis = 1.0
	defaultWorkerStopGrace   = telegram.RecommendedParentDrainGrace
	defaultMaintenancePeriod = 10 * time.Minute
)

var (
	ErrRequiredWorkerStopped = errors.New("required service worker stopped unexpectedly")
	ErrWorkerShutdownStuck   = errors.New("service workers did not stop after cancellation")
)

type mediaProgress struct {
	Path                     string
	State                    obs.MediaState
	CursorMillis             float64
	HasCursor                bool
	GenerationStartedAt      time.Time
	LastProgressAt           time.Time
	LastDefinitiveProgressAt time.Time
}

type mediaInspection struct {
	Status       obs.MediaInputStatus
	PathMismatch bool
	Stalled      bool
	Settling     bool
}

type UploadRequest struct {
	LocalPath        string
	TelegramFileID   string
	TelegramUniqueID string
	SubmitterID      int64
	SubmitterName    string
	ChatID           int64
	MessageID        int
	FileName         string
	MimeType         string
	SizeBytes        int64
}

type publicError string

func (e publicError) Error() string {
	return string(e)
}

func (e publicError) PublicMessage() string {
	return string(e)
}

func New(cfg config.Config, logger *slog.Logger) (*Service, error) {
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

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}
	store, err := queue.Open(context.Background(), cfg.DatabasePath)
	if err != nil {
		return nil, err
	}
	manager, err := media.NewManager(cfg.MediaDir, cfg.FFProbePath)
	if err != nil {
		_ = store.Close()
		return nil, err
	}
	var libDB *medialib.StateStore
	if cfg.PlayerMode == "library" {
		if err := os.MkdirAll(cfg.LoopMediaDir, 0o755); err != nil {
			_ = store.Close()
			return nil, err
		}
		if err := os.MkdirAll(cfg.MusicMediaDir, 0o755); err != nil {
			_ = store.Close()
			return nil, err
		}
		libDB, err = medialib.OpenState(context.Background(), cfg.DatabasePath)
		if err != nil {
			_ = store.Close()
			return nil, err
		}
	}
	obsClient, err := obs.NewClient(obs.Options{
		URL:             cfg.OBSURL(),
		Password:        cfg.OBSPassword,
		MediaSourceName: cfg.OBSMediaSourceName,
		EventBuffer:     16,
		Logger:          logger.With("component", "obs"),
	})
	if err != nil {
		if libDB != nil {
			_ = libDB.Close()
		}
		_ = store.Close()
		return nil, err
	}

	service := &Service{
		cfg:                 cfg,
		logger:              logger,
		instanceLock:        instanceLock,
		store:               store,
		libDB:               libDB,
		media:               manager,
		obs:                 obsClient,
		now:                 time.Now,
		rng:                 rand.New(rand.NewSource(time.Now().UnixNano())),
		removeFile:          media.RemoveFile,
		diskUsage:           media.DiskUsageForPath,
		playback:            playbackIdle,
		workerStopGrace:     defaultWorkerStopGrace,
		maintenanceInterval: defaultMaintenancePeriod,
		shutdown:            []func() error{instanceLock.Close, obsClient.Close, store.Close},
	}
	releaseUnownedLock = false
	if libDB != nil {
		service.shutdown = append(service.shutdown, libDB.Close)
	}

	bot, err := telegram.New(telegram.Config{
		Token:              cfg.TelegramBotToken,
		APIBaseURL:         cfg.TelegramAPIBaseURL,
		AllowedChatID:      cfg.AllowedChatID,
		MaxUploadSizeBytes: cfg.MaxVideoSizeBytes,
		PlayerMode:         cfg.PlayerMode,
	}, service.telegramHooks(), logger.With("component", "telegram"), telegram.WithUpdateJournal(store))
	if err != nil {
		service.Close()
		return nil, err
	}
	service.bot = bot
	return service, nil
}

func (s *Service) Close() {
	for i := len(s.shutdown) - 1; i >= 0; i-- {
		if err := s.shutdown[i](); err != nil {
			s.logger.Warn("shutdown error", "error", s.redactError(err))
		}
	}
}

func (s *Service) Run(parentCtx context.Context) error {
	s.logger.Info("tg-obs-bot starting", "database", s.redactString(s.cfg.DatabasePath), "media_dir", s.redactString(s.cfg.MediaDir), "player_mode", s.cfg.PlayerMode)
	runCtx, cancel := context.WithCancel(parentCtx)

	if err := s.recoverStartupState(runCtx); err != nil {
		cancel()
		return err
	}

	if s.libraryMode() {
		if err := s.ScanLibrary(runCtx); err != nil {
			s.logger.Warn("initial media library scan found issues", "error", s.redactError(err))
		}
	}

	workers := []serviceWorker{
		{name: "telegram", run: s.bot.Run},
		{name: "obs-reconnect", run: s.obsReconnectLoop},
		{name: "obs-events", run: s.obsEventLoop},
		{name: "maintenance", run: s.maintenanceLoop},
	}
	if s.libraryMode() {
		workers = append(workers, serviceWorker{name: "library-scheduler", run: s.librarySchedulerLoop})
	} else {
		workers = append(workers, serviceWorker{name: "playback-watchdog", run: s.playbackWatchdogLoop})
	}

	workerResults := make(chan serviceWorkerResult, len(workers))
	pendingWorkers := make(map[string]struct{}, len(workers))
	for _, worker := range workers {
		pendingWorkers[worker.name] = struct{}{}
		go func(worker serviceWorker) {
			workerResults <- serviceWorkerResult{name: worker.name, err: worker.run(runCtx)}
		}(worker)
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
		}
	}

	cancel()
	return errors.Join(runErr, s.waitForWorkers(workerResults, pendingWorkers))
}

type serviceWorker struct {
	name string
	run  func(context.Context) error
}

type serviceWorkerResult struct {
	name string
	err  error
}

func requiredWorkerError(result serviceWorkerResult) error {
	stopped := fmt.Errorf("%w: %s", ErrRequiredWorkerStopped, result.name)
	if result.err == nil {
		return stopped
	}
	return errors.Join(stopped, fmt.Errorf("%s worker: %w", result.name, result.err))
}

func (s *Service) waitForWorkers(results <-chan serviceWorkerResult, pending map[string]struct{}) error {
	if len(pending) == 0 {
		return nil
	}
	grace := s.workerStopGrace
	if grace <= 0 {
		grace = defaultWorkerStopGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()

	var drainErr error
	for len(pending) > 0 {
		select {
		case result := <-results:
			delete(pending, result.name)
			if !onlyContextTermination(result.err) {
				drainErr = errors.Join(drainErr, requiredWorkerError(result))
			}
		case <-timer.C:
			names := make([]string, 0, len(pending))
			for name := range pending {
				names = append(names, name)
			}
			sort.Strings(names)
			return errors.Join(
				drainErr,
				fmt.Errorf(
					"%w: grace=%s pending=%s",
					ErrWorkerShutdownStuck,
					grace,
					strings.Join(names, ","),
				),
			)
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
	interval := s.maintenanceInterval
	if interval <= 0 {
		interval = defaultMaintenancePeriod
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			maintenance := s.maintenanceFn
			if maintenance == nil {
				maintenance = s.performMaintenance
			}
			if err := maintenance(ctx); err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return ctxErr
				}
				s.setLastErr(err)
				s.logger.Warn("periodic maintenance failed", "error", s.redactError(err))
			}
		}
	}
}

func (s *Service) EnqueueUpload(ctx context.Context, req UploadRequest) (queue.Video, error) {
	if strings.TrimSpace(req.LocalPath) == "" {
		err := errors.New("local video path is required")
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if !filepath.IsAbs(req.LocalPath) {
		err := fmt.Errorf("local video path must be absolute: %s", req.LocalPath)
		s.setLastErr(err)
		return queue.Video{}, err
	}
	s.storageMu.Lock()
	video, err := s.addDownloadingUpload(ctx, req)
	s.storageMu.Unlock()
	if err != nil {
		s.setLastErr(err)
		return queue.Video{}, err
	}

	probeCtx, cancelProbe := context.WithTimeout(ctx, uploadProbeTimeout)
	meta, err := s.media.Probe(probeCtx, req.LocalPath)
	cancelProbe()
	if err != nil {
		s.markUploadFailed(ctx, video.ID, err)
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if err := s.media.Validate(meta, s.cfg.MaxVideoSizeBytes, s.cfg.MaxVideoDurationSeconds); err != nil {
		s.markUploadFailed(ctx, video.ID, err)
		s.setLastErr(err)
		return queue.Video{}, err
	}

	ready, err := s.store.MarkReady(ctx, video.ID, req.LocalPath, meta.SizeBytes, meta.DurationSeconds)
	if err != nil {
		s.markUploadFailed(ctx, video.ID, err)
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if err := s.playIfIdle(ctx); err != nil {
		s.logger.Warn("play after enqueue failed", "error", s.redactError(err))
	}
	return ready, nil
}

func (s *Service) addDownloadingUpload(ctx context.Context, req UploadRequest) (queue.Video, error) {
	if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, req.LocalPath); err != nil {
		return queue.Video{}, err
	}
	info, err := os.Stat(req.LocalPath)
	if err != nil {
		return queue.Video{}, fmt.Errorf("stat local video path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return queue.Video{}, fmt.Errorf("local video path is not a regular file: %s", req.LocalPath)
	}
	actualSize := info.Size()
	if actualSize <= 0 {
		return queue.Video{}, publicError("影片檔案不可為空。")
	}
	if actualSize > s.cfg.MaxVideoSizeBytes {
		return queue.Video{}, publicError(fmt.Sprintf("檔案太大，上限是 %s", formatBytes(s.cfg.MaxVideoSizeBytes)))
	}
	if err := s.ensureStorageHeadroom(req.LocalPath, actualSize); err != nil {
		return queue.Video{}, err
	}
	length, err := s.store.QueueLength(ctx)
	if err != nil {
		return queue.Video{}, err
	}
	if length >= s.cfg.MaxQueueLength {
		return queue.Video{}, publicError(fmt.Sprintf("佇列已滿，目前上限是 %d 支", s.cfg.MaxQueueLength))
	}
	return s.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   req.TelegramFileID,
		TelegramUniqueID: req.TelegramUniqueID,
		SubmitterID:      req.SubmitterID,
		SubmitterName:    req.SubmitterName,
		ChatID:           req.ChatID,
		MessageID:        req.MessageID,
		FileName:         CleanFileName(req.FileName),
		LocalPath:        req.LocalPath,
		MimeType:         req.MimeType,
		SizeBytes:        actualSize,
	})
}

func (s *Service) advancePlayback(ctx context.Context) (*queue.Video, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()

	return s.advancePlaybackLocked(ctx)
}

func (s *Service) advancePlaybackLocked(ctx context.Context) (*queue.Video, error) {
	return s.advancePlaybackLockedAfter(ctx, 0, "")
}

func (s *Service) advancePlaybackLockedAfter(ctx context.Context, expectedCurrentID int64, expectedCurrentPath string) (*queue.Video, error) {
	if expectedCurrentID > 0 {
		current, err := s.store.Current(ctx)
		if err != nil {
			s.setLastErr(err)
			return nil, err
		}
		if current == nil || current.ID != expectedCurrentID {
			return nil, nil
		}
	}
	if expectedCurrentPath != "" {
		current, err := s.store.Current(ctx)
		if err != nil {
			s.setLastErr(err)
			return nil, err
		}
		if current != nil && current.LocalPath != expectedCurrentPath {
			return nil, nil
		}
		if current == nil && s.currentPlaybackPath() != expectedCurrentPath {
			return nil, nil
		}
	}
	if err := s.store.FinishCurrent(ctx); err != nil {
		s.setLastErr(err)
		return nil, err
	}

	for {
		video, err := s.store.NextReady(ctx)
		if err != nil {
			s.setLastErr(err)
			return nil, err
		}
		if video == nil {
			return nil, s.advanceFallbackLocked(ctx)
		}
		if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, video.LocalPath); err != nil {
			if _, markErr := s.store.FailReady(ctx, video.ID, err.Error()); markErr != nil {
				s.setLastErr(markErr)
				return nil, markErr
			}
			s.setLastErr(err)
			s.logger.Warn("skip invalid ready video path", "video_id", video.ID, "path", s.redactString(video.LocalPath), "error", s.redactError(err))
			continue
		}
		if err := s.obs.PlayFile(ctx, video.LocalPath); err != nil {
			s.setLastErr(err)
			return nil, err
		}
		playing, err := s.store.MarkPlaying(ctx, video.ID)
		if err != nil {
			_ = s.obs.StopCurrent(ctx)
			s.setLastErr(err)
			return nil, err
		}
		s.resetMediaProgressLocked(s.cfg.OBSMediaSourceName, video.LocalPath)
		s.setPlaybackState(playbackNormal, 0, "")
		return &playing, nil
	}
}

func (s *Service) playIfIdle(ctx context.Context) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	return s.playIfIdleLocked(ctx)
}

func (s *Service) playIfIdleLocked(ctx context.Context) error {
	current, err := s.store.Current(ctx)
	if err != nil {
		return err
	}
	if current != nil {
		return nil
	}
	if s.obs.Status().State != obs.StateConnected {
		return nil
	}
	if s.playbackState() != playbackIdle {
		return nil
	}
	_, err = s.advancePlaybackLocked(ctx)
	return err
}

func (s *Service) recoverPlaybackAfterOBSConnect(ctx context.Context) error {
	if s.libraryMode() {
		return s.recoverLibraryPlaybackAfterOBSConnect(ctx)
	}

	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()

	current, err := s.store.Current(ctx)
	if err != nil {
		return err
	}
	if current == nil {
		switch s.playbackState() {
		case playbackRandom, playbackFile:
			err := s.recoverFallbackPlaybackLocked(ctx)
			if err != nil {
				s.setPlaybackState(playbackIdle, 0, "")
				return err
			}
			return nil
		}
		s.setPlaybackState(playbackIdle, 0, "")
		return s.playIfIdleLocked(ctx)
	}
	if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, current.LocalPath); err != nil {
		recoveryErr := fmt.Errorf("current video #%d media path is invalid: %w", current.ID, err)
		if _, markErr := s.store.FailPlaying(ctx, current.ID, recoveryErr.Error()); markErr != nil {
			return markErr
		}
		s.setLastErr(recoveryErr)
		s.logger.Warn("mark invalid current video failed", "video_id", current.ID, "path", s.redactString(current.LocalPath), "error", s.redactError(err))
		s.setPlaybackState(playbackIdle, 0, "")
		return s.playIfIdleLocked(ctx)
	}
	if s.playbackState() == playbackNormal {
		err := s.recoverCurrentQueuePlaybackLocked(ctx, *current)
		if err != nil {
			s.setPlaybackState(playbackIdle, 0, "")
			return err
		}
		return nil
	}
	return s.restartCurrentQueuePlaybackLocked(ctx, *current)
}

// Reconnect recovery must not consume the queue. A terminal OBS state can
// belong to the connection that just failed, so replay the persisted current
// item unless the matching source is observably healthy and progressing.
func (s *Service) recoverCurrentQueuePlaybackLocked(ctx context.Context, current queue.Video) error {
	inspection, err := s.inspectMediaInputLocked(ctx, s.cfg.OBSMediaSourceName, current.LocalPath)
	if err != nil {
		return err
	}
	if !inspection.PathMismatch {
		switch inspection.Status.State {
		case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
			if !inspection.Stalled {
				return nil
			}
		case obs.MediaStateNone, obs.MediaStatePaused, obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		default:
			return fmt.Errorf("OBS media source %s returned unknown state %q", s.cfg.OBSMediaSourceName, inspection.Status.State)
		}
	}
	return s.restartCurrentQueuePlaybackLocked(ctx, current)
}

func (s *Service) recoverFallbackPlaybackLocked(ctx context.Context) error {
	kind := s.playbackState()
	if kind != playbackRandom && kind != playbackFile {
		return nil
	}
	expectedPath := s.currentPlaybackPath()
	if expectedPath == "" {
		return nil
	}
	inspection, err := s.inspectMediaInputLocked(ctx, s.cfg.OBSMediaSourceName, expectedPath)
	if err != nil {
		return err
	}
	if !inspection.PathMismatch {
		switch inspection.Status.State {
		case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
			if !inspection.Stalled {
				return nil
			}
		case obs.MediaStateNone, obs.MediaStatePaused, obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		default:
			return fmt.Errorf("OBS media source %s returned unknown state %q", s.cfg.OBSMediaSourceName, inspection.Status.State)
		}
	}
	return s.replayFallbackPlaybackLocked(ctx, kind, expectedPath)
}

func (s *Service) restartCurrentQueuePlaybackLocked(ctx context.Context, current queue.Video) error {
	if err := s.obs.PlayFile(ctx, current.LocalPath); err != nil {
		s.setPlaybackState(playbackIdle, 0, "")
		s.setLastErr(err)
		return err
	}
	if _, err := s.store.RestartPlaying(ctx, current.ID); err != nil {
		_ = s.obs.StopCurrent(ctx)
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		s.setPlaybackState(playbackIdle, 0, "")
		s.setLastErr(err)
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSMediaSourceName, current.LocalPath)
	s.setPlaybackState(playbackNormal, 0, "")
	s.logger.Info("recovered OBS playback", "video_id", current.ID, "path", s.redactString(current.LocalPath))
	return nil
}

func (s *Service) reconcileCurrentQueuePlaybackLocked(ctx context.Context, current queue.Video) (*queue.Video, bool, error) {
	inspection, err := s.inspectMediaInputLocked(ctx, s.cfg.OBSMediaSourceName, current.LocalPath)
	if err != nil {
		return nil, false, err
	}
	if inspection.PathMismatch {
		if inspection.Settling || s.queuePlaybackSettling(current) {
			return nil, false, nil
		}
		return nil, true, s.restartCurrentQueuePlaybackLocked(ctx, current)
	}
	switch inspection.Status.State {
	case obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		if inspection.Settling || s.queuePlaybackSettling(current) {
			return nil, false, nil
		}
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		next, err := s.advancePlaybackLockedAfter(ctx, current.ID, current.LocalPath)
		return next, true, err
	case obs.MediaStateNone, obs.MediaStatePaused:
		return nil, true, s.restartCurrentQueuePlaybackLocked(ctx, current)
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
		if inspection.Stalled {
			return nil, true, s.restartCurrentQueuePlaybackLocked(ctx, current)
		}
		return nil, false, nil
	default:
		return nil, false, fmt.Errorf("OBS media source %s returned unknown state %q", s.cfg.OBSMediaSourceName, inspection.Status.State)
	}
}

func (s *Service) queuePlaybackSettling(current queue.Video) bool {
	if current.StartedAt == nil {
		return true
	}
	return s.nowUTC().Before(current.StartedAt.Add(obsEndedEventSettleGrace))
}

func (s *Service) reconcileFallbackPlaybackLocked(ctx context.Context) (*queue.Video, bool, error) {
	kind := s.playbackState()
	if kind != playbackRandom && kind != playbackFile {
		return nil, false, nil
	}
	expectedPath := s.currentPlaybackPath()
	if expectedPath == "" {
		return nil, false, nil
	}
	inspection, err := s.inspectMediaInputLocked(ctx, s.cfg.OBSMediaSourceName, expectedPath)
	if err != nil {
		return nil, false, err
	}
	if inspection.PathMismatch {
		if inspection.Settling {
			return nil, false, nil
		}
		return nil, true, s.replayFallbackPlaybackLocked(ctx, kind, expectedPath)
	}
	switch inspection.Status.State {
	case obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		if inspection.Settling {
			return nil, false, nil
		}
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		s.setPlaybackState(playbackIdle, 0, "")
		next, err := s.advancePlaybackLocked(ctx)
		return next, true, err
	case obs.MediaStateNone, obs.MediaStatePaused:
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
		if !inspection.PathMismatch && !inspection.Stalled {
			return nil, false, nil
		}
	default:
		return nil, false, fmt.Errorf("OBS media source %s returned unknown state %q", s.cfg.OBSMediaSourceName, inspection.Status.State)
	}

	return nil, true, s.replayFallbackPlaybackLocked(ctx, kind, expectedPath)
}

func (s *Service) replayFallbackPlaybackLocked(ctx context.Context, kind playbackKind, expectedPath string) error {
	if err := s.obs.PlayFile(ctx, expectedPath); err != nil {
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSMediaSourceName, expectedPath)
	var randomID int64
	if kind == playbackRandom {
		randomID, _ = s.randomFallbackLock()
	}
	s.setPlaybackState(kind, randomID, expectedPath)
	return nil
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
	return inspection, nil
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
	return s.nowUTC().Before(progress.GenerationStartedAt.Add(obsEndedEventSettleGrace))
}

func (s *Service) clearMediaProgressLocked(inputName string) {
	delete(s.mediaProgressByInput, inputName)
}

func (s *Service) skipCurrent(ctx context.Context) (string, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()

	next, err := s.store.NextReady(ctx)
	if err != nil {
		return "", err
	}
	if next == nil {
		if s.obs.Status().State == obs.StateConnected {
			if err := s.obs.StopCurrent(ctx); err != nil {
				s.setLastErr(err)
				return "", err
			}
		}
		if err := s.store.FinishCurrent(ctx); err != nil {
			s.setLastErr(err)
			return "", err
		}
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		s.setPlaybackState(playbackIdle, 0, "")
		return "已跳過，目前沒有下一支影片。", nil
	}
	video, err := s.advancePlaybackLocked(ctx)
	if err != nil {
		return "", err
	}
	if video == nil {
		return "已跳過，目前沒有下一支影片。", nil
	}
	return fmt.Sprintf("已跳到下一支：#%d %s", video.ID, video.FileName), nil
}

func (s *Service) advanceFallbackLocked(ctx context.Context) error {
	switch s.cfg.FallbackMode {
	case "off":
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		s.setPlaybackState(playbackIdle, 0, "")
		return nil
	case "file":
		return s.playFallbackFileLocked(ctx)
	case "random_played":
		if video, err := s.playRandomFallbackLocked(ctx); err != nil {
			return err
		} else if video != nil {
			return nil
		}
		return s.playFallbackFileLocked(ctx)
	default:
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		s.setPlaybackState(playbackIdle, 0, "")
		return nil
	}
}

func (s *Service) playFallbackFileLocked(ctx context.Context) error {
	if s.cfg.OBSFallbackFile == "" {
		s.clearMediaProgressLocked(s.cfg.OBSMediaSourceName)
		s.setPlaybackState(playbackIdle, 0, "")
		return nil
	}
	if err := s.obs.PlayFile(ctx, s.cfg.OBSFallbackFile); err != nil {
		s.setLastErr(err)
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSMediaSourceName, s.cfg.OBSFallbackFile)
	s.setPlaybackState(playbackFile, 0, s.cfg.OBSFallbackFile)
	return nil
}

func (s *Service) playRandomFallbackLocked(ctx context.Context) (*queue.Video, error) {
	candidates, err := s.store.PlayedFallbackCandidates(ctx, queue.MaxFallbackCandidates)
	if err != nil {
		s.setLastErr(err)
		return nil, err
	}
	order := rand.Perm(len(candidates))
	if s.rng != nil {
		order = s.rng.Perm(len(candidates))
	}
	for _, idx := range order {
		video := candidates[idx]
		if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, video.LocalPath); err != nil {
			if _, markErr := s.store.QuarantinePlayed(ctx, video.ID, err.Error()); markErr != nil {
				s.setLastErr(markErr)
				return nil, markErr
			}
			s.logger.Warn("skip invalid random fallback file", "video_id", video.ID, "path", s.redactString(video.LocalPath), "error", s.redactError(err))
			continue
		}
		if err := s.obs.PlayFile(ctx, video.LocalPath); err != nil {
			s.setLastErr(err)
			return nil, err
		}
		s.resetMediaProgressLocked(s.cfg.OBSMediaSourceName, video.LocalPath)
		notify := s.setPlaybackState(playbackRandom, video.ID, video.LocalPath)
		if notify {
			_ = s.bot.SendMessage(ctx, s.cfg.AllowedChatID, fmt.Sprintf("佇列已播放完，正在隨機播放歷史影片：#%d %s", video.ID, video.FileName))
		}
		return &video, nil
	}
	return nil, nil
}

func (s *Service) RemoveQueued(ctx context.Context, id int64) error {
	if err := s.store.Cancel(ctx, id); err != nil {
		s.setLastErr(err)
		return err
	}
	return nil
}

func (s *Service) MoveQueued(ctx context.Context, id int64, position int) error {
	if err := s.store.Move(ctx, id, position); err != nil {
		s.setLastErr(err)
		return err
	}
	return nil
}

func (s *Service) QueueText(ctx context.Context) (string, error) {
	videos, err := s.store.ListQueue(ctx, 20)
	if err != nil {
		return "", err
	}
	if len(videos) == 0 {
		return "佇列目前是空的。", nil
	}
	lines := []string{"目前佇列："}
	for _, v := range videos {
		label := fmt.Sprintf("#%d", v.ID)
		if v.Status == queue.StatusPlaying {
			label += " [播放中]"
		} else {
			label += fmt.Sprintf(" [第 %d 位]", v.QueuePosition)
		}
		lines = append(lines, fmt.Sprintf("%s %s %s", label, v.FileName, formatDuration(v.DurationSeconds)))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) NowText(ctx context.Context) (string, error) {
	if s.libraryMode() {
		return s.LibraryNowText(ctx)
	}
	video, err := s.store.Current(ctx)
	if err != nil {
		return "", err
	}
	if video == nil {
		return "目前沒有正在播放的影片。", nil
	}
	return fmt.Sprintf("正在播放：#%d %s %s", video.ID, video.FileName, formatDuration(video.DurationSeconds)), nil
}

func (s *Service) HistoryText(ctx context.Context) (string, error) {
	videos, err := s.store.History(ctx, 10)
	if err != nil {
		return "", err
	}
	if len(videos) == 0 {
		return "尚無歷史紀錄。", nil
	}
	lines := []string{"最近紀錄："}
	for _, v := range videos {
		lines = append(lines, fmt.Sprintf("#%d [%s] %s", v.ID, v.Status, v.FileName))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) StatusText(ctx context.Context, obsConnected bool) (string, error) {
	if s.libraryMode() {
		return s.LibraryStatusText(ctx, obsConnected)
	}
	stats, err := s.store.Stats(ctx)
	if err != nil {
		return "", err
	}
	diskText := "未知"
	if usage, err := s.media.DiskUsage(); err == nil {
		diskText = fmt.Sprintf("%s free / %s total", formatBytes(int64(usage.AvailableBytes)), formatBytes(int64(usage.TotalBytes)))
	}
	botAPIDiskText := "未知"
	if usage, err := media.DiskUsageForPath(s.cfg.TelegramBotAPIDir); err == nil {
		botAPIDiskText = fmt.Sprintf("%s free / %s total", formatBytes(int64(usage.AvailableBytes)), formatBytes(int64(usage.TotalBytes)))
	}
	lastErr := s.lastError()
	if lastErr == "" {
		lastErr = "無"
	}
	return fmt.Sprintf(
		"狀態：\nOBS：%s\nReady：%d\nDownloading：%d\nPlayed：%d\nFailed：%d\nFallback：%s (%s)\nMedia DB：%s\nMedia Disk：%s\nBot API Disk：%s\nLast error：%s",
		boolText(obsConnected),
		stats.ReadyCount,
		stats.DownloadingCount,
		stats.PlayedCount,
		stats.FailedCount,
		s.cfg.FallbackMode,
		s.playbackState(),
		formatBytes(stats.TotalBytes),
		diskText,
		botAPIDiskText,
		lastErr,
	), nil
}

func (s *Service) telegramHooks() telegram.Hooks {
	return telegram.Hooks{
		EnqueueUpload: func(ctx context.Context, upload telegram.Upload) (string, error) {
			if s.libraryMode() {
				return s.ImportLibraryUpload(ctx, UploadRequest{
					LocalPath:        upload.LocalPath,
					TelegramFileID:   upload.FileID,
					TelegramUniqueID: upload.FileUniqueID,
					SubmitterID:      upload.SubmitterID,
					SubmitterName:    upload.SubmitterName,
					ChatID:           upload.ChatID,
					MessageID:        upload.MessageID,
					FileName:         upload.FileName,
					MimeType:         upload.MimeType,
					SizeBytes:        upload.SizeBytes,
				})
			}
			video, err := s.EnqueueUpload(ctx, UploadRequest{
				LocalPath:        upload.LocalPath,
				TelegramFileID:   upload.FileID,
				TelegramUniqueID: upload.FileUniqueID,
				SubmitterID:      upload.SubmitterID,
				SubmitterName:    upload.SubmitterName,
				ChatID:           upload.ChatID,
				MessageID:        upload.MessageID,
				FileName:         upload.FileName,
				MimeType:         upload.MimeType,
				SizeBytes:        upload.SizeBytes,
			})
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("已加入佇列：#%d %s，第 %d 位。", video.ID, video.FileName, video.QueuePosition), nil
		},
		Library:    s.LibraryText,
		Scan:       s.ScanLibraryText,
		Preview:    s.PreviewText,
		SetTheme:   s.SetThemeText,
		SelectLoop: s.SelectLoopText,
		SkipLoop:   s.SkipLoopText,
		SkipMusic:  s.SkipMusicText,
		ListQueue: func(ctx context.Context) (string, error) {
			if s.libraryMode() {
				return s.LibraryText(ctx)
			}
			return s.QueueText(ctx)
		},
		Now: s.NowText,
		History: func(ctx context.Context) (string, error) {
			if s.libraryMode() {
				return "Library mode 沒有 queue history；請使用 /library 查看素材。", nil
			}
			return s.HistoryText(ctx)
		},
		Status: func(ctx context.Context) (string, error) {
			return s.StatusText(ctx, s.obs.Status().State == obs.StateConnected)
		},
		Remove: func(ctx context.Context, id int64) (string, error) {
			if err := s.RemoveQueued(ctx, id); err != nil {
				return "", err
			}
			return fmt.Sprintf("已取消佇列影片 #%d。", id), nil
		},
		Move: func(ctx context.Context, id int64, position int) (string, error) {
			if err := s.MoveQueued(ctx, id, position); err != nil {
				return "", err
			}
			return fmt.Sprintf("已將 #%d 移到第 %d 位。", id, position), nil
		},
		Skip: func(ctx context.Context) (string, error) {
			if s.libraryMode() {
				return s.SkipLoopText(ctx)
			}
			return s.skipCurrent(ctx)
		},
	}
}

func (s *Service) obsReconnectLoop(ctx context.Context) error {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		s.maintainOBSConnection(ctx)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) maintainOBSConnection(ctx context.Context) {
	switch s.obs.Status().State {
	case obs.StateDisconnected:
		if !s.obsRecoveryInProgress.CompareAndSwap(false, true) {
			return
		}
		defer s.obsRecoveryInProgress.Store(false)

		connectCtx, cancelConnect := context.WithTimeout(ctx, obsConnectAttemptTimeout)
		err := s.obs.Connect(connectCtx)
		cancelConnect()
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setLastErr(err)
			s.logger.Warn("connect OBS failed", "error", s.redactError(err))
			return
		}
		s.logger.Info("connected to OBS")
		if err := s.recoverPlaybackAfterOBSConnect(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setLastErr(err)
			s.logger.Warn("resume playback failed", "error", s.redactError(err))
		}
	case obs.StateConnected:
		if s.obsRecoveryInProgress.Load() {
			return
		}
		if err := s.obs.Probe(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.setLastErr(err)
			s.logger.Warn("probe OBS failed", "error", s.redactError(err))
			return
		}
		if s.playbackState() == playbackIdle {
			if !s.obsRecoveryInProgress.CompareAndSwap(false, true) {
				return
			}
			defer s.obsRecoveryInProgress.Store(false)
			if s.obs.Status().State != obs.StateConnected || s.playbackState() != playbackIdle {
				return
			}
			if err := s.recoverPlaybackAfterOBSConnect(ctx); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.setLastErr(err)
				s.logger.Warn("resume playback failed", "error", s.redactError(err))
			}
		}
	}
}

func (s *Service) playbackWatchdogLoop(ctx context.Context) error {
	ticker := time.NewTicker(playbackWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if err := s.checkPlaybackWatchdog(ctx); err != nil {
				s.setLastErr(err)
				s.logger.Warn("playback watchdog failed", "error", s.redactError(err))
			}
		}
	}
}

func (s *Service) checkPlaybackWatchdog(ctx context.Context) error {
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return nil
	}

	var video *queue.Video
	if err := func() error {
		s.playbackMu.Lock()
		defer s.playbackMu.Unlock()

		if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
			return nil
		}
		current, err := s.store.Current(ctx)
		if err != nil {
			return err
		}
		if current == nil {
			video, _, err = s.reconcileFallbackPlaybackLocked(ctx)
			return err
		}
		if s.playbackState() != playbackNormal {
			return s.restartCurrentQueuePlaybackLocked(ctx, *current)
		}

		var changed bool
		video, changed, err = s.reconcileCurrentQueuePlaybackLocked(ctx, *current)
		if err != nil || changed {
			return err
		}
		if current.StartedAt == nil || current.DurationSeconds <= 0 {
			return nil
		}
		deadline := current.StartedAt.Add(time.Duration(current.DurationSeconds)*time.Second + playbackWatchdogGrace)
		if s.nowUTC().Before(deadline) {
			return nil
		}

		s.logger.Warn("playback exceeded expected duration; advancing without OBS ended event",
			"video_id", current.ID,
			"started_at", current.StartedAt,
			"duration_seconds", current.DurationSeconds,
			"deadline", deadline,
		)
		var advanceErr error
		video, advanceErr = s.advancePlaybackLockedAfter(ctx, current.ID, current.LocalPath)
		return advanceErr
	}(); err != nil {
		return err
	}
	if video != nil {
		_ = s.bot.SendMessage(ctx, s.cfg.AllowedChatID, fmt.Sprintf("開始播放：#%d %s", video.ID, video.FileName))
	}
	return nil
}

func (s *Service) obsEventLoop(ctx context.Context) error {
	events := s.obs.Events()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case event, ok := <-events:
			if !ok {
				if err := ctx.Err(); err != nil {
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
			if s.libraryMode() {
				if err := s.handleLibraryOBSEvent(ctx, event); err != nil {
					s.logger.Warn("advance library playback after OBS event failed", "error", s.redactError(err))
				}
				continue
			}
			video, err := s.advancePlaybackForEndedEvent(ctx, event)
			if err != nil {
				s.logger.Warn("advance playback after OBS event failed", "error", s.redactError(err))
				continue
			}
			if video != nil {
				_ = s.bot.SendMessage(ctx, s.cfg.AllowedChatID, fmt.Sprintf("開始播放：#%d %s", video.ID, video.FileName))
			}
		}
	}
}

func (s *Service) advancePlaybackForEndedEvent(ctx context.Context, event obs.Event) (*queue.Video, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()

	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return nil, nil
	}
	// MediaEnded is only a reconciliation hint. OBS status and input identity,
	// read under the same playback lock as queue mutation, are authoritative.
	current, err := s.store.Current(ctx)
	if err != nil {
		return nil, err
	}
	if current != nil {
		if event.Path != "" && !sameMediaPath(event.Path, current.LocalPath) {
			return nil, nil
		}
		if s.queueEndedEventIsTooEarly(event, *current) {
			s.logger.Warn("ignore OBS ended event inside current playback guard window",
				"video_id", current.ID,
				"event_path", s.redactString(event.Path),
				"event_at", event.At,
				"started_at", current.StartedAt,
				"duration_seconds", current.DurationSeconds,
			)
			return nil, nil
		}
		video, _, err := s.reconcileCurrentQueuePlaybackLocked(ctx, *current)
		return video, err
	}
	video, _, err := s.reconcileFallbackPlaybackLocked(ctx)
	return video, err
}

func (s *Service) queueEndedEventIsTooEarly(event obs.Event, current queue.Video) bool {
	if current.StartedAt == nil {
		return true
	}
	trustedAfter := current.StartedAt.Add(obsEndedEventSettleGrace)
	if current.DurationSeconds > 0 {
		duration := time.Duration(current.DurationSeconds) * time.Second
		tolerance := obsEndedEventSettleGrace
		if duration <= tolerance {
			tolerance = duration / 2
		}
		expectedEndGuard := current.StartedAt.Add(duration - tolerance)
		if expectedEndGuard.After(trustedAfter) {
			trustedAfter = expectedEndGuard
		}
	}
	eventAt := event.At
	if eventAt.IsZero() {
		eventAt = s.nowUTC()
	}
	return eventAt.Before(trustedAfter)
}

func (s *Service) recoverStartupState(ctx context.Context) error {
	staleErr := s.failStaleDownloading(ctx, "startup recovery: stale downloading item")
	tempErr := s.sweepStaleLibraryImportTemps(ctx)
	return errors.Join(staleErr, tempErr)
}

func (s *Service) performMaintenance(ctx context.Context) error {
	staleErr := s.failStaleDownloading(ctx, "periodic recovery: stale downloading item")
	retentionErr := s.CleanupRetention(ctx)
	_, _, journalErr := s.store.PruneTelegramUpdateJournal(ctx)
	tempErr := s.sweepStaleLibraryImportTemps(ctx)
	return errors.Join(staleErr, retentionErr, journalErr, tempErr)
}

func (s *Service) failStaleDownloading(ctx context.Context, cause string) error {
	count, err := s.store.FailStaleDownloading(ctx, staleDownloadingAge, cause)
	if err != nil {
		return err
	}
	if count > 0 {
		s.logger.Warn("marked stale downloading queue items failed", "count", count)
	}
	return nil
}

func (s *Service) CleanupRetention(ctx context.Context) error {
	maxAge := s.cfg.RetentionMaxAge()
	maxFiles := s.cfg.RetentionMaxFiles
	if maxAge <= 0 && maxFiles <= 0 {
		return nil
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	s.storageMu.Lock()
	defer s.storageMu.Unlock()

	fallbackID, fallbackPath := s.randomFallbackLock()
	playedCount, err := s.store.TerminalCount(ctx, queue.StatusPlayed)
	if err != nil {
		return err
	}
	failedCanceledCount, err := s.store.TerminalCount(ctx, queue.StatusFailed, queue.StatusCanceled)
	if err != nil {
		return err
	}
	var cutoff time.Time
	if maxAge > 0 {
		cutoff = s.nowUTC().Add(-maxAge)
	}

	remaining := retentionBatchSize
	scanned, deleted, err := s.cleanupTerminalGroup(
		ctx,
		[]queue.Status{queue.StatusPlayed},
		playedCount,
		maxFiles,
		cutoff,
		fallbackID,
		fallbackPath,
		remaining/2,
	)
	if err != nil {
		return err
	}
	remaining -= scanned
	playedCount -= deleted
	scanned, _, err = s.cleanupTerminalGroup(
		ctx,
		[]queue.Status{queue.StatusFailed, queue.StatusCanceled},
		failedCanceledCount,
		maxFiles,
		cutoff,
		0,
		"",
		remaining,
	)
	if err != nil {
		return err
	}
	remaining -= scanned
	if remaining == 0 {
		return nil
	}
	_, _, err = s.cleanupTerminalGroup(
		ctx,
		[]queue.Status{queue.StatusPlayed},
		playedCount,
		maxFiles,
		cutoff,
		fallbackID,
		fallbackPath,
		remaining,
	)
	return err
}

func (s *Service) cleanupTerminalGroup(
	ctx context.Context,
	statuses []queue.Status,
	count int,
	maxFiles int,
	cutoff time.Time,
	protectedID int64,
	protectedPath string,
	limit int,
) (int, int, error) {
	if limit <= 0 || (cutoff.IsZero() && (maxFiles <= 0 || count <= maxFiles)) {
		return 0, 0, nil
	}
	videos, err := s.store.OldestTerminal(ctx, statuses, limit)
	if err != nil {
		return 0, 0, err
	}
	deletedCount := 0
	for _, video := range videos {
		expired := !cutoff.IsZero() && terminalTime(video).Before(cutoff)
		excess := maxFiles > 0 && count > maxFiles
		if !expired && !excess {
			continue
		}
		if video.ID == protectedID {
			continue
		}

		deleteLocalFile := false
		if s.cfg.RetentionDeleteLocalFiles && (protectedPath == "" || video.LocalPath != protectedPath) {
			referenced, err := s.store.LocalPathReferenced(ctx, video.LocalPath, video.ID)
			if err != nil {
				return 0, 0, err
			}
			deleteLocalFile = !referenced
		}
		if deleteLocalFile {
			if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, video.LocalPath); err != nil {
				if !errors.Is(err, os.ErrNotExist) {
					s.logger.Warn("skip retention local file delete", "video_id", video.ID, "path", s.redactString(video.LocalPath), "error", s.redactError(err))
				}
			} else {
				removeFile := s.removeFile
				if removeFile == nil {
					removeFile = media.RemoveFile
				}
				if err := removeFile(video.LocalPath); err != nil {
					return 0, 0, err
				}
			}
		}
		deleted, err := s.store.DeleteTerminal(ctx, video.ID, video.Status)
		if err != nil {
			return 0, 0, err
		}
		if !deleted {
			continue
		}
		count--
		deletedCount++
	}
	return len(videos), deletedCount, nil
}

func terminalTime(video queue.Video) time.Time {
	if video.FinishedAt != nil {
		return *video.FinishedAt
	}
	return video.UpdatedAt
}

func (s *Service) markUploadFailed(ctx context.Context, id int64, cause error) {
	failCtx, cancel := context.WithTimeout(context.Background(), uploadFailureTimeout)
	defer cancel()
	changed, err := s.store.MarkFailed(failCtx, id, cause.Error())
	if err != nil {
		s.logger.Warn("mark upload failed", "video_id", id, "error", s.redactError(err))
		return
	}
	if !changed && ctx.Err() == nil {
		s.logger.Debug("upload failure state already changed", "video_id", id)
	}
}

func validateLocalBotAPIPath(root string, path string) error {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("resolve TELEGRAM_BOT_API_DIR: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return fmt.Errorf("resolve TELEGRAM_BOT_API_DIR: %w", err)
	}
	resolvedPath, err := filepath.EvalSymlinks(path)
	if err != nil {
		return fmt.Errorf("resolve local video path: %w", err)
	}
	info, err := os.Stat(resolvedPath)
	if err != nil {
		return fmt.Errorf("stat local video path: %w", err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("local video path is not a regular file: %s", path)
	}
	rel, err := filepath.Rel(resolvedRoot, resolvedPath)
	if err != nil {
		return fmt.Errorf("compare local video path with TELEGRAM_BOT_API_DIR: %w", err)
	}
	if rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("local video path is outside TELEGRAM_BOT_API_DIR: %s", path)
	}
	return nil
}

func (s *Service) setLastErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = secret.RedactString(err.Error(), s.cfg.SensitiveValues()...)
}

func (s *Service) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *Service) setPlaybackState(kind playbackKind, randomID int64, path string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	notify := kind == playbackRandom && !s.randomFallbackNotice
	s.playback = kind
	if kind == playbackRandom {
		s.randomFallbackID = randomID
		s.randomFallbackPath = path
		s.randomFallbackNotice = true
	} else if kind == playbackFile {
		s.randomFallbackID = 0
		s.randomFallbackPath = path
		s.randomFallbackNotice = false
	} else {
		s.randomFallbackID = 0
		s.randomFallbackPath = ""
		s.randomFallbackNotice = false
	}
	return notify
}

func (s *Service) playbackState() playbackKind {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.playback
}

func (s *Service) currentPlaybackPath() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.playback {
	case playbackRandom, playbackFile:
		return s.randomFallbackPath
	default:
		return ""
	}
}

func (s *Service) randomFallbackLock() (int64, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.playback != playbackRandom {
		return 0, ""
	}
	return s.randomFallbackID, s.randomFallbackPath
}

func (s *Service) redactError(err error) error {
	return secret.RedactError(err, s.cfg.SensitiveValues()...)
}

func (s *Service) redactString(value string) string {
	return secret.RedactString(value, s.cfg.SensitiveValues()...)
}

func (s *Service) nowUTC() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func formatDuration(seconds int) string {
	if seconds <= 0 {
		return ""
	}
	return fmt.Sprintf("(%02d:%02d)", seconds/60, seconds%60)
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
