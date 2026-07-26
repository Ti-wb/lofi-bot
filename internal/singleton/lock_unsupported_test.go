//go:build !darwin && !linux

package singleton

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestAcquireFailsClosedOnUnsupportedPlatform(t *testing.T) {
	if _, err := Acquire(filepath.Join(t.TempDir(), "queue.db"), "test-token"); !errors.Is(err, ErrUnsupportedPlatform) {
		t.Fatalf("Acquire error = %v, want unsupported-platform failure", err)
	}
}
