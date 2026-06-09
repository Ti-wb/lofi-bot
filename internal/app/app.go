package app

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	"github.com/tiwb/tg-obs-bot/internal/queue"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

type Service struct {
	cfg    config.Config
	logger *slog.Logger
	store  *queue.Store
	media  *media.Manager
	obs    *obs.Client
	bot    *telegram.Service

	mu         sync.Mutex
	playbackMu sync.Mutex
	lastErr    string
	shutdown   []func() error
}

type UploadRequest struct {
	FileURL          string
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
	obsClient, err := obs.NewClient(obs.Options{
		URL:             cfg.OBSURL(),
		Password:        cfg.OBSPassword,
		MediaSourceName: cfg.OBSMediaSourceName,
		EventBuffer:     16,
		Logger:          logger.With("component", "obs"),
	})
	if err != nil {
		_ = store.Close()
		return nil, err
	}

	service := &Service{
		cfg:      cfg,
		logger:   logger,
		store:    store,
		media:    manager,
		obs:      obsClient,
		shutdown: []func() error{obsClient.Close, store.Close},
	}

	bot, err := telegram.New(telegram.Config{
		Token:              cfg.TelegramBotToken,
		AllowedChatID:      cfg.AllowedChatID,
		MaxUploadSizeBytes: cfg.MaxVideoSizeBytes,
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
			s.logger.Warn("shutdown error", "error", err)
		}
	}
}

func (s *Service) Run(ctx context.Context) error {
	s.logger.Info("tg-obs-bot starting", "database", s.cfg.DatabasePath, "media_dir", s.cfg.MediaDir)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	errCh := make(chan error, 4)
	go func() { errCh <- s.bot.Run(ctx) }()
	go s.obsReconnectLoop(ctx)
	go s.obsEventLoop(ctx)

	cleanupTicker := time.NewTicker(10 * time.Minute)
	defer cleanupTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-errCh:
			if err != nil && ctx.Err() == nil {
				return err
			}
		case <-cleanupTicker.C:
			if err := s.CleanupRetention(ctx); err != nil {
				s.setLastErr(err)
				s.logger.Warn("retention cleanup failed", "error", err)
			}
		}
	}
}

func (s *Service) EnqueueUpload(ctx context.Context, req UploadRequest) (queue.Video, error) {
	if req.SizeBytes > s.cfg.MaxVideoSizeBytes {
		err := fmt.Errorf("檔案太大，上限是 %s", formatBytes(s.cfg.MaxVideoSizeBytes))
		s.setLastErr(err)
		return queue.Video{}, err
	}

	length, err := s.store.QueueLength(ctx)
	if err != nil {
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if length >= s.cfg.MaxQueueLength {
		err := fmt.Errorf("佇列已滿，目前上限是 %d 支", s.cfg.MaxQueueLength)
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

	localPath, meta, err := s.media.Download(ctx, req.FileURL, fileName, s.cfg.MaxVideoSizeBytes)
	if err != nil {
		_ = s.store.MarkFailed(ctx, video.ID, err.Error())
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if err := s.media.Validate(meta, s.cfg.MaxVideoSizeBytes, s.cfg.MaxVideoDurationSeconds); err != nil {
		_ = media.RemoveFile(localPath)
		_ = s.store.MarkFailed(ctx, video.ID, err.Error())
		s.setLastErr(err)
		return queue.Video{}, err
	}

	ready, err := s.store.MarkReady(ctx, video.ID, localPath, meta.SizeBytes, meta.DurationSeconds)
	if err != nil {
		_ = media.RemoveFile(localPath)
		s.setLastErr(err)
		return queue.Video{}, err
	}
	if err := s.playIfIdle(ctx); err != nil {
		s.logger.Warn("play after enqueue failed", "error", err)
	}
	return ready, nil
}

func (s *Service) advancePlayback(ctx context.Context) (*queue.Video, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()

	if err := s.store.FinishCurrent(ctx); err != nil {
		s.setLastErr(err)
		return nil, err
	}

	video, err := s.store.NextReady(ctx)
	if err != nil {
		s.setLastErr(err)
		return nil, err
	}
	if video == nil {
		if s.cfg.OBSFallbackFile != "" {
			if err := s.obs.PlayFile(ctx, s.cfg.OBSFallbackFile); err != nil {
				s.setLastErr(err)
				return nil, err
			}
		}
		return nil, nil
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
	return &playing, nil
}

func (s *Service) playIfIdle(ctx context.Context) error {
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
	_, err = s.advancePlayback(ctx)
	return err
}

func (s *Service) skipCurrent(ctx context.Context) (string, error) {
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
		return "已跳過，目前沒有下一支影片。", nil
	}
	video, err := s.advancePlayback(ctx)
	if err != nil {
		return "", err
	}
	if video == nil {
		return "已跳過，目前沒有下一支影片。", nil
	}
	return fmt.Sprintf("已跳到下一支：#%d %s", video.ID, video.FileName), nil
}

func (s *Service) RemoveQueued(ctx context.Context, id int64) error {
	video, err := s.store.Get(ctx, id)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		s.setLastErr(err)
		return err
	}
	if err := s.store.Cancel(ctx, id); err != nil {
		s.setLastErr(err)
		return err
	}
	if video.LocalPath != "" {
		_ = media.RemoveFile(video.LocalPath)
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
	stats, err := s.store.Stats(ctx)
	if err != nil {
		return "", err
	}
	diskText := "未知"
	if usage, err := s.media.DiskUsage(); err == nil {
		diskText = fmt.Sprintf("%s free / %s total", formatBytes(int64(usage.AvailableBytes)), formatBytes(int64(usage.TotalBytes)))
	}
	lastErr := s.lastError()
	if lastErr == "" {
		lastErr = "無"
	}
	return fmt.Sprintf(
		"狀態：\nOBS：%s\nReady：%d\nDownloading：%d\nPlayed：%d\nFailed：%d\nMedia DB：%s\nDisk：%s\nLast error：%s",
		boolText(obsConnected),
		stats.ReadyCount,
		stats.DownloadingCount,
		stats.PlayedCount,
		stats.FailedCount,
		formatBytes(stats.TotalBytes),
		diskText,
		lastErr,
	), nil
}

func (s *Service) telegramHooks() telegram.Hooks {
	return telegram.Hooks{
		EnqueueUpload: func(ctx context.Context, upload telegram.Upload) (string, error) {
			video, err := s.EnqueueUpload(ctx, UploadRequest{
				FileURL:          upload.DownloadURL,
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
		ListQueue: s.QueueText,
		Now:       s.NowText,
		History:   s.HistoryText,
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
		Skip: s.skipCurrent,
	}
}

func (s *Service) obsReconnectLoop(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		if s.obs.Status().State == obs.StateDisconnected {
			if err := s.obs.Connect(ctx); err != nil {
				s.setLastErr(err)
				s.logger.Warn("connect OBS failed", "error", err)
			} else {
				s.logger.Info("connected to OBS")
				if err := s.playIfIdle(ctx); err != nil {
					s.logger.Warn("resume playback failed", "error", err)
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
			video, err := s.advancePlayback(ctx)
			if err != nil {
				s.logger.Warn("advance playback after OBS event failed", "error", err)
				continue
			}
			if video != nil {
				_ = s.bot.SendMessage(ctx, s.cfg.AllowedChatID, fmt.Sprintf("開始播放：#%d %s", video.ID, video.FileName))
			}
		}
	}
}

func (s *Service) CleanupRetention(ctx context.Context) error {
	videos, err := s.store.Played(ctx)
	if err != nil {
		return err
	}
	deleteIDs := make(map[int64]queue.Video)
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
		if err := media.RemoveFile(video.LocalPath); err != nil {
			return err
		}
		if err := s.store.Delete(ctx, video.ID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) setLastErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = err.Error()
}

func (s *Service) lastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
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
