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
	"sync"
	"syscall"
	"testing"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

func TestUploadPreflightRejectsUnknownDeclaredSize(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		t.Fatal("unknown size should fail before disk admission")
		return media.DiskUsage{}, nil
	}
	preflight := svc.telegramHooks().PreflightUpload

	for _, size := range []int64{0, -1} {
		err := preflight(context.Background(), telegram.Upload{
			FileName:  "video.mp4",
			SizeBytes: size,
		})
		if err == nil || !strings.Contains(err.Error(), "無法確認檔案大小") {
			t.Fatalf("declared size %d error = %v, want fail-closed rejection", size, err)
		}
	}
}

func TestLibraryUploadPreflightRejectsInvalidNameAndExistingDestination(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		t.Fatal("invalid or duplicate name should fail before disk admission")
		return media.DiskUsage{}, nil
	}
	preflight := svc.telegramHooks().PreflightUpload

	err := preflight(ctx, telegram.Upload{FileName: "clip.mp4", SizeBytes: 5})
	if err == nil || !strings.Contains(err.Error(), "檔名不符合素材規則") {
		t.Fatalf("invalid-name error = %v, want library filename rejection", err)
	}

	const existing = "loop_morning_cafe_existing.mp4"
	writeLibraryFile(t, svc.cfg.LoopMediaDir, existing)
	err = preflight(ctx, telegram.Upload{FileName: existing, SizeBytes: 5})
	if err == nil || !strings.Contains(err.Error(), "媒體庫已有同名素材") {
		t.Fatalf("existing-destination error = %v, want duplicate rejection", err)
	}
}

func TestLibraryUploadPreflightCombinesCacheAndCopyHeadroomOnSharedFilesystem(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	svc.cfg.MinFreeDiskBytes = 100
	var probedPaths []string
	available := uint64(109)
	svc.diskUsage = func(path string) (media.DiskUsage, error) {
		probedPaths = append(probedPaths, filepath.Clean(path))
		return media.DiskUsage{AvailableBytes: available}, nil
	}

	preflight := svc.telegramHooks().PreflightUpload
	upload := telegram.Upload{
		FileName:  "loop_morning_cafe_new.mp4",
		SizeBytes: 5,
	}
	err := preflight(context.Background(), upload)
	if err == nil || !strings.Contains(err.Error(), "磁碟可用空間不足") {
		t.Fatalf("preflight error = %v, want combined cache-and-copy headroom rejection", err)
	}
	wantPaths := []string{filepath.Clean(svc.cfg.LoopMediaDir)}
	if fmt.Sprint(probedPaths) != fmt.Sprint(wantPaths) {
		t.Fatalf("disk probe paths = %q, want one shared-filesystem probe %q", probedPaths, wantPaths)
	}
	available = 110
	if err := preflight(context.Background(), upload); err != nil {
		t.Fatalf("admissible library metadata rejected: %v", err)
	}
}

func TestLibraryUploadCapacityPreflightUsesBoundedLimit(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	diskCalls := 0
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		diskCalls++
		return media.DiskUsage{AvailableBytes: math.MaxUint64}, nil
	}
	const limit = 2
	const incoming = "loop_morning_cafe_capacity.mp4"

	if err := svc.preflightLibraryUploadWithLimit(
		context.Background(),
		incoming,
		5,
		limit,
	); err != nil {
		t.Fatalf("preflight with room for staging and destination: %v", err)
	}
	if err := os.Mkdir(filepath.Join(svc.cfg.LoopMediaDir, libraryStagingDirName), 0o700); err != nil {
		t.Fatalf("create existing staging directory: %v", err)
	}
	if err := svc.preflightLibraryUploadWithLimit(
		context.Background(),
		incoming,
		5,
		limit,
	); err != nil {
		t.Fatalf("preflight at limit with existing staging plus destination: %v", err)
	}
	callsBeforeFull := diskCalls
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "unrelated.txt")
	err := svc.preflightLibraryUploadWithLimit(
		context.Background(),
		incoming,
		5,
		limit,
	)
	if err == nil || !strings.Contains(err.Error(), "移除") {
		t.Fatalf("full-directory preflight error = %v, want actionable capacity rejection", err)
	}
	if diskCalls != callsBeforeFull {
		t.Fatal("full directory reached filesystem admission after capacity rejection")
	}
}

func TestLibraryImportCapacityRecheckCreatesNoStagingOrDestination(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_source.mp4")
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "unrelated.txt")
	dest := filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_new.mp4")
	staging := filepath.Join(svc.cfg.LoopMediaDir, libraryStagingDirName)
	svc.media = nil

	err := svc.storeLibraryUploadWithLimit(
		ctx,
		medialib.KindLoop,
		dest,
		source,
		2,
	)
	if err == nil || !strings.Contains(err.Error(), "移除") {
		t.Fatalf("import capacity error = %v, want actionable rejection", err)
	}
	if fileExists(staging) {
		t.Fatal("capacity rejection created a staging directory")
	}
	if fileExists(dest) {
		t.Fatal("capacity rejection created a destination file")
	}
}

func TestOverCapacityScanRetainsSnapshotButOrdinaryIssuesRemainPartial(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	original := medialib.Library{
		Loops: []medialib.Loop{{ID: "last-known-good"}},
	}
	svc.librarySnapshot = original

	capacityErr := fmt.Errorf("external writer exceeded limit: %w", medialib.ErrDirectoryCapacity)
	err := svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
		return medialib.Library{
			Loops: []medialib.Loop{{ID: "incomplete-over-capacity"}},
		}, capacityErr
	}, false)
	if !errors.Is(err, medialib.ErrDirectoryCapacity) {
		t.Fatalf("scan error = %v, want capacity sentinel", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "last-known-good" {
		t.Fatalf("over-capacity scan replaced snapshot: %#v", svc.librarySnapshot)
	}
	if svc.libraryScanErr != "媒體庫目錄超過 10000 個項目的安全上限；詳細資訊請查看服務日誌。" {
		t.Fatalf("library scan error = %q, want safe capacity summary", svc.libraryScanErr)
	}

	partial := medialib.Library{
		Loops: []medialib.Loop{{ID: "valid-partial"}},
	}
	ordinaryErr := &medialib.ScanError{Issues: []*medialib.Error{{
		Code: medialib.ErrorInvalidFilename,
		Kind: medialib.KindLoop,
		Err:  errors.New("bad filename"),
	}}}
	err = svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
		return partial, ordinaryErr
	}, false)
	if !errors.Is(err, ordinaryErr) {
		t.Fatalf("ordinary scan error = %v", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "valid-partial" {
		t.Fatalf("ordinary scan issue did not publish partial snapshot: %#v", svc.librarySnapshot)
	}
}

func TestLibraryScanIssueCountAggregatesJoinedScanErrors(t *testing.T) {
	first := &medialib.ScanError{Issues: []*medialib.Error{
		{Code: medialib.ErrorInvalidFilename},
	}}
	second := &medialib.ScanError{Issues: []*medialib.Error{
		{Code: medialib.ErrorInvalidAsset},
		{Code: medialib.ErrorInvalidAsset},
	}}
	if got := libraryScanIssueCount(errors.Join(first, second)); got != 3 {
		t.Fatalf("joined scan issue count = %d, want 3", got)
	}
}

func TestUnreadableDirectoryScanRetainsLastKnownGoodSnapshot(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.librarySnapshot = medialib.Library{
		Loops: []medialib.Loop{{ID: "last-known-good"}},
	}
	readErr := &medialib.ScanError{Issues: []*medialib.Error{{
		Code:  medialib.ErrorReadDirectory,
		Kind:  medialib.KindMusic,
		Field: "directory",
		Err:   errors.New("injected unreadable mount"),
	}}}

	err := svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
		return medialib.Library{
			Loops: []medialib.Loop{{ID: "incomplete"}},
		}, readErr
	}, false)
	if !errors.Is(err, readErr) {
		t.Fatalf("scan error = %v, want read-directory failure", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "last-known-good" {
		t.Fatalf("unreadable-directory scan replaced snapshot: %#v", svc.librarySnapshot)
	}
	if svc.libraryScanErr != "媒體庫掃描發現 1 個無效或不可播放項目；詳細資訊請查看服務日誌。" {
		t.Fatalf("scan warning = %q, want safe issue-count summary", svc.libraryScanErr)
	}
}

func TestLibraryScanDoesNotHoldPlaybackLockDuringSlowEnumeration(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)

	go func() {
		done <- svc.scanLibraryWith(context.Background(), func(string, string) (medialib.Library, error) {
			close(started)
			<-release
			return medialib.Library{}, nil
		}, false)
	}()
	<-started

	lockAcquired := make(chan struct{})
	go func() {
		svc.playbackMu.Lock()
		close(lockAcquired)
		svc.playbackMu.Unlock()
	}()
	select {
	case <-lockAcquired:
	case <-time.After(500 * time.Millisecond):
		close(release)
		t.Fatal("slow library scan held playbackMu")
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("slow scan: %v", err)
	}
}

func TestCanceledLibraryScanPreservesLastKnownGoodSnapshot(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	svc.librarySnapshot = medialib.Library{
		Loops: []medialib.Loop{{ID: "last-known-good"}},
	}
	svc.libraryScanErr = "previous warning"
	svc.librarySnapshotPublished = true
	ctx, cancel := context.WithCancel(context.Background())

	err := svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
		cancel()
		return medialib.Library{
				Loops: []medialib.Loop{{ID: "canceled-partial"}},
			}, &medialib.ScanError{Issues: []*medialib.Error{{
				Code: medialib.ErrorInvalidAsset,
				Kind: medialib.KindLoop,
				Err:  context.Canceled,
			}}}
	}, false)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled scan error = %v, want context cancellation", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "last-known-good" {
		t.Fatalf("canceled scan replaced snapshot: %#v", svc.librarySnapshot)
	}
	if svc.libraryScanErr != "previous warning" {
		t.Fatalf("canceled scan warning = %q, want previous warning preserved", svc.libraryScanErr)
	}
}

func TestCanceledScanWaitingToPublishPreservesLastKnownGoodState(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	svc.librarySnapshot = medialib.Library{
		Loops: []medialib.Loop{{ID: "last-known-good"}},
	}
	svc.libraryScanErr = "previous warning"
	svc.libraryRejectedCount = 7
	svc.librarySnapshotPublished = true

	ctx, cancel := context.WithCancel(context.Background())
	scanReturned := make(chan struct{})
	done := make(chan error, 1)
	svc.playbackMu.Lock()
	go func() {
		done <- svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
			close(scanReturned)
			return medialib.Library{
				Loops: []medialib.Loop{{ID: "must-not-publish"}},
			}, errors.New("must-not-publish warning")
		}, false)
	}()
	<-scanReturned
	cancel()
	svc.playbackMu.Unlock()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want cancellation at publication barrier", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "last-known-good" {
		t.Fatalf("canceled publication replaced snapshot: %#v", svc.librarySnapshot)
	}
	if svc.libraryScanErr != "previous warning" || svc.libraryRejectedCount != 7 {
		t.Fatalf(
			"canceled publication changed diagnostics: warning=%q rejected=%d",
			svc.libraryScanErr,
			svc.libraryRejectedCount,
		)
	}
}

func TestConcurrentLibraryScansSerializeAndNewestCompletedScanPublishes(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	olderStarted := make(chan struct{})
	releaseOlder := make(chan struct{})
	olderDone := make(chan error, 1)
	newerStarted := make(chan struct{})
	newerDone := make(chan error, 1)

	go func() {
		olderDone <- svc.scanLibraryWith(context.Background(), func(string, string) (medialib.Library, error) {
			close(olderStarted)
			<-releaseOlder
			return medialib.Library{Loops: []medialib.Loop{{ID: "older"}}}, nil
		}, false)
	}()
	<-olderStarted

	go func() {
		newerDone <- svc.scanLibraryWith(context.Background(), func(string, string) (medialib.Library, error) {
			close(newerStarted)
			return medialib.Library{Loops: []medialib.Loop{{ID: "newer"}}}, nil
		}, false)
	}()
	select {
	case <-newerStarted:
		close(releaseOlder)
		t.Fatal("newer scan started before the older complete scan released its gate")
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseOlder)
	if err := <-olderDone; err != nil {
		t.Fatalf("older scan: %v", err)
	}
	select {
	case <-newerStarted:
	case <-time.After(time.Second):
		t.Fatal("newer scan did not start after the older scan completed")
	}
	if err := <-newerDone; err != nil {
		t.Fatalf("newer scan: %v", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "newer" {
		t.Fatalf("newest completed scan did not publish: %#v", svc.librarySnapshot)
	}
}

func TestCanceledScanWaitingOnGateDoesNotSuppressCompleteScan(t *testing.T) {
	svc, _ := newLibraryTestService(t)
	olderStarted := make(chan struct{})
	releaseOlder := make(chan struct{})
	olderDone := make(chan error, 1)
	go func() {
		olderDone <- svc.scanLibraryWith(context.Background(), func(string, string) (medialib.Library, error) {
			close(olderStarted)
			<-releaseOlder
			return medialib.Library{Loops: []medialib.Loop{{ID: "complete"}}}, nil
		}, false)
	}()
	<-olderStarted

	ctx, cancel := context.WithCancel(context.Background())
	invoked := make(chan struct{}, 1)
	waiterDone := make(chan error, 1)
	go func() {
		waiterDone <- svc.scanLibraryWith(ctx, func(string, string) (medialib.Library, error) {
			invoked <- struct{}{}
			return medialib.Library{}, nil
		}, false)
	}()
	select {
	case <-invoked:
		close(releaseOlder)
		t.Fatal("waiting scan invoked filesystem enumeration before acquiring the gate")
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	err := <-waiterDone
	if !errors.Is(err, context.Canceled) {
		close(releaseOlder)
		t.Fatalf("waiting scan error = %v, want context cancellation", err)
	}
	select {
	case <-invoked:
		close(releaseOlder)
		t.Fatal("canceled waiting scan invoked filesystem enumeration")
	default:
	}
	close(releaseOlder)
	if err := <-olderDone; err != nil {
		t.Fatalf("complete scan: %v", err)
	}
	if len(svc.librarySnapshot.Loops) != 1 ||
		svc.librarySnapshot.Loops[0].ID != "complete" {
		t.Fatalf("canceled waiter suppressed complete scan: %#v", svc.librarySnapshot)
	}
}

func TestZeroDiskReserveStillRequiresActualFileSize(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.cfg.MinFreeDiskBytes = 0
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_zero-reserve.mp4")
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		return media.DiskUsage{AvailableBytes: 4}, nil
	}

	_, err := svc.ImportLibraryUpload(ctx, UploadRequest{
		LocalPath: source,
		FileName:  "loop_morning_cafe_zero-reserve.mp4",
	})
	if err == nil || !strings.Contains(err.Error(), "磁碟可用空間不足") {
		t.Fatalf("import error = %v, want actual-size admission with zero reserve", err)
	}
	if !fileExists(source) {
		t.Fatal("low-space rejection deleted Telegram-owned input")
	}
	if fileExists(filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_zero-reserve.mp4")) {
		t.Fatal("library destination exists after rejected admission")
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

func TestCopyLivenessAdvancesOnlyAfterSuccessfulWriteCheckpoint(t *testing.T) {
	registry := liveness.NewRegistry(liveness.Options{ProgressInterval: time.Millisecond})
	worker, err := registry.Bind(liveness.WorkerTelegram, liveness.OwnerTelegram)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	ctx := liveness.WithWorker(context.Background(), worker)
	writeStarted := make(chan struct{})
	releaseWrite := make(chan struct{})
	writer := writeFunc(func(data []byte) (int, error) {
		close(writeStarted)
		<-releaseWrite
		return len(data), nil
	})
	done := make(chan error, 1)
	go func() {
		_, copyErr := copyDataWithContext(ctx, writer, strings.NewReader("actual-progress"))
		done <- copyErr
	}()

	select {
	case <-writeStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("copy did not reach blocked write")
	}
	blocked := worker.Snapshot()
	time.Sleep(20 * time.Millisecond)
	if got := worker.Snapshot(); got.Sequence != blocked.Sequence {
		t.Fatalf("blocked write manufactured progress: before=%+v after=%+v", blocked, got)
	}

	close(releaseWrite)
	if err := <-done; err != nil {
		t.Fatalf("copyDataWithContext: %v", err)
	}
	completed := worker.Snapshot()
	if completed.Sequence <= blocked.Sequence || completed.Phase != liveness.PhaseOperation {
		t.Fatalf("successful write did not checkpoint progress: blocked=%+v completed=%+v", blocked, completed)
	}
}

func TestAtomicLibraryCopyScopesDurabilityAndRestoresNormalPhase(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	writeTestFile(t, source)
	registry := liveness.NewRegistry(liveness.Options{})
	worker, err := registry.Bind(liveness.WorkerTelegram, liveness.OwnerTelegram)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	worker.Advance(liveness.PhaseOperation)
	ctx := liveness.WithWorker(context.Background(), worker)
	syncStarted := make(chan struct{})
	releaseSync := make(chan struct{})
	var blockFirstSync sync.Once
	done := make(chan error, 1)
	go func() {
		_, copyErr := copyFileAtomicWith(ctx, dest, source, 1024, nil, nil, atomicCopyOps{
			syncDirectory: func(string) error {
				blockFirstSync.Do(func() {
					close(syncStarted)
					<-releaseSync
				})
				return nil
			},
		})
		done <- copyErr
	}()

	select {
	case <-syncStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("copy did not reach durability sync")
	}
	blocked := worker.Snapshot()
	if blocked.Phase != liveness.PhaseDurabilitySync {
		t.Fatalf("blocked phase = %s, want %s", blocked.Phase, liveness.PhaseDurabilitySync)
	}
	time.Sleep(20 * time.Millisecond)
	if got := worker.Snapshot(); got.Sequence != blocked.Sequence {
		t.Fatalf("blocked sync manufactured progress: before=%+v after=%+v", blocked, got)
	}

	close(releaseSync)
	if err := <-done; err != nil {
		t.Fatalf("copyFileAtomic: %v", err)
	}
	if phase := worker.Snapshot().Phase; phase != liveness.PhaseOperation {
		t.Fatalf("final phase = %s, want restored %s", phase, liveness.PhaseOperation)
	}
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

type writeFunc func([]byte) (int, error)

func (write writeFunc) Write(data []byte) (int, error) {
	return write(data)
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
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat destination: %v", err)
	}
	if got := info.Mode().Perm(); got != libraryMediaFileMode {
		t.Fatalf("destination mode = %04o, want %04o", got, libraryMediaFileMode)
	}
	requireNoOwnedImportTemps(t, dir)
}

func TestAtomicLibraryCopyNeverReplacesConcurrentDestination(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source.mp4")
	dest := filepath.Join(dir, "dest.mp4")
	if err := os.WriteFile(source, []byte("validated-library-media"), 0o600); err != nil {
		t.Fatalf("write source: %v", err)
	}
	external := []byte("external-writer-wins")
	publishReached := false

	_, err := copyFileAtomicWith(context.Background(), dest, source, 1024, nil, nil, atomicCopyOps{
		publishFile: func(stagingPath, destinationPath string) error {
			publishReached = true
			if err := os.WriteFile(destinationPath, external, 0o600); err != nil {
				t.Fatalf("create concurrent destination: %v", err)
			}
			return publishFileNoReplace(stagingPath, destinationPath)
		},
	})
	if !publishReached {
		t.Fatal("copy never reached publication")
	}
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("copy error = %v, want destination-exists failure", err)
	}
	got, readErr := os.ReadFile(dest)
	if readErr != nil {
		t.Fatalf("read concurrent destination: %v", readErr)
	}
	if string(got) != string(external) {
		t.Fatalf("destination = %q, want untouched external bytes %q", got, external)
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
	svc.media = media.NewManager(probePath)
	source := writeBotAPIFile(t, svc, "loop_morning_cafe_invalid.mp4")
	dest := filepath.Join(svc.cfg.LoopMediaDir, "loop_morning_cafe_invalid.mp4")

	_, err := svc.ImportLibraryUpload(ctx, UploadRequest{
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

	registry := liveness.NewRegistry(liveness.Options{})
	worker, err := registry.Bind(liveness.WorkerMaintenance, liveness.OwnerMaintenance)
	if err != nil {
		t.Fatalf("Bind maintenance: %v", err)
	}
	worker.Advance(liveness.PhaseOperation)
	ctx := liveness.WithWorker(context.Background(), worker)
	beforeSweep := worker.Snapshot().Sequence
	if err := svc.sweepStaleLibraryImportTemps(ctx); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if worker.Snapshot().Sequence <= beforeSweep {
		t.Fatal("stale import sweep did not record an actual entry checkpoint")
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
