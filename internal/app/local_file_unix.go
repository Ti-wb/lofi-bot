//go:build darwin || linux

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const (
	localDirectoryOpenFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_DIRECTORY | unix.O_NOFOLLOW
	localFileOpenFlags      = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
)

// openLocalBotAPIFile opens one regular file beneath root without following a
// symlink in any cache-relative component. The returned descriptor, rather than
// the mutable pathname, is the authority carried into media admission.
func openLocalBotAPIFile(root, path string) (*os.File, error) {
	root = strings.TrimSpace(root)
	if root == "" {
		return nil, fmt.Errorf("TELEGRAM_BOT_API_DIR is required")
	}
	if !filepath.IsAbs(path) {
		return nil, fmt.Errorf("local media path must be absolute: %s", path)
	}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve TELEGRAM_BOT_API_DIR: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return nil, fmt.Errorf("resolve TELEGRAM_BOT_API_DIR: %w", err)
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve local media path: %w", err)
	}
	relativePath, ok := relativePathWithin(absRoot, absPath)
	if !ok && resolvedRoot != absRoot {
		relativePath, ok = relativePathWithin(resolvedRoot, absPath)
	}
	if !ok {
		return nil, fmt.Errorf("local media path is outside TELEGRAM_BOT_API_DIR: %s", path)
	}

	rootFD, err := openAbsoluteDirectoryNoFollow(resolvedRoot)
	if err != nil {
		return nil, fmt.Errorf("open TELEGRAM_BOT_API_DIR: %w", err)
	}
	currentFD := rootFD
	components := strings.Split(relativePath, string(filepath.Separator))
	for _, component := range components[:len(components)-1] {
		nextFD, openErr := unix.Openat(currentFD, component, localDirectoryOpenFlags, 0)
		_ = unix.Close(currentFD)
		if openErr != nil {
			return nil, fmt.Errorf("open local media parent directory: %w", openErr)
		}
		currentFD = nextFD
	}

	finalFD, openErr := unix.Openat(currentFD, components[len(components)-1], localFileOpenFlags, 0)
	_ = unix.Close(currentFD)
	if openErr != nil {
		return nil, fmt.Errorf("open local media path: %w", openErr)
	}
	closeFinalFD := true
	defer func() {
		if closeFinalFD {
			_ = unix.Close(finalFD)
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(finalFD, &stat); err != nil {
		return nil, fmt.Errorf("stat local media path: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("local media path is not a regular file: %s", path)
	}
	file := os.NewFile(uintptr(finalFD), absPath)
	if file == nil {
		return nil, fmt.Errorf("open local media path: invalid file descriptor")
	}
	closeFinalFD = false
	return file, nil
}

func relativePathWithin(root, path string) (string, bool) {
	relativePath, err := filepath.Rel(root, path)
	if err != nil ||
		relativePath == "." ||
		relativePath == ".." ||
		filepath.IsAbs(relativePath) ||
		strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.Clean(relativePath), true
}

// openAbsoluteDirectoryNoFollow pins every component of an already-resolved
// absolute directory path. Renaming or replacing a pathname after a component
// is opened cannot redirect later openat calls away from that descriptor.
func openAbsoluteDirectoryNoFollow(path string) (int, error) {
	cleanPath := filepath.Clean(path)
	if !filepath.IsAbs(cleanPath) {
		return -1, fmt.Errorf("directory path must be absolute: %s", path)
	}
	currentFD, err := unix.Open(string(filepath.Separator), localDirectoryOpenFlags, 0)
	if err != nil {
		return -1, err
	}
	if cleanPath == string(filepath.Separator) {
		return currentFD, nil
	}
	components := strings.Split(strings.TrimPrefix(cleanPath, string(filepath.Separator)), string(filepath.Separator))
	for _, component := range components {
		nextFD, openErr := unix.Openat(currentFD, component, localDirectoryOpenFlags, 0)
		_ = unix.Close(currentFD)
		if openErr != nil {
			return -1, openErr
		}
		currentFD = nextFD
	}
	return currentFD, nil
}
