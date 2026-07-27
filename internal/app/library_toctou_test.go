//go:build darwin || linux

package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestLibraryImportRejectsFinalSymlinkEscape(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	outsidePath := filepath.Join(t.TempDir(), "outside.mp4")
	if err := os.WriteFile(outsidePath, []byte("outside-media"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	sourcePath := filepath.Join(svc.cfg.TelegramBotAPIDir, "source.mp4")
	if err := os.Symlink(outsidePath, sourcePath); err != nil {
		t.Fatalf("create final symlink: %v", err)
	}

	_, err := svc.ImportLibraryUpload(context.Background(), UploadRequest{
		LocalPath: sourcePath,
		FileName:  "music_final_escape.mp3",
		SizeBytes: int64(len("outside-media")),
	})
	if err == nil {
		t.Fatal("expected final symlink rejection")
	}
	if fileExists(filepath.Join(svc.cfg.MusicMediaDir, "music_final_escape.mp3")) {
		t.Fatal("final symlink escape was imported")
	}
	requireNoOwnedImportTemps(t, svc.cfg.MusicMediaDir)
}

func TestLibraryImportRejectsParentSymlinkEscape(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	outsideDir := t.TempDir()
	outsidePath := filepath.Join(outsideDir, "outside.mp4")
	if err := os.WriteFile(outsidePath, []byte("outside-media"), 0o600); err != nil {
		t.Fatalf("write outside file: %v", err)
	}
	parentPath := filepath.Join(svc.cfg.TelegramBotAPIDir, "parent")
	if err := os.Symlink(outsideDir, parentPath); err != nil {
		t.Fatalf("create parent symlink: %v", err)
	}

	_, err := svc.ImportLibraryUpload(context.Background(), UploadRequest{
		LocalPath: filepath.Join(parentPath, "outside.mp4"),
		FileName:  "music_parent_escape.mp3",
		SizeBytes: int64(len("outside-media")),
	})
	if err == nil {
		t.Fatal("expected parent symlink rejection")
	}
	if fileExists(filepath.Join(svc.cfg.MusicMediaDir, "music_parent_escape.mp3")) {
		t.Fatal("parent symlink escape was imported")
	}
	requireNoOwnedImportTemps(t, svc.cfg.MusicMediaDir)
}

func TestLibraryImportCopyKeepsSecurelyOpenedSourceIdentity(t *testing.T) {
	cacheDir := t.TempDir()
	sourcePath := filepath.Join(cacheDir, "source.mp4")
	original := []byte("original-media")
	if err := os.WriteFile(sourcePath, original, 0o600); err != nil {
		t.Fatalf("write original source: %v", err)
	}
	source, err := openLocalBotAPIFile(cacheDir, sourcePath)
	if err != nil {
		t.Fatalf("securely open original source: %v", err)
	}
	defer source.Close()

	replacementPath := filepath.Join(t.TempDir(), "replacement.mp4")
	if err := os.WriteFile(replacementPath, []byte("replacement!!"), 0o600); err != nil {
		t.Fatalf("write replacement source: %v", err)
	}
	if err := os.Remove(sourcePath); err != nil {
		t.Fatalf("remove original pathname: %v", err)
	}
	if err := os.Symlink(replacementPath, sourcePath); err != nil {
		t.Fatalf("replace source with symlink: %v", err)
	}

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "imported.mp4")
	if _, err := copyOpenedFileAtomic(context.Background(), destPath, source, 1024, nil, nil); err != nil {
		t.Fatalf("copy securely opened source: %v", err)
	}
	got, err := os.ReadFile(destPath)
	if err != nil {
		t.Fatalf("read imported file: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("imported bytes = %q, want securely opened bytes %q", got, original)
	}
}

func TestOpenedLibraryCopyCancellationCleansTempAndLeavesSourceOpen(t *testing.T) {
	cacheDir := t.TempDir()
	sourcePath := filepath.Join(cacheDir, "source.mp4")
	if err := os.WriteFile(sourcePath, []byte("source-media"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	source, err := openLocalBotAPIFile(cacheDir, sourcePath)
	if err != nil {
		t.Fatalf("securely open source: %v", err)
	}
	t.Cleanup(func() {
		_ = source.Close()
	})

	destDir := t.TempDir()
	destPath := filepath.Join(destDir, "imported.mp4")
	ctx, cancel := context.WithCancel(context.Background())
	_, err = copyOpenedFileAtomicWith(ctx, destPath, source, 1024, nil, nil, atomicCopyOps{
		copyData: func(copyCtx context.Context, dst io.Writer, src io.Reader) (int64, error) {
			written, copyErr := io.CopyN(dst, src, 1)
			if copyErr != nil {
				return written, copyErr
			}
			cancel()
			return written, copyCtx.Err()
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("copy error = %v, want context cancellation", err)
	}
	if fileExists(destPath) {
		t.Fatal("destination exists after canceled copy")
	}
	requireNoOwnedImportTemps(t, destDir)
	if _, err := source.Stat(); err != nil {
		t.Fatalf("copy closed caller-owned source: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close source: %v", err)
	}
	if _, err := source.Stat(); err == nil {
		t.Fatal("closed source descriptor remained usable")
	}
}
