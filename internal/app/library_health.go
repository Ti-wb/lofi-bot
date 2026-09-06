package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
)

const libraryScanProbeTimeout = 30 * time.Second

var (
	errUnhealthyLibraryAsset               = errors.New("unhealthy library asset")
	errLibraryAssetChangedDuringValidation = errors.New("library asset changed while it was being validated")
)

type libraryAssetStamp struct {
	identity        os.FileInfo
	sizeBytes       int64
	modTimeUnixNano int64
}

func (s libraryAssetStamp) same(other libraryAssetStamp) bool {
	return s.identity != nil &&
		other.identity != nil &&
		os.SameFile(s.identity, other.identity) &&
		s.sizeBytes == other.sizeBytes &&
		s.modTimeUnixNano == other.modTimeUnixNano
}

type libraryValidationEntry struct {
	kind  medialib.Kind
	stamp libraryAssetStamp
}

type libraryQuarantineEntry struct {
	stamp  libraryAssetStamp
	reason string
}

func (s *Service) libraryValidationCacheSnapshot() map[string]libraryValidationEntry {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	snapshot := make(map[string]libraryValidationEntry, len(s.libraryValidationCache))
	for path, entry := range s.libraryValidationCache {
		snapshot[path] = entry
	}
	return snapshot
}

func (s *Service) validateScannedLibrary(
	ctx context.Context,
	lib medialib.Library,
	cacheSnapshot map[string]libraryValidationEntry,
	progress func(),
) (
	medialib.Library,
	map[string]libraryValidationEntry,
	map[string]struct{},
	[]*medialib.Error,
) {
	validated := medialib.Library{
		Loops: make([]medialib.Loop, 0, len(lib.Loops)),
		Music: make([]medialib.Music, 0, len(lib.Music)),
	}
	validatedCache := make(map[string]libraryValidationEntry, len(lib.Loops)+len(lib.Music))
	issues := make([]*medialib.Error, 0)
	seen := make(map[string]struct{}, len(lib.Loops)+len(lib.Music))

	for _, loop := range lib.Loops {
		if ctx.Err() != nil {
			break
		}
		seen[loop.Path] = struct{}{}
		entry, err := s.validateScannedAsset(ctx, medialib.KindLoop, loop.Path, cacheSnapshot)
		reportLibraryScanProgress(progress)
		if err != nil {
			issues = append(issues, invalidPlayableAssetIssue(
				medialib.KindLoop,
				loop.RelPath,
				err,
			))
			continue
		}
		validatedCache[loop.Path] = entry
		validated.Loops = append(validated.Loops, loop)
	}
	if ctx.Err() == nil {
		for _, music := range lib.Music {
			if ctx.Err() != nil {
				break
			}
			seen[music.Path] = struct{}{}
			entry, err := s.validateScannedAsset(ctx, medialib.KindMusic, music.Path, cacheSnapshot)
			reportLibraryScanProgress(progress)
			if err != nil {
				issues = append(issues, invalidPlayableAssetIssue(
					medialib.KindMusic,
					music.RelPath,
					err,
				))
				continue
			}
			validatedCache[music.Path] = entry
			validated.Music = append(validated.Music, music)
		}
	}
	return validated, validatedCache, seen, issues
}

func reportLibraryScanProgress(progress func()) {
	if progress != nil {
		progress()
	}
}

func invalidPlayableAssetIssue(kind medialib.Kind, relPath string, err error) *medialib.Error {
	return &medialib.Error{
		Code:  medialib.ErrorInvalidAsset,
		Kind:  kind,
		Path:  relPath,
		Field: "playable",
		Err:   err,
	}
}

func (s *Service) validateScannedAssetLocked(
	ctx context.Context,
	kind medialib.Kind,
	path string,
) error {
	if s.libraryValidationCache == nil {
		s.libraryValidationCache = make(map[string]libraryValidationEntry)
	}
	entry, err := s.validateScannedAsset(ctx, kind, path, s.libraryValidationCache)
	if err != nil {
		if ctx.Err() != nil ||
			errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			// Cancellation says nothing about asset health. Preserve any
			// previous validation entry and never turn an operation deadline
			// into a persistent quarantine decision.
			return err
		}
		delete(s.libraryValidationCache, path)
		return err
	}
	s.libraryValidationCache[path] = entry
	return nil
}

func (s *Service) validateScannedAsset(
	ctx context.Context,
	kind medialib.Kind,
	path string,
	cache map[string]libraryValidationEntry,
) (libraryValidationEntry, error) {
	stamp, err := libraryAssetStampForPath(path)
	if err != nil {
		return libraryValidationEntry{}, err
	}
	if cached, ok := cache[path]; ok &&
		cached.kind == kind &&
		cached.stamp.same(stamp) {
		return cached, nil
	}

	probeCtx, cancel := context.WithTimeout(ctx, libraryScanProbeTimeout)
	defer cancel()
	meta, err := s.media.Probe(probeCtx, path)
	if err == nil {
		switch kind {
		case medialib.KindLoop:
			err = s.media.ValidateVideo(
				meta,
				s.cfg.MaxVideoSizeBytes,
				s.cfg.MaxVideoDurationSeconds,
			)
		case medialib.KindMusic:
			err = s.media.ValidateAudio(
				meta,
				s.cfg.MaxVideoSizeBytes,
				s.cfg.MaxVideoDurationSeconds,
			)
		default:
			err = fmt.Errorf("unsupported media library kind: %s", kind)
		}
	}
	if err != nil {
		return libraryValidationEntry{}, err
	}
	validatedStamp, err := libraryAssetStampForPath(path)
	if err != nil {
		return libraryValidationEntry{}, fmt.Errorf(
			"%w: %w",
			errLibraryAssetChangedDuringValidation,
			err,
		)
	}
	if !stamp.same(validatedStamp) {
		return libraryValidationEntry{}, errLibraryAssetChangedDuringValidation
	}
	return libraryValidationEntry{kind: kind, stamp: validatedStamp}, nil
}

func (s *Service) ensurePlayableLibraryAssetLocked(
	ctx context.Context,
	kind medialib.Kind,
	path string,
) error {
	if reason := s.libraryAssetQuarantineReasonLocked(path); reason != "" {
		return fmt.Errorf("%w: %s: %s", errUnhealthyLibraryAsset, filepath.Base(path), reason)
	}
	if err := s.validateScannedAssetLocked(ctx, kind, path); err != nil {
		if ctx.Err() != nil ||
			errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		s.quarantineLibraryAssetLocked(path, err)
		return fmt.Errorf("%w: %s: %v", errUnhealthyLibraryAsset, filepath.Base(path), err)
	}
	return nil
}

func (s *Service) quarantineLibraryAssetLocked(path string, cause error) {
	if s.libraryQuarantine == nil {
		s.libraryQuarantine = make(map[string]libraryQuarantineEntry)
	}
	stamp, _ := libraryAssetStampForPath(path)
	reason := "OBS reported a media playback error"
	if cause != nil {
		reason = cause.Error()
	}
	s.libraryQuarantine[path] = libraryQuarantineEntry{
		stamp:  stamp,
		reason: reason,
	}
}

func (s *Service) libraryAssetQuarantineReasonLocked(path string) string {
	if len(s.libraryQuarantine) == 0 {
		return ""
	}
	entry, ok := s.libraryQuarantine[path]
	if !ok {
		return ""
	}
	stamp, err := libraryAssetStampForPath(path)
	if err == nil && !stamp.same(entry.stamp) {
		delete(s.libraryQuarantine, path)
		delete(s.libraryValidationCache, path)
		return ""
	}
	return entry.reason
}

func (s *Service) playableLibraryLocked() medialib.Library {
	playable := medialib.Library{
		Loops: make([]medialib.Loop, 0, len(s.librarySnapshot.Loops)),
		Music: make([]medialib.Music, 0, len(s.librarySnapshot.Music)),
	}
	for _, loop := range s.librarySnapshot.Loops {
		if s.libraryAssetQuarantineReasonLocked(loop.Path) == "" {
			playable.Loops = append(playable.Loops, loop)
		}
	}
	for _, music := range s.librarySnapshot.Music {
		if s.libraryAssetQuarantineReasonLocked(music.Path) == "" {
			playable.Music = append(playable.Music, music)
		}
	}
	return playable
}

func (s *Service) libraryQuarantineCountLocked() int {
	count := 0
	for path := range s.libraryQuarantine {
		if s.libraryAssetQuarantineReasonLocked(path) != "" {
			count++
		}
	}
	return count
}

func libraryAssetStampForPath(path string) (libraryAssetStamp, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return libraryAssetStamp{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() <= 0 {
		return libraryAssetStamp{}, errors.New("asset must be a non-empty regular file")
	}
	return libraryAssetStamp{
		identity:        info,
		sizeBytes:       info.Size(),
		modTimeUnixNano: info.ModTime().UnixNano(),
	}, nil
}
