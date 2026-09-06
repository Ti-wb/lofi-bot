package obsevidence

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	SchemaVersion = "tg-obs-stale-evidence/v1"
	ToolName      = "obs-stale-monitor"

	OtherBasename = "other"

	DefaultMaxTraceBytes int64 = 1 << 20
	MinMaxTraceBytes     int64 = 64 << 10
	MaxMaxTraceBytes     int64 = 4 << 20

	DefaultMaxDuration = 20 * time.Minute
	MinMaxDuration     = time.Minute
	MaxMaxDuration     = time.Hour

	MaxFrameBytes    int64 = 1 << 20
	MaxPasswordBytes       = 1024

	CorrelationWindow = 2 * time.Second

	Conclusion = "bounded correlation consistent with superseded A"
)

var (
	ErrTraceClosed = errors.New("OBS evidence trace is closed")
	ErrTraceLimit  = errors.New("OBS evidence trace byte limit reached")
)

// Record is deliberately a closed, typed representation of every value that
// may reach the evidence file. Protocol payloads must never be serialized
// directly into it.
type Record struct {
	Schema    string `json:"schema,omitempty"`
	Seq       uint64 `json:"seq"`
	UTC       string `json:"utc"`
	ElapsedNS int64  `json:"elapsed_ns"`
	Kind      string `json:"kind"`

	Tool                 string `json:"tool,omitempty"`
	Revision             string `json:"revision,omitempty"`
	Modified             *bool  `json:"modified,omitempty"`
	Input                string `json:"input,omitempty"`
	ABasename            string `json:"a_basename,omitempty"`
	BBasename            string `json:"b_basename,omitempty"`
	OBSWebSocketVersion  string `json:"obs_websocket_version,omitempty"`
	RPCVersion           int    `json:"rpc_version,omitempty"`
	Subscriptions        int    `json:"subscriptions,omitempty"`
	CorrelationWindowNS  int64  `json:"correlation_window_ns,omitempty"`
	MaxBytes             int64  `json:"max_bytes,omitempty"`
	MaxFrameBytes        int64  `json:"max_frame_bytes,omitempty"`
	MaxDurationNS        int64  `json:"max_duration_ns,omitempty"`
	Event                string `json:"event,omitempty"`
	Action               string `json:"action,omitempty"`
	Method               string `json:"method,omitempty"`
	Basename             string `json:"basename,omitempty"`
	Trigger              string `json:"trigger,omitempty"`
	RelatedSeq           uint64 `json:"related_seq,omitempty"`
	RequestSentElapsedNS int64  `json:"request_sent_elapsed_ns,omitempty"`
	ResponseElapsedNS    int64  `json:"response_elapsed_ns,omitempty"`
	Result               *bool  `json:"result,omitempty"`
	Code                 int    `json:"code,omitempty"`
	LastRestartSeq       uint64 `json:"last_restart_seq,omitempty"`
	SinceRestartNS       *int64 `json:"since_restart_ns,omitempty"`
	Reason               string `json:"reason,omitempty"`
	Records              uint64 `json:"records,omitempty"`
	Bytes                int64  `json:"bytes,omitempty"`
}

type Header struct {
	Revision            string
	Modified            bool
	Input               string
	ABasename           string
	BBasename           string
	OBSWebSocketVersion string
	RPCVersion          int
	MaxBytes            int64
	MaxDuration         time.Duration
}

type TraceWriter struct {
	mu       sync.Mutex
	file     *os.File
	start    time.Time
	maxBytes int64
	bytes    int64
	seq      uint64
	closed   bool
}

func NewTraceWriter(path string, start time.Time, maxBytes int64) (*TraceWriter, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("trace output path must be absolute")
	}
	if maxBytes < MinMaxTraceBytes || maxBytes > MaxMaxTraceBytes {
		return nil, errors.New("trace byte limit is out of range")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create trace output: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("set trace output permissions: %w", err)
	}
	return &TraceWriter{
		file:     file,
		start:    start,
		maxBytes: maxBytes,
	}, nil
}

func (w *TraceWriter) WriteHeader(at time.Time, header Header) (Record, error) {
	if err := validateSafeName(header.Input); err != nil {
		return Record{}, ErrInvalidConfig
	}
	if _, err := newBasenameMapper(header.ABasename, header.BBasename); err != nil {
		return Record{}, ErrInvalidConfig
	}
	normalizedVersion, ok := normalizeOBSWebSocketVersion(header.OBSWebSocketVersion)
	if !ok ||
		header.RPCVersion != 1 ||
		header.MaxBytes < MinMaxTraceBytes ||
		header.MaxBytes > MaxMaxTraceBytes ||
		header.MaxDuration < MinMaxDuration ||
		header.MaxDuration > MaxMaxDuration {
		return Record{}, ErrInvalidConfig
	}
	modified := header.Modified
	return w.Write(at, Record{
		Schema:              SchemaVersion,
		Kind:                "header",
		Tool:                ToolName,
		Revision:            safeRevision(header.Revision),
		Modified:            &modified,
		Input:               header.Input,
		ABasename:           header.ABasename,
		BBasename:           header.BBasename,
		OBSWebSocketVersion: normalizedVersion,
		RPCVersion:          header.RPCVersion,
		Subscriptions:       eventSubscriptions,
		CorrelationWindowNS: int64(CorrelationWindow),
		MaxBytes:            header.MaxBytes,
		MaxFrameBytes:       MaxFrameBytes,
		MaxDurationNS:       int64(header.MaxDuration),
	})
}

func (w *TraceWriter) Write(at time.Time, record Record) (Record, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.writeLocked(at, record)
}

func (w *TraceWriter) writeLocked(at time.Time, record Record) (Record, error) {
	if w.closed {
		return Record{}, ErrTraceClosed
	}
	record.Seq = w.seq + 1
	record.UTC = at.UTC().Format(time.RFC3339Nano)
	record.ElapsedNS = elapsedSince(w.start, at)

	encoded, err := json.Marshal(record)
	if err != nil {
		return Record{}, errors.New("encode trace record")
	}
	encoded = append(encoded, '\n')
	if int64(len(encoded)) > w.maxBytes-w.bytes {
		return Record{}, ErrTraceLimit
	}
	if _, err := w.file.Write(encoded); err != nil {
		return Record{}, errors.New("write trace record")
	}
	w.seq = record.Seq
	w.bytes += int64(len(encoded))
	return record, nil
}

func (w *TraceWriter) WriteIntegrityFailure(at time.Time, reason string) {
	_, _ = w.Write(at, Record{
		Kind:   "integrity_failure",
		Reason: safeIntegrityReason(reason),
	})
}

func (w *TraceWriter) Close(at time.Time, reason string) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	_, recordErr := w.writeLocked(at, Record{
		Kind:    "stopped",
		Reason:  safeStopReason(reason),
		Records: w.seq,
		Bytes:   w.bytes,
	})
	w.closed = true
	syncErr := w.file.Sync()
	closeErr := w.file.Close()
	var failures []error
	if recordErr != nil {
		failures = append(failures, recordErr)
	}
	if syncErr != nil {
		failures = append(failures, errors.New("sync trace output"))
	}
	if closeErr != nil {
		failures = append(failures, errors.New("close trace output"))
	}
	return errors.Join(failures...)
}

func elapsedSince(start, at time.Time) int64 {
	elapsed := at.Sub(start)
	if elapsed < 0 {
		return 0
	}
	return int64(elapsed)
}

func safeRevision(value string) string {
	if value == "" {
		return "unknown"
	}
	if len(value) != 40 && len(value) != 64 {
		return "unknown"
	}
	for _, ch := range value {
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return "unknown"
		}
	}
	return value
}

func safeIntegrityReason(value string) string {
	switch value {
	case "protocol", "connection_closed", "duration_limit", "request_timeout",
		"pending_limit", "trace_limit":
		return value
	default:
		return "protocol"
	}
}

func safeStopReason(value string) string {
	switch value {
	case "signal", "completed", "failure":
		return value
	default:
		return "failure"
	}
}

func validateSafeName(value string) error {
	if value == "" || len(value) > 128 {
		return errors.New("safe name length is invalid")
	}
	for _, ch := range value {
		if (ch >= 'a' && ch <= 'z') ||
			(ch >= 'A' && ch <= 'Z') ||
			(ch >= '0' && ch <= '9') ||
			ch == '_' || ch == '-' || ch == '.' {
			continue
		}
		return errors.New("safe name contains an invalid character")
	}
	return nil
}

func validateBasename(value string) error {
	if err := validateSafeName(value); err != nil {
		return err
	}
	if value == "." || value == ".." ||
		strings.Contains(value, "/") ||
		strings.Contains(value, `\`) ||
		filepath.Base(value) != value {
		return errors.New("basename must be a safe leaf name")
	}
	return nil
}

type basenameMapper struct {
	a string
	b string
}

func newBasenameMapper(a, b string) (basenameMapper, error) {
	if err := validateBasename(a); err != nil {
		return basenameMapper{}, err
	}
	if err := validateBasename(b); err != nil {
		return basenameMapper{}, err
	}
	if a == b {
		return basenameMapper{}, errors.New("A and B basenames must differ")
	}
	return basenameMapper{a: a, b: b}, nil
}

func (m basenameMapper) mapPath(path string) string {
	base := filepath.Base(path)
	switch base {
	case m.a:
		return m.a
	case m.b:
		return m.b
	default:
		return OtherBasename
	}
}

func (m basenameMapper) validMapped(value string) bool {
	return value == m.a || value == m.b || value == OtherBasename
}
