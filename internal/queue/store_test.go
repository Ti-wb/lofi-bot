package queue

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestOpenConfiguresSQLitePragmasAndSequentialWritesPreservePositions(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("expected max open connections 1, got %d", got)
	}

	var busyTimeout int
	if err := store.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("expected busy_timeout 5000, got %d", busyTimeout)
	}

	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("expected foreign_keys on, got %d", foreignKeys)
	}

	var expected []int64
	for i := 1; i <= 5; i++ {
		ready := addReady(t, ctx, store, fmt.Sprintf("write-%d.mp4", i))
		expected = append(expected, ready.ID)
	}

	items, err := store.ListQueue(ctx, 10)
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	assertOrder(t, items, expected)
	assertPositions(t, items)
}

func TestOpenMigratesLegacyVideosTableMissingLocalPath(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	legacyID := createLegacyQueueDB(t, ctx, dbPath)

	store, err := Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open legacy store: %v", err)
	}
	defer store.Close()

	legacy, err := store.Get(ctx, legacyID)
	if err != nil {
		t.Fatalf("get migrated legacy video: %v", err)
	}
	if legacy.LocalPath != "" {
		t.Fatalf("expected legacy local_path default empty, got %q", legacy.LocalPath)
	}

	readyLegacy, err := store.MarkReady(ctx, legacy.ID, "/tmp/legacy.mp4", 123, 45)
	if err != nil {
		t.Fatalf("mark migrated legacy video ready: %v", err)
	}
	if readyLegacy.LocalPath != "/tmp/legacy.mp4" {
		t.Fatalf("expected marked local_path, got %q", readyLegacy.LocalPath)
	}

	added, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "new-file",
		TelegramUniqueID: "new-unique",
		FileName:         "new.mp4",
	})
	if err != nil {
		t.Fatalf("add downloading after migration: %v", err)
	}
	readyAdded, err := store.MarkReady(ctx, added.ID, "/tmp/new.mp4", 456, 67)
	if err != nil {
		t.Fatalf("mark new video ready after migration: %v", err)
	}

	items, err := store.ListQueue(ctx, 10)
	if err != nil {
		t.Fatalf("list queue after migration: %v", err)
	}
	assertOrder(t, items, []int64{readyLegacy.ID, readyAdded.ID})
	assertPositions(t, items)

	for _, indexName := range []string{"idx_videos_status_position", "idx_videos_created"} {
		var count int
		if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?
`, indexName).Scan(&count); err != nil {
			t.Fatalf("check index %s: %v", indexName, err)
		}
		if count != 1 {
			t.Fatalf("expected index %s to exist, count=%d", indexName, count)
		}
	}
}

func TestQueueMoveCancelAndStartNext(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	first := addReady(t, ctx, store, "first.mp4")
	second := addReady(t, ctx, store, "second.mp4")
	third := addReady(t, ctx, store, "third.mp4")

	if err := store.Move(ctx, third.ID, 1); err != nil {
		t.Fatalf("move: %v", err)
	}
	items, err := store.ListQueue(ctx, 10)
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	assertOrder(t, items, []int64{third.ID, first.ID, second.ID})

	if err := store.Cancel(ctx, first.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	items, err = store.ListQueue(ctx, 10)
	if err != nil {
		t.Fatalf("list after cancel: %v", err)
	}
	assertOrder(t, items, []int64{third.ID, second.ID})

	current, err := store.StartNext(ctx)
	if err != nil {
		t.Fatalf("start next: %v", err)
	}
	if current == nil || current.ID != third.ID {
		t.Fatalf("expected third to start, got %#v", current)
	}

	current, err = store.Current(ctx)
	if err != nil {
		t.Fatalf("current: %v", err)
	}
	if current == nil || current.Status != StatusPlaying {
		t.Fatalf("expected playing current, got %#v", current)
	}

	next, err := store.StartNext(ctx)
	if err != nil {
		t.Fatalf("start second: %v", err)
	}
	if next == nil || next.ID != second.ID {
		t.Fatalf("expected second to start, got %#v", next)
	}
}

func TestRestartPlayingRefreshesStartedAt(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ready := addReady(t, ctx, store, "restart.mp4")
	playing, err := store.MarkPlaying(ctx, ready.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	oldStarted := playing.StartedAt.Add(-2 * time.Hour)
	if _, err := store.db.ExecContext(ctx, `
UPDATE videos SET started_at = ?, updated_at = ? WHERE id = ?
`, formatTime(oldStarted), formatTime(oldStarted), playing.ID); err != nil {
		t.Fatalf("age playing row: %v", err)
	}

	restarted, err := store.RestartPlaying(ctx, playing.ID)
	if err != nil {
		t.Fatalf("restart playing: %v", err)
	}
	if restarted.StartedAt == nil || !restarted.StartedAt.After(oldStarted) {
		t.Fatalf("started_at = %v, want after %s", restarted.StartedAt, oldStarted)
	}
	if restarted.Status != StatusPlaying {
		t.Fatalf("status = %s, want %s", restarted.Status, StatusPlaying)
	}
}

func TestQueueLengthIncludesDownloadingAndReady(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	_, err = store.AddDownloading(ctx, Video{TelegramFileID: "a", TelegramUniqueID: "a", FileName: "a.mp4"})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	_ = addReady(t, ctx, store, "b.mp4")

	length, err := store.QueueLength(ctx)
	if err != nil {
		t.Fatalf("queue length: %v", err)
	}
	if length != 2 {
		t.Fatalf("expected length 2, got %d", length)
	}
}

func TestMarkReadyFailsAfterCancel(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	video, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "file",
		TelegramUniqueID: "unique",
		FileName:         "cancel-me.mp4",
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	if err := store.Cancel(ctx, video.ID); err != nil {
		t.Fatalf("cancel downloading: %v", err)
	}
	if _, err := store.MarkReady(ctx, video.ID, "/tmp/cancel-me.mp4", 100, 60); err == nil {
		t.Fatal("expected mark ready to fail after cancel")
	}

	items, err := store.ListQueue(ctx, 10)
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("expected empty queue, got %#v", items)
	}
}

func TestFailStaleDownloadingOnlyMarksOldRows(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	oldDownloading, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "old",
		TelegramUniqueID: "old",
		FileName:         "old.mp4",
	})
	if err != nil {
		t.Fatalf("add old downloading: %v", err)
	}
	newDownloading, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "new",
		TelegramUniqueID: "new",
		FileName:         "new.mp4",
	})
	if err != nil {
		t.Fatalf("add new downloading: %v", err)
	}
	ready := addReady(t, ctx, store, "ready.mp4")
	oldCreated := time.Now().UTC().Add(-8 * time.Hour)
	if _, err := store.db.ExecContext(ctx, `
UPDATE videos SET created_at = ?, updated_at = ? WHERE id = ?
`, formatTime(oldCreated), formatTime(oldCreated), oldDownloading.ID); err != nil {
		t.Fatalf("age old downloading: %v", err)
	}

	count, err := store.FailStaleDownloading(ctx, 6*time.Hour, "stale download")
	if err != nil {
		t.Fatalf("fail stale downloading: %v", err)
	}
	if count != 1 {
		t.Fatalf("count = %d, want 1", count)
	}
	oldStored, err := store.Get(ctx, oldDownloading.ID)
	if err != nil {
		t.Fatalf("get old downloading: %v", err)
	}
	if oldStored.Status != StatusFailed || oldStored.Error != "stale download" {
		t.Fatalf("old status/error = %s/%q, want failed/stale download", oldStored.Status, oldStored.Error)
	}
	newStored, err := store.Get(ctx, newDownloading.ID)
	if err != nil {
		t.Fatalf("get new downloading: %v", err)
	}
	if newStored.Status != StatusDownloading {
		t.Fatalf("new status = %s, want %s", newStored.Status, StatusDownloading)
	}
	readyStored, err := store.Get(ctx, ready.ID)
	if err != nil {
		t.Fatalf("get ready: %v", err)
	}
	if readyStored.Status != StatusReady {
		t.Fatalf("ready status = %s, want %s", readyStored.Status, StatusReady)
	}
}

func TestPlayedFallbackCandidatesOnlyReturnsPlayedWithLocalPath(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	played := addReady(t, ctx, store, "played.mp4")
	if _, err := store.StartNext(ctx); err != nil {
		t.Fatalf("start played: %v", err)
	}
	if err := store.FinishCurrent(ctx); err != nil {
		t.Fatalf("finish played: %v", err)
	}
	_ = addReady(t, ctx, store, "ready.mp4")

	candidates, err := store.PlayedFallbackCandidates(ctx, 10)
	if err != nil {
		t.Fatalf("fallback candidates: %v", err)
	}
	if len(candidates) != 1 {
		t.Fatalf("expected 1 candidate, got %d: %#v", len(candidates), candidates)
	}
	if candidates[0].ID != played.ID {
		t.Fatalf("expected played candidate id %d, got %d", played.ID, candidates[0].ID)
	}
}

func TestPlayedFallbackCandidatesZeroLimitReturnsAll(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	playedIDs := make(map[int64]bool)
	for _, name := range []string{"first.mp4", "second.mp4", "third.mp4"} {
		ready := addReady(t, ctx, store, name)
		playedIDs[ready.ID] = true
		if _, err := store.StartNext(ctx); err != nil {
			t.Fatalf("start %s: %v", name, err)
		}
		if err := store.FinishCurrent(ctx); err != nil {
			t.Fatalf("finish %s: %v", name, err)
		}
	}

	candidates, err := store.PlayedFallbackCandidates(ctx, 0)
	if err != nil {
		t.Fatalf("fallback candidates: %v", err)
	}
	if len(candidates) != len(playedIDs) {
		t.Fatalf("expected %d candidates, got %d: %#v", len(playedIDs), len(candidates), candidates)
	}
	for _, candidate := range candidates {
		if !playedIDs[candidate.ID] {
			t.Fatalf("unexpected candidate id %d", candidate.ID)
		}
	}
}

func createLegacyQueueDB(t *testing.T, ctx context.Context, path string) int64 {
	t.Helper()

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	defer db.Close()

	if _, err := db.ExecContext(ctx, `
CREATE TABLE videos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	telegram_file_id TEXT NOT NULL,
	telegram_unique_id TEXT NOT NULL,
	submitter_id INTEGER NOT NULL,
	submitter_name TEXT NOT NULL,
	chat_id INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	file_name TEXT NOT NULL,
	mime_type TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	duration_seconds INTEGER NOT NULL DEFAULT 0,
	queue_position INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);
`); err != nil {
		t.Fatalf("create legacy schema: %v", err)
	}

	res, err := db.ExecContext(ctx, `
INSERT INTO videos (
	telegram_file_id, telegram_unique_id, submitter_id, submitter_name, chat_id, message_id,
	file_name, mime_type, size_bytes, duration_seconds, queue_position, status, error, created_at, updated_at
) VALUES (?, ?, 0, '', 0, 0, ?, '', 0, 0, 0, ?, '', ?, ?)
`, "legacy-file", "legacy-unique", "legacy.mp4", string(StatusDownloading), "2026-06-10T00:00:00Z", "2026-06-10T00:00:00Z")
	if err != nil {
		t.Fatalf("insert legacy video: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("legacy id: %v", err)
	}
	return id
}

func addReady(t *testing.T, ctx context.Context, store *Store, name string) Video {
	t.Helper()
	video, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   name,
		TelegramUniqueID: name,
		FileName:         name,
		LocalPath:        "/tmp/" + name,
	})
	if err != nil {
		t.Fatalf("add downloading %s: %v", name, err)
	}
	ready, err := store.MarkReady(ctx, video.ID, video.LocalPath, 100, 60)
	if err != nil {
		t.Fatalf("mark ready %s: %v", name, err)
	}
	return ready
}

func assertOrder(t *testing.T, videos []Video, expected []int64) {
	t.Helper()
	if len(videos) != len(expected) {
		t.Fatalf("expected %d videos, got %d: %#v", len(expected), len(videos), videos)
	}
	for idx, want := range expected {
		if videos[idx].ID != want {
			t.Fatalf("position %d: expected id %d, got %d", idx, want, videos[idx].ID)
		}
	}
}

func assertPositions(t *testing.T, videos []Video) {
	t.Helper()
	for idx, video := range videos {
		if video.QueuePosition != idx+1 {
			t.Fatalf("position %d: expected queue position %d, got %d", idx, idx+1, video.QueuePosition)
		}
	}
}
