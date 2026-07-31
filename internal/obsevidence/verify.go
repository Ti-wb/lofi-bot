package obsevidence

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

var (
	ErrInvalidTrace  = errors.New("invalid OBS evidence trace")
	ErrNoCorrelation = errors.New("required OBS stale-event correlation was not found")
)

type Verdict struct {
	Schema           string `json:"schema"`
	Result           string `json:"result"`
	Conclusion       string `json:"conclusion"`
	ARestartSeq      uint64 `json:"a_restart_seq"`
	BRestartSeq      uint64 `json:"b_restart_seq"`
	EndedSeq         uint64 `json:"ended_seq"`
	ImmediateSeq     uint64 `json:"immediate_snapshot_seq"`
	SettledSeq       uint64 `json:"settled_snapshot_seq"`
	DeltaNS          int64  `json:"delta_ns"`
	CorrelationLimit int64  `json:"correlation_limit_ns"`
}

func VerifyTrace(path string) (Verdict, error) {
	if !filepath.IsAbs(path) {
		return Verdict{}, ErrInvalidTrace
	}
	data, err := readIdentityBoundRegularFile(path)
	if err != nil || len(data) == 0 || data[len(data)-1] != '\n' {
		return Verdict{}, ErrInvalidTrace
	}

	rawLines := bytes.Split(data[:len(data)-1], []byte{'\n'})
	if len(rawLines) < 2 {
		return Verdict{}, ErrInvalidTrace
	}
	records := make([]Record, 0, len(rawLines))
	var header Record
	var previousElapsed int64
	offset := int64(0)

	for idx, line := range rawLines {
		if len(line) == 0 || len(line) > 16<<10 {
			return Verdict{}, ErrInvalidTrace
		}
		record, fields, err := decodeStrictRecord(line)
		if err != nil ||
			record.Seq != uint64(idx+1) ||
			record.ElapsedNS < 0 ||
			(idx > 0 && record.ElapsedNS < previousElapsed) {
			return Verdict{}, ErrInvalidTrace
		}
		if _, err := time.Parse(time.RFC3339Nano, record.UTC); err != nil {
			return Verdict{}, ErrInvalidTrace
		}
		if !recordFieldsAllowed(record.Kind, fields) {
			return Verdict{}, ErrInvalidTrace
		}

		if idx == 0 {
			if err := validateHeader(record, int64(len(data))); err != nil {
				return Verdict{}, err
			}
			header = record
		} else if err := validateBodyRecord(record, header); err != nil {
			return Verdict{}, err
		}
		if record.Kind == "stopped" {
			if idx != len(rawLines)-1 ||
				record.Records != record.Seq-1 ||
				record.Bytes != offset {
				return Verdict{}, ErrInvalidTrace
			}
		}
		if record.Kind == "integrity_failure" {
			return Verdict{}, ErrInvalidTrace
		}

		records = append(records, record)
		previousElapsed = record.ElapsedNS
		offset += int64(len(line) + 1)
	}
	if records[len(records)-1].Kind != "stopped" {
		return Verdict{}, ErrInvalidTrace
	}
	return findCorrelation(records, header)
}

func readIdentityBoundRegularFile(path string) ([]byte, error) {
	return readIdentityBoundRegularFileAfterLstat(path, nil)
}

func readIdentityBoundRegularFileAfterLstat(
	path string,
	afterLstat func(),
) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return nil, ErrInvalidTrace
	}
	if afterLstat != nil {
		afterLstat()
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrInvalidTrace
	}
	defer file.Close()

	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, ErrInvalidTrace
	}
	after, err := os.Lstat(path)
	if err != nil ||
		!after.Mode().IsRegular() ||
		!os.SameFile(opened, after) {
		return nil, ErrInvalidTrace
	}

	data, err := io.ReadAll(io.LimitReader(file, MaxMaxTraceBytes+1))
	if err != nil || int64(len(data)) > MaxMaxTraceBytes {
		return nil, ErrInvalidTrace
	}
	return data, nil
}

func decodeStrictRecord(line []byte) (Record, map[string]struct{}, error) {
	fields := make(map[string]struct{})
	tokenDecoder := json.NewDecoder(bytes.NewReader(line))
	token, err := tokenDecoder.Token()
	if err != nil || token != json.Delim('{') {
		return Record{}, nil, ErrInvalidTrace
	}
	for tokenDecoder.More() {
		keyToken, err := tokenDecoder.Token()
		if err != nil {
			return Record{}, nil, ErrInvalidTrace
		}
		key, ok := keyToken.(string)
		if !ok {
			return Record{}, nil, ErrInvalidTrace
		}
		if _, duplicate := fields[key]; duplicate {
			return Record{}, nil, ErrInvalidTrace
		}
		fields[key] = struct{}{}
		var value json.RawMessage
		if err := tokenDecoder.Decode(&value); err != nil {
			return Record{}, nil, ErrInvalidTrace
		}
	}
	if token, err = tokenDecoder.Token(); err != nil || token != json.Delim('}') {
		return Record{}, nil, ErrInvalidTrace
	}
	if token, err = tokenDecoder.Token(); !errors.Is(err, io.EOF) || token != nil {
		return Record{}, nil, ErrInvalidTrace
	}

	decoder := json.NewDecoder(bytes.NewReader(line))
	decoder.DisallowUnknownFields()
	var record Record
	if err := decoder.Decode(&record); err != nil {
		return Record{}, nil, ErrInvalidTrace
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return Record{}, nil, ErrInvalidTrace
	}
	return record, fields, nil
}

func recordFieldsAllowed(kind string, actual map[string]struct{}) bool {
	common := []string{"seq", "utc", "elapsed_ns", "kind"}
	var specific []string
	var requiredSpecific []string
	switch kind {
	case "header":
		specific = []string{
			"schema", "tool", "revision", "modified", "input", "a_basename",
			"b_basename", "obs_websocket_version", "rpc_version",
			"subscriptions", "correlation_window_ns", "max_bytes",
			"max_frame_bytes", "max_duration_ns",
		}
		requiredSpecific = specific
	case "settings_changed":
		specific = []string{"event", "input", "basename"}
		requiredSpecific = specific
	case "media_action":
		specific = []string{"event", "input", "action", "basename"}
		requiredSpecific = specific
	case "settings_snapshot":
		specific = []string{
			"method", "input", "trigger", "related_seq",
			"request_sent_elapsed_ns", "response_elapsed_ns", "result",
			"code", "basename",
		}
		requiredSpecific = []string{
			"method", "input", "trigger", "request_sent_elapsed_ns",
			"response_elapsed_ns", "result",
		}
	case "playback_ended":
		specific = []string{
			"event", "input", "basename", "last_restart_seq",
			"since_restart_ns",
		}
		requiredSpecific = specific
	case "integrity_failure":
		specific = []string{"reason"}
		requiredSpecific = specific
	case "stopped":
		specific = []string{"reason", "records", "bytes"}
		requiredSpecific = specific
	default:
		return false
	}
	allowed := make(map[string]struct{}, len(common)+len(specific))
	for _, key := range append(common, specific...) {
		allowed[key] = struct{}{}
	}
	for key := range actual {
		if _, ok := allowed[key]; !ok {
			return false
		}
	}
	for _, key := range common {
		if _, ok := actual[key]; !ok {
			return false
		}
	}
	for _, key := range requiredSpecific {
		if _, ok := actual[key]; !ok {
			return false
		}
	}
	return true
}

func validateHeader(record Record, fileSize int64) error {
	if record.Kind != "header" ||
		record.Schema != SchemaVersion ||
		record.Tool != ToolName ||
		record.Modified == nil ||
		*record.Modified ||
		record.Revision == "unknown" ||
		safeRevision(record.Revision) != record.Revision ||
		validateSafeName(record.Input) != nil ||
		validateBasename(record.ABasename) != nil ||
		validateBasename(record.BBasename) != nil ||
		record.ABasename == record.BBasename ||
		!supportedOBSWebSocketVersion(record.OBSWebSocketVersion) ||
		record.RPCVersion != 1 ||
		record.Subscriptions != eventSubscriptions ||
		record.CorrelationWindowNS != int64(CorrelationWindow) ||
		record.MaxBytes < fileSize ||
		record.MaxBytes < MinMaxTraceBytes ||
		record.MaxBytes > MaxMaxTraceBytes ||
		record.MaxFrameBytes != MaxFrameBytes ||
		time.Duration(record.MaxDurationNS) < MinMaxDuration ||
		time.Duration(record.MaxDurationNS) > MaxMaxDuration {
		return ErrInvalidTrace
	}
	return nil
}

func validateBodyRecord(record Record, header Record) error {
	mapper, err := newBasenameMapper(header.ABasename, header.BBasename)
	if err != nil {
		return ErrInvalidTrace
	}
	validCommon := func() bool {
		return record.Input == header.Input && mapper.validMapped(record.Basename)
	}
	switch record.Kind {
	case "settings_changed":
		if record.Event != "InputSettingsChanged" || !validCommon() {
			return ErrInvalidTrace
		}
	case "media_action":
		if record.Event != "MediaInputActionTriggered" ||
			!validCommon() ||
			(record.Action != "restart" &&
				record.Action != "stop" &&
				record.Action != "other") {
			return ErrInvalidTrace
		}
	case "settings_snapshot":
		if record.Method != "GetInputSettings" ||
			record.Input != header.Input ||
			record.Result == nil ||
			record.RequestSentElapsedNS < 0 ||
			record.ResponseElapsedNS != record.ElapsedNS ||
			record.RequestSentElapsedNS > record.ResponseElapsedNS ||
			!validSnapshotTrigger(record.Trigger) {
			return ErrInvalidTrace
		}
		if record.Trigger == "initial" {
			if record.RelatedSeq != 0 {
				return ErrInvalidTrace
			}
		} else if record.RelatedSeq == 0 || record.RelatedSeq >= record.Seq {
			return ErrInvalidTrace
		}
		if *record.Result {
			if record.Code != 100 || !mapper.validMapped(record.Basename) {
				return ErrInvalidTrace
			}
		} else {
			return ErrInvalidTrace
		}
	case "playback_ended":
		if record.Event != "MediaInputPlaybackEnded" ||
			!validCommon() ||
			record.LastRestartSeq == 0 ||
			record.LastRestartSeq >= record.Seq ||
			record.SinceRestartNS == nil ||
			*record.SinceRestartNS < 0 {
			return ErrInvalidTrace
		}
	case "stopped":
		if record.Reason != "signal" && record.Reason != "completed" {
			return ErrInvalidTrace
		}
	case "integrity_failure":
		return ErrInvalidTrace
	default:
		return ErrInvalidTrace
	}
	return nil
}

func validSnapshotTrigger(value string) bool {
	switch value {
	case "initial", "settings_changed", "restart",
		"ended_immediate", "ended_settle":
		return true
	default:
		return false
	}
}

func findCorrelation(records []Record, header Record) (Verdict, error) {
	restartSnapshots := make(map[uint64]Record)
	for _, record := range records {
		if record.Kind == "settings_snapshot" &&
			record.Trigger == "restart" &&
			record.Result != nil &&
			*record.Result {
			restartSnapshots[record.RelatedSeq] = record
		}
	}

	for bIdx, bRestart := range records {
		if !confirmedRestart(bRestart, header.BBasename, restartSnapshots) {
			continue
		}
		aIdx, aRestart, ok := precedingConfirmedARestart(
			records[:bIdx],
			header.ABasename,
			restartSnapshots,
			bRestart.Seq,
		)
		if !ok || !cleanAToBTransition(
			records[aIdx+1:bIdx],
			aRestart,
			header.ABasename,
			header.BBasename,
		) {
			continue
		}

		for endedIdx := bIdx + 1; endedIdx < len(records); endedIdx++ {
			ended := records[endedIdx]
			if ended.Kind != "playback_ended" ||
				ended.LastRestartSeq != bRestart.Seq ||
				ended.Basename != header.BBasename {
				continue
			}
			delta := ended.ElapsedNS - bRestart.ElapsedNS
			if delta < 0 ||
				delta >= int64(CorrelationWindow) ||
				ended.SinceRestartNS == nil ||
				*ended.SinceRestartNS != delta {
				continue
			}
			bSnapshot := restartSnapshots[bRestart.Seq]
			if bSnapshot.Seq <= bRestart.Seq ||
				bSnapshot.Seq >= ended.Seq ||
				bSnapshot.ResponseElapsedNS > ended.ElapsedNS {
				continue
			}

			immediate, settled, ok := endedSnapshots(
				records[endedIdx+1:],
				ended,
				header.BBasename,
			)
			if !ok {
				continue
			}
			if switchedAway(
				records[bIdx+1:],
				bRestart.ElapsedNS,
				settled.ResponseElapsedNS,
				header.BBasename,
				ended.Seq,
			) {
				continue
			}
			return Verdict{
				Schema:           "tg-obs-stale-verdict/v1",
				Result:           "pass",
				Conclusion:       Conclusion,
				ARestartSeq:      aRestart.Seq,
				BRestartSeq:      bRestart.Seq,
				EndedSeq:         ended.Seq,
				ImmediateSeq:     immediate.Seq,
				SettledSeq:       settled.Seq,
				DeltaNS:          delta,
				CorrelationLimit: int64(CorrelationWindow),
			}, nil
		}
	}
	return Verdict{}, ErrNoCorrelation
}

func confirmedRestart(
	record Record,
	basename string,
	snapshots map[uint64]Record,
) bool {
	if record.Kind != "media_action" ||
		record.Action != "restart" ||
		record.Basename != basename {
		return false
	}
	snapshot, ok := snapshots[record.Seq]
	return ok &&
		snapshot.Seq > record.Seq &&
		snapshot.Basename == basename
}

func precedingConfirmedARestart(
	records []Record,
	aBasename string,
	snapshots map[uint64]Record,
	beforeSeq uint64,
) (int, Record, bool) {
	for idx := len(records) - 1; idx >= 0; idx-- {
		if confirmedRestart(records[idx], aBasename, snapshots) &&
			snapshots[records[idx].Seq].Seq < beforeSeq {
			return idx, records[idx], true
		}
	}
	return 0, Record{}, false
}

func cleanAToBTransition(
	records []Record,
	aRestart Record,
	aBasename string,
	bBasename string,
) bool {
	bChanges := 0
	for _, record := range records {
		switch record.Kind {
		case "settings_changed":
			if record.Basename != bBasename {
				return false
			}
			bChanges++
			if bChanges > 1 {
				return false
			}
		case "media_action":
			return false
		case "settings_snapshot":
			if record.Trigger == "restart" &&
				(record.RelatedSeq != aRestart.Seq ||
					record.Basename != aBasename) {
				return false
			}
		}
	}
	return bChanges == 1
}

func endedSnapshots(
	records []Record,
	ended Record,
	bBasename string,
) (Record, Record, bool) {
	var immediate Record
	var settled Record
	for _, record := range records {
		if record.Kind != "settings_snapshot" ||
			record.RelatedSeq != ended.Seq ||
			record.Result == nil ||
			!*record.Result ||
			record.Basename != bBasename {
			continue
		}
		switch record.Trigger {
		case "ended_immediate":
			if record.RequestSentElapsedNS >= ended.ElapsedNS &&
				record.RequestSentElapsedNS-ended.ElapsedNS < int64(time.Second) &&
				immediate.Seq == 0 {
				immediate = record
			}
		case "ended_settle":
			if record.RequestSentElapsedNS-ended.ElapsedNS >= int64(CorrelationWindow) &&
				record.RequestSentElapsedNS-ended.ElapsedNS <
					int64(CorrelationWindow+time.Second) &&
				settled.Seq == 0 {
				settled = record
			}
		}
	}
	return immediate, settled,
		immediate.Seq != 0 &&
			settled.Seq != 0 &&
			immediate.Seq < settled.Seq &&
			immediate.ResponseElapsedNS <= settled.RequestSentElapsedNS
}

func switchedAway(
	records []Record,
	fromNS int64,
	throughNS int64,
	bBasename string,
	allowedEndedSeq uint64,
) bool {
	for _, record := range records {
		if record.ElapsedNS > throughNS {
			break
		}
		switch record.Kind {
		case "settings_changed":
			if record.Basename != bBasename {
				return true
			}
		case "media_action":
			return true
		case "playback_ended":
			if record.Seq != allowedEndedSeq {
				return true
			}
		case "settings_snapshot":
			if record.RequestSentElapsedNS >= fromNS &&
				record.Result != nil &&
				*record.Result &&
				record.Basename != bBasename {
				return true
			}
		}
	}
	return false
}

func SanitizedVerdictJSON(verdict Verdict) ([]byte, error) {
	if verdict.Result != "pass" ||
		verdict.Conclusion != Conclusion ||
		verdict.Schema != "tg-obs-stale-verdict/v1" {
		return nil, ErrInvalidTrace
	}
	return json.Marshal(verdict)
}
