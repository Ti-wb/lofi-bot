package app

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/media"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

func TestLibraryStatusReportsExactDiskPathsAndDegradedState(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.now = fixedNow("2026-07-31T12:00:00+08:00")
	if err := svc.libDB.SetThemeOverride(
		ctx,
		overrideDateKey(svc.now()),
		"midnight",
	); err != nil {
		t.Fatalf("set theme override: %v", err)
	}

	// Keep the snapshot empty so preview deterministically fails, and keep a
	// scan warning set so the status call does not replace this fixture.
	svc.librarySnapshotPublished = true
	svc.libraryScanErr = "fixture scan warning"
	svc.libraryRejectedCount = 2
	svc.libraryQuarantine = map[string]libraryQuarantineEntry{
		"/missing/quarantined-loop.mp4": {
			reason: "OBS rejected the asset",
		},
	}

	const mib = uint64(1024 * 1024)
	wantUsage := map[string]media.DiskUsage{
		svc.cfg.LoopMediaDir: {
			AvailableBytes: 1 * mib,
			TotalBytes:     10 * mib,
		},
		svc.cfg.MusicMediaDir: {
			AvailableBytes: 2 * mib,
			TotalBytes:     20 * mib,
		},
		svc.cfg.TelegramBotAPIDir: {
			AvailableBytes: 3 * mib,
			TotalBytes:     30 * mib,
		},
	}
	probed := make(map[string]int, len(wantUsage))
	svc.diskUsage = func(path string) (media.DiskUsage, error) {
		usage, ok := wantUsage[path]
		if !ok {
			t.Fatalf("diskUsage path = %q, want one of the three configured exact paths", path)
		}
		probed[path]++
		return usage, nil
	}

	text, err := svc.LibraryStatusText(ctx, false)
	if err != nil {
		t.Fatalf("library status: %v", err)
	}
	for path := range wantUsage {
		if got := probed[path]; got != 1 {
			t.Errorf("diskUsage calls for %q = %d, want 1", path, got)
		}
	}
	for _, want := range []string{
		"Loop Disk：1.0 MiB free / 10.0 MiB total",
		"Music Disk：2.0 MiB free / 20.0 MiB total",
		"Telegram Bot API Disk：3.0 MiB free / 30.0 MiB total",
		"Override：主題 midnight",
		"Rejected/quarantined：2/1",
		"Next：未產生",
		"Last error：媒體庫沒有可播放的 loop 影片。",
		"Scan warning：fixture scan warning",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("status text missing %q:\n%s", want, text)
		}
	}
}

func TestLibraryStatusNeverExposesRawInternalPaths(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.now = fixedNow("2026-07-31T12:00:00+08:00")
	svc.librarySnapshot = medialib.Library{Loops: []medialib.Loop{{
		ID:       "loop-safe",
		Path:     "/operator/library/loop_evening_safe_001.mp4",
		RelPath:  "loops/loop_evening_safe_001.mp4",
		Filename: "loop_evening_safe_001.mp4",
		Period:   medialib.PeriodEvening,
		Theme:    "safe",
	}}}
	privateCachePath := svc.cfg.TelegramBotAPIDir + "/private dir/secret video.mp4"
	relativeCachePath := "./relative private/cache secret.bin"
	secondPath := "/another private root/operator only.mov"
	generation := svc.libraryScanGeneration.Add(1)
	_ = svc.publishLibraryScanFailure(
		ctx,
		generation,
		fmt.Errorf(
			"open %s: permission denied, compare %s: input/output error",
			privateCachePath,
			secondPath,
		),
	)
	svc.setLastErr(fmt.Errorf(
		"read %s: input/output error, cache %s unavailable",
		privateCachePath,
		relativeCachePath,
	))
	svc.diskUsage = func(string) (media.DiskUsage, error) {
		return media.DiskUsage{}, nil
	}

	text, err := svc.LibraryStatusText(ctx, true)
	if err != nil {
		t.Fatalf("LibraryStatusText: %v", err)
	}
	for _, leaked := range []string{
		svc.cfg.TelegramBotAPIDir,
		"private dir",
		"secret video.mp4",
		"relative private",
		"cache secret.bin",
		"another private root",
		"operator only.mov",
		"permission denied",
		"input/output error",
	} {
		if strings.Contains(text, leaked) {
			t.Fatalf("status leaked %q:\n%s", leaked, text)
		}
	}
	for _, want := range []string{
		"Last error：內部操作失敗；詳細資訊請查看服務日誌。",
		"Scan warning：媒體庫掃描失敗；詳細資訊請查看服務日誌。",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("status missing safe diagnostic %q:\n%s", want, text)
		}
	}
}

func TestLibraryPageTwelveThirteenBoundaryAndShrink(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	loops := makeLibraryPageLoops(t, 13, false)

	svc.librarySnapshot = medialib.Library{Loops: append([]medialib.Loop(nil), loops[:12]...)}
	page, err := svc.LibraryPage(ctx, 1)
	if err != nil {
		t.Fatalf("12-item page 1: %v", err)
	}
	if page.Page != 1 || page.TotalPages != 1 {
		t.Fatalf("12-item result = page %d/%d, want 1/1", page.Page, page.TotalPages)
	}
	for _, loop := range loops[:12] {
		if !strings.Contains(page.Text, loop.Filename) {
			t.Errorf("12-item page missing %q", loop.Filename)
		}
	}
	if _, err := svc.LibraryPage(ctx, 2); err == nil {
		t.Fatal("12-item page 2 succeeded, want out-of-range error")
	}

	svc.librarySnapshot = medialib.Library{Loops: append([]medialib.Loop(nil), loops...)}
	first, err := svc.LibraryPage(ctx, 1)
	if err != nil {
		t.Fatalf("13-item page 1: %v", err)
	}
	if first.Page != 1 || first.TotalPages != 2 {
		t.Fatalf("13-item first result = page %d/%d, want 1/2", first.Page, first.TotalPages)
	}
	if strings.Contains(first.Text, loops[12].Filename) {
		t.Errorf("13th item leaked onto page 1: %q", loops[12].Filename)
	}
	second, err := svc.LibraryPage(ctx, 2)
	if err != nil {
		t.Fatalf("13-item page 2: %v", err)
	}
	if second.Page != 2 || second.TotalPages != 2 {
		t.Fatalf("13-item second result = page %d/%d, want 2/2", second.Page, second.TotalPages)
	}
	if !strings.Contains(second.Text, loops[12].Filename) {
		t.Errorf("13-item page 2 missing %q", loops[12].Filename)
	}
	for _, loop := range loops[:12] {
		if strings.Contains(second.Text, loop.Filename) {
			t.Errorf("page 1 item leaked onto page 2: %q", loop.Filename)
		}
	}

	svc.librarySnapshot = medialib.Library{Loops: append([]medialib.Loop(nil), loops[:12]...)}
	shrunk, err := svc.LibraryPage(ctx, 2)
	if err == nil {
		t.Fatalf("page 2 after shrink = %#v, want out-of-range error", shrunk)
	}
	if shrunk.Text != "" || shrunk.Page != 0 || shrunk.TotalPages != 0 {
		t.Fatalf("page 2 after shrink returned partial result %#v", shrunk)
	}
	if !strings.Contains(err.Error(), "目前共有 1 頁") {
		t.Fatalf("page shrink error = %q, want current total page count", err)
	}
}

func TestLibraryPageMaximumLegalFilenamesFitTelegramLimit(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.librarySnapshot = medialib.Library{Loops: makeLibraryPageLoops(t, 13, true)}

	first, err := svc.LibraryPage(ctx, 1)
	if err != nil {
		t.Fatalf("maximum-name page 1: %v", err)
	}
	for pageNumber := 1; pageNumber <= first.TotalPages; pageNumber++ {
		result, err := svc.LibraryPage(ctx, pageNumber)
		if err != nil {
			t.Fatalf("maximum-name page %d: %v", pageNumber, err)
		}
		if got := len([]byte(result.Text)); got > 4096 {
			t.Errorf(
				"maximum-name page %d UTF-8 length = %d bytes, want <= Telegram limit 4096",
				pageNumber,
				got,
			)
		}
	}
}

func TestThemeLengthBoundaryPersistsOnlyBoundedStatusText(t *testing.T) {
	ctx := context.Background()
	svc, _ := newLibraryTestService(t)
	svc.now = fixedNow("2026-07-31T12:00:00+08:00")

	maxTheme := strings.Repeat("t", telegram.MaxThemeUTF8Bytes)
	if got := utf8.RuneCountInString(maxTheme); got != telegram.MaxThemeRunes {
		t.Fatalf("maximum fixture runes = %d, want %d", got, telegram.MaxThemeRunes)
	}
	filename := "loop_day_" + maxTheme + "_v.mp4"
	if got := len(filename); got != 255 {
		t.Fatalf("maximum fixture filename bytes = %d, want 255", got)
	}
	writeLibraryFile(t, svc.cfg.LoopMediaDir, filename)
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan maximum theme fixture: %v", err)
	}

	setText, err := svc.SetThemeText(ctx, maxTheme)
	if err != nil {
		t.Fatalf("set maximum theme: %v", err)
	}
	assertTelegramTextBound := func(name string, text string) {
		t.Helper()
		if got := utf8.RuneCountInString(text); got > 4096 {
			t.Fatalf("%s runes = %d, want <= Telegram limit 4096", name, got)
		}
		if got := len(text); got > 4096 {
			t.Fatalf("%s UTF-8 bytes = %d, want conservative bound <= 4096", name, got)
		}
	}
	assertTelegramTextBound("maximum theme response", setText)

	date := overrideDateKey(svc.now())
	override, err := svc.libDB.Override(ctx, date)
	if err != nil {
		t.Fatalf("read maximum theme override: %v", err)
	}
	if override.Theme != maxTheme {
		t.Fatalf("persisted maximum theme bytes = %d, want %d", len(override.Theme), len(maxTheme))
	}

	status, err := svc.LibraryStatusText(ctx, true)
	if err != nil {
		t.Fatalf("status with maximum theme: %v", err)
	}
	if !strings.Contains(status, "Override：主題 "+maxTheme) {
		t.Fatal("status omitted maximum accepted theme")
	}
	assertTelegramTextBound("status with maximum theme", status)

	overlongThemes := []struct {
		name  string
		theme string
	}{
		{
			name:  "rune and byte limit",
			theme: strings.Repeat("t", telegram.MaxThemeRunes+1),
		},
		{
			name:  "UTF-8 byte limit",
			theme: strings.Repeat("界", telegram.MaxThemeUTF8Bytes/len("界")+1),
		},
	}
	for _, overlong := range overlongThemes {
		t.Run("rejects "+overlong.name, func(t *testing.T) {
			text, err := svc.SetThemeText(ctx, overlong.theme)
			if err == nil {
				t.Fatal("oversized theme succeeded")
			}
			if text != "" {
				t.Fatalf("oversized theme response = %q, want empty", text)
			}
			override, readErr := svc.libDB.Override(ctx, date)
			if readErr != nil {
				t.Fatalf("read override after rejection: %v", readErr)
			}
			if override.Theme != maxTheme {
				t.Fatalf(
					"theme after rejection = %d bytes, want prior %d-byte value",
					len(override.Theme),
					len(maxTheme),
				)
			}
		})
	}

	// A database written by an older build can bypass current command
	// validation. /status must not reflect that unbounded value.
	legacyOversizedTheme := strings.Repeat("z", 5000)
	if err := svc.libDB.SetThemeOverride(ctx, date, legacyOversizedTheme); err != nil {
		t.Fatalf("seed legacy oversized theme: %v", err)
	}
	svc.libraryScanErr = strings.Repeat("界", 2000)
	status, err = svc.LibraryStatusText(ctx, true)
	if err != nil {
		t.Fatalf("status with legacy oversized theme: %v", err)
	}
	if strings.Contains(status, strings.Repeat("z", 64)) {
		t.Fatal("status reflected a legacy oversized theme")
	}
	if !strings.Contains(status, "Override：主題（已隱藏：文字無效或超過安全長度）") {
		t.Fatalf("status missing bounded legacy-theme marker:\n%s", status)
	}
	assertTelegramTextBound("status with legacy oversized theme", status)
	if !strings.HasSuffix(status, "\n…") {
		t.Fatal("oversized status fixture was not visibly truncated")
	}
}

func makeLibraryPageLoops(t *testing.T, count int, maximumFilename bool) []medialib.Loop {
	t.Helper()

	loops := make([]medialib.Loop, 0, count)
	for index := 1; index <= count; index++ {
		theme := fmt.Sprintf("theme%03d", index)
		if maximumFilename {
			const maxFilenameBytes = 255
			suffix := fmt.Sprintf("%03d", index)
			themeBytes := maxFilenameBytes - len("loop_day_") - len("_v.mp4")
			theme = strings.Repeat("t", themeBytes-len(suffix)) + suffix
		}
		filename := fmt.Sprintf("loop_day_%s_v.mp4", theme)
		if maximumFilename {
			if got := len([]byte(filename)); got != 255 {
				t.Fatalf("fixture filename length = %d, want 255: %q", got, filename)
			}
			if _, err := medialib.ParseLoopFilename(filename); err != nil {
				t.Fatalf("255-byte fixture filename is not library-legal: %v", err)
			}
		}
		relPath := filename
		loops = append(loops, medialib.Loop{
			ID:       medialib.StableID(medialib.KindLoop, relPath),
			Path:     "/library/loops/" + filename,
			RelPath:  relPath,
			Filename: filename,
			Period:   medialib.PeriodDay,
			Theme:    theme,
			Variant:  "v",
			Ext:      ".mp4",
		})
	}
	return loops
}
