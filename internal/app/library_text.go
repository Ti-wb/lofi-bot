package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
)

func (s *Service) LibraryText(ctx context.Context) (string, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if len(s.librarySnapshot.Loops) == 0 && len(s.librarySnapshot.Music) == 0 && s.libraryScanErr == "" {
		_ = s.scanLibraryLocked(ctx)
	}
	summary := s.librarySnapshot.Summary()
	lines := []string{
		"媒體庫：",
		fmt.Sprintf("Loop：%d", summary.LoopCount),
		fmt.Sprintf("Music：%d", summary.MusicCount),
	}
	for _, period := range medialib.Periods() {
		themes := s.librarySnapshot.AvailableThemes(period)
		lines = append(lines, fmt.Sprintf("%s：%d 支 / %d 主題", periodLabel(period), summary.LoopCountByPeriod[period], len(themes)))
	}
	if len(s.librarySnapshot.Loops) > 0 {
		lines = append(lines, "", "Loop 素材：")
		for _, loop := range firstLoops(s.librarySnapshot.Loops, 12) {
			lines = append(lines, fmt.Sprintf("%s %s %s %s", loop.ID, periodLabel(loop.Period), loop.Theme, loop.Filename))
		}
		if len(s.librarySnapshot.Loops) > 12 {
			lines = append(lines, fmt.Sprintf("...另有 %d 支", len(s.librarySnapshot.Loops)-12))
		}
	}
	if s.libraryScanErr != "" {
		lines = append(lines, "", "掃描提醒："+s.libraryScanErr)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) ScanLibraryText(ctx context.Context) (string, error) {
	err := s.ScanLibrary(ctx)
	s.playbackMu.Lock()
	loops := len(s.librarySnapshot.Loops)
	music := len(s.librarySnapshot.Music)
	scanErr := s.libraryScanErr
	s.playbackMu.Unlock()
	if err != nil {
		return fmt.Sprintf("已掃描媒體庫：Loop %d / Music %d\n提醒：%s", loops, music, scanErr), nil
	}
	return fmt.Sprintf("已掃描媒體庫：Loop %d / Music %d", loops, music), nil
}

func (s *Service) PreviewText(ctx context.Context) (string, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	loop, info, reason, err := s.previewLoopLocked(ctx)
	if err != nil {
		s.setLastErr(err)
		return "", err
	}
	lines := []string{
		"下一時段預告：",
		fmt.Sprintf("時段：%s", periodLabel(info.Period)),
		fmt.Sprintf("主題：%s", loop.Theme),
		fmt.Sprintf("影片：%s", loop.Filename),
		fmt.Sprintf("ID：%s", loop.ID),
	}
	if reason != "" {
		lines = append(lines, "提醒："+reason)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) LibraryNowText(ctx context.Context) (string, error) {
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if err := s.ensureLibraryPlaybackLocked(ctx, false); err != nil {
		s.setLastErr(err)
		return "", err
	}
	music := "無"
	if s.activeMusicPath != "" {
		music = filepath.Base(s.activeMusicPath)
	}
	return strings.Join([]string{
		"目前播放：",
		fmt.Sprintf("時段：%s（到 %s）", periodLabel(s.activeLoopPeriod), s.activeLoopEndsAt.Format("15:04")),
		fmt.Sprintf("主題：%s", s.activeLoopTheme),
		fmt.Sprintf("Loop：%s", filepath.Base(s.activeLoopPath)),
		fmt.Sprintf("Loop ID：%s", s.activeLoopID),
		fmt.Sprintf("Music：%s", music),
	}, "\n"), nil
}

func (s *Service) LibraryStatusText(ctx context.Context, obsConnected bool) (string, error) {
	s.playbackMu.Lock()
	if len(s.librarySnapshot.Loops) == 0 && len(s.librarySnapshot.Music) == 0 && s.libraryScanErr == "" {
		_ = s.scanLibraryLocked(ctx)
	}
	summary := s.librarySnapshot.Summary()
	activeLoop := filepath.Base(s.activeLoopPath)
	activeMusic := filepath.Base(s.activeMusicPath)
	scanErr := s.libraryScanErr
	s.playbackMu.Unlock()

	diskText := "未知"
	if usage, err := media.DiskUsageForPath(s.cfg.MediaDir); err == nil {
		diskText = fmt.Sprintf("%s free / %s total", formatBytes(int64(usage.AvailableBytes)), formatBytes(int64(usage.TotalBytes)))
	}
	lastErr := s.lastError()
	if lastErr == "" {
		lastErr = "無"
	}
	nextPreview := "未產生"
	if text, err := s.PreviewText(ctx); err == nil {
		nextPreview = strings.ReplaceAll(text, "\n", " / ")
	}
	if activeLoop == "." {
		activeLoop = "無"
	}
	if activeMusic == "." {
		activeMusic = "無"
	}
	lines := []string{
		"狀態：",
		fmt.Sprintf("OBS：%s", boolText(obsConnected)),
		"Mode：library",
		fmt.Sprintf("Loop：%d", summary.LoopCount),
		fmt.Sprintf("Music：%d", summary.MusicCount),
		fmt.Sprintf("Current loop：%s", activeLoop),
		fmt.Sprintf("Current music：%s", activeMusic),
		fmt.Sprintf("Next：%s", nextPreview),
		fmt.Sprintf("Media Disk：%s", diskText),
		fmt.Sprintf("Last error：%s", lastErr),
	}
	if scanErr != "" {
		lines = append(lines, "Scan warning："+scanErr)
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) SetThemeText(ctx context.Context, theme string) (string, error) {
	if s.libDB == nil {
		return "", errorsLibraryUnavailable()
	}
	date := overrideDateKey(s.now())
	if strings.EqualFold(theme, "random") {
		if err := s.libDB.ClearThemeOverride(ctx, date); err != nil {
			s.setLastErr(err)
			return "", err
		}
		if err := s.libDB.ClearPlansForDate(ctx, date); err != nil {
			s.setLastErr(err)
			return "", err
		}
		_ = s.ensureLibraryPlayback(ctx, true)
		return "已切回隨機主題。", nil
	}
	theme = strings.TrimSpace(theme)
	if theme == "" || strings.Contains(theme, "_") || strings.ContainsAny(theme, `/\`) {
		return "", publicError("主題不可為空，也不能包含底線或路徑符號。")
	}
	if err := s.libDB.SetThemeOverride(ctx, date, theme); err != nil {
		s.setLastErr(err)
		return "", err
	}
	if err := s.libDB.ClearPlansForDate(ctx, date); err != nil {
		s.setLastErr(err)
		return "", err
	}
	_ = s.ensureLibraryPlayback(ctx, true)
	return fmt.Sprintf("今日主題已指定為：%s", theme), nil
}

func (s *Service) SelectLoopText(ctx context.Context, assetID string) (string, error) {
	if s.libDB == nil {
		return "", errorsLibraryUnavailable()
	}
	date := overrideDateKey(s.now())
	if strings.EqualFold(assetID, "clear") {
		if err := s.libDB.ClearDirectLoopOverride(ctx, date); err != nil {
			s.setLastErr(err)
			return "", err
		}
		_ = s.ensureLibraryPlayback(ctx, true)
		return "已清除指定影片。", nil
	}
	s.playbackMu.Lock()
	loop, ok := s.findLoopByID(assetID)
	s.playbackMu.Unlock()
	if !ok {
		return "", publicError("找不到這個 loop asset ID，請用 /library 查看。")
	}
	if err := s.libDB.SetDirectLoopOverride(ctx, date, assetID); err != nil {
		s.setLastErr(err)
		return "", err
	}
	_ = s.ensureLibraryPlayback(ctx, true)
	return fmt.Sprintf("今日指定影片：%s（%s / %s）", loop.Filename, periodLabel(loop.Period), loop.Theme), nil
}

func (s *Service) SkipLoopText(ctx context.Context) (string, error) {
	if s.libDB == nil {
		return "", errorsLibraryUnavailable()
	}
	if err := s.libDB.ClearDirectLoopOverride(ctx, overrideDateKey(s.now())); err != nil {
		s.setLastErr(err)
		return "", err
	}
	if err := s.ensureLibraryPlayback(ctx, true); err != nil {
		s.setLastErr(err)
		return "", err
	}
	return "已重抽目前時段 loop。", nil
}

func (s *Service) SkipMusicText(ctx context.Context) (string, error) {
	if err := s.playNextMusic(ctx, true); err != nil {
		s.setLastErr(err)
		return "", err
	}
	return "已切換音樂。", nil
}

func firstLoops(loops []medialib.Loop, limit int) []medialib.Loop {
	copied := make([]medialib.Loop, len(loops))
	copy(copied, loops)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].RelPath < copied[j].RelPath
	})
	if len(copied) <= limit {
		return copied
	}
	return copied[:limit]
}

func periodLabel(period medialib.Period) string {
	switch period {
	case medialib.PeriodMorning:
		return "早晨"
	case medialib.PeriodDay:
		return "白天"
	case medialib.PeriodEvening:
		return "傍晚"
	case medialib.PeriodNight:
		return "晚上"
	default:
		return string(period)
	}
}

func errorsLibraryUnavailable() error {
	return publicError("目前不是 library 播放模式。")
}
