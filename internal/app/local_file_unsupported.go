//go:build !darwin && !linux

package app

import (
	"fmt"
	"os"
	"runtime"
)

func openLocalBotAPIFile(string, string) (*os.File, error) {
	return nil, fmt.Errorf("secure Local Bot API file open is unsupported on %s", runtime.GOOS)
}
