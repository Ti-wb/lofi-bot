package queue

import (
	"context"
	"path/filepath"
	"testing"
)

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
