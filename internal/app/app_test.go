package app

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	"github.com/tiwb/tg-obs-bot/internal/queue"
)

func TestRandomFallbackStartsAndNotifiesOnce(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, fakeBot := newFallbackTestService(t, config.Config{FallbackMode: "random_played"})
	played := addPlayedVideo(t, ctx, svc.store, "history.mp4", true)

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

func TestReadyQueueTakesPriorityAfterFallbackEnds(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "random_played"})
	_ = addPlayedVideo(t, ctx, svc.store, "history.mp4", true)

	if _, err := svc.advancePlayback(ctx); err != nil {
		t.Fatalf("start fallback: %v", err)
	}
	ready := addReadyVideo(t, ctx, svc.store, "ready.mp4")

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
	locked := addPlayedVideo(t, ctx, svc.store, "locked.mp4", true)
	time.Sleep(time.Millisecond)
	_ = addPlayedVideo(t, ctx, svc.store, "other.mp4", true)
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
	removeCandidate := addPlayedVideo(t, ctx, svc.store, "old.mp4", true)
	time.Sleep(time.Millisecond)
	keepCandidate := addPlayedVideo(t, ctx, svc.store, "new.mp4", true)

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

func TestMissingRandomFallbackUsesStaticFile(t *testing.T) {
	ctx := context.Background()
	staticPath := filepath.Join(t.TempDir(), "fallback.mp4")
	writeTestFile(t, staticPath)
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{
		FallbackMode:    "random_played",
		OBSFallbackFile: staticPath,
	})
	_ = addPlayedVideo(t, ctx, svc.store, "missing.mp4", false)

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
	path := filepath.Join(t.TempDir(), "upload.mp4")
	writeTestFile(t, path)

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

func TestRemoveQueuedDoesNotDeleteLocalBotAPIFile(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
	})
	path := filepath.Join(t.TempDir(), "upload.mp4")
	writeTestFile(t, path)

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
	ready := addReadyVideo(t, ctx, svc.store, "queued.mp4")
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

func newFallbackTestService(t *testing.T, cfg config.Config) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	ctx := context.Background()
	store, err := queue.Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
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
	cfg.AllowedChatID = -100123
	fakeOBS := &fakeOBS{state: obs.StateConnected}
	fakeBot := &fakeBot{}
	return &Service{
		cfg:      cfg,
		logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		store:    store,
		obs:      fakeOBS,
		bot:      fakeBot,
		playback: playbackIdle,
	}, fakeOBS, fakeBot
}

func newLocalUploadTestService(t *testing.T, cfg config.Config) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	svc, fakeOBS, fakeBot := newFallbackTestService(t, cfg)
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

func addReadyVideo(t *testing.T, ctx context.Context, store *queue.Store, name string) queue.Video {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
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
	ready, err := store.MarkReady(ctx, video.ID, path, 100, 60)
	if err != nil {
		t.Fatalf("mark ready: %v", err)
	}
	return ready
}

func addPlayedVideo(t *testing.T, ctx context.Context, store *queue.Store, name string, createFile bool) queue.Video {
	t.Helper()
	ready := addReadyVideo(t, ctx, store, name)
	if !createFile {
		if err := osRemove(ready.LocalPath); err != nil {
			t.Fatalf("remove test file: %v", err)
		}
	}
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

func writeTestFile(t *testing.T, path string) {
	t.Helper()
	if err := osWriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
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

func formatTestInt(value int) string {
	return fmt.Sprintf("%d", value)
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
	state      obs.State
	lastPlayed string
}

func (f *fakeOBS) Connect(context.Context) error { return nil }
func (f *fakeOBS) Close() error                  { return nil }
func (f *fakeOBS) Events() <-chan obs.Event      { return nil }
func (f *fakeOBS) PlayFile(_ context.Context, path string) error {
	f.lastPlayed = path
	return nil
}
func (f *fakeOBS) StopCurrent(context.Context) error {
	f.lastPlayed = ""
	return nil
}
func (f *fakeOBS) Status() obs.Status { return obs.Status{State: f.state} }

type fakeBot struct {
	messages []string
}

func (f *fakeBot) Run(context.Context) error { return nil }
func (f *fakeBot) SendMessage(_ context.Context, _ int64, text string) error {
	f.messages = append(f.messages, text)
	return nil
}
