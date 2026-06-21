package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
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

func TestFallbackEndedEventAdvancesFallbackPlayback(t *testing.T) {
	ctx := context.Background()
	svc, fakeOBS, _ := newFallbackTestService(t, config.Config{FallbackMode: "file"})
	firstPath := filepath.Join(t.TempDir(), "fallback-one.mp4")
	secondPath := filepath.Join(t.TempDir(), "fallback-two.mp4")
	writeTestFile(t, firstPath)
	writeTestFile(t, secondPath)
	svc.cfg.OBSFallbackFile = firstPath
	if _, err := svc.advancePlayback(ctx); err != nil {
		t.Fatalf("start fallback: %v", err)
	}
	svc.cfg.OBSFallbackFile = secondPath

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
	svc, _, _ := newLocalUploadTestService(t, config.Config{FallbackMode: "off"})
	firstReady := addReadyVideoWithDuration(t, ctx, svc, "first.mp4", 30)
	firstPlaying, err := svc.store.MarkPlaying(ctx, firstReady.ID)
	if err != nil {
		t.Fatalf("mark first playing: %v", err)
	}
	second := addReadyVideo(t, ctx, svc, "second.mp4")
	third := addReadyVideo(t, ctx, svc, "third.mp4")
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

	video, err := svc.advancePlaybackForEndedEvent(ctx, obs.Event{
		Type: obs.EventMediaEnded,
		Path: second.LocalPath,
		At:   currentAfterWatchdog.StartedAt.Add(time.Second),
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

func TestRunReturnsWhenBotStopsUnexpectedly(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newFallbackTestService(t, config.Config{FallbackMode: "off"})

	err := svc.Run(ctx)

	if err == nil || !strings.Contains(err.Error(), "telegram service stopped unexpectedly") {
		t.Fatalf("err = %v, want unexpected telegram stop", err)
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

func newFallbackTestService(t *testing.T, cfg config.Config) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	return newFallbackTestServiceAtDBPath(t, cfg, filepath.Join(t.TempDir(), "queue.db"))
}

func newFallbackTestServiceAtDBPath(t *testing.T, cfg config.Config, dbPath string) (*Service, *fakeOBS, *fakeBot) {
	t.Helper()
	ctx := context.Background()
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
	if err := os.MkdirAll(cfg.TelegramBotAPIDir, 0o755); err != nil {
		t.Fatalf("create telegram bot api dir: %v", err)
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
		now:      time.Now,
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
	store := svc.store
	ready := addReadyVideo(t, ctx, svc, name)
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
	state      obs.State
	lastPlayed string
	playErr    error
}

func (f *fakeOBS) Connect(context.Context) error { return nil }
func (f *fakeOBS) Close() error                  { return nil }
func (f *fakeOBS) Events() <-chan obs.Event      { return nil }
func (f *fakeOBS) PlayFile(_ context.Context, path string) error {
	if f.playErr != nil {
		return f.playErr
	}
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
