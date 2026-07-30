//go:build !darwin && !linux

package singleton

import (
	"fmt"
	"os"
	"runtime"
)

func checkPlatformSupport() error {
	return fmt.Errorf("%w: %s", ErrUnsupportedPlatform, runtime.GOOS)
}

func securePlatformUserLockDirectory() (string, error) {
	return "", checkPlatformSupport()
}

func openPlatformLock(string) (*os.File, error) {
	return nil, checkPlatformSupport()
}

func unlockPlatform(*os.File) error {
	return nil
}
