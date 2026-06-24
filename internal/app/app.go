package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	"github.com/tiwb/tg-obs-bot/internal/queue"
	"github.com/tiwb/tg-obs-bot/internal/secret"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

type Service struct {
	cfg    config.Config
	logger *slog.Logger
	store  *queue.Store
	libDB  *medialib.StateStore
	media  *media.Manager
	obs    obsController
	bot    telegramMessenger
	now    func() time.Time
	rng    *rand.Rand

	mu                   sync.Mutex
	playbackMu           sync.Mutex
	lastErr              string
	playback             playbackKind
	randomFallbackID     int64
	randomFallbackPath   string
	randomFallbackNotice bool
	librarySnapshot      medialib.Library
	libraryScanErr       string
	activeLoopID         string
	activeLoopPath       string
	activeLoopTheme      string
	activeLoopPeriod     medialib.Period
	activeLoopEndsAt     time.Time
	activeMusicID        string
	activeMusicPath      string
	shutdown             []func() error
}

type obsController interface {
	Connect(context.Context) error
	Close() error
	Events() <-chan obs.Event
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
	obsEndedEarlyTolerance   = 2 * time.Second
	staleDownloadingAge      = 6 * time.Hour
	obsConnectAttemptTimeout = 15 * time.Second
	uploadProbeTimeout       = 2 * time.Minute
	uploadFailureTimeout     = 5 * time.Second
)

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
		cfg:      cfg,
		logger:   logger,
		store:    store,
		libDB:    libDB,
		media:    manager,
		obs:      obsClient,
		now:      time.Now,
		rng:      rand.New(rand.NewSource(time.Now().UnixNano())),
		playback: playbackIdle,
		shutdown: []func() error{obsClient.Close, store.Close},
	}
	if libDB != nil {
		service.shutdown = append(service.shutdown, libDB.Close)
	}

	bot, err := telegram.New(telegram.Config{
		Token:              cfg.TelegramBotToken,
		APIBaseURL:         cfg.TelegramAPIBaseURL,
		AllowedChatID:      cfg.AllowedChatID,
		MaxUploadSizeBytes: cfg.MaxVideoSizeBytes,
		PlayerMode:         cfg.PlayerMode,
	}, service.telegramHooks(), logger.With("component", "telegram"))
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

func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("tg-obs-bot starting", "database", s.redactString(s.cfg.DatabasePath), "media_dir", s.redactString(s.cfg.MediaDir), "player_mode", s.cfg.PlayerMode)
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()

	if err := s.recoverStartupState(ctx); err != nil {
		return err
	}

	errCh := make(chan error, 1)
	wg.Add(1)
	go func() {
		defer wg.Done()
		err := s.bot.Run(ctx)
		select {
		case errCh <- err:
		case <-ctx.Done():
		}
	}()
	startLoop := func(loop func(context.Context)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			loop(ctx)
		}()
	}
	startLoop(s.obsReconnectLoop)
	startLoop(s.obsEventLoop)
	if s.libraryMode() {
		if err := s.ScanLibrary(ctx); err != nil {
			s.logger.Warn("initial media library scan found issues", "error", s.redactError(err))
		}
		startLoop(s.librarySchedulerLoop)
	} else {
		startLoop(s.playbackWatchdogLoop)
	}

	cleanupTicker := time.NewTicker(10 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err == nil {
				return errors.New("telegram service stopped unexpectedly")
			}
			cancel()
			return err
		case <-cleanupTicker.C:
			if s.libraryMode() {
				continue
			}
			if err := s.CleanupRetention(ctx); err != nil {
				s.setLastErr(err)
				s.logger.Warn("retention cleanup failed", "error", s.redactError(err))
			}
		}
	}
}

func (s *Service) EnqueueUpload(ctx context.Context, req UploadRequest) (queue.Video, error) {
	if req.SizeBytes > s.cfg.MaxVideoSizeBytes {
		err := publicError(fmt.Sprintf("檔案太大，上限是 %s", formatBytes(s.cfg.MaxVideoSizeBytes)))
		s.setLastErr(err)
		return queue.Video{}, err
	}
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
	if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, req.LocalPath); err != nil {
		s.setLastErr(err)
		return queue.Video{}, err
	}

	length, err := s.store.QueueLength(ctx)
	if err != nil {
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if length >= s.cfg.MaxQueueLength {
		err := publicError(fmt.Sprintf("佇列已滿，目前上限是 %d 支", s.cfg.MaxQueueLength))
		s.setLastErr(err)
		return queue.Video{}, err
	}

	fileName := CleanFileName(req.FileName)
	video, err := s.store.AddDownloading(ctx, queue.Video{
		TelegramFileID:   req.TelegramFileID,
		TelegramUniqueID: req.TelegramUniqueID,
		SubmitterID:      req.SubmitterID,
		SubmitterName:    req.SubmitterName,
		ChatID:           req.ChatID,
		MessageID:        req.MessageID,
		FileName:         fileName,
		MimeType:         req.MimeType,
		SizeBytes:        req.SizeBytes,
	})
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
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if err := s.playIfIdle(ctx); err != nil {
		s.logger.Warn("play after enqueue failed", "error", s.redactError(err))
	}
	return ready, nil
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
			if markErr := s.store.MarkFailed(ctx, video.ID, err.Error()); markErr != nil {
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
		s.setPlaybackState(playbackIdle, 0, "")
		return s.playIfIdleLocked(ctx)
	}
	if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, current.LocalPath); err != nil {
		recoveryErr := fmt.Errorf("current video #%d media path is invalid: %w", current.ID, err)
		if markErr := s.store.MarkFailed(ctx, current.ID, recoveryErr.Error()); markErr != nil {
			return markErr
		}
		s.setLastErr(recoveryErr)
		s.logger.Warn("mark invalid current video failed", "video_id", current.ID, "path", s.redactString(current.LocalPath), "error", s.redactError(err))
		s.setPlaybackState(playbackIdle, 0, "")
		return s.playIfIdleLocked(ctx)
	}
	if err := s.obs.PlayFile(ctx, current.LocalPath); err != nil {
		s.setPlaybackState(playbackIdle, 0, "")
		s.setLastErr(err)
		return err
	}
	if _, err := s.store.RestartPlaying(ctx, current.ID); err != nil {
		s.setLastErr(err)
		return err
	}
	s.setPlaybackState(playbackNormal, 0, "")
	s.logger.Info("recovered OBS playback", "video_id", current.ID, "path", s.redactString(current.LocalPath))
	return nil
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
		s.setPlaybackState(playbackIdle, 0, "")
		return nil
	}
}

func (s *Service) playFallbackFileLocked(ctx context.Context) error {
	if s.cfg.OBSFallbackFile == "" {
		s.setPlaybackState(playbackIdle, 0, "")
		return nil
	}
	if err := s.obs.PlayFile(ctx, s.cfg.OBSFallbackFile); err != nil {
		s.setLastErr(err)
		return err
	}
	s.setPlaybackState(playbackFile, 0, s.cfg.OBSFallbackFile)
	return nil
}

func (s *Service) playRandomFallbackLocked(ctx context.Context) (*queue.Video, error) {
	candidates, err := s.store.PlayedFallbackCandidates(ctx, 0)
	if err != nil {
		s.setLastErr(err)
		return nil, err
	}
	for len(candidates) > 0 {
		idx := rand.Intn(len(candidates))
		video := candidates[idx]
		candidates = append(candidates[:idx], candidates[idx+1:]...)
		if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, video.LocalPath); err != nil {
			s.logger.Warn("skip invalid random fallback file", "video_id", video.ID, "path", s.redactString(video.LocalPath), "error", s.redactError(err))
			continue
		}
		if err := s.obs.PlayFile(ctx, video.LocalPath); err != nil {
			s.setLastErr(err)
			return nil, err
		}
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

func (s *Service) obsReconnectLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		switch s.obs.Status().State {
		case obs.StateDisconnected:
			connectCtx, cancelConnect := context.WithTimeout(ctx, obsConnectAttemptTimeout)
			err := s.obs.Connect(connectCtx)
			cancelConnect()
			if err != nil {
				s.setLastErr(err)
				s.logger.Warn("connect OBS failed", "error", s.redactError(err))
			} else {
				s.logger.Info("connected to OBS")
				if err := s.recoverPlaybackAfterOBSConnect(ctx); err != nil {
					s.logger.Warn("resume playback failed", "error", s.redactError(err))
				}
			}
		case obs.StateConnected:
			if s.playbackState() == playbackIdle {
				if err := s.recoverPlaybackAfterOBSConnect(ctx); err != nil {
					s.logger.Warn("resume playback failed", "error", s.redactError(err))
				}
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (s *Service) playbackWatchdogLoop(ctx context.Context) {
	ticker := time.NewTicker(playbackWatchdogInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.checkPlaybackWatchdog(ctx); err != nil {
				s.setLastErr(err)
				s.logger.Warn("playback watchdog failed", "error", s.redactError(err))
			}
		}
	}
}

func (s *Service) checkPlaybackWatchdog(ctx context.Context) error {
	if s.obs.Status().State != obs.StateConnected {
		return nil
	}

	var video *queue.Video
	if err := func() error {
		s.playbackMu.Lock()
		defer s.playbackMu.Unlock()

		current, err := s.store.Current(ctx)
		if err != nil {
			return err
		}
		if current == nil || current.StartedAt == nil || current.DurationSeconds <= 0 {
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

func (s *Service) obsEventLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-s.obs.Events():
			if !ok {
				return
			}
			if event.Type != obs.EventMediaEnded {
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

	current, err := s.store.Current(ctx)
	if err != nil {
		return nil, err
	}
	if current != nil {
		if event.Path != "" && current.LocalPath != event.Path {
			return nil, nil
		}
		if s.obsEventTooEarlyForCurrent(event, *current) {
			s.logger.Warn("ignore early OBS ended event",
				"video_id", current.ID,
				"event_path", s.redactString(event.Path),
				"event_at", event.At,
				"started_at", current.StartedAt,
				"duration_seconds", current.DurationSeconds,
			)
			return nil, nil
		}
		return s.advancePlaybackLockedAfter(ctx, current.ID, current.LocalPath)
	}
	return s.advancePlaybackLockedAfter(ctx, 0, event.Path)
}

func (s *Service) obsEventTooEarlyForCurrent(event obs.Event, current queue.Video) bool {
	if current.StartedAt == nil || current.DurationSeconds <= 0 {
		return false
	}
	tolerance := obsEndedEarlyTolerance
	duration := time.Duration(current.DurationSeconds) * time.Second
	if duration <= tolerance {
		tolerance = duration / 2
	}
	trustedAfter := current.StartedAt.Add(duration - tolerance)
	eventAt := event.At
	if eventAt.IsZero() {
		eventAt = s.nowUTC()
	}
	return eventAt.Before(trustedAfter)
}

func (s *Service) recoverStartupState(ctx context.Context) error {
	count, err := s.store.FailStaleDownloading(ctx, staleDownloadingAge, "startup recovery: stale downloading item")
	if err != nil {
		return err
	}
	if count > 0 {
		s.logger.Warn("marked stale downloading queue items failed", "count", count)
	}
	return nil
}

func (s *Service) CleanupRetention(ctx context.Context) error {
	if s.cfg.RetentionMaxAge() <= 0 && s.cfg.RetentionMaxFiles <= 0 {
		return nil
	}
	videos, err := s.store.Played(ctx)
	if err != nil {
		return err
	}
	deleteIDs := make(map[int64]queue.Video)
	fallbackID, fallbackPath := s.randomFallbackLock()
	if maxAge := s.cfg.RetentionMaxAge(); maxAge > 0 {
		cutoff := time.Now().UTC().Add(-maxAge)
		for _, video := range videos {
			if video.FinishedAt != nil && video.FinishedAt.Before(cutoff) {
				deleteIDs[video.ID] = video
			}
		}
	}
	if s.cfg.RetentionMaxFiles > 0 && len(videos) > s.cfg.RetentionMaxFiles {
		for _, video := range videos[:len(videos)-s.cfg.RetentionMaxFiles] {
			deleteIDs[video.ID] = video
		}
	}
	for _, video := range deleteIDs {
		if video.ID == fallbackID || (fallbackPath != "" && video.LocalPath == fallbackPath) {
			continue
		}
		deleteLocalFile := false
		if s.cfg.RetentionDeleteLocalFiles {
			referenced, err := s.store.LocalPathReferenced(ctx, video.LocalPath, video.ID)
			if err != nil {
				return err
			}
			deleteLocalFile = !referenced
		}
		if err := s.store.Delete(ctx, video.ID); err != nil {
			return err
		}
		if deleteLocalFile {
			if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, video.LocalPath); err != nil {
				s.logger.Warn("skip retention local file delete", "video_id", video.ID, "path", s.redactString(video.LocalPath), "error", s.redactError(err))
				continue
			}
			if err := media.RemoveFile(video.LocalPath); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Service) markUploadFailed(ctx context.Context, id int64, cause error) {
	failCtx := ctx
	cancel := func() {}
	if ctx.Err() != nil {
		failCtx, cancel = context.WithTimeout(context.Background(), uploadFailureTimeout)
	}
	defer cancel()
	if err := s.store.MarkFailed(failCtx, id, cause.Error()); err != nil {
		s.logger.Warn("mark upload failed", "video_id", id, "error", s.redactError(err))
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
