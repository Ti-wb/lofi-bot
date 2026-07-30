package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

const librarySchedulerInterval = 15 * time.Second

type libraryUploadPlan struct {
	fileName string
	kind     medialib.Kind
	destDir  string
	label    string
}

type libraryScanFunc func(string, string) (medialib.Library, error)

func (s *Service) libraryMode() bool {
	return s.cfg.PlayerMode == "library"
}

func (s *Service) ScanLibrary(ctx context.Context) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	return s.scanLibraryLocked(ctx)
}

func (s *Service) scanLibraryLocked(ctx context.Context) error {
	return s.scanLibraryLockedWith(ctx, medialib.ScanDirs)
}

func (s *Service) scanLibraryLockedWith(ctx context.Context, scan libraryScanFunc) error {
	tracker := liveness.WorkerFromContext(ctx)
	scanScope := tracker.Scope(liveness.PhaseLibraryScan)
	defer scanScope.Close()

	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.MkdirAll(s.cfg.LoopMediaDir, 0o755); err != nil {
		s.setLastErr(err)
		return err
	}
	if err := os.MkdirAll(s.cfg.MusicMediaDir, 0o755); err != nil {
		s.setLastErr(err)
		return err
	}
	lib, err := scan(s.cfg.LoopMediaDir, s.cfg.MusicMediaDir)
	if !errors.Is(err, medialib.ErrDirectoryCapacity) {
		s.librarySnapshot = lib
	}
	if err != nil {
		s.libraryScanErr = err.Error()
		s.setLastErr(err)
		return err
	}
	s.libraryScanErr = ""
	return nil
}

func (s *Service) librarySchedulerLoop(ctx context.Context) error {
	tracker := liveness.WorkerFromContext(ctx)
	retries := newRecurringRetry(librarySchedulerInterval, libraryRetryMaxDelay, s.retryRandom)
	schedule := newRecurringSchedule(librarySchedulerInterval, false, s.retryClockNow)
	delay := schedule.delay()
	waitPhase := liveness.PhaseScheduledWait
	for {
		if err := s.waitRecurring(ctx, tracker, waitPhase, delay); err != nil {
			return err
		}
		tracker.Advance(liveness.PhaseOperation)
		schedule.beginCycle()
		if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
			delay = schedule.delay()
			waitPhase = liveness.PhaseScheduledWait
			continue
		}
		attempted, err := s.reconcileLibraryPlaybackAttempt(ctx)
		if !attempted {
			delay = schedule.delay()
			waitPhase = liveness.PhaseScheduledWait
			continue
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			s.setLastErr(err)
			failure := retries.failure()
			logRecurringFailure(s.logger, "library playback check failed", s.redactError(err), failure)
			delay = schedule.failureDelay(failure.attempt.Delay)
			waitPhase = liveness.PhaseRetryWait
			continue
		}
		logRecurringRecovery(s.logger, "library playback recovered", retries.recovery())
		delay = schedule.delay()
		waitPhase = liveness.PhaseScheduledWait
	}
}

func (s *Service) recoverLibraryPlaybackAfterOBSConnect(ctx context.Context) error {
	var scanErr error
	var reconcileErr error
	func() {
		s.playbackMu.Lock()
		defer s.playbackMu.Unlock()
		scanErr = s.scanLibraryLocked(ctx)
		reconcileErr = s.reconcileLibraryPlaybackLocked(ctx, true)
	}()

	// Scan issues are retained in status and sampled here after releasing
	// playbackMu so repeated reconnects cannot amplify logs or block playback.
	s.observeLibraryRecoveryScan(scanErr)
	return reconcileErr
}

func (s *Service) handleLibraryOBSEvent(ctx context.Context, event obs.Event) error {
	_, err := s.handleLibraryOBSEventAttempt(ctx, event)
	return err
}

func (s *Service) handleLibraryOBSEventAttempt(ctx context.Context, event obs.Event) (bool, error) {
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	if event.InputName == s.cfg.OBSMusicSourceName &&
		event.Path != "" &&
		s.activeMusicPath != "" &&
		event.Path != s.activeMusicPath {
		return true, nil
	}
	return true, s.reconcileLibrarySourceLocked(ctx, event.InputName, false)
}

func (s *Service) restartLibraryLoopAfterEndedLocked(ctx context.Context) error {
	s.clearActiveLoopLocked()
	return s.restartLibraryLoopLocked(ctx)
}

func (s *Service) restartLibraryLoopLocked(ctx context.Context) error {
	loop, info, _, err := s.loopForTimeLocked(ctx, s.now(), false)
	if err != nil {
		return err
	}
	return s.playLibraryLoopLocked(ctx, loop, info)
}

func (s *Service) playLibraryLoopLocked(ctx context.Context, loop medialib.Loop, info medialib.PeriodInfo) error {
	looping := true
	mute := true
	if err := s.obs.PlaySourceFile(ctx, s.cfg.OBSLoopSourceName, loop.Path, obs.PlaySourceOptions{
		Restart:         true,
		Looping:         &looping,
		Mute:            &mute,
		CenterSceneItem: true,
	}); err != nil {
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSLoopSourceName, loop.Path)
	s.activeLoopID = loop.ID
	s.activeLoopPath = loop.Path
	s.activeLoopTheme = loop.Theme
	s.activeLoopPeriod = loop.Period
	s.activeLoopEndsAt = info.EndsAt
	s.setPlaybackState(playbackFile, 0, loop.Path)
	return nil
}

func (s *Service) clearActiveLoopLocked() {
	s.activeLoopID = ""
	s.activeLoopPath = ""
	s.activeLoopTheme = ""
	s.activeLoopPeriod = ""
	s.activeLoopEndsAt = time.Time{}
}

func (s *Service) clearActiveMusicLocked() {
	s.activeMusicID = ""
	s.activeMusicPath = ""
}

func (s *Service) reconcileLibraryPlayback(ctx context.Context) error {
	_, err := s.reconcileLibraryPlaybackAttempt(ctx)
	return err
}

func (s *Service) reconcileLibraryPlaybackAttempt(ctx context.Context) (bool, error) {
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if s.obsRecoveryInProgress.Load() || s.obs.Status().State != obs.StateConnected {
		return false, nil
	}
	return true, s.reconcileLibraryPlaybackLocked(ctx, false)
}

func (s *Service) reconcileLibraryPlaybackLocked(ctx context.Context, recovering bool) error {
	previousLoopID := s.activeLoopID
	previousLoopPath := s.activeLoopPath
	previousMusicID := s.activeMusicID
	previousMusicPath := s.activeMusicPath
	if err := s.ensureLibraryPlaybackLocked(ctx, false); err != nil {
		return err
	}

	var reconcileErr error
	if previousLoopID == s.activeLoopID && previousLoopPath == s.activeLoopPath && s.activeLoopPath != "" {
		reconcileErr = s.reconcileLibrarySourceLocked(ctx, s.cfg.OBSLoopSourceName, recovering)
	}
	if reconcileErr != nil && s.obs.Status().State != obs.StateConnected {
		return reconcileErr
	}
	if previousMusicID == s.activeMusicID && previousMusicPath == s.activeMusicPath && s.activeMusicPath != "" {
		reconcileErr = errors.Join(reconcileErr, s.reconcileLibrarySourceLocked(ctx, s.cfg.OBSMusicSourceName, recovering))
	}
	return reconcileErr
}

func (s *Service) reconcileLibrarySourceLocked(ctx context.Context, inputName string, recovering bool) error {
	if inputName != s.cfg.OBSLoopSourceName && inputName != s.cfg.OBSMusicSourceName {
		return nil
	}

	expectedPath := s.activeLoopPath
	if inputName == s.cfg.OBSMusicSourceName {
		expectedPath = s.activeMusicPath
	}
	inspection, err := s.inspectMediaInputLocked(ctx, inputName, expectedPath)
	if err != nil {
		return err
	}
	if inspection.PathMismatch || inspection.Stalled {
		if inspection.PathMismatch && inspection.Settling && !recovering {
			return nil
		}
		if inputName == s.cfg.OBSLoopSourceName {
			return s.restartLibraryLoopLocked(ctx)
		}
		return s.replayActiveMusicLocked(ctx)
	}
	switch inspection.Status.State {
	case obs.MediaStatePlaying, obs.MediaStateOpening, obs.MediaStateBuffering:
		return nil
	case obs.MediaStateStopped, obs.MediaStateEnded, obs.MediaStateError:
		if inspection.Settling && !recovering {
			return nil
		}
	case obs.MediaStateNone, obs.MediaStatePaused:
	default:
		return fmt.Errorf("OBS media source %s returned unknown state %q", inputName, inspection.Status.State)
	}

	if expectedPath == "" {
		return nil
	}

	if inputName == s.cfg.OBSLoopSourceName {
		if inspection.Status.State == obs.MediaStateStopped ||
			inspection.Status.State == obs.MediaStateEnded ||
			inspection.Status.State == obs.MediaStateError {
			return s.restartLibraryLoopAfterEndedLocked(ctx)
		}
		return s.restartLibraryLoopLocked(ctx)
	}
	if !recovering && (inspection.Status.State == obs.MediaStateStopped || inspection.Status.State == obs.MediaStateEnded || inspection.Status.State == obs.MediaStateError) {
		s.clearActiveMusicLocked()
		return s.playNextMusicLocked(ctx, true)
	}
	return s.replayActiveMusicLocked(ctx)
}

func (s *Service) ensureLibraryPlayback(ctx context.Context, forceLoop bool) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	return s.ensureLibraryPlaybackLocked(ctx, forceLoop)
}

func (s *Service) ensureLibraryPlaybackLocked(ctx context.Context, forceLoop bool) error {
	if s.libDB == nil {
		return errors.New("library state store is not configured")
	}
	if len(s.librarySnapshot.Loops) == 0 && len(s.librarySnapshot.Music) == 0 && s.libraryScanErr == "" {
		_ = s.scanLibraryLocked(ctx)
	}

	now := s.now()
	loop, info, _, err := s.loopForTimeLocked(ctx, now, forceLoop)
	if err != nil {
		s.setPlaybackState(playbackIdle, 0, "")
		return err
	}
	if forceLoop || s.activeLoopID != loop.ID || s.activeLoopPath != loop.Path || now.After(s.activeLoopEndsAt) || now.Equal(s.activeLoopEndsAt) {
		if err := s.playLibraryLoopLocked(ctx, loop, info); err != nil {
			return err
		}
	}

	if s.activeMusicID == "" || s.activeMusicPath == "" {
		return s.playNextMusicLocked(ctx, false)
	}
	return nil
}

func (s *Service) playNextMusic(ctx context.Context, force bool) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	return s.playNextMusicLocked(ctx, force)
}

func (s *Service) playNextMusicLocked(ctx context.Context, force bool) error {
	if s.libDB == nil {
		return nil
	}
	if len(s.librarySnapshot.Music) == 0 {
		if force {
			return publicError("媒體庫沒有可播放的音樂。")
		}
		return nil
	}
	previousID := s.activeMusicID
	if previousID == "" {
		if stored, err := s.libDB.LastMusicID(ctx); err == nil {
			previousID = stored
		}
	}
	music, err := s.librarySnapshot.ChooseMusic(previousID, s.rng)
	if err != nil {
		return err
	}
	if err := s.playMusicAssetLocked(ctx, music); err != nil {
		return err
	}
	if err := s.libDB.SetLastMusicID(ctx, music.ID); err != nil && force {
		return err
	}
	return nil
}

func (s *Service) replayActiveMusicLocked(ctx context.Context) error {
	if s.activeMusicID != "" && s.activeMusicPath != "" {
		if music, ok := s.findMusicByID(s.activeMusicID); ok && music.Path == s.activeMusicPath {
			if err := s.playMusicAssetLocked(ctx, music); err != nil {
				s.clearActiveMusicLocked()
				return err
			}
			return nil
		}
	}
	s.clearActiveMusicLocked()
	return s.playNextMusicLocked(ctx, false)
}

func (s *Service) playMusicAssetLocked(ctx context.Context, music medialib.Music) error {
	looping := false
	mute := false
	if err := s.obs.PlaySourceFile(ctx, s.cfg.OBSMusicSourceName, music.Path, obs.PlaySourceOptions{
		Restart:         true,
		Looping:         &looping,
		Mute:            &mute,
		CenterSceneItem: false,
	}); err != nil {
		return err
	}
	s.resetMediaProgressLocked(s.cfg.OBSMusicSourceName, music.Path)
	s.activeMusicID = music.ID
	s.activeMusicPath = music.Path
	return nil
}

func (s *Service) loopForTimeLocked(ctx context.Context, t time.Time, force bool) (medialib.Loop, medialib.PeriodInfo, string, error) {
	if len(s.librarySnapshot.Loops) == 0 && s.libraryScanErr == "" {
		_ = s.scanLibraryLocked(ctx)
	}
	info := medialib.PeriodInfoAt(t)
	date := periodPlanDate(t, info.Period)
	overrideDate := overrideDateKey(t)
	override, err := s.libDB.Override(ctx, overrideDate)
	if err != nil {
		return medialib.Loop{}, info, "", err
	}
	expiredPreviousOverride := false
	if info.Period == medialib.PeriodNight && t.Hour() < 6 && override.Theme == "" && override.DirectLoopID == "" {
		if previousOverride, err := s.libDB.Override(ctx, date); err != nil {
			return medialib.Loop{}, info, "", err
		} else if previousOverride.Theme != "" || previousOverride.DirectLoopID != "" {
			expiredPreviousOverride = true
			if err := s.libDB.ClearOverride(ctx, date); err != nil {
				return medialib.Loop{}, info, "", err
			}
			if err := s.libDB.ClearPeriodPlan(ctx, date, info.Period); err != nil {
				return medialib.Loop{}, info, "", err
			}
		}
	}

	if override.DirectLoopID != "" {
		if loop, ok := s.findLoopByID(override.DirectLoopID); ok {
			return loop, info, "指定影片", nil
		}
		_ = s.libDB.ClearDirectLoopOverride(ctx, overrideDate)
	}

	if force {
		_ = s.libDB.ClearPeriodPlan(ctx, date, info.Period)
	}
	if plan, ok, err := s.libDB.PeriodPlan(ctx, date, info.Period); err != nil {
		return medialib.Loop{}, info, "", err
	} else if ok && !force && !expiredPreviousOverride && s.canReusePeriodPlan(info.Period, override.Theme, plan.Theme) {
		if loop, found := s.findLoopByID(plan.LoopID); found {
			return loop, info, "", nil
		}
		_ = s.libDB.ClearPeriodPlan(ctx, date, info.Period)
	}

	preferredTheme := override.Theme
	loop, reason, err := s.chooseLoop(info.Period, preferredTheme)
	if err != nil {
		return medialib.Loop{}, info, "", err
	}
	plan := medialib.PeriodPlan{
		Date:   date,
		Period: info.Period,
		Theme:  loop.Theme,
		LoopID: loop.ID,
	}
	if err := s.libDB.SavePeriodPlan(ctx, plan); err != nil {
		return medialib.Loop{}, info, "", err
	}
	return loop, info, reason, nil
}

func (s *Service) canReusePeriodPlan(period medialib.Period, overrideTheme string, planTheme string) bool {
	if overrideTheme == "" || overrideTheme == planTheme {
		return true
	}
	for _, loop := range s.librarySnapshot.Loops {
		if loop.Period == period && loop.Theme == overrideTheme {
			return false
		}
	}
	return true
}

func (s *Service) previewLoopLocked(ctx context.Context) (medialib.Loop, medialib.PeriodInfo, string, error) {
	nextAt := medialib.PeriodInfoAt(s.now()).EndsAt
	return s.loopForTimeLocked(ctx, nextAt, false)
}

func (s *Service) chooseLoop(period medialib.Period, preferredTheme string) (medialib.Loop, string, error) {
	if preferredTheme != "" {
		if loop, err := s.librarySnapshot.ChooseLoop(period, preferredTheme, s.rng); err == nil {
			return loop, "", nil
		}
	}
	theme, err := s.librarySnapshot.ChooseTheme(period, s.rng)
	if err == nil {
		loop, err := s.librarySnapshot.ChooseLoop(period, theme, s.rng)
		return loop, fallbackReason(preferredTheme, theme), err
	}
	if len(s.librarySnapshot.Loops) == 0 {
		return medialib.Loop{}, "", publicError("媒體庫沒有可播放的 loop 影片。")
	}
	loops := make([]medialib.Loop, len(s.librarySnapshot.Loops))
	copy(loops, s.librarySnapshot.Loops)
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].RelPath < loops[j].RelPath
	})
	return loops[s.rng.Intn(len(loops))], "目前時段沒有素材，已退回任一可用 loop", nil
}

func fallbackReason(preferredTheme string, actualTheme string) string {
	if preferredTheme == "" || preferredTheme == actualTheme {
		return ""
	}
	return fmt.Sprintf("指定主題 %s 沒有此時段素材，已退回 %s", preferredTheme, actualTheme)
}

func (s *Service) findLoopByID(id string) (medialib.Loop, bool) {
	for _, loop := range s.librarySnapshot.Loops {
		if loop.ID == id {
			return loop, true
		}
	}
	return medialib.Loop{}, false
}

func (s *Service) findMusicByID(id string) (medialib.Music, bool) {
	for _, music := range s.librarySnapshot.Music {
		if music.ID == id {
			return music, true
		}
	}
	return medialib.Music{}, false
}

func periodPlanDate(t time.Time, period medialib.Period) string {
	if period == medialib.PeriodNight && t.Hour() < 6 {
		t = t.AddDate(0, 0, -1)
	}
	return t.Format("2006-01-02")
}

func overrideDateKey(t time.Time) string {
	return t.Format("2006-01-02")
}

func (s *Service) ImportLibraryUpload(ctx context.Context, req UploadRequest) (string, error) {
	if strings.TrimSpace(req.LocalPath) == "" {
		err := errors.New("local media path is required")
		s.setLastErr(err)
		return "", err
	}
	if !filepath.IsAbs(req.LocalPath) {
		err := fmt.Errorf("local media path must be absolute: %s", req.LocalPath)
		s.setLastErr(err)
		return "", err
	}
	plan, err := s.planLibraryUpload(req.FileName)
	if err != nil {
		s.setLastErr(err)
		return "", err
	}

	if err := os.MkdirAll(plan.destDir, 0o755); err != nil {
		s.setLastErr(err)
		return "", err
	}
	destPath := filepath.Join(plan.destDir, plan.fileName)
	if err := s.storeLibraryUpload(ctx, plan.kind, destPath, req.LocalPath); err != nil {
		s.setLastErr(err)
		return "", err
	}

	if err := s.ScanLibrary(ctx); err != nil {
		return fmt.Sprintf("已匯入素材：%s（掃描時發現問題：%v）", plan.label, err), nil
	}
	return fmt.Sprintf("已匯入素材：%s", plan.label), nil
}

func (s *Service) planLibraryUpload(rawFileName string) (libraryUploadPlan, error) {
	fileName := CleanFileName(rawFileName)
	if fileName == "" {
		return libraryUploadPlan{}, publicError("檔名不可為空，請使用新版素材命名規則。")
	}
	if parsed, err := medialib.ParseLoopFilename(fileName); err == nil {
		return libraryUploadPlan{
			fileName: fileName,
			kind:     medialib.KindLoop,
			destDir:  s.cfg.LoopMediaDir,
			label:    fmt.Sprintf("loop %s/%s", parsed.Period, parsed.Theme),
		}, nil
	}
	if parsed, err := medialib.ParseMusicFilename(fileName); err == nil {
		return libraryUploadPlan{
			fileName: fileName,
			kind:     medialib.KindMusic,
			destDir:  s.cfg.MusicMediaDir,
			label:    fmt.Sprintf("music %s", parsed.Track),
		}, nil
	}
	return libraryUploadPlan{}, publicError("檔名不符合素材規則。loop 請用 loop_<period>_<theme>_<variant>，音樂請用 music_<track>。")
}

func (s *Service) preflightLibraryUpload(ctx context.Context, fileName string, declaredSize int64) error {
	return s.preflightLibraryUploadWithLimit(ctx, fileName, declaredSize, medialib.MaxDirectoryEntries)
}

func (s *Service) preflightLibraryUploadWithLimit(
	ctx context.Context,
	fileName string,
	declaredSize int64,
	limit int,
) error {
	plan, err := s.planLibraryUpload(fileName)
	if err != nil {
		return err
	}

	s.storageMu.Lock()
	defer s.storageMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	destPath := filepath.Join(plan.destDir, plan.fileName)
	if _, err := os.Stat(destPath); err == nil {
		return publicError("媒體庫已有同名素材，請換一個 variant 或 track 名稱。")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureLibraryImportCapacityWithLimit(plan.destDir, limit); err != nil {
		return err
	}
	sharedFilesystem, err := pathsShareFilesystem(s.cfg.TelegramBotAPIDir, plan.destDir)
	if err != nil {
		return err
	}
	if sharedFilesystem {
		combinedIncoming, err := requiredAvailableBytes(uint64(declaredSize), uint64(declaredSize))
		if err != nil {
			return err
		}
		return s.ensureStorageHeadroomBytes(plan.destDir, combinedIncoming)
	}
	if err := s.ensureStorageHeadroom(s.cfg.TelegramBotAPIDir, declaredSize); err != nil {
		return err
	}
	return s.ensureStorageHeadroom(plan.destDir, declaredSize)
}

func (s *Service) storeLibraryUpload(ctx context.Context, kind medialib.Kind, destPath, sourcePath string) error {
	return s.storeLibraryUploadWithLimit(
		ctx,
		kind,
		destPath,
		sourcePath,
		medialib.MaxDirectoryEntries,
	)
}

func (s *Service) storeLibraryUploadWithLimit(
	ctx context.Context,
	kind medialib.Kind,
	destPath string,
	sourcePath string,
	limit int,
) error {
	source, err := openLocalBotAPIFile(s.cfg.TelegramBotAPIDir, sourcePath)
	if err != nil {
		return err
	}
	defer source.Close()
	return s.storeOpenedLibraryUploadWithLimit(ctx, kind, destPath, source, limit)
}

func (s *Service) storeOpenedLibraryUploadWithLimit(
	ctx context.Context,
	kind medialib.Kind,
	destPath string,
	source *os.File,
	limit int,
) error {
	s.storageMu.Lock()
	defer s.storageMu.Unlock()

	if err := ctx.Err(); err != nil {
		return err
	}
	if _, err := os.Stat(destPath); err == nil {
		return publicError("媒體庫已有同名素材，請換一個 variant 或 track 名稱。")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := ensureLibraryImportCapacityWithLimit(filepath.Dir(destPath), limit); err != nil {
		return err
	}
	if kind != medialib.KindLoop && kind != medialib.KindMusic {
		return fmt.Errorf("unsupported media library kind: %s", kind)
	}
	validator := func(validationCtx context.Context, stagingPath string) error {
		probeCtx, cancelProbe := context.WithTimeout(validationCtx, uploadProbeTimeout)
		defer cancelProbe()
		meta, err := s.media.Probe(probeCtx, stagingPath)
		if err != nil {
			return err
		}
		return s.media.Validate(meta, s.cfg.MaxVideoSizeBytes, s.cfg.MaxVideoDurationSeconds)
	}
	_, err := copyOpenedFileAtomic(
		ctx,
		destPath,
		source,
		s.cfg.MaxVideoSizeBytes,
		func(actualSize int64) error {
			return s.ensureStorageHeadroom(filepath.Dir(destPath), actualSize)
		},
		validator,
	)
	if err != nil {
		return err
	}
	return nil
}

func ensureLibraryImportCapacityWithLimit(destDir string, limit int) error {
	if limit <= 0 {
		return errors.New("media library directory limit must be positive")
	}
	additionalEntries := 1
	stagingDir := filepath.Join(destDir, libraryStagingDirName)
	if info, err := os.Lstat(stagingDir); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("library staging path is not an owned directory: %s", stagingDir)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		additionalEntries++
	} else {
		return err
	}

	handle, err := os.Open(destDir)
	if err != nil {
		return err
	}
	defer handle.Close()
	entries, err := handle.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if additionalEntries > limit || len(entries) > limit-additionalEntries {
		return publicError(fmt.Sprintf(
			"媒體庫目錄已達 %d 個項目上限，請先移除不需要的素材後再重試。",
			limit,
		))
	}
	return nil
}
