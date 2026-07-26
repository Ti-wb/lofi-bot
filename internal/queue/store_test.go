package queue

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
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

	for _, indexName := range []string{
		"idx_videos_status_position",
		"idx_videos_created",
		"idx_videos_status_finished",
		"idx_videos_status_updated",
		"idx_videos_local_path",
		"idx_videos_finished_status",
		"idx_videos_updated_status",
	} {
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

func TestMigrationBackfillsTerminalFinishedAtForIndexedRetention(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	played := addReady(t, ctx, store, "legacy-played.mp4")
	if _, err := store.MarkPlaying(ctx, played.ID); err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	if err := store.FinishCurrent(ctx); err != nil {
		t.Fatalf("finish current: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE videos SET finished_at = NULL WHERE id = ?`, played.ID); err != nil {
		t.Fatalf("clear legacy finished_at: %v", err)
	}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("rerun migration: %v", err)
	}
	migrated, err := store.Get(ctx, played.ID)
	if err != nil {
		t.Fatalf("get migrated row: %v", err)
	}
	if migrated.FinishedAt == nil || !migrated.FinishedAt.Equal(migrated.UpdatedAt) {
		t.Fatalf("finished_at = %v, updated_at = %s; want backfilled equality", migrated.FinishedAt, migrated.UpdatedAt)
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

func TestStateAdvancingMethodsRollbackWhenPersistedRowCannotBeDecoded(t *testing.T) {
	ctx := context.Background()

	t.Run("MarkReady", func(t *testing.T) {
		store := openTestStore(t, ctx)
		video, err := store.AddDownloading(ctx, Video{
			TelegramFileID:   "bad-ready",
			TelegramUniqueID: "bad-ready",
			FileName:         "bad-ready.mp4",
			LocalPath:        "/cache/bad-ready.mp4",
		})
		if err != nil {
			t.Fatalf("add downloading: %v", err)
		}
		corruptCreatedAt(t, ctx, store, video.ID)

		if _, err := store.MarkReady(ctx, video.ID, "/cache/new-ready.mp4", 100, 60); err == nil ||
			!strings.Contains(err.Error(), "videos.created_at") {
			t.Fatalf("MarkReady error = %v, want decode error", err)
		}
		assertRawStatus(t, ctx, store, video.ID, StatusDownloading)
		var localPath string
		if err := store.db.QueryRowContext(ctx, `SELECT local_path FROM videos WHERE id = ?`, video.ID).Scan(&localPath); err != nil {
			t.Fatalf("read local path: %v", err)
		}
		if localPath != video.LocalPath {
			t.Fatalf("local path advanced despite rollback: got %q want %q", localPath, video.LocalPath)
		}
	})

	t.Run("MarkPlaying", func(t *testing.T) {
		store := openTestStore(t, ctx)
		ready := addReady(t, ctx, store, "bad-playing.mp4")
		corruptCreatedAt(t, ctx, store, ready.ID)

		if _, err := store.MarkPlaying(ctx, ready.ID); err == nil ||
			!strings.Contains(err.Error(), "videos.created_at") {
			t.Fatalf("MarkPlaying error = %v, want decode error", err)
		}
		assertRawStatus(t, ctx, store, ready.ID, StatusReady)
	})

	t.Run("RestartPlaying", func(t *testing.T) {
		store := openTestStore(t, ctx)
		ready := addReady(t, ctx, store, "bad-restart.mp4")
		playing, err := store.MarkPlaying(ctx, ready.ID)
		if err != nil {
			t.Fatalf("mark playing: %v", err)
		}
		var startedBefore string
		if err := store.db.QueryRowContext(ctx, `SELECT started_at FROM videos WHERE id = ?`, playing.ID).Scan(&startedBefore); err != nil {
			t.Fatalf("read started_at before restart: %v", err)
		}
		corruptCreatedAt(t, ctx, store, playing.ID)

		if _, err := store.RestartPlaying(ctx, playing.ID); err == nil ||
			!strings.Contains(err.Error(), "videos.created_at") {
			t.Fatalf("RestartPlaying error = %v, want decode error", err)
		}
		assertRawStatus(t, ctx, store, playing.ID, StatusPlaying)
		var startedAfter string
		if err := store.db.QueryRowContext(ctx, `SELECT started_at FROM videos WHERE id = ?`, playing.ID).Scan(&startedAfter); err != nil {
			t.Fatalf("read started_at after restart: %v", err)
		}
		if startedAfter != startedBefore {
			t.Fatalf("started_at advanced despite rollback: before=%q after=%q", startedBefore, startedAfter)
		}
	})

	t.Run("StartNext", func(t *testing.T) {
		store := openTestStore(t, ctx)
		current := addReady(t, ctx, store, "current.mp4")
		next := addReady(t, ctx, store, "bad-next.mp4")
		if _, err := store.MarkPlaying(ctx, current.ID); err != nil {
			t.Fatalf("mark current playing: %v", err)
		}
		corruptCreatedAt(t, ctx, store, next.ID)

		if _, err := store.StartNext(ctx); err == nil ||
			!strings.Contains(err.Error(), "videos.created_at") {
			t.Fatalf("StartNext error = %v, want decode error", err)
		}
		assertRawStatus(t, ctx, store, current.ID, StatusPlaying)
		assertRawStatus(t, ctx, store, next.ID, StatusReady)
	})
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

func TestSQLiteFullLeavesDownloadingRowRecoverable(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	video, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "sqlite-full",
		TelegramUniqueID: "sqlite-full",
		FileName:         "sqlite-full.mp4",
		LocalPath:        "/cache/sqlite-full.mp4",
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	var pageCount int
	if err := store.db.QueryRowContext(ctx, `PRAGMA page_count`).Scan(&pageCount); err != nil {
		t.Fatalf("page count: %v", err)
	}
	var maxPageCount int
	if err := store.db.QueryRowContext(ctx, fmt.Sprintf(`PRAGMA max_page_count = %d`, pageCount)).Scan(&maxPageCount); err != nil {
		t.Fatalf("set max page count: %v", err)
	}
	if maxPageCount != pageCount {
		t.Fatalf("max page count = %d, want %d", maxPageCount, pageCount)
	}

	oversizedPath := "/cache/" + strings.Repeat("x", 64*1024)
	if _, err := store.MarkReady(ctx, video.ID, oversizedPath, 100, 60); err == nil ||
		!strings.Contains(strings.ToLower(err.Error()), "database or disk is full") {
		t.Fatalf("MarkReady error = %v, want SQLITE_FULL", err)
	}
	stillDownloading, err := store.Get(ctx, video.ID)
	if err != nil {
		t.Fatalf("get after SQLITE_FULL: %v", err)
	}
	if stillDownloading.Status != StatusDownloading {
		t.Fatalf("status after SQLITE_FULL = %s, want %s", stillDownloading.Status, StatusDownloading)
	}

	if err := store.db.QueryRowContext(ctx, fmt.Sprintf(`PRAGMA max_page_count = %d`, pageCount+128)).Scan(&maxPageCount); err != nil {
		t.Fatalf("restore max page count: %v", err)
	}
	changed, err := store.MarkFailed(ctx, video.ID, "mark-ready failed after SQLITE_FULL")
	if err != nil {
		t.Fatalf("finalize after restoring capacity: %v", err)
	}
	if !changed {
		t.Fatal("downloading row should be recoverable after SQLITE_FULL")
	}
}

func TestFailureTransitionsAreCompareAndSwapAndSetFinishedAt(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	downloading, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "downloading",
		TelegramUniqueID: "downloading",
		FileName:         "downloading.mp4",
		LocalPath:        "/cache/downloading.mp4",
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	storedDownloading, err := store.Get(ctx, downloading.ID)
	if err != nil {
		t.Fatalf("get downloading: %v", err)
	}
	if storedDownloading.LocalPath != downloading.LocalPath {
		t.Fatalf("local path = %q, want %q", storedDownloading.LocalPath, downloading.LocalPath)
	}
	changed, err := store.MarkFailed(ctx, downloading.ID, "probe failed")
	if err != nil {
		t.Fatalf("mark downloading failed: %v", err)
	}
	if !changed {
		t.Fatal("downloading transition should change one row")
	}
	changed, err = store.MarkFailed(ctx, downloading.ID, "must not overwrite")
	if err != nil {
		t.Fatalf("repeat mark failed: %v", err)
	}
	if changed {
		t.Fatal("repeat downloading transition must not overwrite terminal row")
	}
	failed, err := store.Get(ctx, downloading.ID)
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	if failed.Status != StatusFailed || failed.Error != "probe failed" || failed.FinishedAt == nil {
		t.Fatalf("failed row = %#v, want first cause and finished_at", failed)
	}

	first := addReady(t, ctx, store, "first.mp4")
	second := addReady(t, ctx, store, "second.mp4")
	changed, err = store.FailReady(ctx, first.ID, "path missing")
	if err != nil {
		t.Fatalf("fail ready: %v", err)
	}
	if !changed {
		t.Fatal("ready transition should change one row")
	}
	queueRows, err := store.ListQueue(ctx, 10)
	if err != nil {
		t.Fatalf("list queue: %v", err)
	}
	if len(queueRows) != 1 || queueRows[0].ID != second.ID || queueRows[0].QueuePosition != 1 {
		t.Fatalf("queue after FailReady = %#v, want compacted second item", queueRows)
	}

	playing, err := store.MarkPlaying(ctx, second.ID)
	if err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	changed, err = store.FailPlaying(ctx, playing.ID, "current path invalid")
	if err != nil {
		t.Fatalf("fail playing: %v", err)
	}
	if !changed {
		t.Fatal("playing transition should change one row")
	}
	playingFailed, err := store.Get(ctx, playing.ID)
	if err != nil {
		t.Fatalf("get failed playing: %v", err)
	}
	if playingFailed.FinishedAt == nil {
		t.Fatal("failed playing row must have finished_at")
	}

	played := addReady(t, ctx, store, "played.mp4")
	if _, err := store.MarkPlaying(ctx, played.ID); err != nil {
		t.Fatalf("mark played item playing: %v", err)
	}
	if err := store.FinishCurrent(ctx); err != nil {
		t.Fatalf("finish current: %v", err)
	}
	changed, err = store.QuarantinePlayed(ctx, played.ID, "fallback file missing")
	if err != nil {
		t.Fatalf("quarantine played: %v", err)
	}
	if !changed {
		t.Fatal("played quarantine should change one row")
	}
}

func TestCancelSetsFinishedAtAndCannotBeOverwrittenByUploadFailure(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	video, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "cancel",
		TelegramUniqueID: "cancel",
		FileName:         "cancel.mp4",
		LocalPath:        "/cache/cancel.mp4",
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	if err := store.Cancel(ctx, video.ID); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	changed, err := store.MarkFailed(ctx, video.ID, "late probe error")
	if err != nil {
		t.Fatalf("late mark failed: %v", err)
	}
	if changed {
		t.Fatal("late upload failure must not overwrite canceled row")
	}
	canceled, err := store.Get(ctx, video.ID)
	if err != nil {
		t.Fatalf("get canceled: %v", err)
	}
	if canceled.Status != StatusCanceled || canceled.FinishedAt == nil {
		t.Fatalf("canceled row = %#v, want canceled with finished_at", canceled)
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

func TestPlayedFallbackCandidatesHardCapsLargeResultSets(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	seedTerminalRows(t, ctx, store, StatusPlayed, MaxFallbackCandidates+744)

	for _, limit := range []int{0, MaxFallbackCandidates + 1, 100_000} {
		candidates, err := store.PlayedFallbackCandidates(ctx, limit)
		if err != nil {
			t.Fatalf("fallback candidates limit %d: %v", limit, err)
		}
		if len(candidates) != MaxFallbackCandidates {
			t.Fatalf("limit %d returned %d rows, want hard cap %d", limit, len(candidates), MaxFallbackCandidates)
		}
	}

	history, err := store.History(ctx, 100_000)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != maxHistoryRows {
		t.Fatalf("history returned %d rows, want hard cap %d", len(history), maxHistoryRows)
	}

	planRows, err := store.db.QueryContext(ctx, `
EXPLAIN QUERY PLAN
SELECT id FROM videos INDEXED BY idx_videos_updated_status
WHERE status IN (?, ?, ?)
ORDER BY updated_at DESC, id DESC
LIMIT ?
`, string(StatusPlayed), string(StatusCanceled), string(StatusFailed), maxHistoryRows)
	if err != nil {
		t.Fatalf("explain history query: %v", err)
	}
	defer planRows.Close()
	var details []string
	for planRows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := planRows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan history query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := planRows.Err(); err != nil {
		t.Fatalf("history query plan rows: %v", err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "idx_videos_updated_status") {
		t.Fatalf("history query plan does not use order-first index:\n%s", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("history query plan performs an unbounded temp sort:\n%s", plan)
	}
}

func TestOldestTerminalUsesBoundedIndexedPlan(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	seedTerminalRows(t, ctx, store, StatusPlayed, 500)
	seedTerminalRows(t, ctx, store, StatusFailed, 500)
	seedTerminalRows(t, ctx, store, StatusCanceled, 500)

	rows, err := store.OldestTerminal(ctx, []Status{StatusPlayed, StatusFailed, StatusCanceled}, 100_000)
	if err != nil {
		t.Fatalf("oldest terminal: %v", err)
	}
	if len(rows) != maxRetentionBatchRows {
		t.Fatalf("oldest terminal returned %d rows, want %d", len(rows), maxRetentionBatchRows)
	}
	count, err := store.TerminalCount(ctx, StatusPlayed, StatusFailed, StatusCanceled)
	if err != nil {
		t.Fatalf("terminal count: %v", err)
	}
	if count != 1500 {
		t.Fatalf("terminal count = %d, want 1500", count)
	}

	planRows, err := store.db.QueryContext(ctx, `
EXPLAIN QUERY PLAN
SELECT id FROM videos INDEXED BY idx_videos_finished_status
WHERE status IN (?, ?, ?)
ORDER BY finished_at ASC, updated_at ASC, id ASC
LIMIT ?
`, string(StatusPlayed), string(StatusFailed), string(StatusCanceled), maxRetentionBatchRows)
	if err != nil {
		t.Fatalf("explain retention query: %v", err)
	}
	defer planRows.Close()
	var details []string
	for planRows.Next() {
		var id, parent, notUsed int
		var detail string
		if err := planRows.Scan(&id, &parent, &notUsed, &detail); err != nil {
			t.Fatalf("scan query plan: %v", err)
		}
		details = append(details, detail)
	}
	if err := planRows.Err(); err != nil {
		t.Fatalf("query plan rows: %v", err)
	}
	plan := strings.Join(details, "\n")
	if !strings.Contains(plan, "idx_videos_finished_status") {
		t.Fatalf("query plan does not use retention index:\n%s", plan)
	}
	if strings.Contains(plan, "USE TEMP B-TREE") {
		t.Fatalf("query plan performs an unbounded temp sort:\n%s", plan)
	}
}

func TestDeleteTerminalIsCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	ready := addReady(t, ctx, store, "ready.mp4")
	deleted, err := store.DeleteTerminal(ctx, ready.ID, StatusPlayed)
	if err != nil {
		t.Fatalf("delete with stale terminal status: %v", err)
	}
	if deleted {
		t.Fatal("stale terminal delete must not remove a ready row")
	}
	if _, err := store.Get(ctx, ready.ID); err != nil {
		t.Fatalf("ready row should remain: %v", err)
	}
	if _, err := store.MarkPlaying(ctx, ready.ID); err != nil {
		t.Fatalf("mark playing: %v", err)
	}
	if err := store.FinishCurrent(ctx); err != nil {
		t.Fatalf("finish current: %v", err)
	}
	deleted, err = store.DeleteTerminal(ctx, ready.ID, StatusPlayed)
	if err != nil {
		t.Fatalf("delete terminal: %v", err)
	}
	if !deleted {
		t.Fatal("matching terminal delete should remove the row")
	}
}

func TestMalformedPersistedTimestampIsReported(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	video, err := store.AddDownloading(ctx, Video{
		TelegramFileID:   "bad-time",
		TelegramUniqueID: "bad-time",
		FileName:         "bad-time.mp4",
	})
	if err != nil {
		t.Fatalf("add downloading: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `UPDATE videos SET created_at = 'not-a-time' WHERE id = ?`, video.ID); err != nil {
		t.Fatalf("corrupt timestamp: %v", err)
	}
	if _, err := store.Get(ctx, video.ID); err == nil || !strings.Contains(err.Error(), "videos.created_at") {
		t.Fatalf("get error = %v, want explicit created_at parse error", err)
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

func openTestStore(t *testing.T, ctx context.Context) *Store {
	t.Helper()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "queue.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() {
		_ = store.Close()
	})
	return store
}

func corruptCreatedAt(t *testing.T, ctx context.Context, store *Store, id int64) {
	t.Helper()
	if _, err := store.db.ExecContext(ctx, `UPDATE videos SET created_at = 'not-a-time' WHERE id = ?`, id); err != nil {
		t.Fatalf("corrupt created_at: %v", err)
	}
}

func assertRawStatus(t *testing.T, ctx context.Context, store *Store, id int64, want Status) {
	t.Helper()
	var got Status
	if err := store.db.QueryRowContext(ctx, `SELECT status FROM videos WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read raw status: %v", err)
	}
	if got != want {
		t.Fatalf("raw status = %s, want %s", got, want)
	}
}

func seedTerminalRows(t *testing.T, ctx context.Context, store *Store, status Status, count int) {
	t.Helper()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin terminal seed: %v", err)
	}
	defer rollback(tx)
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO videos (
	telegram_file_id, telegram_unique_id, submitter_id, submitter_name, chat_id, message_id,
	file_name, local_path, mime_type, size_bytes, duration_seconds, queue_position, status,
	error, created_at, updated_at, finished_at
) VALUES (?, ?, 0, '', 0, 0, ?, ?, 'video/mp4', 100, 60, 0, ?, '', ?, ?, ?)
`)
	if err != nil {
		t.Fatalf("prepare terminal seed: %v", err)
	}
	defer stmt.Close()
	base := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("%s-%06d.mp4", status, i)
		at := formatTime(base.Add(time.Duration(i) * time.Second))
		if _, err := stmt.ExecContext(ctx, name, name, name, "/cache/"+name, string(status), at, at, at); err != nil {
			t.Fatalf("seed terminal row %d: %v", i, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close terminal seed statement: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit terminal seed: %v", err)
	}
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
