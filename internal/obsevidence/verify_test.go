package obsevidence

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type verifyScenario struct {
	delta            time.Duration
	initialEnded     bool
	omitRestart      bool
	lateASnapshot    bool
	otherBetween     bool
	duplicateBChange bool
	thirdRestart     bool
	switchAfterB     bool
}

func TestVerifyTraceAllowsEndBeforeFirstObservedRestart(t *testing.T) {
	path := writeVerificationTrace(t, verifyScenario{initialEnded: true})
	if verdict, err := VerifyTrace(path); err != nil || verdict.Result != "pass" {
		t.Fatalf("later A/B correlation rejected after initial ended event: verdict=%+v err=%v", verdict, err)
	}
}

func TestVerifyTraceDoesNotCorrelateInitialEndAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "initial-end.jsonl")
	start := time.Now()
	writer, err := NewTraceWriter(path, start, DefaultMaxTraceBytes)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteHeader(start, validTestHeader(DefaultMaxTraceBytes)); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(start.Add(time.Millisecond), Record{
		Kind: "playback_ended", Event: "MediaInputPlaybackEnded",
		Input: "tg_music_player", Basename: "music_stale-short-a.m4a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(start.Add(time.Second), "signal"); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTrace(path); !errors.Is(err, ErrNoCorrelation) {
		t.Fatalf("initial end error = %v, want valid trace without correlation", err)
	}
}

func TestVerifyTraceStillRequiresMetadataAfterObservedRestart(t *testing.T) {
	path := writeVerificationTrace(t, verifyScenario{omitRestart: true})
	if _, err := VerifyTrace(path); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("VerifyTrace error = %v, want missing restart metadata rejected", err)
	}
}

func TestVerifyTraceAcceptsOnlyBoundedSupersededACorrelation(t *testing.T) {
	path := writeVerificationTrace(t, verifyScenario{delta: 500 * time.Millisecond})
	verdict, err := VerifyTrace(path)
	if err != nil {
		t.Fatalf("VerifyTrace: %v", err)
	}
	if verdict.Result != "pass" {
		t.Fatalf("result = %q", verdict.Result)
	}
	if verdict.Conclusion != Conclusion {
		t.Fatalf("conclusion = %q, want exact bounded conclusion", verdict.Conclusion)
	}
	if verdict.DeltaNS != int64(500*time.Millisecond) {
		t.Fatalf("delta = %d", verdict.DeltaNS)
	}
	if verdict.ARestartSeq == 0 ||
		verdict.BRestartSeq == 0 ||
		verdict.EndedSeq == 0 ||
		verdict.ImmediateSeq == 0 ||
		verdict.SettledSeq == 0 {
		t.Fatalf("incomplete verdict: %+v", verdict)
	}
}

func TestVerifyTraceRejectsUnprovenOrContaminatedTransitions(t *testing.T) {
	tests := []struct {
		name     string
		scenario verifyScenario
	}{
		{
			name:     "ended_at_two_seconds",
			scenario: verifyScenario{delta: CorrelationWindow},
		},
		{
			name: "A_confirmation_after_B_restart",
			scenario: verifyScenario{
				delta:         500 * time.Millisecond,
				lateASnapshot: true,
			},
		},
		{
			name: "other_settings_between_A_and_B",
			scenario: verifyScenario{
				delta:        500 * time.Millisecond,
				otherBetween: true,
			},
		},
		{
			name: "duplicate_B_settings_change",
			scenario: verifyScenario{
				delta:            500 * time.Millisecond,
				duplicateBChange: true,
			},
		},
		{
			name: "third_restart_between_A_and_B",
			scenario: verifyScenario{
				delta:        500 * time.Millisecond,
				thirdRestart: true,
			},
		},
		{
			name: "switch_after_B_before_settle",
			scenario: verifyScenario{
				delta:        500 * time.Millisecond,
				switchAfterB: true,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := writeVerificationTrace(t, test.scenario)
			if _, err := VerifyTrace(path); !errors.Is(err, ErrNoCorrelation) {
				t.Fatalf("VerifyTrace error = %v, want %v", err, ErrNoCorrelation)
			}
		})
	}
}

func TestVerifyTraceRejectsSymlinkAndNonRegularFile(t *testing.T) {
	valid := writeVerificationTrace(t, verifyScenario{delta: 500 * time.Millisecond})
	link := filepath.Join(t.TempDir(), "trace-link.jsonl")
	if err := os.Symlink(valid, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := VerifyTrace(link); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("symlink VerifyTrace error = %v", err)
	}
	if _, err := VerifyTrace(filepath.Dir(valid)); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("directory VerifyTrace error = %v", err)
	}
}

func TestIdentityBoundReadRejectsAtomicReplacementAfterLstat(t *testing.T) {
	for _, replacement := range []string{"regular", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "trace.jsonl")
			original := filepath.Join(dir, "original.jsonl")
			if err := os.WriteFile(path, []byte("original\n"), 0o600); err != nil {
				t.Fatalf("WriteFile original: %v", err)
			}
			_, err := readIdentityBoundRegularFileAfterLstat(path, func() {
				if renameErr := os.Rename(path, original); renameErr != nil {
					t.Fatalf("Rename: %v", renameErr)
				}
				switch replacement {
				case "regular":
					if writeErr := os.WriteFile(path, []byte("replacement\n"), 0o600); writeErr != nil {
						t.Fatalf("WriteFile replacement: %v", writeErr)
					}
				case "symlink":
					if linkErr := os.Symlink(original, path); linkErr != nil {
						t.Fatalf("Symlink replacement: %v", linkErr)
					}
				}
			})
			if !errors.Is(err, ErrInvalidTrace) {
				t.Fatalf("identity-bound read error = %v, want %v", err, ErrInvalidTrace)
			}
		})
	}
}

func writeVerificationTrace(t *testing.T, scenario verifyScenario) string {
	t.Helper()
	if scenario.delta == 0 {
		scenario.delta = 500 * time.Millisecond
	}
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	start := time.Now()
	writer, err := NewTraceWriter(path, start, DefaultMaxTraceBytes)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	mustWrite := func(at time.Time, record Record) Record {
		t.Helper()
		written, err := writer.Write(at, record)
		if err != nil {
			t.Fatalf("Write %s: %v", record.Kind, err)
		}
		return written
	}
	if _, err := writer.WriteHeader(start, validTestHeader(DefaultMaxTraceBytes)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	success := true
	mustWrite(start.Add(10*time.Millisecond), Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "initial",
		RequestSentElapsedNS: int64(5 * time.Millisecond),
		ResponseElapsedNS:    int64(10 * time.Millisecond),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-short-a.m4a",
	})
	if scenario.initialEnded {
		mustWrite(start.Add(20*time.Millisecond), Record{
			Kind: "playback_ended", Event: "MediaInputPlaybackEnded",
			Input: "tg_music_player", Basename: "music_stale-short-a.m4a",
		})
	}
	mustWrite(start.Add(100*time.Millisecond), Record{
		Kind:     "settings_changed",
		Event:    "InputSettingsChanged",
		Input:    "tg_music_player",
		Basename: "music_stale-short-a.m4a",
	})
	aRestart := mustWrite(start.Add(200*time.Millisecond), Record{
		Kind:     "media_action",
		Event:    "MediaInputActionTriggered",
		Input:    "tg_music_player",
		Action:   "restart",
		Basename: "music_stale-short-a.m4a",
	})
	if !scenario.lateASnapshot {
		mustWrite(start.Add(210*time.Millisecond), restartSnapshot(
			aRestart.Seq,
			"music_stale-short-a.m4a",
			205*time.Millisecond,
			210*time.Millisecond,
		))
	}
	nextAt := 250 * time.Millisecond
	if scenario.otherBetween {
		mustWrite(start.Add(nextAt), Record{
			Kind:     "settings_changed",
			Event:    "InputSettingsChanged",
			Input:    "tg_music_player",
			Basename: OtherBasename,
		})
		nextAt += 10 * time.Millisecond
	}
	if scenario.thirdRestart {
		mustWrite(start.Add(nextAt), Record{
			Kind:     "media_action",
			Event:    "MediaInputActionTriggered",
			Input:    "tg_music_player",
			Action:   "restart",
			Basename: "music_stale-short-a.m4a",
		})
		nextAt += 10 * time.Millisecond
	}
	mustWrite(start.Add(nextAt), Record{
		Kind:     "settings_changed",
		Event:    "InputSettingsChanged",
		Input:    "tg_music_player",
		Basename: "music_stale-long-b.m4a",
	})
	nextAt += 10 * time.Millisecond
	if scenario.duplicateBChange {
		mustWrite(start.Add(nextAt), Record{
			Kind:     "settings_changed",
			Event:    "InputSettingsChanged",
			Input:    "tg_music_player",
			Basename: "music_stale-long-b.m4a",
		})
		nextAt += 10 * time.Millisecond
	}
	bRestart := mustWrite(start.Add(nextAt), Record{
		Kind:     "media_action",
		Event:    "MediaInputActionTriggered",
		Input:    "tg_music_player",
		Action:   "restart",
		Basename: "music_stale-long-b.m4a",
	})
	nextAt += 10 * time.Millisecond
	if scenario.lateASnapshot {
		mustWrite(start.Add(nextAt), restartSnapshot(
			aRestart.Seq,
			"music_stale-short-a.m4a",
			nextAt-time.Millisecond,
			nextAt,
		))
		nextAt += 10 * time.Millisecond
	}
	mustWrite(start.Add(nextAt), restartSnapshot(
		bRestart.Seq,
		"music_stale-long-b.m4a",
		nextAt-time.Millisecond,
		nextAt,
	))

	endedAt := time.Duration(bRestart.ElapsedNS) + scenario.delta
	since := int64(scenario.delta)
	endedRecord := Record{
		Kind:           "playback_ended",
		Event:          "MediaInputPlaybackEnded",
		Input:          "tg_music_player",
		Basename:       "music_stale-long-b.m4a",
		LastRestartSeq: bRestart.Seq,
		SinceRestartNS: &since,
	}
	if scenario.omitRestart {
		endedRecord.LastRestartSeq = 0
		endedRecord.SinceRestartNS = nil
	}
	ended := mustWrite(start.Add(endedAt), endedRecord)
	immediateAt := endedAt + 10*time.Millisecond
	mustWrite(start.Add(immediateAt), Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "ended_immediate",
		RelatedSeq:           ended.Seq,
		RequestSentElapsedNS: int64(endedAt + time.Millisecond),
		ResponseElapsedNS:    int64(immediateAt),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-long-b.m4a",
	})
	if scenario.switchAfterB {
		mustWrite(start.Add(endedAt+time.Second), Record{
			Kind:     "settings_changed",
			Event:    "InputSettingsChanged",
			Input:    "tg_music_player",
			Basename: "music_stale-short-a.m4a",
		})
	}
	settledAt := endedAt + CorrelationWindow + 10*time.Millisecond
	mustWrite(start.Add(settledAt), Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "ended_settle",
		RelatedSeq:           ended.Seq,
		RequestSentElapsedNS: int64(endedAt + CorrelationWindow),
		ResponseElapsedNS:    int64(settledAt),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-long-b.m4a",
	})
	if err := writer.Close(start.Add(settledAt+time.Second), "signal"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

func restartSnapshot(
	relatedSeq uint64,
	basename string,
	sentAt time.Duration,
	responseAt time.Duration,
) Record {
	success := true
	return Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "restart",
		RelatedSeq:           relatedSeq,
		RequestSentElapsedNS: int64(sentAt),
		ResponseElapsedNS:    int64(responseAt),
		Result:               &success,
		Code:                 100,
		Basename:             basename,
	}
}
