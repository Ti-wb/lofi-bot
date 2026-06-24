package library

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Kind identifies a media asset family.
type Kind string

const (
	KindLoop  Kind = "loop"
	KindMusic Kind = "music"
)

var supportedLoopExtensions = map[string]struct{}{
	".mp4":  {},
	".mov":  {},
	".m4v":  {},
	".mkv":  {},
	".webm": {},
}

var supportedMusicExtensions = map[string]struct{}{
	".mp3":  {},
	".m4a":  {},
	".aac":  {},
	".wav":  {},
	".flac": {},
	".ogg":  {},
}

// LoopFile is the parsed metadata encoded in a loop filename.
type LoopFile struct {
	Period  Period
	Theme   string
	Variant string
	Ext     string
}

// MusicFile is the parsed metadata encoded in a music filename.
type MusicFile struct {
	Track string
	Ext   string
}

// Loop is a scanned loop asset.
type Loop struct {
	ID       string
	Path     string
	RelPath  string
	Filename string
	Period   Period
	Theme    string
	Variant  string
	Ext      string
}

// Music is a scanned music asset.
type Music struct {
	ID       string
	Path     string
	RelPath  string
	Filename string
	Track    string
	Ext      string
}

// Library is an immutable-by-convention snapshot of scanned media assets.
type Library struct {
	Loops []Loop
	Music []Music
}

// Summary contains simple counts useful for status surfaces.
type Summary struct {
	LoopCount            int
	MusicCount           int
	LoopCountByPeriod    map[Period]int
	ThemeCountByPeriod   map[Period]int
	MusicCountByExt      map[string]int
	LoopCountByExtension map[string]int
}

func IsSupportedLoopExtension(ext string) bool {
	_, ok := supportedLoopExtensions[strings.ToLower(ext)]
	return ok
}

func IsSupportedMusicExtension(ext string) bool {
	_, ok := supportedMusicExtensions[strings.ToLower(ext)]
	return ok
}

// StableID returns a deterministic ID from the asset kind and canonical
// relative path. It does not include MEDIA_DIR, so moving a library preserves
// IDs.
func StableID(kind Kind, relPath string) string {
	key := string(kind) + ":" + canonicalRelPath(relPath)
	sum := sha256.Sum256([]byte(key))
	return string(kind) + "_" + hex.EncodeToString(sum[:])[:16]
}

func ParseLoopFilename(name string) (LoopFile, error) {
	if hasPathSeparator(name) {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "filename", name, errors.New("filename must not contain path separators"))
	}
	rawExt := filepath.Ext(name)
	ext := strings.ToLower(rawExt)
	if !IsSupportedLoopExtension(ext) {
		return LoopFile{}, fileError(ErrorUnsupportedExtension, KindLoop, name, "extension", rawExt, nil)
	}
	stem := name[:len(name)-len(rawExt)]
	if !strings.HasPrefix(stem, "loop_") {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "prefix", stem, errors.New("loop filename must start with loop_"))
	}
	parts := strings.SplitN(strings.TrimPrefix(stem, "loop_"), "_", 3)
	if len(parts) != 3 {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "filename", name, errors.New("loop filename must be loop_<period>_<theme>_<variant>.<ext>"))
	}
	period, err := ParsePeriod(parts[0])
	if err != nil {
		return LoopFile{}, withFile(err, KindLoop, name)
	}
	theme := parts[1]
	if isBlank(theme) {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "theme", theme, errors.New("theme must not be blank"))
	}
	if strings.Contains(theme, "_") || hasPathSeparator(theme) {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "theme", theme, errors.New("theme must not contain underscores or path separators"))
	}
	variant := parts[2]
	if isBlank(variant) {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "variant", variant, errors.New("variant must not be blank"))
	}
	if hasPathSeparator(variant) {
		return LoopFile{}, fileError(ErrorInvalidFilename, KindLoop, name, "variant", variant, errors.New("variant must not contain path separators"))
	}
	return LoopFile{
		Period:  period,
		Theme:   theme,
		Variant: variant,
		Ext:     ext,
	}, nil
}

func ParseMusicFilename(name string) (MusicFile, error) {
	if hasPathSeparator(name) {
		return MusicFile{}, fileError(ErrorInvalidFilename, KindMusic, name, "filename", name, errors.New("filename must not contain path separators"))
	}
	rawExt := filepath.Ext(name)
	ext := strings.ToLower(rawExt)
	if !IsSupportedMusicExtension(ext) {
		return MusicFile{}, fileError(ErrorUnsupportedExtension, KindMusic, name, "extension", rawExt, nil)
	}
	stem := name[:len(name)-len(rawExt)]
	if !strings.HasPrefix(stem, "music_") {
		return MusicFile{}, fileError(ErrorInvalidFilename, KindMusic, name, "prefix", stem, errors.New("music filename must start with music_"))
	}
	track := strings.TrimPrefix(stem, "music_")
	if isBlank(track) {
		return MusicFile{}, fileError(ErrorInvalidFilename, KindMusic, name, "track", track, errors.New("track must not be blank"))
	}
	if hasPathSeparator(track) {
		return MusicFile{}, fileError(ErrorInvalidFilename, KindMusic, name, "track", track, errors.New("track must not contain path separators"))
	}
	return MusicFile{
		Track: track,
		Ext:   ext,
	}, nil
}

// Scan reads MEDIA_DIR/loops and MEDIA_DIR/music non-recursively.
func Scan(mediaDir string) (Library, error) {
	return ScanDirs(filepath.Join(mediaDir, "loops"), filepath.Join(mediaDir, "music"))
}

// ScanDirs reads explicit loop and music directories non-recursively.
func ScanDirs(loopDir string, musicDir string) (Library, error) {
	var (
		lib    Library
		issues []*Error
	)

	loops, loopIssues := scanLoops(loopDir)
	music, musicIssues := scanMusic(musicDir)
	lib.Loops = loops
	lib.Music = music
	issues = append(issues, loopIssues...)
	issues = append(issues, musicIssues...)

	return lib, scanErr(issues)
}

func (l Library) LoopCount() int {
	return len(l.Loops)
}

func (l Library) MusicCount() int {
	return len(l.Music)
}

func (l Library) LoopCountForPeriod(period Period) int {
	count := 0
	for _, loop := range l.Loops {
		if loop.Period == period {
			count++
		}
	}
	return count
}

func (l Library) Summary() Summary {
	summary := Summary{
		LoopCount:            len(l.Loops),
		MusicCount:           len(l.Music),
		LoopCountByPeriod:    make(map[Period]int),
		ThemeCountByPeriod:   make(map[Period]int),
		MusicCountByExt:      make(map[string]int),
		LoopCountByExtension: make(map[string]int),
	}
	themeSets := make(map[Period]map[string]struct{})
	for _, loop := range l.Loops {
		summary.LoopCountByPeriod[loop.Period]++
		summary.LoopCountByExtension[loop.Ext]++
		if themeSets[loop.Period] == nil {
			themeSets[loop.Period] = make(map[string]struct{})
		}
		themeSets[loop.Period][loop.Theme] = struct{}{}
	}
	for period, themes := range themeSets {
		summary.ThemeCountByPeriod[period] = len(themes)
	}
	for _, music := range l.Music {
		summary.MusicCountByExt[music.Ext]++
	}
	return summary
}

func scanLoops(mediaDir string) ([]Loop, []*Error) {
	dir := mediaDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []*Error{fileError(ErrorReadDirectory, KindLoop, "", "directory", dir, err)}
	}

	var (
		loops  []Loop
		issues []*Error
	)
	for _, entry := range entries {
		if entry.IsDir() || !IsSupportedLoopExtension(filepath.Ext(entry.Name())) {
			continue
		}
		parsed, err := ParseLoopFilename(entry.Name())
		relPath := slashRel(filepath.Join("loops", entry.Name()))
		if err != nil {
			issues = append(issues, withFile(err, KindLoop, relPath))
			continue
		}
		loops = append(loops, Loop{
			ID:       StableID(KindLoop, relPath),
			Path:     filepath.Join(dir, entry.Name()),
			RelPath:  relPath,
			Filename: entry.Name(),
			Period:   parsed.Period,
			Theme:    parsed.Theme,
			Variant:  parsed.Variant,
			Ext:      parsed.Ext,
		})
	}
	sort.Slice(loops, func(i, j int) bool {
		return loops[i].RelPath < loops[j].RelPath
	})
	return loops, issues
}

func scanMusic(mediaDir string) ([]Music, []*Error) {
	dir := mediaDir
	entries, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, []*Error{fileError(ErrorReadDirectory, KindMusic, "", "directory", dir, err)}
	}

	var (
		music  []Music
		issues []*Error
	)
	for _, entry := range entries {
		if entry.IsDir() || !IsSupportedMusicExtension(filepath.Ext(entry.Name())) {
			continue
		}
		parsed, err := ParseMusicFilename(entry.Name())
		relPath := slashRel(filepath.Join("music", entry.Name()))
		if err != nil {
			issues = append(issues, withFile(err, KindMusic, relPath))
			continue
		}
		music = append(music, Music{
			ID:       StableID(KindMusic, relPath),
			Path:     filepath.Join(dir, entry.Name()),
			RelPath:  relPath,
			Filename: entry.Name(),
			Track:    parsed.Track,
			Ext:      parsed.Ext,
		})
	}
	sort.Slice(music, func(i, j int) bool {
		return music[i].RelPath < music[j].RelPath
	})
	return music, issues
}

func withFile(err error, kind Kind, path string) *Error {
	var libErr *Error
	if errors.As(err, &libErr) {
		clone := *libErr
		clone.Kind = kind
		clone.Path = path
		return &clone
	}
	return fileError(ErrorInvalidFilename, kind, path, "", "", err)
}

func canonicalRelPath(path string) string {
	return filepath.ToSlash(filepath.Clean(path))
}

func slashRel(path string) string {
	return filepath.ToSlash(path)
}

func hasPathSeparator(value string) bool {
	return strings.ContainsAny(value, `/\`)
}

func isBlank(value string) bool {
	return strings.TrimSpace(value) == ""
}
