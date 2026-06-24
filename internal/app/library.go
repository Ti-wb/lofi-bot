package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

const librarySchedulerInterval = 15 * time.Second

func (s *Service) libraryMode() bool {
	return s.cfg.PlayerMode == "library"
}

func (s *Service) ScanLibrary(ctx context.Context) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	return s.scanLibraryLocked(ctx)
}

func (s *Service) scanLibraryLocked(ctx context.Context) error {
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
	lib, err := medialib.ScanDirs(s.cfg.LoopMediaDir, s.cfg.MusicMediaDir)
	s.librarySnapshot = lib
	if err != nil {
		s.libraryScanErr = err.Error()
		s.setLastErr(err)
		return err
	}
	s.libraryScanErr = ""
	return nil
}

func (s *Service) librarySchedulerLoop(ctx context.Context) {
	ticker := time.NewTicker(librarySchedulerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if s.obs.Status().State != obs.StateConnected {
				continue
			}
			if err := s.ensureLibraryPlayback(ctx, false); err != nil {
				s.setLastErr(err)
				s.logger.Warn("library playback check failed", "error", s.redactError(err))
			}
		}
	}
}

func (s *Service) recoverLibraryPlaybackAfterOBSConnect(ctx context.Context) error {
	if err := s.ScanLibrary(ctx); err != nil {
		s.logger.Warn("media library scan found issues during OBS recovery", "error", s.redactError(err))
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	s.clearActiveLoopLocked()
	s.clearActiveMusicLocked()
	return s.ensureLibraryPlaybackLocked(ctx, false)
}

func (s *Service) handleLibraryOBSEvent(ctx context.Context, event obs.Event) error {
	switch event.InputName {
	case s.cfg.OBSMusicSourceName:
		return s.playNextMusicAfterEnded(ctx)
	case s.cfg.OBSLoopSourceName:
		return s.restartLibraryLoopAfterEnded(ctx)
	default:
		return nil
	}
}

func (s *Service) restartLibraryLoopAfterEnded(ctx context.Context) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	s.clearActiveLoopLocked()

	loop, info, _, err := s.loopForTimeLocked(ctx, s.now(), false)
	if err != nil {
		return err
	}
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
		s.activeLoopID = loop.ID
		s.activeLoopPath = loop.Path
		s.activeLoopTheme = loop.Theme
		s.activeLoopPeriod = loop.Period
		s.activeLoopEndsAt = info.EndsAt
		s.setPlaybackState(playbackFile, 0, loop.Path)
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

func (s *Service) playNextMusicAfterEnded(ctx context.Context) error {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	s.clearActiveMusicLocked()
	return s.playNextMusicLocked(ctx, true)
}

func (s *Service) playNextMusicLocked(ctx context.Context, force bool) error {
	if s.libDB == nil {
		return nil
	}
	if len(s.librarySnapshot.Music) == 0 {
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
		return nil
	}
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
	s.activeMusicID = music.ID
	s.activeMusicPath = music.Path
	if err := s.libDB.SetLastMusicID(ctx, music.ID); err != nil && force {
		return err
	}
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
	} else if ok && !force && !expiredPreviousOverride && (override.Theme == "" || override.Theme == plan.Theme) {
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
	if req.SizeBytes > s.cfg.MaxVideoSizeBytes {
		err := publicError(fmt.Sprintf("檔案太大，上限是 %s", formatBytes(s.cfg.MaxVideoSizeBytes)))
		s.setLastErr(err)
		return "", err
	}
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
	if err := validateLocalBotAPIPath(s.cfg.TelegramBotAPIDir, req.LocalPath); err != nil {
		s.setLastErr(err)
		return "", err
	}
	info, err := os.Stat(req.LocalPath)
	if err != nil {
		s.setLastErr(err)
		return "", err
	}
	if info.Size() > s.cfg.MaxVideoSizeBytes {
		err := publicError(fmt.Sprintf("檔案太大，上限是 %s", formatBytes(s.cfg.MaxVideoSizeBytes)))
		s.setLastErr(err)
		return "", err
	}

	fileName := CleanFileName(req.FileName)
	if fileName == "" {
		err := publicError("檔名不可為空，請使用新版素材命名規則。")
		s.setLastErr(err)
		return "", err
	}

	var (
		kind    medialib.Kind
		destDir string
		label   string
	)
	if parsed, err := medialib.ParseLoopFilename(fileName); err == nil {
		kind = medialib.KindLoop
		destDir = s.cfg.LoopMediaDir
		label = fmt.Sprintf("loop %s/%s", parsed.Period, parsed.Theme)
	} else if parsed, musicErr := medialib.ParseMusicFilename(fileName); musicErr == nil {
		kind = medialib.KindMusic
		destDir = s.cfg.MusicMediaDir
		label = fmt.Sprintf("music %s", parsed.Track)
	} else {
		err := publicError("檔名不符合素材規則。loop 請用 loop_<period>_<theme>_<variant>，音樂請用 music_<track>。")
		s.setLastErr(err)
		return "", err
	}

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		s.setLastErr(err)
		return "", err
	}
	destPath := filepath.Join(destDir, fileName)
	if _, err := os.Stat(destPath); err == nil {
		err := publicError("媒體庫已有同名素材，請換一個 variant 或 track 名稱。")
		s.setLastErr(err)
		return "", err
	} else if !errors.Is(err, os.ErrNotExist) {
		s.setLastErr(err)
		return "", err
	}
	if err := copyFile(destPath, req.LocalPath); err != nil {
		s.setLastErr(err)
		return "", err
	}

	if kind == medialib.KindLoop {
		probeCtx, cancelProbe := context.WithTimeout(ctx, uploadProbeTimeout)
		meta, err := s.media.Probe(probeCtx, destPath)
		cancelProbe()
		if err != nil {
			_ = os.Remove(destPath)
			s.setLastErr(err)
			return "", err
		}
		if err := s.media.Validate(meta, s.cfg.MaxVideoSizeBytes, s.cfg.MaxVideoDurationSeconds); err != nil {
			_ = os.Remove(destPath)
			s.setLastErr(err)
			return "", err
		}
	}

	if err := s.ScanLibrary(ctx); err != nil {
		return fmt.Sprintf("已匯入素材：%s（掃描時發現問題：%v）", label, err), nil
	}
	return fmt.Sprintf("已匯入素材：%s", label), nil
}

func copyFile(dst string, src string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".tmp-" + shortHash(src+time.Now().String())
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	closeErr := out.Close()
	if copyErr != nil {
		_ = os.Remove(tmp)
		return copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmp)
		return closeErr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func shortHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])[:8]
}
