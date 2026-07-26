package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/media"
)

func TestQueueAdmissionUsesActualSizeAndSourceFilesystem(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
		MinFreeDiskBytes:        100,
	})
	source := writeBotAPIFile(t, svc, "actual-size.mp4")
	var probedPath string
	svc.diskUsage = func(path string) (media.DiskUsage, error) {
		probedPath = path
		return media.DiskUsage{AvailableBytes: 104}, nil
	}

	_, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        source,
		TelegramFileID:   "actual",
		TelegramUniqueID: "actual",
		FileName:         "actual-size.mp4",
		SizeBytes:        1,
	})
	if err == nil || !strings.Contains(err.Error(), "磁碟可用空間不足") {
		t.Fatalf("enqueue error = %v, want low-space rejection based on actual 5-byte file", err)
	}
	if probedPath != source {
		t.Fatalf("disk probe path = %q, want source filesystem path %q", probedPath, source)
	}
	if length, lengthErr := svc.store.QueueLength(ctx); lengthErr != nil {
		t.Fatalf("queue length: %v", lengthErr)
	} else if length != 0 {
		t.Fatalf("queue length = %d, want no AddDownloading before admission", length)
	}
	if !fileExists(source) {
		t.Fatal("low-space rejection deleted Telegram-owned input")
	}
}

func TestQueueAdmissionIgnoresDeclaredSizeAndStoresActualSize(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
		MinFreeDiskBytes:        100,
	})
	source := writeBotAPIFile(t, svc, "declared-size.mp4")
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		return media.DiskUsage{AvailableBytes: 105}, nil
	}

	video, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        source,
		TelegramFileID:   "declared",
		TelegramUniqueID: "declared",
		FileName:         "declared-size.mp4",
		SizeBytes:        1 << 30,
	})
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if video.SizeBytes != 5 {
		t.Fatalf("stored size = %d, want actual size 5", video.SizeBytes)
	}
}

func TestZeroDiskReserveStillRequiresActualFileSize(t *testing.T) {
	ctx := context.Background()
	svc, _, _ := newLocalUploadTestService(t, config.Config{
		MaxVideoSizeBytes:       1024,
		MaxVideoDurationSeconds: 120,
		MinFreeDiskBytes:        0,
	})
	source := writeBotAPIFile(t, svc, "zero-reserve.mp4")
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		return media.DiskUsage{AvailableBytes: 4}, nil
	}

	_, err := svc.EnqueueUpload(ctx, UploadRequest{
		LocalPath:        source,
		TelegramFileID:   "zero-reserve",
		TelegramUniqueID: "zero-reserve",
		FileName:         "zero-reserve.mp4",
	})
	if err == nil || !strings.Contains(err.Error(), "磁碟可用空間不足") {
		t.Fatalf("enqueue error = %v, want actual-size admission with zero reserve", err)
	}
	if length, lengthErr := svc.store.QueueLength(ctx); lengthErr != nil {
		t.Fatalf("queue length: %v", lengthErr)
	} else if length != 0 {
		t.Fatalf("queue length = %d, want no row after admission failure", length)
	}
}

func TestLibraryAdmissionUsesActualSizeAndDestinationFilesystem(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.cfg.MinFreeDiskBytes = 100
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_lowspace.mp4")
	var paths []string
	svc.diskUsage = func(path string) (media.DiskUsage, error) {
		paths = append(paths, path)
		return media.DiskUsage{AvailableBytes: 104}, nil
	}

	_, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_lowspace.mp4",
		SizeBytes: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "磁碟可用空間不足") {
		t.Fatalf("import error = %v, want destination low-space rejection", err)
	}
	if len(paths) != 1 || filepath.Clean(paths[0]) != filepath.Clean(svc.cfg.LoopMediaDir) {
		t.Fatalf("disk probe paths = %q, want destination directory only", paths)
	}
	if !fileExists(source) {
		t.Fatal("library low-space rejection deleted Telegram-owned input")
	}
	if fileExists(filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_lowspace.mp4")) {
		t.Fatal("library destination exists after rejected admission")
	}
	requireNoOwnedImportTemps(t, svc.cfg.LoopMediaDir)
}

func TestLibraryMusicImportEnforcesOpenedSourceActualSize(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.cfg.MaxVideoSizeBytes = 4
	source := writeBotAPIFile(t, svc, "music_oversized.mp3")
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		t.Fatal("oversized opened source should fail before disk admission")
		return media.DiskUsage{}, nil
	}

	_, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "music_oversized.mp3",
		SizeBytes: 1,
	})
	if err == nil || !strings.Contains(err.Error(), "檔案太大") {
		t.Fatalf("import error = %v, want opened-source actual-size rejection", err)
	}
	if !fileExists(source) {
		t.Fatal("oversized music rejection deleted Telegram-owned input")
	}
	if fileExists(filepath.Join(svc.cfg.MusicMediaDir, "music_oversized.mp3")) {
		t.Fatal("oversized music was published")
	}
	requireNoOwnedImportTemps(t, svc.cfg.MusicMediaDir)
}

func TestRequiredAvailableBytesRejectsOverflow(t *testing.T) {
	if _, err := requiredAvailableBytes(math.MaxUint64, 1); err == nil {
		t.Fatal("expected overflow rejection")
	}
	if got, err := requiredAvailableBytes(5, 100); err != nil || got != 105 {
		t.Fatalf("required bytes = %d err=%v, want 105", got, err)
	}
}

func TestAtomicLibraryCopyCleansTempOnCancellation(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)
	ctx, cancel := context.WithCancel(context.Background())

	_, err := copyFileAtomicWith(ctx, dest, source, 1024, nil, nil, atomicCopyOps{
		copyData: func(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
			written, err := io.CopyN(dst, src, 1)
			if err != nil {
				return written, err
			}
			cancel()
			return written, ctx.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy error = %v, want context cancellation", err)
	}
	if fileExists(dest) {
		t.Fatal("destination exists after canceled copy")
	}
	if !fileExists(source) {
		t.Fatal("canceled copy deleted source")
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestAtomicLibraryCopyCleansTempOnENOSPC(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)

	_, err := copyFileAtomicWith(context.Background(), dest, source, 1024, nil, nil, atomicCopyOps{
		copyData: func(_ context.Context, dst io.Writer, _ io.Reader) (int64, error) {
			written, writeErr := dst.Write([]byte("x"))
			if writeErr != nil {
				return int64(written), writeErr
			}
			return int64(written), syscall.ENOSPC
		},
	})
	if !errors.Is(err, syscall.ENOSPC) {
		t.Fatalf("copy error = %v, want ENOSPC", err)
	}
	if fileExists(dest) {
		t.Fatal("destination exists after ENOSPC")
	}
	if !fileExists(source) {
		t.Fatal("ENOSPC copy deleted source")
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestAtomicLibraryCopyPublishesOnlyCompleteFile(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	if err := os.WriteFile(source, []byte("complete-media"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	if _, err := copyFileAtomic(context.Background(), dest, source, 1024, nil, nil); err != nil {
		t.Fatalf("copy: %v", err)
	}
	body, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read destination: %v", err)
	}
	if string(body) != "complete-media" {
		t.Fatalf("destination = %q, want complete media", body)
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestAtomicLibraryCopySyncsStagingAndDestinationDirectories(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)
	var synced []string

	_, err := copyFileAtomicWith(context.Background(), dest, source, 1024, nil, nil, atomicCopyOps{
		syncDirectory: func(path string) error {
			synced = append(synced, filepath.Clean(path))
			return nil
		},
	})
	if err != nil {
		t.Fatalf("copy: %v", err)
	}
	want := map[string]bool{
		filepath.Join(filepath.Clean(dir), libraryStagingDirName): false,
		filepath.Clean(dir): false,
	}
	for _, path := range synced {
		if _, ok := want[path]; ok {
			want[path] = true
		}
	}
	for path, seen := range want {
		if !seen {
			t.Fatalf("directory %s was not synchronized; calls=%v", path, synced)
		}
	}
}

func TestAtomicLibraryCopyRejectsSourceGrowthBeyondOpenedSize(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)
	var admittedSize int64

	_, err := copyFileAtomicWith(context.Background(), dest, source, 5, func(actualSize int64) error {
		admittedSize = actualSize
		return nil
	}, nil, atomicCopyOps{
		copyData: func(_ context.Context, dst io.Writer, _ io.Reader) (int64, error) {
			written, writeErr := dst.Write([]byte("123456"))
			return int64(written), writeErr
		},
	})
	if err == nil || !strings.Contains(err.Error(), "檔案太大") {
		t.Fatalf("copy error = %v, want hard max-size rejection", err)
	}
	if admittedSize != 5 {
		t.Fatalf("admitted size = %d, want opened-FD fstat size 5", admittedSize)
	}
	if fileExists(dest) {
		t.Fatal("destination exists after source-growth rejection")
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestAtomicLibraryCopyReportsFailedRollbackAfterDirectorySync(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)
	syncErr := errors.New("directory sync failed")
	removeErr := errors.New("rollback remove failed")

	_, err := copyFileAtomicWith(context.Background(), dest, source, 1024, nil, nil, atomicCopyOps{
		syncDirectory: func(string) error { return syncErr },
		removeFile:    func(string) error { return removeErr },
	})
	if !errors.Is(err, syncErr) || !errors.Is(err, removeErr) {
		t.Fatalf("copy error = %v, want joined sync and rollback errors", err)
	}
	if !fileExists(dest) {
		t.Fatal("injected rollback failure should leave published file visible")
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestAtomicLibraryCopyValidationFailureNeverPublishesFinal(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)
	validationErr := errors.New("invalid staged media")
	validatorCalled := false

	_, err := copyFileAtomic(context.Background(), dest, source, 1024, nil, func(_ context.Context, stagingPath string) error {
		validatorCalled = true
		wantStagingDir := filepath.Join(filepath.Clean(dir), libraryStagingDirName)
		if filepath.Clean(filepath.Dir(stagingPath)) != wantStagingDir {
			t.Fatalf("staging path = %q, want dedicated dir %q", stagingPath, wantStagingDir)
		}
		if fileExists(dest) {
			t.Fatal("final destination was visible before validation")
		}
		if !fileExists(stagingPath) {
			t.Fatal("validator did not receive the closed staging file")
		}
		return validationErr
	})
	if !errors.Is(err, validationErr) {
		t.Fatalf("copy error = %v, want validation failure", err)
	}
	if !validatorCalled {
		t.Fatal("pre-publish validator was not called")
	}
	if fileExists(dest) {
		t.Fatal("validation failure published a final file")
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestLibraryLoopValidationFailureNeverPublishesFinal(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	probePath := filepath.Join(t.TempDir(), "ffprobe")
	if err := os.WriteFile(probePath, []byte("#!/bin/sh\nprintf '%s\\n' '{\"format\":{}}'\n"), 0o700); err != nil {
		t.Fatalf("write durationless ffprobe: %v", err)
	}
	manager, err := media.NewManager(svc.cfg.MediaDir, probePath)
	if err != nil {
		t.Fatalf("new media manager: %v", err)
	}
	svc.media = manager
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_invalid.mp4")
	dest := filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_invalid.mp4")

	_, err = svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_invalid.mp4",
	})
	if err == nil || !strings.Contains(err.Error(), "duration is unavailable or invalid") {
		t.Fatalf("import error = %v, want fail-closed staging validation", err)
	}
	if fileExists(dest) {
		t.Fatal("invalid loop was published")
	}
	if !fileExists(source) {
		t.Fatal("invalid loop import deleted Telegram-owned source")
	}
	requireNoOwnedImportTemps(t, svc.cfg.LoopMediaDir)
}

func TestStaleImportSweepRemovesOnlyOwnedOldRegularTemps(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	now := time.Now().UTC().Truncate(time.Second)
	svc.now = func() time.Time { return now }
	old := now.Add(-staleLibraryImportAge - time.Minute)
	fresh := now.Add(-staleLibraryImportAge + time.Minute)

	stagingDir, err := ensureLibraryStagingDir(svc.cfg.LoopMediaDir)
	if err != nil {
		t.Fatalf("create staging dir: %v", err)
	}
	oldOwned := filepath.Join(stagingDir, libraryImportTempPrefix+"old"+libraryImportTempSuffix)
	freshOwned := filepath.Join(stagingDir, libraryImportTempPrefix+"fresh"+libraryImportTempSuffix)
	oldUnowned := filepath.Join(stagingDir, libraryImportTempPrefix+"old.media")
	oldOwnedDir := filepath.Join(stagingDir, libraryImportTempPrefix+"dir"+libraryImportTempSuffix)
	oldOwnedLink := filepath.Join(stagingDir, libraryImportTempPrefix+"link"+libraryImportTempSuffix)
	oldRootMatch := filepath.Join(svc.cfg.LoopMediaDir, libraryImportTempPrefix+"root"+libraryImportTempSuffix)
	linkTarget := filepath.Join(t.TempDir(), "link-target")
	for _, path := range []string{oldOwned, freshOwned, oldUnowned, oldRootMatch} {
		if err := os.WriteFile(path, []byte("staging"), 0o600); err != nil {
			t.Fatalf("write temp %s: %v", path, err)
		}
	}
	if err := os.Mkdir(oldOwnedDir, 0o700); err != nil {
		t.Fatalf("mkdir owned-looking temp: %v", err)
	}
	if err := os.WriteFile(linkTarget, []byte("target"), 0o600); err != nil {
		t.Fatalf("write symlink target: %v", err)
	}
	if err := os.Symlink(linkTarget, oldOwnedLink); err != nil {
		t.Fatalf("create owned-looking symlink: %v", err)
	}
	for _, path := range []string{oldOwned, oldUnowned, oldOwnedDir, oldRootMatch} {
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age %s: %v", path, err)
		}
	}
	if err := os.Chtimes(freshOwned, fresh, fresh); err != nil {
		t.Fatalf("age fresh temp: %v", err)
	}

	if err := svc.sweepStaleLibraryImportTemps(context.Background()); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if fileExists(oldOwned) {
		t.Fatal("old owned import temp was not removed")
	}
	for _, path := range []string{freshOwned, oldUnowned, oldOwnedDir, oldOwnedLink, oldRootMatch} {
		if !fileExists(path) {
			t.Fatalf("sweep removed protected path %s", path)
		}
	}
}

func TestStaleImportSweepIsBoundedAndConvergesAcrossBatches(t *testing.T) {
	destDir := t.TempDir()
	stagingDir, err := ensureLibraryStagingDir(destDir)
	if err != nil {
		t.Fatalf("create staging dir: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	old := now.Add(-staleLibraryImportAge - time.Minute)
	const staleCount = 600
	for i := 0; i < staleCount; i++ {
		path := filepath.Join(stagingDir, fmt.Sprintf("%s%04d%s", libraryImportTempPrefix, i, libraryImportTempSuffix))
		if err := os.WriteFile(path, []byte("staging"), 0o600); err != nil {
			t.Fatalf("write stale temp %d: %v", i, err)
		}
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("age stale temp %d: %v", i, err)
		}
	}

	expected := []stagingSweepStats{
		{Inspected: 256, Deleted: 256},
		{Inspected: 256, Deleted: 256},
		{Inspected: 88, Deleted: 88},
	}
	for i, want := range expected {
		got, err := sweepStaleLibraryImportTempsInDir(context.Background(), stagingDir, now.Add(-staleLibraryImportAge), stagingSweepBatchSize)
		if err != nil {
			t.Fatalf("sweep batch %d: %v", i, err)
		}
		if got != want {
			t.Fatalf("sweep batch %d stats = %#v, want %#v", i, got, want)
		}
		if got.Inspected > stagingSweepBatchSize || got.Deleted > stagingSweepBatchSize {
			t.Fatalf("sweep batch %d exceeded bound: %#v", i, got)
		}
	}
	final, err := sweepStaleLibraryImportTempsInDir(context.Background(), stagingDir, now.Add(-staleLibraryImportAge), stagingSweepBatchSize)
	if err != nil {
		t.Fatalf("final sweep: %v", err)
	}
	if final != (stagingSweepStats{}) {
		t.Fatalf("final sweep stats = %#v, want empty", final)
	}
}

func TestStartupAndPeriodicMaintenanceSweepStaleImportTemps(t *testing.T) {
	tests := []struct {
		name string
		run  func(context.Context, *Service) error
	}{
		{name: "startup", run: func(ctx context.Context, svc *Service) error { return svc.recoverStartupState(ctx) }},
		{name: "periodic", run: func(ctx context.Context, svc *Service) error { return svc.performMaintenance(ctx) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, _ := newLibraryTestService(t)
			now := time.Now().UTC().Truncate(time.Second)
			svc.now = func() time.Time { return now }
			stagingDir, err := ensureLibraryStagingDir(svc.cfg.MusicMediaDir)
			if err != nil {
				t.Fatalf("create staging dir: %v", err)
			}
			path := filepath.Join(stagingDir, libraryImportTempPrefix+tt.name+libraryImportTempSuffix)
			if err := os.WriteFile(path, []byte("staging"), 0o600); err != nil {
				t.Fatalf("write stale temp: %v", err)
			}
			old := now.Add(-staleLibraryImportAge - time.Second)
			if err := os.Chtimes(path, old, old); err != nil {
				t.Fatalf("age stale temp: %v", err)
			}
			if err := tt.run(context.Background(), svc); err != nil {
				t.Fatalf("%s maintenance: %v", tt.name, err)
			}
			if fileExists(path) {
				t.Fatalf("%s maintenance left stale temp", tt.name)
			}
		})
	}
}

func requireNoOwnedImportTemps(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, libraryStagingDirName, libraryImportTempPrefix+"*"+libraryImportTempSuffix))
	if err != nil {
		t.Fatalf("glob owned import temps: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("owned import temps remain: %v", matches)
	}
}
