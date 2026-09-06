package app

import (
	"context"
	"errors"
	"testing"
	"time"

	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/obs"
)

// Unlike fakeOBS, this wrapper honors cancellation at the stop request boundary
// and models a later candidate spending time in OBS before reporting failure.
type delayedCandidateOBS struct {
	*fakeOBS
	playCalls int
	delay     time.Duration
}

func (f *delayedCandidateOBS) PlaySourceFile(ctx context.Context, source, path string, options obs.PlaySourceOptions) error {
	f.playCalls++
	if f.playCalls == 2 {
		timer := time.NewTimer(f.delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}
	}
	return f.fakeOBS.PlaySourceFile(ctx, source, path, options)
}

func (f *delayedCandidateOBS) StopSource(ctx context.Context, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return f.fakeOBS.StopSource(ctx, source)
}

func TestLaterCandidateFailureRetainsCleanupTime(t *testing.T) {
	for _, kind := range []string{"loop", "music"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			svc, fake := newLibraryTestService(t)
			svc.now = fixedNow("2026-06-24T12:00:00+08:00")
			writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
			writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_study_001.mp4")
			writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
			writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_beta.mp3")
			if err := svc.ScanLibrary(ctx); err != nil {
				t.Fatalf("ScanLibrary: %v", err)
			}
			svc.obsCleanupTimeout = 80 * time.Millisecond
			svc.obsPlaybackTimeout = time.Second
			fake.playErrAfterMutation = errors.New("mute failed after settings applied")
			svc.obs = &delayedCandidateOBS{fakeOBS: fake, delay: 2 * svc.obsCleanupTimeout}

			source := svc.cfg.OBSMusicSourceName
			var err error
			if kind == "loop" {
				source = svc.cfg.OBSLoopSourceName
				svc.playbackMu.Lock()
				_, err = svc.activateScheduledLibraryLoopLocked(ctx, svc.now(), false, true)
				svc.playbackMu.Unlock()
			} else {
				err = svc.playNextMusic(ctx, false)
			}
			if err == nil || errors.Is(err, errOBSPlayCleanupFailed) {
				t.Fatalf("playback error = %v, want candidate failure with successful cleanup", err)
			}
			if got := fake.sourceStopCalls[source]; got != 2 {
				t.Fatalf("stop calls = %d, want both failed candidates stopped", got)
			}
			if got := fake.sourcePlayed[source]; got != "" {
				t.Fatalf("failed candidate is still playing: %s", got)
			}
			if svc.activeLoopPath != "" || svc.activeMusicPath != "" {
				t.Fatal("failed playback retained active source state")
			}
			if got := svc.libraryQuarantineCountLocked(); got != 0 {
				t.Fatalf("common OBS failure retained %d quarantined assets", got)
			}
			if kind == "loop" {
				if _, exists, err := svc.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay); err != nil || exists {
					t.Fatalf("failed selection was not restored: exists=%v err=%v", exists, err)
				}
			}
		})
	}
}
