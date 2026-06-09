package media

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDownloadRejectsHTTP500(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	path, meta, err := manager.Download(context.Background(), server.URL, "clip.mp4", 1024)
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("expected HTTP 500 error, got %v", err)
	}
	if path != "" {
		t.Fatalf("expected no path, got %q", path)
	}
	if meta != (Metadata{}) {
		t.Fatalf("expected empty metadata, got %#v", meta)
	}
	requireNoFiles(t, manager.Dir())
}

func TestDownloadRejectsContentLengthOverLimit(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "11")
		_, _ = w.Write([]byte("too large!!"))
	}))
	defer server.Close()

	path, meta, err := manager.Download(context.Background(), server.URL, "clip.mp4", 10)
	if err == nil {
		t.Fatal("expected oversized content-length error")
	}
	if !strings.Contains(err.Error(), "file is too large") {
		t.Fatalf("expected file is too large error, got %v", err)
	}
	if path != "" {
		t.Fatalf("expected no path, got %q", path)
	}
	if meta != (Metadata{}) {
		t.Fatalf("expected empty metadata, got %#v", meta)
	}
	requireNoFiles(t, manager.Dir())
}

func TestDownloadRejectsStreamingOverLimitAndCleansTempFile(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Fatal("response writer does not support flushing")
		}
		w.WriteHeader(http.StatusOK)
		flusher.Flush()
		_, _ = w.Write([]byte("12345678901"))
	}))
	defer server.Close()

	path, meta, err := manager.Download(context.Background(), server.URL, "clip.mp4", 10)
	if err == nil {
		t.Fatal("expected streaming oversized error")
	}
	if !strings.Contains(err.Error(), "file exceeds max size") {
		t.Fatalf("expected file exceeds max size error, got %v", err)
	}
	if path != "" {
		t.Fatalf("expected no path, got %q", path)
	}
	if meta != (Metadata{}) {
		t.Fatalf("expected empty metadata, got %#v", meta)
	}
	requireNoFiles(t, manager.Dir())
}

func TestDownloadLeavesFinalFileWhenProbeFailsAndCleansTempFile(t *testing.T) {
	manager, err := NewManager(t.TempDir(), fakeFFProbe(t, "exit 7\n"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("video"))
	}))
	defer server.Close()

	path, meta, err := manager.Download(context.Background(), server.URL, "clip.mp4", 10)
	if err == nil {
		t.Fatal("expected probe error")
	}
	if !strings.Contains(err.Error(), "ffprobe failed") {
		t.Fatalf("expected ffprobe failure, got %v", err)
	}
	if path == "" {
		t.Fatal("expected final path on probe failure")
	}
	if meta.SizeBytes != 5 {
		t.Fatalf("expected size metadata from download, got %#v", meta)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected final file to remain: %v", err)
	}
	requireNoTmpFiles(t, manager.Dir())
}

func TestProbeRoundsDuration(t *testing.T) {
	manager, err := NewManager(t.TempDir(), fakeFFProbe(t, "printf '%s\\n' '{\"format\":{\"duration\":\"1.6\"}}'\n"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	path := writeMediaFile(t, manager.Dir(), "clip.mp4", "video")

	meta, err := manager.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.SizeBytes != 5 {
		t.Fatalf("expected size 5, got %d", meta.SizeBytes)
	}
	if meta.DurationSeconds != 2 {
		t.Fatalf("expected rounded duration 2, got %d", meta.DurationSeconds)
	}
}

func TestProbeRejectsInvalidJSON(t *testing.T) {
	manager, err := NewManager(t.TempDir(), fakeFFProbe(t, "printf '%s\\n' 'not-json'\n"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	path := writeMediaFile(t, manager.Dir(), "clip.mp4", "video")

	if _, err := manager.Probe(context.Background(), path); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestProbeReturnsFFProbeFailure(t *testing.T) {
	manager, err := NewManager(t.TempDir(), fakeFFProbe(t, "exit 7\n"))
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	path := writeMediaFile(t, manager.Dir(), "clip.mp4", "video")

	if _, err := manager.Probe(context.Background(), path); err == nil || !strings.Contains(err.Error(), "ffprobe failed") {
		t.Fatalf("expected ffprobe failure, got %v", err)
	}
}

func TestValidateRejectsEmptyOversizedAndLongVideos(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}

	if err := manager.Validate(Metadata{SizeBytes: 0}, 100, 10); err == nil {
		t.Fatal("expected empty file rejection")
	}
	if err := manager.Validate(Metadata{SizeBytes: 101}, 100, 10); err == nil {
		t.Fatal("expected oversized file rejection")
	}
	if err := manager.Validate(Metadata{SizeBytes: 50, DurationSeconds: 11}, 100, 10); err == nil {
		t.Fatal("expected long video rejection")
	}
	if err := manager.Validate(Metadata{SizeBytes: 50, DurationSeconds: 10}, 100, 10); err != nil {
		t.Fatalf("expected valid metadata: %v", err)
	}
}

func TestDiskUsage(t *testing.T) {
	manager, err := NewManager(t.TempDir(), "")
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	usage, err := manager.DiskUsage()
	if err != nil {
		t.Fatalf("disk usage: %v", err)
	}
	if usage.TotalBytes == 0 || usage.AvailableBytes == 0 {
		t.Fatalf("unexpected disk usage: %#v", usage)
	}
}

func fakeFFProbe(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\n" + body
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	return path
}

func writeMediaFile(t *testing.T, dir, name, contents string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write media file: %v", err)
	}
	return path
}

func requireNoFiles(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read media dir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected media dir to be empty, found %d files", len(entries))
	}
}

func requireNoTmpFiles(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, "download-*.tmp"))
	if err != nil {
		t.Fatalf("glob tmp files: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected no tmp files, found %v", matches)
	}
}
