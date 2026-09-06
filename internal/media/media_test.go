package media

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiwb/tg-obs-bot/internal/liveness"
)

func TestProbeRecordsOnlyEntryAndExitBoundaries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.mp4")
	if err := os.WriteFile(path, []byte("media"), 0o600); err != nil {
		t.Fatalf("write media: %v", err)
	}
	manager := NewManager(fakeFFProbe(t, "printf '%s\\n' '{\"format\":{\"duration\":\"1\"}}'\n"))
	registry := liveness.NewRegistry(liveness.Options{})
	worker, err := registry.Bind(liveness.WorkerTelegram, liveness.OwnerTelegram)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	worker.Advance(liveness.PhaseOperation)

	if _, err := manager.Probe(liveness.WithWorker(context.Background(), worker), path); err != nil {
		t.Fatalf("Probe: %v", err)
	}
	snapshot := worker.Snapshot()
	if snapshot.Phase != liveness.PhaseOperation || snapshot.Sequence != 3 {
		t.Fatalf("probe snapshot = %+v, want entry plus restored normal phase", snapshot)
	}
}

func TestProbeRoundsDurationUp(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(fakeFFProbe(t, "printf '%s\\n' '{\"format\":{\"duration\":\"1.01\"},\"streams\":[{\"codec_type\":\"video\"},{\"codec_type\":\"audio\"}]}'\n"))
	path := writeMediaFile(t, dir, "clip.mp4", "video")

	meta, err := manager.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if meta.SizeBytes != 5 {
		t.Fatalf("expected size 5, got %d", meta.SizeBytes)
	}
	if meta.DurationSeconds != 2 {
		t.Fatalf("expected ceiling duration 2, got %d", meta.DurationSeconds)
	}
	if !meta.HasVideoStream || !meta.HasAudioStream {
		t.Fatalf("stream metadata = %#v, want video and audio", meta)
	}
}

func TestValidateFailsClosedForUnavailableOrInvalidDuration(t *testing.T) {
	tests := []string{"", "not-a-number", "0", "-1", "NaN", "+Inf", "1e100"}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			body := "printf '%s\\n' '{\"format\":{}}'\n"
			if raw != "" {
				body = "printf '%s\\n' '{\"format\":{\"duration\":\"" + raw + "\"}}'\n"
			}
			dir := t.TempDir()
			manager := NewManager(fakeFFProbe(t, body))
			path := writeMediaFile(t, dir, "clip.mp4", "video")
			meta, err := manager.Probe(context.Background(), path)
			if err != nil {
				t.Fatalf("probe: %v", err)
			}
			if meta.DurationSeconds != 0 {
				t.Fatalf("duration = %d, want unavailable", meta.DurationSeconds)
			}
			if err := manager.Validate(meta, 100, 10); err == nil || !strings.Contains(err.Error(), "duration is unavailable or invalid") {
				t.Fatalf("validate error = %v, want fail-closed duration error", err)
			}
			if err := manager.Validate(meta, 100, 0); err != nil {
				t.Fatalf("disabled duration limit should accept metadata: %v", err)
			}
		})
	}
}

func TestValidateFailsClosedWhenFFProbeIsDisabledAndDurationLimitIsEnabled(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager("")
	path := writeMediaFile(t, dir, "clip.mp4", "video")
	meta, err := manager.Probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if err := manager.Validate(meta, 100, 10); err == nil || !strings.Contains(err.Error(), "duration is unavailable or invalid") {
		t.Fatalf("validate error = %v, want fail-closed duration error", err)
	}
}

func TestProbeRejectsInvalidJSON(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(fakeFFProbe(t, "printf '%s\\n' 'not-json'\n"))
	path := writeMediaFile(t, dir, "clip.mp4", "video")

	if _, err := manager.Probe(context.Background(), path); err == nil {
		t.Fatal("expected invalid JSON error")
	}
}

func TestProbeReturnsFFProbeFailure(t *testing.T) {
	dir := t.TempDir()
	manager := NewManager(fakeFFProbe(t, "exit 7\n"))
	path := writeMediaFile(t, dir, "clip.mp4", "video")

	if _, err := manager.Probe(context.Background(), path); err == nil || !strings.Contains(err.Error(), "ffprobe failed") {
		t.Fatalf("expected ffprobe failure, got %v", err)
	}
}

func TestValidateRejectsEmptyOversizedAndLongVideos(t *testing.T) {
	manager := NewManager("")

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

func TestValidateVideoAndAudioRequireMatchingStreams(t *testing.T) {
	manager := NewManager("")
	base := Metadata{SizeBytes: 50, DurationSeconds: 10}
	if err := manager.ValidateVideo(base, 100, 10); err == nil || !strings.Contains(err.Error(), "video stream") {
		t.Fatalf("video validation error = %v", err)
	}
	if err := manager.ValidateAudio(base, 100, 10); err == nil || !strings.Contains(err.Error(), "audio stream") {
		t.Fatalf("audio validation error = %v", err)
	}
	if err := manager.ValidateVideo(
		Metadata{SizeBytes: 50, DurationSeconds: 10, HasVideoStream: true},
		100,
		10,
	); err != nil {
		t.Fatalf("valid video metadata: %v", err)
	}
	if err := manager.ValidateAudio(
		Metadata{SizeBytes: 50, DurationSeconds: 10, HasAudioStream: true},
		100,
		10,
	); err != nil {
		t.Fatalf("valid audio metadata: %v", err)
	}
}

func TestDiskUsage(t *testing.T) {
	usage, err := DiskUsageForPath(t.TempDir())
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
