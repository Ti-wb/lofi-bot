package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/obsevidence"
)

func TestCaptureCLIReadsPasswordOnlyFromStdinAndNeverEchoesIt(t *testing.T) {
	const secret = "obs-password-cli-canary"
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{
			"capture",
			"--password-stdin",
			"--host", "not-loopback.invalid",
			"--input", "tg_music_player",
			"--a-basename", "music_stale-short-a.m4a",
			"--b-basename", "music_stale-long-b.m4a",
			"--output", filepath.Join(t.TempDir(), "trace-"+secret+".jsonl"),
		},
		strings.NewReader(secret),
		&stdout,
		&stderr,
	)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if got := stderr.String(); got != "E_CONFIG\n" {
		t.Fatalf("stderr = %q", got)
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("CLI output leaked stdin password or secret-bearing output path")
	}

	stdout.Reset()
	stderr.Reset()
	code = run(
		context.Background(),
		[]string{"capture", "--password", secret},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if code != 2 || stderr.String() != "E_USAGE\n" {
		t.Fatalf("password argv exit=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("unknown password flag value was echoed")
	}
}

func TestCaptureCLIRejectsOversizedPasswordWithoutEcho(t *testing.T) {
	secret := strings.Repeat("P", obsevidence.MaxPasswordBytes+1)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"capture", "--password-stdin"},
		strings.NewReader(secret),
		&stdout,
		&stderr,
	)
	if code != 2 || stderr.String() != "E_PASSWORD\n" {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), secret) {
		t.Fatal("oversized stdin password was echoed")
	}
}

func TestVerifyCLIEmitsOnlyBoundedCorrelationVerdict(t *testing.T) {
	trace := writeCLITrace(t)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"verify", "--trace", trace},
		strings.NewReader(""),
		&stdout,
		&stderr,
	)
	if code != 0 {
		t.Fatalf("exit=%d stderr=%q", code, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q", stderr.String())
	}
	text := stdout.String()
	if !strings.Contains(text, `"result":"pass"`) ||
		!strings.Contains(text, obsevidence.Conclusion) {
		t.Fatalf("unexpected verdict: %s", text)
	}
	for _, forbidden := range []string{trace, "/private/", "password", "authentication"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("verdict leaked %q: %s", forbidden, text)
		}
	}
}

func writeCLITrace(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	start := time.Now()
	writer, err := obsevidence.NewTraceWriter(
		path,
		start,
		obsevidence.DefaultMaxTraceBytes,
	)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	if _, err := writer.WriteHeader(start, obsevidence.Header{
		Revision:            "0123456789abcdef0123456789abcdef01234567",
		Input:               "tg_music_player",
		ABasename:           "music_stale-short-a.m4a",
		BBasename:           "music_stale-long-b.m4a",
		OBSWebSocketVersion: "5.7.3",
		RPCVersion:          1,
		MaxBytes:            obsevidence.DefaultMaxTraceBytes,
		MaxDuration:         obsevidence.MinMaxDuration,
	}); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	write := func(at time.Duration, record obsevidence.Record) obsevidence.Record {
		t.Helper()
		written, err := writer.Write(start.Add(at), record)
		if err != nil {
			t.Fatalf("Write %s: %v", record.Kind, err)
		}
		return written
	}
	success := true
	write(100*time.Millisecond, obsevidence.Record{
		Kind:     "settings_changed",
		Event:    "InputSettingsChanged",
		Input:    "tg_music_player",
		Basename: "music_stale-short-a.m4a",
	})
	aRestart := write(200*time.Millisecond, obsevidence.Record{
		Kind:     "media_action",
		Event:    "MediaInputActionTriggered",
		Input:    "tg_music_player",
		Action:   "restart",
		Basename: "music_stale-short-a.m4a",
	})
	write(210*time.Millisecond, obsevidence.Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "restart",
		RelatedSeq:           aRestart.Seq,
		RequestSentElapsedNS: int64(205 * time.Millisecond),
		ResponseElapsedNS:    int64(210 * time.Millisecond),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-short-a.m4a",
	})
	write(300*time.Millisecond, obsevidence.Record{
		Kind:     "settings_changed",
		Event:    "InputSettingsChanged",
		Input:    "tg_music_player",
		Basename: "music_stale-long-b.m4a",
	})
	bRestart := write(400*time.Millisecond, obsevidence.Record{
		Kind:     "media_action",
		Event:    "MediaInputActionTriggered",
		Input:    "tg_music_player",
		Action:   "restart",
		Basename: "music_stale-long-b.m4a",
	})
	write(410*time.Millisecond, obsevidence.Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "restart",
		RelatedSeq:           bRestart.Seq,
		RequestSentElapsedNS: int64(405 * time.Millisecond),
		ResponseElapsedNS:    int64(410 * time.Millisecond),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-long-b.m4a",
	})
	since := int64(500 * time.Millisecond)
	ended := write(900*time.Millisecond, obsevidence.Record{
		Kind:           "playback_ended",
		Event:          "MediaInputPlaybackEnded",
		Input:          "tg_music_player",
		Basename:       "music_stale-long-b.m4a",
		LastRestartSeq: bRestart.Seq,
		SinceRestartNS: &since,
	})
	write(910*time.Millisecond, obsevidence.Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "ended_immediate",
		RelatedSeq:           ended.Seq,
		RequestSentElapsedNS: int64(901 * time.Millisecond),
		ResponseElapsedNS:    int64(910 * time.Millisecond),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-long-b.m4a",
	})
	write(2910*time.Millisecond, obsevidence.Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                "tg_music_player",
		Trigger:              "ended_settle",
		RelatedSeq:           ended.Seq,
		RequestSentElapsedNS: int64(2900 * time.Millisecond),
		ResponseElapsedNS:    int64(2910 * time.Millisecond),
		Result:               &success,
		Code:                 100,
		Basename:             "music_stale-long-b.m4a",
	})
	if err := writer.Close(start.Add(4*time.Second), "signal"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		t.Fatalf("trace is not regular: %v", err)
	}
	return path
}
