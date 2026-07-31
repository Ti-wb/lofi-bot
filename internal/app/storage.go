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
	"time"

	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/media"
)

const (
	libraryStagingDirName   = ".tg-obs-bot-staging"
	libraryImportTempPrefix = "import-"
	libraryImportTempSuffix = ".tmp"
	staleLibraryImportAge   = 6 * time.Hour
	stagingSweepBatchSize   = 256
	copyBufferSize          = 256 * 1024
	libraryMediaFileMode    = 0o644
)

type copyDataFunc func(context.Context, io.Writer, io.Reader) (int64, error)
type prePublishFunc func(context.Context, string) error
type publishNoReplaceFunc func(string, string) error

type stagingSweepStats struct {
	Inspected int
	Deleted   int
}

type atomicCopyOps struct {
	copyData      copyDataFunc
	publishFile   publishNoReplaceFunc
	syncDirectory func(string) error
	removeFile    func(string) error
}

func (s *Service) preflightUpload(ctx context.Context, fileName string, declaredSize int64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if declaredSize <= 0 {
		return publicError("無法確認檔案大小，請重新上傳。")
	}
	return s.preflightLibraryUpload(ctx, fileName, declaredSize)
}

func (s *Service) ensureStorageHeadroom(path string, incomingBytes int64) error {
	if incomingBytes < 0 {
		return errors.New("incoming media size is invalid")
	}
	return s.ensureStorageHeadroomBytes(path, uint64(incomingBytes))
}

func (s *Service) ensureStorageHeadroomBytes(path string, incomingBytes uint64) error {
	if s.cfg.MinFreeDiskBytes < 0 {
		return errors.New("minimum free disk reserve is invalid")
	}
	required, err := requiredAvailableBytes(incomingBytes, uint64(s.cfg.MinFreeDiskBytes))
	if err != nil {
		return err
	}
	probe := s.diskUsage
	if probe == nil {
		probe = media.DiskUsageForPath
	}
	usage, err := probe(path)
	if err != nil {
		return fmt.Errorf("inspect media filesystem capacity: %w", err)
	}
	if usage.AvailableBytes < required {
		return publicError("磁碟可用空間不足，請先清理空間再重試。")
	}
	return nil
}

func pathsShareFilesystem(left, right string) (bool, error) {
	var leftStat syscall.Stat_t
	if err := syscall.Stat(left, &leftStat); err != nil {
		return false, fmt.Errorf("inspect media filesystem: %w", err)
	}
	var rightStat syscall.Stat_t
	if err := syscall.Stat(right, &rightStat); err != nil {
		return false, fmt.Errorf("inspect media filesystem: %w", err)
	}
	return leftStat.Dev == rightStat.Dev, nil
}

func requiredAvailableBytes(incomingBytes, reserveBytes uint64) (uint64, error) {
	if incomingBytes > math.MaxUint64-reserveBytes {
		return 0, errors.New("required media disk headroom overflows")
	}
	return incomingBytes + reserveBytes, nil
}

func copyFileAtomic(
	ctx context.Context,
	dst string,
	src string,
	maxBytes int64,
	admit func(int64) error,
	prePublish prePublishFunc,
) (int64, error) {
	return copyFileAtomicWith(ctx, dst, src, maxBytes, admit, prePublish, atomicCopyOps{})
}

func copyFileAtomicWith(
	ctx context.Context,
	dst string,
	src string,
	maxBytes int64,
	admit func(int64) error,
	prePublish prePublishFunc,
	ops atomicCopyOps,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	return copyOpenedFileAtomicWith(ctx, dst, in, maxBytes, admit, prePublish, ops)
}

func copyOpenedFileAtomic(
	ctx context.Context,
	dst string,
	src *os.File,
	maxBytes int64,
	admit func(int64) error,
	prePublish prePublishFunc,
) (int64, error) {
	return copyOpenedFileAtomicWith(ctx, dst, src, maxBytes, admit, prePublish, atomicCopyOps{})
}

// copyOpenedFileAtomicWith copies from the exact file identity supplied by the
// caller. Ownership of src remains with the caller.
func copyOpenedFileAtomicWith(
	ctx context.Context,
	dst string,
	src *os.File,
	maxBytes int64,
	admit func(int64) error,
	prePublish prePublishFunc,
	ops atomicCopyOps,
) (int64, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if src == nil {
		return 0, errors.New("source media file is required")
	}
	info, err := src.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return 0, publicError("素材檔案必須是非空的一般檔案。")
	}
	if maxBytes <= 0 {
		return 0, errors.New("maximum media size is invalid")
	}
	if info.Size() > maxBytes {
		return 0, publicError(fmt.Sprintf("檔案太大，上限是 %s", formatBytes(maxBytes)))
	}
	if admit != nil {
		if err := admit(info.Size()); err != nil {
			return 0, err
		}
	}

	destDir := filepath.Dir(dst)
	stagingDir, err := ensureLibraryStagingDir(destDir)
	if err != nil {
		return 0, err
	}
	tmp, err := os.CreateTemp(stagingDir, libraryImportTempPrefix+"*"+libraryImportTempSuffix)
	if err != nil {
		return 0, err
	}
	tmpPath := tmp.Name()
	cleanupTemp := true
	defer func() {
		if cleanupTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	copyData := ops.copyData
	if copyData == nil {
		copyData = copyDataWithContext
	}
	tracker := liveness.WorkerFromContext(ctx)
	// A one-byte fence detects growth after fstat without allowing a mutable
	// source to consume unbounded destination space.
	var written int64
	func() {
		copyScope := tracker.Scope(liveness.PhaseMediaCopy)
		defer copyScope.Close()
		written, err = copyData(ctx, tmp, io.LimitReader(src, info.Size()+1))
	}()
	if err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if written != info.Size() {
		_ = tmp.Close()
		if written > maxBytes {
			return 0, publicError(fmt.Sprintf("檔案太大，上限是 %s", formatBytes(maxBytes)))
		}
		return 0, errors.New("source media changed while it was being copied")
	}
	if err := ctx.Err(); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	// The staging directory remains private, but the published media must be
	// readable by OBS when it runs as a different OS user or container UID.
	// Change the mode through the still-open file descriptor so the permission
	// applies to the exact validated file identity that will be published.
	if err := tmp.Chmod(libraryMediaFileMode); err != nil {
		_ = tmp.Close()
		return 0, fmt.Errorf("make imported library media readable: %w", err)
	}
	var fileSyncErr error
	func() {
		syncScope := tracker.Scope(liveness.PhaseDurabilitySync)
		defer syncScope.Close()
		fileSyncErr = tmp.Sync()
	}()
	if fileSyncErr != nil {
		_ = tmp.Close()
		return 0, fileSyncErr
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if prePublish != nil {
		if err := prePublish(ctx, tmpPath); err != nil {
			return 0, err
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	publish := ops.publishFile
	if publish == nil {
		publish = publishFileNoReplace
	}
	if err := publish(tmpPath, dst); err != nil {
		return 0, err
	}
	cleanupTemp = false
	syncDir := ops.syncDirectory
	if syncDir == nil {
		syncDir = syncDirectory
	}
	syncScope := tracker.Scope(liveness.PhaseDurabilitySync)
	defer syncScope.Close()
	if syncErr := syncDirectories(syncDir, stagingDir, destDir); syncErr != nil {
		remove := ops.removeFile
		if remove == nil {
			remove = os.Remove
		}
		removeErr := remove(dst)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		rollbackSyncErr := syncDirectories(syncDir, stagingDir, destDir)
		if removeErr != nil {
			removeErr = fmt.Errorf("rollback published library file: %w", removeErr)
		}
		if rollbackSyncErr != nil {
			rollbackSyncErr = fmt.Errorf("sync library directories after rollback: %w", rollbackSyncErr)
		}
		return 0, errors.Join(syncErr, removeErr, rollbackSyncErr)
	}
	return written, nil
}

// publishFileNoReplace atomically makes a same-filesystem staging file visible
// without ever replacing an existing destination. The hard link is the
// publication point; unlinking the staging name leaves the destination bound
// to the exact bytes that were validated.
func publishFileNoReplace(stagingPath, destinationPath string) error {
	if err := os.Link(stagingPath, destinationPath); err != nil {
		return err
	}
	if err := os.Remove(stagingPath); err != nil {
		rollbackErr := os.Remove(destinationPath)
		if errors.Is(rollbackErr, os.ErrNotExist) {
			rollbackErr = nil
		}
		if rollbackErr != nil {
			rollbackErr = fmt.Errorf("rollback published library file: %w", rollbackErr)
		}
		return errors.Join(fmt.Errorf("remove published staging link: %w", err), rollbackErr)
	}
	return nil
}

func ensureLibraryStagingDir(destDir string) (string, error) {
	stagingDir := filepath.Join(destDir, libraryStagingDirName)
	if err := os.MkdirAll(stagingDir, 0o700); err != nil {
		return "", err
	}
	info, err := os.Lstat(stagingDir)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("library staging path is not an owned directory: %s", stagingDir)
	}
	if err := os.Chmod(stagingDir, 0o700); err != nil {
		return "", err
	}
	return stagingDir, nil
}

func syncDirectories(syncDir func(string) error, paths ...string) error {
	var syncErr error
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if err := syncDir(path); err != nil {
			syncErr = errors.Join(syncErr, fmt.Errorf("sync directory %s: %w", path, err))
		}
	}
	return syncErr
}

func copyDataWithContext(ctx context.Context, dst io.Writer, src io.Reader) (int64, error) {
	buffer := make([]byte, copyBufferSize)
	var written int64
	tracker := liveness.WorkerFromContext(ctx)
	copyScope := tracker.Scope(liveness.PhaseMediaCopy)
	defer copyScope.Close()
	for {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			offset := 0
			for offset < n {
				if err := ctx.Err(); err != nil {
					return written, err
				}
				count, writeErr := dst.Write(buffer[offset:n])
				if count > 0 {
					offset += count
					written += int64(count)
				}
				if writeErr != nil {
					return written, writeErr
				}
				if count == 0 {
					return written, io.ErrShortWrite
				}
				copyScope.Checkpoint()
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return written, nil
			}
			return written, readErr
		}
	}
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil && !errors.Is(err, syscall.EINVAL) && !errors.Is(err, syscall.ENOTSUP) {
		return err
	}
	return nil
}

func (s *Service) sweepStaleLibraryImportTemps(ctx context.Context) error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	return s.sweepStaleLibraryImportTempsLocked(ctx)
}

func (s *Service) trySweepStaleLibraryImportTemps(ctx context.Context) (bool, error) {
	if err := ctx.Err(); err != nil {
		return true, err
	}
	if !s.storageMu.TryLock() {
		return false, nil
	}
	defer s.storageMu.Unlock()
	return true, s.sweepStaleLibraryImportTempsLocked(ctx)
}

func (s *Service) sweepStaleLibraryImportTempsLocked(ctx context.Context) error {
	cutoff := s.nowUTC().Add(-staleLibraryImportAge)
	seen := make(map[string]struct{}, 2)
	var sweepErr error
	for _, dir := range []string{s.cfg.LoopMediaDir, s.cfg.MusicMediaDir} {
		dir = strings.TrimSpace(dir)
		if dir == "" {
			continue
		}
		stagingDir := filepath.Join(filepath.Clean(dir), libraryStagingDirName)
		if _, ok := seen[stagingDir]; ok {
			continue
		}
		seen[stagingDir] = struct{}{}
		if _, err := sweepStaleLibraryImportTempsInDir(ctx, stagingDir, cutoff, stagingSweepBatchSize); err != nil {
			sweepErr = errors.Join(sweepErr, err)
		}
	}
	return sweepErr
}

func sweepStaleLibraryImportTempsInDir(ctx context.Context, dir string, cutoff time.Time, limit int) (stagingSweepStats, error) {
	var stats stagingSweepStats
	if limit <= 0 {
		return stats, nil
	}
	handle, err := os.Open(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stats, nil
		}
		return stats, err
	}
	defer handle.Close()
	entries, readErr := handle.ReadDir(limit)
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return stats, readErr
	}
	stats.Inspected = len(entries)
	var sweepErr error
	tracker := liveness.WorkerFromContext(ctx)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return stats, errors.Join(sweepErr, err)
		}
		tracker.Advance(liveness.PhaseOperation)
		name := entry.Name()
		if !strings.HasPrefix(name, libraryImportTempPrefix) ||
			!strings.HasSuffix(name, libraryImportTempSuffix) ||
			entry.Type()&os.ModeSymlink != 0 ||
			entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			sweepErr = errors.Join(sweepErr, fmt.Errorf("inspect stale library import temp %q: %w", name, err))
			continue
		}
		if !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				sweepErr = errors.Join(sweepErr, fmt.Errorf("remove stale library import temp %q: %w", name, err))
			}
		} else {
			stats.Deleted++
			tracker.Advance(liveness.PhaseOperation)
		}
	}
	return stats, sweepErr
}
