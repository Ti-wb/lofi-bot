package library

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
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

func TestScanMissingDirectoriesIsEmptyLibrary(t *testing.T) {
	lib, err := Scan(t.TempDir())
	if err != nil {
		t.Fatalf("Scan() error = %v", err)
	}
	if lib.LoopCount() != 0 || lib.MusicCount() != 0 {
		t.Fatalf("expected empty library, got %#v", lib)
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
