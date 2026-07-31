package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/media"
)

func TestRunStartsLivenessReporterBeforeInitialLibraryProbeCompletes(t *testing.T) {
	svc, _, bot := newRuntimeTestService(t)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")

	controlDir := t.TempDir()
	probeStartedPath := filepath.Join(controlDir, "probe-started")
	releaseProbePath := filepath.Join(controlDir, "release-probe")
	probePath := filepath.Join(controlDir, "ffprobe")
	script := fmt.Sprintf(`#!/bin/sh
: > %q
while [ ! -f %q ]; do
	sleep 0.01
done
printf '%%s\n' '{"format":{"duration":"30"},"streams":[{"codec_type":"video"},{"codec_type":"audio"}]}'
`, probeStartedPath, releaseProbePath)
	if err := os.WriteFile(probePath, []byte(script), 0o700); err != nil {
		t.Fatalf("write blocking ffprobe: %v", err)
	}
	svc.media = media.NewManager(probePath)

	bot.run = func(ctx context.Context) error {
		<-ctx.Done()
		return ctx.Err()
	}
	reporterStarted := make(chan struct{})
	svc.livenessReporter = &fakeRequiredReporter{
		start: func(ctx context.Context) (<-chan error, error) {
			close(reporterStarted)
			results := make(chan error, 1)
			go func() {
				<-ctx.Done()
				results <- ctx.Err()
			}()
			return results, nil
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	runResult := make(chan error, 1)
	go func() {
		runResult <- svc.Run(ctx)
	}()

	waitForTestPath(t, probeStartedPath)
	select {
	case <-reporterStarted:
		// The real probe is still blocked, proving startup liveness did not
		// wait for the complete library scan.
	case <-time.After(time.Second):
		t.Fatal("liveness reporter waited for initial library validation")
	}
	if err := os.WriteFile(releaseProbePath, []byte("release"), 0o600); err != nil {
		t.Fatalf("release blocking probe: %v", err)
	}
	cancel()

	select {
	case err := <-runResult:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after releasing the probe and cancelling")
	}
}

func waitForTestPath(t *testing.T, path string) {
	t.Helper()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("timed out waiting for %s", path)
		case <-ticker.C:
		}
	}
}
