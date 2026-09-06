package app

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

const libraryPageSize = 12

func (s *Service) LibraryText(ctx context.Context) (string, error) {
	return s.LibraryPageText(ctx, 1)
}

func (s *Service) LibraryPageText(ctx context.Context, page int) (string, error) {
	result, err := s.LibraryPage(ctx, page)
	return result.Text, err
}

func (s *Service) LibraryPage(ctx context.Context, page int) (telegram.LibraryPageResult, error) {
	if page <= 0 {
		return telegram.LibraryPageResult{}, publicError("頁碼必須是正整數。")
	}
	if err := s.ensureLibraryScanned(ctx); err != nil && ctx.Err() != nil {
		return telegram.LibraryPageResult{}, ctx.Err()
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return telegram.LibraryPageResult{}, ctxErr
	}
	summary := s.librarySnapshot.Summary()
	loops := sortedLoops(s.librarySnapshot.Loops)
	totalPages := 1
	if len(loops) > 0 {
		totalPages = (len(loops) + libraryPageSize - 1) / libraryPageSize
	}
	if page > totalPages {
		return telegram.LibraryPageResult{}, publicError(fmt.Sprintf("頁碼超出範圍；媒體庫目前共有 %d 頁。", totalPages))
	}
	lines := []string{
		"媒體庫：",
		fmt.Sprintf("Loop：%d", summary.LoopCount),
		fmt.Sprintf("Music：%d", summary.MusicCount),
	}
	for _, period := range medialib.Periods() {
		themes := s.librarySnapshot.AvailableThemes(period)
		lines = append(lines, fmt.Sprintf("%s：%d 支 / %d 主題", periodLabel(period), summary.LoopCountByPeriod[period], len(themes)))
	}
	if len(loops) > 0 {
		start := (page - 1) * libraryPageSize
		end := start + libraryPageSize
		if end > len(loops) {
			end = len(loops)
		}
		lines = append(lines, "", "Loop 素材：")
		for _, loop := range loops[start:end] {
			// The filename already contains the theme. Avoid repeating it so a
			// full 12-item page still fits Telegram's 4096-byte message limit
			// even at the maximum accepted filename length.
			lines = append(lines, fmt.Sprintf("%s %s %s", loop.ID, periodLabel(loop.Period), loop.Filename))
		}
	}
	lines = append(lines, "", fmt.Sprintf("頁面：%d/%d", page, totalPages))
	if totalPages > 1 {
		lines = append(lines, "使用 /library <頁碼> 或下方按鈕查看其他素材。")
	}
	if s.libraryScanErr != "" {
		lines = append(lines, "", "掃描提醒："+s.libraryScanErr)
	}
	return telegram.LibraryPageResult{
		Text:       strings.Join(lines, "\n"),
		Page:       page,
		TotalPages: totalPages,
	}, nil
}

func (s *Service) ScanLibraryText(ctx context.Context) (string, error) {
	err := s.ScanLibrary(ctx)
	s.playbackMu.Lock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.playbackMu.Unlock()
		return "", ctxErr
	}
	loops := len(s.librarySnapshot.Loops)
	music := len(s.librarySnapshot.Music)
	scanErr := s.libraryScanErr
	s.playbackMu.Unlock()
	if err != nil {
		//nolint:nilerr // A partial scan is a successful command response with a user-visible warning.
		return fmt.Sprintf("已掃描媒體庫：Loop %d / Music %d\n提醒：%s", loops, music, scanErr), nil
	}
	return fmt.Sprintf("已掃描媒體庫：Loop %d / Music %d", loops, music), nil
}

func (s *Service) PreviewText(ctx context.Context) (string, error) {
	if err := s.ensureLibraryScanned(ctx); err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
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
	if err := s.ensureLibraryScanned(ctx); err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", ctxErr
	}
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
	if err := s.ensureLibraryScanned(ctx); err != nil && ctx.Err() != nil {
		return "", ctx.Err()
	}
	s.playbackMu.Lock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		s.playbackMu.Unlock()
		return "", ctxErr
	}
	summary := s.librarySnapshot.Summary()
	playableSummary := s.playableLibraryLocked().Summary()
	activeLoop := filepath.Base(s.activeLoopPath)
	activeMusic := filepath.Base(s.activeMusicPath)
	scanErr := s.libraryScanErr
	rejectedCount := s.libraryRejectedCount
	quarantineCount := s.libraryQuarantineCountLocked()
	s.playbackMu.Unlock()

	overrideText := "無"
	invalidThemeOverride := false
	if s.libDB != nil {
		override, err := s.libDB.Override(ctx, overrideDateKey(s.now()))
		if err != nil {
			return "", err
		}
		switch {
		case override.DirectLoopID != "":
			overrideText = "指定 loop " + override.DirectLoopID
		case override.Theme != "":
			if libraryThemeWithinTextBounds(override.Theme) {
				overrideText = "主題 " + override.Theme
			} else {
				// Older databases and direct state-store callers may predate
				// the ingress bound. Keep /status useful without reflecting
				// an unbounded persisted value back into Telegram.
				overrideText = "主題（已隱藏：文字無效或超過安全長度）"
				invalidThemeOverride = true
			}
		}
	}

	nextPreview := "未產生"
	if invalidThemeOverride {
		// Preview fallback diagnostics include the requested theme. Do not
		// invoke that path for legacy unbounded state.
		nextPreview = "未產生（主題設定無效或超過安全長度）"
	} else if text, err := s.PreviewText(ctx); err == nil {
		nextPreview = strings.ReplaceAll(text, "\n", " / ")
	}
	lastErr := s.lastError()
	if lastErr == "" {
		lastErr = "無"
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
		fmt.Sprintf("Loop：%d", summary.LoopCount),
		fmt.Sprintf("Music：%d", summary.MusicCount),
		fmt.Sprintf("Current loop：%s", activeLoop),
		fmt.Sprintf("Current music：%s", activeMusic),
		fmt.Sprintf("Next：%s", nextPreview),
		fmt.Sprintf("Override：%s", overrideText),
		fmt.Sprintf("Playable loop/music：%d/%d", playableSummary.LoopCount, playableSummary.MusicCount),
		fmt.Sprintf("Rejected/quarantined：%d/%d", rejectedCount, quarantineCount),
		fmt.Sprintf("Loop Disk：%s", s.diskUsageText(s.cfg.LoopMediaDir)),
		fmt.Sprintf("Music Disk：%s", s.diskUsageText(s.cfg.MusicMediaDir)),
		fmt.Sprintf("Telegram Bot API Disk：%s", s.diskUsageText(s.cfg.TelegramBotAPIDir)),
		fmt.Sprintf("Last error：%s", lastErr),
	}
	if scanErr != "" {
		lines = append(lines, "Scan warning："+scanErr)
	}
	return boundedTelegramText(strings.Join(lines, "\n")), nil
}

func (s *Service) diskUsageText(path string) string {
	probe := s.diskUsage
	if probe == nil {
		probe = media.DiskUsageForPath
	}
	usage, err := probe(path)
	if err != nil {
		return "未知"
	}
	return fmt.Sprintf(
		"%s free / %s total",
		formatBytes(int64(usage.AvailableBytes)),
		formatBytes(int64(usage.TotalBytes)),
	)
}

func (s *Service) SetThemeText(ctx context.Context, theme string) (string, error) {
	if s.libDB == nil {
		return "", errorsLibraryUnavailable()
	}
	theme = strings.TrimSpace(theme)
	if strings.EqualFold(theme, "random") {
		if err := s.applyLibraryPlaybackMutation(ctx, func() error {
			date := overrideDateKey(s.now())
			if err := s.libDB.ClearThemeOverride(ctx, date); err != nil {
				return err
			}
			return s.libDB.ClearPlansForDate(ctx, date)
		}); err != nil {
			s.setLastErr(err)
			return "", err
		}
		return "已切回隨機主題。", nil
	}
	if theme == "" || strings.Contains(theme, "_") || strings.ContainsAny(theme, `/\`) {
		return "", publicError("主題不可為空，也不能包含底線或路徑符號。")
	}
	if !utf8.ValidString(theme) {
		return "", publicError("主題必須是有效的 UTF-8 文字。")
	}
	if !libraryThemeWithinTextBounds(theme) {
		return "", publicError(fmt.Sprintf(
			"主題不可超過 %d 個字元或 %d UTF-8 位元組。",
			telegram.MaxThemeRunes,
			telegram.MaxThemeUTF8Bytes,
		))
	}
	if err := s.applyLibraryPlaybackMutation(ctx, func() error {
		date := overrideDateKey(s.now())
		if err := s.libDB.SetThemeOverride(ctx, date, theme); err != nil {
			return err
		}
		return s.libDB.ClearPlansForDate(ctx, date)
	}); err != nil {
		s.setLastErr(err)
		return "", err
	}
	return fmt.Sprintf("今日主題已指定為：%s", theme), nil
}

func libraryThemeWithinTextBounds(theme string) bool {
	return utf8.ValidString(theme) &&
		utf8.RuneCountInString(theme) <= telegram.MaxThemeRunes &&
		len(theme) <= telegram.MaxThemeUTF8Bytes
}

func boundedTelegramText(text string) string {
	const (
		maxRunes = 4096
		maxBytes = 4096
	)
	if utf8.ValidString(text) &&
		utf8.RuneCountInString(text) <= maxRunes &&
		len(text) <= maxBytes {
		return text
	}

	const suffix = "\n…"
	runeBudget := maxRunes - utf8.RuneCountInString(suffix)
	byteBudget := maxBytes - len(suffix)
	var bounded strings.Builder
	bounded.Grow(min(len(text), byteBudget+len(suffix)))
	runes := 0
	for _, value := range text {
		size := utf8.RuneLen(value)
		if size < 0 || runes >= runeBudget || bounded.Len()+size > byteBudget {
			break
		}
		bounded.WriteRune(value)
		runes++
	}
	bounded.WriteString(suffix)
	return bounded.String()
}

func (s *Service) SelectLoopText(ctx context.Context, assetID string) (string, error) {
	if s.libDB == nil {
		return "", errorsLibraryUnavailable()
	}
	if strings.EqualFold(assetID, "clear") {
		if err := s.applyLibraryPlaybackMutation(ctx, func() error {
			return s.libDB.ClearDirectLoopOverride(ctx, overrideDateKey(s.now()))
		}); err != nil {
			s.setLastErr(err)
			return "", err
		}
		return "已清除指定影片。", nil
	}
	var loop medialib.Loop
	if err := s.applyLibraryPlaybackMutation(ctx, func() error {
		var ok bool
		loop, ok = s.findLoopByID(assetID)
		if !ok {
			return publicError("找不到這個 loop asset ID，請用 /library 查看。")
		}
		return s.libDB.SetDirectLoopOverride(ctx, overrideDateKey(s.now()), assetID)
	}); err != nil {
		s.setLastErr(err)
		return "", err
	}
	return fmt.Sprintf("今日指定影片：%s（%s / %s）", loop.Filename, periodLabel(loop.Period), loop.Theme), nil
}

func (s *Service) SkipLoopText(ctx context.Context) (string, error) {
	if s.libDB == nil {
		return "", errorsLibraryUnavailable()
	}
	if err := s.applyLibraryPlaybackMutation(ctx, func() error {
		return s.libDB.ClearDirectLoopOverride(ctx, overrideDateKey(s.now()))
	}); err != nil {
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

// applyLibraryPlaybackMutation serializes durable operator intent with
// scheduler candidate selection and its rollback. Scanning remains outside
// playbackMu; only the DB mutation and resulting activation share the lock.
func (s *Service) applyLibraryPlaybackMutation(
	ctx context.Context,
	mutation func() error,
) error {
	if err := s.ensureLibraryScanned(ctx); err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	s.playbackMu.Lock()
	defer s.playbackMu.Unlock()
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if err := mutation(); err != nil {
		return err
	}
	return s.ensureLibraryPlaybackLocked(ctx, true)
}

func sortedLoops(loops []medialib.Loop) []medialib.Loop {
	copied := make([]medialib.Loop, len(loops))
	copy(copied, loops)
	sort.Slice(copied, func(i, j int) bool {
		return copied[i].RelPath < copied[j].RelPath
	})
	return copied
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
	return publicError("媒體庫狀態目前無法使用。")
}
