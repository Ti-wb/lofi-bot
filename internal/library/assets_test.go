package library

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseLoopFilename(t *testing.T) {
	tests := []struct {
		name string
		want LoopFile
	}{
		{
			name: "loop_morning_朝_v1.MP4",
			want: LoopFile{Period: PeriodMorning, Theme: "朝", Variant: "v1", Ext: ".mp4"},
		},
		{
			name: "loop_night_stars_alt_take.webm",
			want: LoopFile{Period: PeriodNight, Theme: "stars", Variant: "alt_take", Ext: ".webm"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseLoopFilename(tt.name)
			if err != nil {
				t.Fatalf("ParseLoopFilename() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("ParseLoopFilename() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseLoopFilenameRejectsInvalidNames(t *testing.T) {
	tests := []struct {
		name  string
		code  ErrorCode
		field string
	}{
		{name: "loop_morning_theme_v1.avi", code: ErrorUnsupportedExtension, field: "extension"},
		{name: "clip_morning_theme_v1.mp4", code: ErrorInvalidFilename, field: "prefix"},
		{name: "loop_dawn_theme_v1.mp4", code: ErrorInvalidPeriod, field: "period"},
		{name: "loop_morning__v1.mp4", code: ErrorInvalidFilename, field: "theme"},
		{name: "loop_morning_theme_.mp4", code: ErrorInvalidFilename, field: "variant"},
		{name: "loop_morning_theme_v1/alt.mp4", code: ErrorInvalidFilename, field: "filename"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseLoopFilename(tt.name)
			requireLibraryError(t, err, tt.code, tt.field)
		})
	}
}

func TestParseMusicFilename(t *testing.T) {
	got, err := ParseMusicFilename("music_深夜_mix_01.FLAC")
	if err != nil {
		t.Fatalf("ParseMusicFilename() error = %v", err)
	}
	want := MusicFile{Track: "深夜_mix_01", Ext: ".flac"}
	if got != want {
		t.Fatalf("ParseMusicFilename() = %#v, want %#v", got, want)
	}
}

func TestParseMusicFilenameRejectsInvalidNames(t *testing.T) {
	tests := []struct {
		name  string
		code  ErrorCode
		field string
	}{
		{name: "music_track.mp4", code: ErrorUnsupportedExtension, field: "extension"},
		{name: "track_song.mp3", code: ErrorInvalidFilename, field: "prefix"},
		{name: "music_.mp3", code: ErrorInvalidFilename, field: "track"},
		{name: "music_track/one.mp3", code: ErrorInvalidFilename, field: "filename"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseMusicFilename(tt.name)
			requireLibraryError(t, err, tt.code, tt.field)
		})
	}
}

func TestSupportedExtensions(t *testing.T) {
	for _, ext := range []string{".mp4", ".mov", ".m4v", ".mkv", ".webm", ".MP4"} {
		if !IsSupportedLoopExtension(ext) {
			t.Fatalf("loop extension %s should be supported", ext)
		}
	}
	for _, ext := range []string{".mp3", ".m4a", ".aac", ".wav", ".flac", ".ogg", ".OGG"} {
		if !IsSupportedMusicExtension(ext) {
			t.Fatalf("music extension %s should be supported", ext)
		}
	}
}

func TestScanFiltersAndBuildsStableAssets(t *testing.T) {
	mediaDir := t.TempDir()
	mkdir(t, filepath.Join(mediaDir, "loops"))
	mkdir(t, filepath.Join(mediaDir, "music"))
	writeFile(t, filepath.Join(mediaDir, "loops", "loop_morning_calm_b.mov"))
	writeFile(t, filepath.Join(mediaDir, "loops", "loop_morning_calm_a.mp4"))
	writeFile(t, filepath.Join(mediaDir, "loops", "ignore.txt"))
	writeFile(t, filepath.Join(mediaDir, "loops", "music_wrong-place.mp3"))
	mkdir(t, filepath.Join(mediaDir, "loops", "nested"))
	writeFile(t, filepath.Join(mediaDir, "loops", "nested", "loop_day_focus_a.mp4"))
	writeFile(t, filepath.Join(mediaDir, "music", "music_theme.mp3"))
	writeFile(t, filepath.Join(mediaDir, "music", "music_track_2.FLAC"))
	writeFile(t, filepath.Join(mediaDir, "music", "cover.jpg"))
	writeFile(t, filepath.Join(mediaDir, "music", "loop_morning_calm_a.mp4"))

	lib, err := Scan(mediaDir)
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}

	gotLoops := relLoopPaths(lib.Loops)
	wantLoops := []string{
		"loops/loop_morning_calm_a.mp4",
		"loops/loop_morning_calm_b.mov",
	}
	if !reflect.DeepEqual(gotLoops, wantLoops) {
		t.Fatalf("loops = %#v, want %#v", gotLoops, wantLoops)
	}
	gotMusic := relMusicPaths(lib.Music)
	wantMusic := []string{
		"music/music_theme.mp3",
		"music/music_track_2.FLAC",
	}
	if !reflect.DeepEqual(gotMusic, wantMusic) {
		t.Fatalf("music = %#v, want %#v", gotMusic, wantMusic)
	}
	if lib.Loops[0].ID != StableID(KindLoop, "loops/loop_morning_calm_a.mp4") {
		t.Fatalf("unexpected loop ID %q", lib.Loops[0].ID)
	}
	if lib.Music[1].Ext != ".flac" {
		t.Fatalf("expected lower-cased extension, got %q", lib.Music[1].Ext)
	}
}

func TestScanDirsReturnsAbsoluteAssetPathsForRelativeDirs(t *testing.T) {
	root := t.TempDir()
	previousWD, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	t.Cleanup(func() {
		if err := os.Chdir(previousWD); err != nil {
			t.Fatalf("restore working directory: %v", err)
		}
	})
	if err := os.Chdir(root); err != nil {
		t.Fatalf("change working directory: %v", err)
	}
	mkdir(t, filepath.Join("media", "loops"))
	mkdir(t, filepath.Join("media", "music"))
	writeFile(t, filepath.Join("media", "loops", "loop_day_cafe_001.mp4"))
	writeFile(t, filepath.Join("media", "music", "music_alpha.mp3"))

	lib, err := ScanDirs(filepath.Join("media", "loops"), filepath.Join("media", "music"))
	if err != nil {
		t.Fatalf("ScanDirs() error = %v", err)
	}
	if len(lib.Loops) != 1 || len(lib.Music) != 1 {
		t.Fatalf("library counts: loops=%d music=%d", len(lib.Loops), len(lib.Music))
	}
	wantLoopPath, err := filepath.Abs(filepath.Join("media", "loops", "loop_day_cafe_001.mp4"))
	if err != nil {
		t.Fatalf("absolute loop path: %v", err)
	}
	if lib.Loops[0].Path != wantLoopPath {
		t.Fatalf("loop path = %q, want %q", lib.Loops[0].Path, wantLoopPath)
	}
	wantMusicPath, err := filepath.Abs(filepath.Join("media", "music", "music_alpha.mp3"))
	if err != nil {
		t.Fatalf("absolute music path: %v", err)
	}
	if lib.Music[0].Path != wantMusicPath {
		t.Fatalf("music path = %q, want %q", lib.Music[0].Path, wantMusicPath)
	}
	if !filepath.IsAbs(lib.Loops[0].Path) || !filepath.IsAbs(lib.Music[0].Path) {
		t.Fatalf("asset paths should be absolute: loop=%q music=%q", lib.Loops[0].Path, lib.Music[0].Path)
	}
	if lib.Loops[0].ID != StableID(KindLoop, "loops/loop_day_cafe_001.mp4") {
		t.Fatalf("loop ID = %q, want stable relpath ID", lib.Loops[0].ID)
	}
	if lib.Music[0].ID != StableID(KindMusic, "music/music_alpha.mp3") {
		t.Fatalf("music ID = %q, want stable relpath ID", lib.Music[0].ID)
	}
}

func TestScanReturnsStructuredIssuesAndValidAssets(t *testing.T) {
	mediaDir := t.TempDir()
	mkdir(t, filepath.Join(mediaDir, "loops"))
	mkdir(t, filepath.Join(mediaDir, "music"))
	writeFile(t, filepath.Join(mediaDir, "loops", "loop_morning_calm_a.mp4"))
	writeFile(t, filepath.Join(mediaDir, "loops", "loop_dawn_calm_a.mp4"))
	writeFile(t, filepath.Join(mediaDir, "music", "song.mp3"))

	lib, err := Scan(mediaDir)
	if err == nil {
		t.Fatal("expected scan issues")
	}
	if len(lib.Loops) != 1 {
		t.Fatalf("expected valid loop to be returned, got %d", len(lib.Loops))
	}
	var scanErr *ScanError
	if !errors.As(err, &scanErr) {
		t.Fatalf("expected ScanError, got %T", err)
	}
	if len(scanErr.Issues) != 2 {
		t.Fatalf("expected 2 issues, got %#v", scanErr.Issues)
	}
	if scanErr.Issues[0].Code != ErrorInvalidPeriod || scanErr.Issues[0].Path != "loops/loop_dawn_calm_a.mp4" {
		t.Fatalf("unexpected first issue: %#v", scanErr.Issues[0])
	}
	if scanErr.Issues[1].Code != ErrorInvalidFilename || scanErr.Issues[1].Path != "music/song.mp3" {
		t.Fatalf("unexpected second issue: %#v", scanErr.Issues[1])
	}
}

func TestScanRejectsEmptyAndSymlinkAssets(t *testing.T) {
	mediaDir := t.TempDir()
	loopDir := filepath.Join(mediaDir, "loops")
	musicDir := filepath.Join(mediaDir, "music")
	mkdir(t, loopDir)
	mkdir(t, musicDir)

	emptyLoop := filepath.Join(loopDir, "loop_morning_empty_001.mp4")
	if err := os.WriteFile(emptyLoop, nil, 0o644); err != nil {
		t.Fatalf("write empty loop: %v", err)
	}
	targetMusic := filepath.Join(mediaDir, "outside.mp3")
	writeFile(t, targetMusic)
	symlinkMusic := filepath.Join(musicDir, "music_link.mp3")
	if err := os.Symlink(targetMusic, symlinkMusic); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	lib, err := Scan(mediaDir)
	if len(lib.Loops) != 0 || len(lib.Music) != 0 {
		t.Fatalf("invalid assets entered snapshot: %#v", lib)
	}
	var scanErr *ScanError
	if !errors.As(err, &scanErr) {
		t.Fatalf("scan error = %v, want structured issues", err)
	}
	if len(scanErr.Issues) != 2 {
		t.Fatalf("issues = %#v, want empty and symlink issues", scanErr.Issues)
	}
	for _, issue := range scanErr.Issues {
		if issue.Code != ErrorInvalidAsset {
			t.Fatalf("issue = %#v, want %s", issue, ErrorInvalidAsset)
		}
	}
}

func TestValidateScannedAssetRejectsFIFOAndDevice(t *testing.T) {
	for _, fixture := range []struct {
		name string
		mode os.FileMode
	}{
		{name: "fifo", mode: os.ModeNamedPipe | 0o600},
		{name: "device", mode: os.ModeDevice | 0o600},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			entry := syntheticDirEntry{
				info: syntheticFileInfo{
					name: "loop_day_cafe_001.mp4",
					mode: fixture.mode,
					size: 1,
				},
			}
			issue := validateScannedAsset(
				entry,
				KindLoop,
				"loops/loop_day_cafe_001.mp4",
			)
			if issue == nil || issue.Code != ErrorInvalidAsset {
				t.Fatalf("issue = %#v, want invalid-asset rejection", issue)
			}
			if issue.Field != "type" || !strings.Contains(issue.Error(), "regular file") {
				t.Fatalf("issue = %#v, want non-regular type rejection", issue)
			}
		})
	}
}

func TestScanMissingDirectoriesIsEmptyLibrary(t *testing.T) {
	lib, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if lib.LoopCount() != 0 || lib.MusicCount() != 0 {
		t.Fatalf("expected empty library, got %#v", lib)
	}
}

type syntheticDirEntry struct {
	info syntheticFileInfo
}

func (entry syntheticDirEntry) Name() string               { return entry.info.Name() }
func (entry syntheticDirEntry) IsDir() bool                { return entry.info.IsDir() }
func (entry syntheticDirEntry) Type() os.FileMode          { return entry.info.Mode().Type() }
func (entry syntheticDirEntry) Info() (os.FileInfo, error) { return entry.info, nil }

type syntheticFileInfo struct {
	name string
	mode os.FileMode
	size int64
}

func (info syntheticFileInfo) Name() string       { return info.name }
func (info syntheticFileInfo) Size() int64        { return info.size }
func (info syntheticFileInfo) Mode() os.FileMode  { return info.mode }
func (info syntheticFileInfo) ModTime() time.Time { return time.Unix(0, 0) }
func (info syntheticFileInfo) IsDir() bool        { return info.mode.IsDir() }
func (info syntheticFileInfo) Sys() any           { return nil }

func TestScanDirectoryCapacityBoundaries(t *testing.T) {
	t.Run("below and equal limit", func(t *testing.T) {
		root := t.TempDir()
		loopDir := filepath.Join(root, "loops")
		musicDir := filepath.Join(root, "music")
		mkdir(t, loopDir)
		mkdir(t, musicDir)
		writeFile(t, filepath.Join(loopDir, "loop_morning_calm_a.mp4"))

		lib, err := scanDirsWithLimit(loopDir, musicDir, 2)
		if err != nil || len(lib.Loops) != 1 {
			t.Fatalf("below-limit scan = %#v, err=%v", lib, err)
		}
		writeFile(t, filepath.Join(loopDir, "loop_day_focus_b.mp4"))
		lib, err = scanDirsWithLimit(loopDir, musicDir, 2)
		if err != nil || len(lib.Loops) != 2 {
			t.Fatalf("equal-limit scan = %#v, err=%v", lib, err)
		}
	})

	t.Run("over limit returns one recognizable issue", func(t *testing.T) {
		root := t.TempDir()
		loopDir := filepath.Join(root, "loops")
		musicDir := filepath.Join(root, "music")
		mkdir(t, loopDir)
		mkdir(t, musicDir)
		writeFile(t, filepath.Join(loopDir, "loop_morning_calm_a.mp4"))
		writeFile(t, filepath.Join(loopDir, "bad-one.mp4"))
		writeFile(t, filepath.Join(loopDir, "bad-two.mp4"))
		writeFile(t, filepath.Join(musicDir, "music_still-visible.mp3"))

		lib, err := scanDirsWithLimit(loopDir, musicDir, 2)
		if !errors.Is(err, ErrDirectoryCapacity) {
			t.Fatalf("over-limit error = %v, want %v", err, ErrDirectoryCapacity)
		}
		if len(lib.Loops) != 0 || len(lib.Music) != 1 {
			t.Fatalf("over-limit library = %#v, want bounded loop failure and normal music scan", lib)
		}
		var scanErr *ScanError
		if !errors.As(err, &scanErr) || len(scanErr.Issues) != 1 {
			t.Fatalf("over-limit issues = %#v, want exactly one", scanErr)
		}
		if scanErr.Issues[0].Code != ErrorDirectoryCapacity ||
			scanErr.Issues[0].Kind != KindLoop {
			t.Fatalf("capacity issue = %#v", scanErr.Issues[0])
		}
	})
}

func TestScanDirsContextCheckpointsCompletedBatchesAndHonorsCancellation(t *testing.T) {
	root := t.TempDir()
	loopDir := filepath.Join(root, "loops")
	musicDir := filepath.Join(root, "music")
	mkdir(t, loopDir)
	mkdir(t, musicDir)
	writeFile(t, filepath.Join(loopDir, "loop_morning_calm_a.mp4"))
	writeFile(t, filepath.Join(loopDir, "loop_day_focus_b.mp4"))

	ctx, cancel := context.WithCancel(context.Background())
	progress := 0
	lib, err := scanDirsWithLimitContext(
		ctx,
		loopDir,
		musicDir,
		10,
		1,
		func() {
			progress++
			cancel()
		},
	)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("scan error = %v, want context cancellation", err)
	}
	if progress != 1 {
		t.Fatalf("progress checkpoints = %d, want exactly one completed batch", progress)
	}
	if len(lib.Loops) != 0 || len(lib.Music) != 0 {
		t.Fatalf("canceled scan published partial result: %#v", lib)
	}
}

func TestSummaryCounts(t *testing.T) {
	lib := Library{
		Loops: []Loop{
			{Period: PeriodMorning, Theme: "calm", Ext: ".mp4"},
			{Period: PeriodMorning, Theme: "calm", Ext: ".mov"},
			{Period: PeriodMorning, Theme: "focus", Ext: ".mp4"},
			{Period: PeriodNight, Theme: "sleep", Ext: ".webm"},
		},
		Music: []Music{
			{Ext: ".mp3"},
			{Ext: ".mp3"},
			{Ext: ".flac"},
		},
	}

	summary := lib.Summary()
	if summary.LoopCount != 4 || summary.MusicCount != 3 {
		t.Fatalf("unexpected summary counts: %#v", summary)
	}
	if summary.LoopCountByPeriod[PeriodMorning] != 3 || summary.ThemeCountByPeriod[PeriodMorning] != 2 {
		t.Fatalf("unexpected morning counts: %#v", summary)
	}
	if summary.LoopCountByExtension[".mp4"] != 2 || summary.MusicCountByExt[".mp3"] != 2 {
		t.Fatalf("unexpected extension counts: %#v", summary)
	}
}

func TestStableIDUsesKindAndCanonicalRelativePath(t *testing.T) {
	id := StableID(KindLoop, "loops/../loops/loop_morning_calm_a.mp4")
	if id != StableID(KindLoop, "loops/loop_morning_calm_a.mp4") {
		t.Fatalf("expected clean relative paths to produce same ID")
	}
	if id == StableID(KindMusic, "loops/loop_morning_calm_a.mp4") {
		t.Fatalf("expected kind to affect ID")
	}
	if id == StableID(KindLoop, "loops/loop_morning_calm_b.mp4") {
		t.Fatalf("expected path to affect ID")
	}
}

func requireLibraryError(t *testing.T, err error, code ErrorCode, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error")
	}
	var libErr *Error
	if !errors.As(err, &libErr) {
		t.Fatalf("expected library Error, got %T", err)
	}
	if libErr.Code != code || libErr.Field != field {
		t.Fatalf("error = %#v, want code %s field %s", libErr, code, field)
	}
}

func mkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
}

func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("media"), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func relLoopPaths(loops []Loop) []string {
	paths := make([]string, len(loops))
	for i, loop := range loops {
		paths[i] = loop.RelPath
	}
	return paths
}

func relMusicPaths(music []Music) []string {
	paths := make([]string, len(music))
	for i, track := range music {
		paths[i] = track.RelPath
	}
	return paths
}
