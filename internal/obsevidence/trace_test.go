package obsevidence

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTraceWriterCreatesExclusiveRegular0600File(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	start := time.Now()
	writer, err := NewTraceWriter(path, start, MinMaxTraceBytes)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	if _, err := writer.WriteHeader(start, validTestHeader(MinMaxTraceBytes)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	if err := writer.Close(start.Add(time.Second), "signal"); err != nil {
		t.Fatalf("Close: %v", err)
	}

	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("Lstat: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Fatalf("mode = %v, want regular", info.Mode())
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("mode = %o, want 600", got)
	}
	if _, err := NewTraceWriter(path, start, MinMaxTraceBytes); err == nil {
		t.Fatal("second NewTraceWriter unexpectedly replaced an existing trace")
	}

	target := filepath.Join(t.TempDir(), "target.jsonl")
	if err := os.WriteFile(target, []byte("do not replace"), 0o600); err != nil {
		t.Fatalf("WriteFile target: %v", err)
	}
	link := filepath.Join(t.TempDir(), "output-link.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("Symlink: %v", err)
	}
	if _, err := NewTraceWriter(link, start, MinMaxTraceBytes); err == nil {
		t.Fatal("NewTraceWriter unexpectedly followed an output symlink")
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != "do not replace" {
		t.Fatalf("symlink target changed: data=%q err=%v", data, err)
	}
}

func TestTraceWriterNeverExceedsByteLimitOrWritesPartialJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	start := time.Now()
	writer, err := NewTraceWriter(path, start, MinMaxTraceBytes)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	if _, err := writer.WriteHeader(start, validTestHeader(MinMaxTraceBytes)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for idx := 0; ; idx++ {
		_, err := writer.Write(start.Add(time.Duration(idx+1)*time.Millisecond), Record{
			Kind:     "settings_changed",
			Event:    "InputSettingsChanged",
			Input:    "tg_music_player",
			Basename: "music_stale-short-a.m4a",
		})
		if errors.Is(err, ErrTraceLimit) {
			break
		}
		if err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	for idx := 0; ; idx++ {
		_, err := writer.Write(start.Add(time.Duration(idx+1)*time.Millisecond), Record{
			Kind:   "integrity_failure",
			Reason: "protocol",
		})
		if errors.Is(err, ErrTraceLimit) {
			break
		}
		if err != nil {
			t.Fatalf("fill with integrity record: %v", err)
		}
	}
	if err := writer.Close(start.Add(time.Minute), "failure"); !errors.Is(err, ErrTraceLimit) {
		t.Fatalf("Close error = %v, want %v", err, ErrTraceLimit)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if int64(len(data)) > MinMaxTraceBytes {
		t.Fatalf("trace size = %d, limit = %d", len(data), MinMaxTraceBytes)
	}
	if len(data) == 0 || data[len(data)-1] != '\n' {
		t.Fatal("trace ended with a partial JSON line")
	}
	for lineNumber, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		if _, _, err := decodeStrictRecord([]byte(line)); err != nil {
			t.Fatalf("line %d is partial/invalid JSON: %v", lineNumber+1, err)
		}
	}
	if _, err := VerifyTrace(path); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("VerifyTrace error = %v, want integrity failure", err)
	}
}

func TestBasenameMapperNeverEmitsRawPathOrUnknownCredentialLikeName(t *testing.T) {
	mapper, err := newBasenameMapper(
		"music_stale-short-a.m4a",
		"music_stale-long-b.m4a",
	)
	if err != nil {
		t.Fatalf("newBasenameMapper: %v", err)
	}
	const secretPath = "/private/cache/123456789:ABCdefghi_jklmnop/music_stale-short-a.m4a"
	if got := mapper.mapPath(secretPath); got != "music_stale-short-a.m4a" {
		t.Fatalf("mapped known path = %q", got)
	}
	for _, path := range []string{
		"/private/cache/obs-password.m4a",
		"/private/cache/123456789:ABCdefghi_jklmnop",
		"/private/cache/0123456789abcdef0123456789abcdef",
	} {
		if got := mapper.mapPath(path); got != OtherBasename {
			t.Fatalf("mapped unknown path %q = %q, want other", path, got)
		}
	}
}

func validTestHeader(maxBytes int64) Header {
	return Header{
		Revision:            "0123456789abcdef0123456789abcdef01234567",
		Input:               "tg_music_player",
		ABasename:           "music_stale-short-a.m4a",
		BBasename:           "music_stale-long-b.m4a",
		OBSWebSocketVersion: "5.7.3",
		RPCVersion:          1,
		MaxBytes:            maxBytes,
		MaxDuration:         MinMaxDuration,
	}
}
