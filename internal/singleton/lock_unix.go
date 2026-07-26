//go:build darwin || linux

package singleton

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func checkPlatformSupport() error {
	return nil
}

func securePlatformUserLockDirectory() (string, error) {
	baseDirectory := "/tmp"
	if runtime.GOOS == "darwin" {
		// /tmp is a symlink on macOS. Use its authoritative real path so the
		// no-follow traversal below never accepts path substitution.
		baseDirectory = "/private/tmp"
	}

	uid := unix.Geteuid()
	if uid < 0 {
		return "", errors.New("resolve effective OS user identity")
	}
	baseFD, err := openDirectoryPathNoFollow(baseDirectory)
	if err != nil {
		return "", fmt.Errorf("open singleton runtime base %q: %w", baseDirectory, err)
	}
	defer unix.Close(baseFD)

	rootName := userLockRootPrefix + strconv.Itoa(uid)
	rootFD, err := openOrCreatePrivateDirectoryAt(baseFD, rootName, uint32(uid))
	if err != nil {
		return "", fmt.Errorf("prepare singleton user root %q: %w", filepath.Join(baseDirectory, rootName), err)
	}
	defer unix.Close(rootFD)

	lockFD, err := openOrCreatePrivateDirectoryAt(rootFD, userLockDirectoryName, uint32(uid))
	if err != nil {
		return "", fmt.Errorf(
			"prepare singleton lock directory %q: %w",
			filepath.Join(baseDirectory, rootName, userLockDirectoryName),
			err,
		)
	}
	_ = unix.Close(lockFD)
	return filepath.Join(baseDirectory, rootName, userLockDirectoryName), nil
}

func openDirectoryPathNoFollow(path string) (int, error) {
	cleaned := filepath.Clean(path)
	if !filepath.IsAbs(cleaned) {
		return -1, fmt.Errorf("directory path %q is not absolute", path)
	}
	fd, err := unix.Open(
		string(filepath.Separator),
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(strings.TrimPrefix(cleaned, string(filepath.Separator)), string(filepath.Separator)) {
		if component == "" {
			continue
		}
		nextFD, openErr := unix.Openat(
			fd,
			component,
			unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			0,
		)
		_ = unix.Close(fd)
		if openErr != nil {
			return -1, openErr
		}
		fd = nextFD
	}
	return fd, nil
}

func openOrCreatePrivateDirectoryAt(parentFD int, name string, uid uint32) (int, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return -1, fmt.Errorf("invalid private directory name %q", name)
	}
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil && !errors.Is(err, unix.EEXIST) {
		return -1, err
	}
	fd, err := unix.Openat(
		parentFD,
		name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return -1, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return -1, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return -1, errors.New("private lock path is not a directory")
	}
	if stat.Uid != uid {
		return -1, fmt.Errorf("private lock directory owner is uid %d, want uid %d", stat.Uid, uid)
	}
	if err := unix.Fchmod(fd, 0o700); err != nil {
		return -1, err
	}
	if err := unix.Fstat(fd, &stat); err != nil {
		return -1, err
	}
	if stat.Mode&0o777 != 0o700 {
		return -1, fmt.Errorf("private lock directory mode is %04o, want 0700", stat.Mode&0o777)
	}
	closeFD = false
	return fd, nil
}

func openPlatformLock(path string) (*os.File, error) {
	fd, err := unix.Open(
		path,
		unix.O_RDWR|unix.O_CREAT|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	closeFD := true
	defer func() {
		if closeFD {
			_ = unix.Close(fd)
		}
	}()

	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, fmt.Errorf("lock path is not a regular file")
	}
	if err := unix.Fchmod(fd, 0o600); err != nil {
		return nil, err
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, errPlatformContended
		}
		return nil, err
	}

	closeFD = false
	return os.NewFile(uintptr(fd), path), nil
}

func unlockPlatform(file *os.File) error {
	return unix.Flock(int(file.Fd()), unix.LOCK_UN)
}
