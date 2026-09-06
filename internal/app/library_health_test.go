package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

func TestLibraryScanExcludesAssetsMissingRequiredStreams(t *testing.T) {
	ctx := context.Background()

	t.Run("loop without video", func(t *testing.T) {
		svc, _ := newLibraryTestService(t)
		useSelectiveLibraryProbe(t, svc)
		const invalidName = "loop_day_novideo_001.mp4"
		writeLibraryFile(t, svc.cfg.LoopMediaDir, invalidName)

		err := svc.ScanLibrary(ctx)
		if err == nil {
			t.Fatal("ScanLibrary succeeded for a loop without a video stream")
		}
		if !strings.Contains(err.Error(), invalidName) ||
			!strings.Contains(err.Error(), "does not contain a video stream") {
			t.Fatalf("ScanLibrary error = %q, want filename and missing-video reason", err)
		}
		if got := len(svc.librarySnapshot.Loops); got != 0 {
			t.Fatalf("validated loop count = %d, want 0", got)
		}

		status, statusErr := svc.LibraryStatusText(ctx, true)
		if statusErr != nil {
			t.Fatalf("LibraryStatusText: %v", statusErr)
		}
		for _, want := range []string{
			"Rejected/quarantined：1/0",
			"Scan warning：媒體庫掃描發現 1 個無效或不可播放項目；詳細資訊請查看服務日誌。",
		} {
			if !strings.Contains(status, want) {
				t.Fatalf("status = %q, want %q", status, want)
			}
		}
		if strings.Contains(status, invalidName) ||
			strings.Contains(status, "does not contain a video stream") {
			t.Fatalf("status exposed raw scan details: %q", status)
		}
	})

	t.Run("music without audio", func(t *testing.T) {
		svc, _ := newLibraryTestService(t)
		useSelectiveLibraryProbe(t, svc)
		writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
		const invalidName = "music_noaudio.mp3"
		writeLibraryFile(t, svc.cfg.MusicMediaDir, invalidName)

		err := svc.ScanLibrary(ctx)
		if err == nil {
			t.Fatal("ScanLibrary succeeded for music without an audio stream")
		}
		if !strings.Contains(err.Error(), invalidName) ||
			!strings.Contains(err.Error(), "does not contain an audio stream") {
			t.Fatalf("ScanLibrary error = %q, want filename and missing-audio reason", err)
		}
		if got := len(svc.librarySnapshot.Music); got != 0 {
			t.Fatalf("validated music count = %d, want 0", got)
		}

		status, statusErr := svc.LibraryStatusText(ctx, true)
		if statusErr != nil {
			t.Fatalf("LibraryStatusText: %v", statusErr)
		}
		for _, want := range []string{
			"Playable loop/music：1/0",
			"Rejected/quarantined：1/0",
			"Scan warning：媒體庫掃描發現 1 個無效或不可播放項目；詳細資訊請查看服務日誌。",
		} {
			if !strings.Contains(status, want) {
				t.Fatalf("status = %q, want %q", status, want)
			}
		}
		if strings.Contains(status, invalidName) ||
			strings.Contains(status, "does not contain an audio stream") {
			t.Fatalf("status exposed raw scan details: %q", status)
		}
	})
}

func TestLibraryMediaStateErrorImmediatelyFailsOver(t *testing.T) {
	ctx := context.Background()

	t.Run("loop", func(t *testing.T) {
		svc, fakeOBS := newLibraryTestService(t)
		writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
		writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
		writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
		svc.now = fixedNow("2026-06-24T12:00:00+08:00")

		if err := svc.ScanLibrary(ctx); err != nil {
			t.Fatalf("ScanLibrary: %v", err)
		}
		if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
			t.Fatalf("ensureLibraryPlayback: %v", err)
		}
		badPath := svc.activeLoopPath
		healthyPath := otherLoopPath(t, svc.librarySnapshot.Loops, badPath)
		playsBefore := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]
		fakeOBS.mediaStatuses[svc.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
			State: obs.MediaStateError,
		}

		if err := svc.reconcileLibraryPlayback(ctx); err != nil {
			t.Fatalf("reconcileLibraryPlayback: %v", err)
		}
		if got := svc.activeLoopPath; got != healthyPath {
			t.Fatalf("active loop after OBS error = %q, want bounded failover to %q", got, healthyPath)
		}
		if reason := svc.libraryAssetQuarantineReasonLocked(badPath); reason == "" {
			t.Fatalf("errored loop %q was not quarantined", badPath)
		}
		if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSLoopSourceName]; got != playsBefore+1 {
			t.Fatalf("loop play calls = %d, want exactly one failover call after %d", got, playsBefore)
		}
		plan, ok, err := svc.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay)
		if err != nil || !ok {
			t.Fatalf("PeriodPlan after failover: ok=%v err=%v", ok, err)
		}
		if plan.LoopID != svc.activeLoopID {
			t.Fatalf("persisted plan = %q, want healthy active loop %q", plan.LoopID, svc.activeLoopID)
		}
	})

	t.Run("music", func(t *testing.T) {
		svc, fakeOBS := newLibraryTestService(t)
		writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
		writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
		writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
		svc.now = fixedNow("2026-06-24T12:00:00+08:00")

		if err := svc.ScanLibrary(ctx); err != nil {
			t.Fatalf("ScanLibrary: %v", err)
		}
		if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
			t.Fatalf("ensureLibraryPlayback: %v", err)
		}
		badPath := svc.activeMusicPath
		healthyPath := otherMusicPath(t, svc.librarySnapshot.Music, badPath)
		playsBefore := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]
		fakeOBS.mediaStatuses[svc.cfg.OBSMusicSourceName] = obs.MediaInputStatus{
			State: obs.MediaStateError,
		}

		if err := svc.reconcileLibraryPlayback(ctx); err != nil {
			t.Fatalf("reconcileLibraryPlayback: %v", err)
		}
		if got := svc.activeMusicPath; got != healthyPath {
			t.Fatalf("active music after OBS error = %q, want bounded failover to %q", got, healthyPath)
		}
		if reason := svc.libraryAssetQuarantineReasonLocked(badPath); reason == "" {
			t.Fatalf("errored music %q was not quarantined", badPath)
		}
		if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != playsBefore+1 {
			t.Fatalf("music play calls = %d, want exactly one failover call after %d", got, playsBefore)
		}
		lastID, err := svc.libDB.LastMusicID(ctx)
		if err != nil {
			t.Fatalf("LastMusicID: %v", err)
		}
		if lastID != svc.activeMusicID {
			t.Fatalf("persisted last music = %q, want healthy active music %q", lastID, svc.activeMusicID)
		}
	})
}

func TestPersistedUnplayableLoopPlanDoesNotPinSelection(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	useSelectiveLibraryProbe(t, svc)
	const (
		badName     = "loop_day_novideo_001.mp4"
		healthyName = "loop_day_cafe_001.mp4"
	)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, badName)
	healthyPath := writeLibraryFile(t, svc.cfg.LoopMediaDir, healthyName)
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")

	if err := svc.ScanLibrary(ctx); err == nil {
		t.Fatal("ScanLibrary succeeded despite the bad persisted-plan candidate")
	}
	badID := medialib.StableID(
		medialib.KindLoop,
		filepath.ToSlash(filepath.Join("loops", badName)),
	)
	if err := svc.libDB.SavePeriodPlan(ctx, medialib.PeriodPlan{
		Date:   "2026-06-24",
		Period: medialib.PeriodDay,
		Theme:  "novideo",
		LoopID: badID,
	}); err != nil {
		t.Fatalf("SavePeriodPlan: %v", err)
	}

	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("ensureLibraryPlayback: %v", err)
	}
	if got := svc.activeLoopPath; got != healthyPath {
		t.Fatalf("active loop = %q, want healthy path %q", got, healthyPath)
	}
	plan, ok, err := svc.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay)
	if err != nil || !ok {
		t.Fatalf("PeriodPlan after recovery: ok=%v err=%v", ok, err)
	}
	if plan.LoopID == badID || plan.LoopID != svc.activeLoopID {
		t.Fatalf("replacement plan = %#v, want active healthy loop %q", plan, svc.activeLoopID)
	}
}

func TestLibraryAssetStampDetectsAtomicIdentityReplacementWithSameMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop_day_cafe_001.mp4")
	if err := os.WriteFile(path, []byte("video"), 0o600); err != nil {
		t.Fatalf("write original asset: %v", err)
	}
	before, err := libraryAssetStampForPath(path)
	if err != nil {
		t.Fatalf("stamp original asset: %v", err)
	}

	atomicallyReplaceLibraryFilePreservingMetadata(t, path, []byte("audio"))

	after, err := libraryAssetStampForPath(path)
	if err != nil {
		t.Fatalf("stamp replacement asset: %v", err)
	}
	if before.sizeBytes != after.sizeBytes ||
		before.modTimeUnixNano != after.modTimeUnixNano {
		t.Fatalf(
			"test replacement metadata changed: before=%+v after=%+v",
			before,
			after,
		)
	}
	if before.same(after) {
		t.Fatal("same-size/same-mtime atomic replacement retained the old file identity")
	}
}

func TestLibraryQuarantineClearsAfterAtomicIdentityReplacementWithSameMetadata(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	path := writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	svc.playbackMu.Lock()
	svc.quarantineLibraryAssetLocked(path, nil)
	if reason := svc.libraryAssetQuarantineReasonLocked(path); reason == "" {
		svc.playbackMu.Unlock()
		t.Fatal("test asset was not quarantined")
	}
	svc.playbackMu.Unlock()

	atomicallyReplaceLibraryFilePreservingMetadata(t, path, []byte("audio"))
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary after asset replacement: %v", err)
	}

	svc.playbackMu.Lock()
	defer svc.playbackMu.Unlock()
	if reason := svc.libraryAssetQuarantineReasonLocked(path); reason != "" {
		t.Fatalf("quarantine survived content/stamp change: %q", reason)
	}
	if got := svc.libraryQuarantineCountLocked(); got != 0 {
		t.Fatalf("quarantine count = %d, want 0", got)
	}
	if got := len(svc.librarySnapshot.Loops); got != 1 {
		t.Fatalf("validated loop count after repair = %d, want 1", got)
	}
}

func TestLibraryValidationRechecksIdentityAfterCacheMissAndProbe(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	path := writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	probeDir := t.TempDir()
	probePath := filepath.Join(probeDir, "ffprobe")
	counterPath := filepath.Join(probeDir, "calls")
	replacementPath := filepath.Join(svc.cfg.LoopMediaDir, ".probe-replacement")
	t.Setenv("TG_OBS_TEST_LIBRARY_PROBE_COUNTER", counterPath)
	t.Setenv("TG_OBS_TEST_LIBRARY_PROBE_REPLACEMENT", replacementPath)
	probe := `#!/bin/sh
printf 'call\n' >> "$TG_OBS_TEST_LIBRARY_PROBE_COUNTER"
for arg do
	path=$arg
done
if [ -n "${TG_OBS_TEST_LIBRARY_PROBE_REPLACEMENT:-}" ] &&
   [ -e "$TG_OBS_TEST_LIBRARY_PROBE_REPLACEMENT" ]; then
	mv "$TG_OBS_TEST_LIBRARY_PROBE_REPLACEMENT" "$path"
fi
printf '{"format":{"duration":"30"},"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}'
`
	if err := os.WriteFile(probePath, []byte(probe), 0o700); err != nil {
		t.Fatalf("write identity-changing ffprobe: %v", err)
	}
	svc.media = media.NewManager(probePath)

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("initial ScanLibrary: %v", err)
	}
	if got := libraryProbeCallCount(t, counterPath); got != 1 {
		t.Fatalf("initial probe count = %d, want 1", got)
	}

	// B invalidates the cached identity without changing the metadata fields
	// formerly used as the complete cache key. C is installed by ffprobe so the
	// post-validation identity fence must reject that second change.
	atomicallyReplaceLibraryFilePreservingMetadata(t, path, []byte("audio"))
	prepareLibraryFileReplacementPreservingMetadata(
		t,
		path,
		replacementPath,
		[]byte("third"),
	)

	err := svc.ScanLibrary(ctx)
	if !errors.Is(err, errLibraryAssetChangedDuringValidation) {
		t.Fatalf(
			"ScanLibrary error = %v, want identity-changed validation failure",
			err,
		)
	}
	if got := libraryProbeCallCount(t, counterPath); got != 2 {
		t.Fatalf("probe count after cache identity change = %d, want 2", got)
	}
	svc.playbackMu.Lock()
	_, cached := svc.libraryValidationCache[path]
	loopCount := len(svc.librarySnapshot.Loops)
	svc.playbackMu.Unlock()
	if cached {
		t.Fatal("asset changed during ffprobe was cached as validated")
	}
	if loopCount != 0 {
		t.Fatalf("playable loop count after probe-time replacement = %d, want 0", loopCount)
	}

	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("ScanLibrary after stable replacement: %v", err)
	}
	if got := libraryProbeCallCount(t, counterPath); got != 3 {
		t.Fatalf("probe count after stable replacement = %d, want 3", got)
	}
}

func atomicallyReplaceLibraryFilePreservingMetadata(
	t *testing.T,
	path string,
	body []byte,
) {
	t.Helper()
	replacementPath := filepath.Join(filepath.Dir(path), ".identity-replacement")
	prepareLibraryFileReplacementPreservingMetadata(t, path, replacementPath, body)
	if err := os.Rename(replacementPath, path); err != nil {
		t.Fatalf("atomically replace library asset: %v", err)
	}
}

func prepareLibraryFileReplacementPreservingMetadata(
	t *testing.T,
	path string,
	replacementPath string,
	body []byte,
) {
	t.Helper()
	original, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("stat original library asset: %v", err)
	}
	if int64(len(body)) != original.Size() {
		t.Fatalf(
			"replacement size = %d, want original size %d",
			len(body),
			original.Size(),
		)
	}
	if err := os.WriteFile(replacementPath, body, original.Mode().Perm()); err != nil {
		t.Fatalf("write replacement library asset: %v", err)
	}
	if err := os.Chtimes(replacementPath, original.ModTime(), original.ModTime()); err != nil {
		t.Fatalf("preserve replacement library asset mtime: %v", err)
	}
	replacement, err := os.Lstat(replacementPath)
	if err != nil {
		t.Fatalf("stat replacement library asset: %v", err)
	}
	if replacement.Size() != original.Size() ||
		replacement.ModTime().UnixNano() != original.ModTime().UnixNano() {
		t.Fatalf(
			"replacement metadata differs: original size=%d mtime=%d replacement size=%d mtime=%d",
			original.Size(),
			original.ModTime().UnixNano(),
			replacement.Size(),
			replacement.ModTime().UnixNano(),
		)
	}
	if os.SameFile(original, replacement) {
		t.Fatal("replacement fixture unexpectedly reused the original file identity")
	}
}

func libraryProbeCallCount(t *testing.T, path string) int {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read ffprobe call counter: %v", err)
	}
	return strings.Count(string(body), "call\n")
}

func useSelectiveLibraryProbe(t *testing.T, svc *Service) {
	t.Helper()
	probePath := filepath.Join(t.TempDir(), "ffprobe")
	probe := `#!/bin/sh
for arg do
	path=$arg
done
case "$path" in
	*novideo*) streams='[{"codec_type":"audio"}]' ;;
	*noaudio*) streams='[{"codec_type":"video"}]' ;;
	*) streams='[{"codec_type":"video"},{"codec_type":"audio"}]' ;;
esac
printf '{"format":{"duration":"30"},"streams":%s}' "$streams"
`
	if err := os.WriteFile(probePath, []byte(probe), 0o700); err != nil {
		t.Fatalf("write selective ffprobe: %v", err)
	}
	svc.media = media.NewManager(probePath)
}

func otherLoopPath(t *testing.T, loops []medialib.Loop, current string) string {
	t.Helper()
	for _, loop := range loops {
		if loop.Path != current {
			return loop.Path
		}
	}
	t.Fatalf("no alternate loop for %q", current)
	return ""
}

func otherMusicPath(t *testing.T, tracks []medialib.Music, current string) string {
	t.Helper()
	for _, track := range tracks {
		if track.Path != current {
			return track.Path
		}
	}
	t.Fatalf("no alternate music for %q", current)
	return ""
}
