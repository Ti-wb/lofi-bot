package obsevidence

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

const (
	eventSubscriptions = (1 << 3) | (1 << 8) // Inputs | MediaInputs

	opHello           = 0
	opIdentify        = 1
	opIdentified      = 2
	opEvent           = 5
	opRequest         = 6
	opRequestResponse = 7

	maxPendingSnapshots = 8
	defaultRequestWait  = 5 * time.Second
	handshakeTimeout    = 10 * time.Second
)

var (
	ErrInvalidConfig    = errors.New("invalid OBS evidence monitor configuration")
	ErrOBSVersion       = errors.New("obs-websocket 5.4 or newer is required")
	ErrProtocol         = errors.New("invalid OBS WebSocket protocol data")
	ErrConnectionClosed = errors.New("OBS evidence connection closed")
	ErrDurationLimit    = errors.New("OBS evidence capture duration limit reached")
	ErrRequestTimeout   = errors.New("OBS evidence snapshot request timed out")
	ErrPendingLimit     = errors.New("too many pending OBS evidence snapshots")
)

type CaptureConfig struct {
	Host        string
	Port        int
	Password    []byte
	Input       string
	ABasename   string
	BBasename   string
	OutputPath  string
	MaxBytes    int64
	MaxDuration time.Duration
	Revision    string
	Modified    bool
	Ready       func()
}

type runtimeOptions struct {
	now            func() time.Time
	settleWindow   time.Duration
	requestTimeout time.Duration
	maxDuration    time.Duration
	frameLimit     int64
}

func defaultRuntimeOptions(cfg CaptureConfig) runtimeOptions {
	return runtimeOptions{
		now:            time.Now,
		settleWindow:   CorrelationWindow,
		requestTimeout: defaultRequestWait,
		maxDuration:    cfg.MaxDuration,
		frameLimit:     MaxFrameBytes,
	}
}

func Capture(ctx context.Context, cfg CaptureConfig) error {
	return captureWithOptions(ctx, cfg, defaultRuntimeOptions(cfg))
}

func captureWithOptions(
	ctx context.Context,
	cfg CaptureConfig,
	opts runtimeOptions,
) (returnErr error) {
	mapper, err := validateCaptureConfig(cfg)
	if err != nil {
		return err
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.settleWindow <= 0 {
		opts.settleWindow = CorrelationWindow
	}
	if opts.requestTimeout <= 0 {
		opts.requestTimeout = defaultRequestWait
	}
	if opts.maxDuration <= 0 {
		opts.maxDuration = cfg.MaxDuration
	}
	if opts.frameLimit <= 0 || opts.frameLimit > MaxFrameBytes {
		opts.frameLimit = MaxFrameBytes
	}

	start := opts.now()
	trace, err := NewTraceWriter(cfg.OutputPath, start, cfg.MaxBytes)
	if err != nil {
		return fmt.Errorf("%w: trace output", ErrInvalidConfig)
	}

	stopReason := "failure"
	defer func() {
		if closeErr := trace.Close(opts.now(), stopReason); closeErr != nil {
			if returnErr == nil {
				returnErr = closeErr
			} else {
				returnErr = errors.Join(returnErr, closeErr)
			}
		}
	}()

	password := append([]byte(nil), cfg.Password...)
	defer zeroBytes(password)

	conn, hello, err := connectAndIdentify(ctx, cfg.Host, cfg.Port, password, opts.frameLimit)
	zeroBytes(password)
	if err != nil {
		if errors.Is(err, ErrProtocol) || errors.Is(err, ErrOBSVersion) {
			trace.WriteIntegrityFailure(opts.now(), "protocol")
		} else {
			trace.WriteIntegrityFailure(opts.now(), "connection_closed")
		}
		return err
	}
	defer conn.Close()

	if _, err := trace.WriteHeader(opts.now(), Header{
		Revision:            cfg.Revision,
		Modified:            cfg.Modified,
		Input:               cfg.Input,
		ABasename:           cfg.ABasename,
		BBasename:           cfg.BBasename,
		OBSWebSocketVersion: hello.OBSWebSocketVersion,
		RPCVersion:          1,
		MaxBytes:            cfg.MaxBytes,
		MaxDuration:         cfg.MaxDuration,
	}); err != nil {
		trace.WriteIntegrityFailure(opts.now(), "trace_limit")
		return err
	}

	runtime := &monitorRuntime{
		cfg:      cfg,
		opts:     opts,
		mapper:   mapper,
		trace:    trace,
		conn:     conn,
		pending:  make(map[string]pendingSnapshot),
		settleCh: make(chan settleRequest, maxPendingSnapshots),
		done:     make(chan struct{}),
		readCh:   make(chan incomingFrame, 16),
		readDone: make(chan struct{}, 1),
		current:  OtherBasename,
	}
	if err := runtime.sendSnapshot(opts.now(), "initial", 0); err != nil {
		runtime.writeFailure(err)
		return err
	}

	if cfg.Ready != nil {
		cfg.Ready()
	}
	err = runtime.run(ctx)
	if err == nil {
		stopReason = "signal"
		return nil
	}
	runtime.writeFailure(err)
	return err
}

func validateCaptureConfig(cfg CaptureConfig) (basenameMapper, error) {
	ip := net.ParseIP(cfg.Host)
	if ip == nil || !ip.IsLoopback() {
		return basenameMapper{}, fmt.Errorf("%w: host must be a loopback IP literal", ErrInvalidConfig)
	}
	if cfg.Port < 1 || cfg.Port > 65535 {
		return basenameMapper{}, fmt.Errorf("%w: port", ErrInvalidConfig)
	}
	if err := validateSafeName(cfg.Input); err != nil {
		return basenameMapper{}, fmt.Errorf("%w: input", ErrInvalidConfig)
	}
	mapper, err := newBasenameMapper(cfg.ABasename, cfg.BBasename)
	if err != nil {
		return basenameMapper{}, fmt.Errorf("%w: basenames", ErrInvalidConfig)
	}
	if cfg.MaxBytes < MinMaxTraceBytes || cfg.MaxBytes > MaxMaxTraceBytes {
		return basenameMapper{}, fmt.Errorf("%w: max bytes", ErrInvalidConfig)
	}
	if cfg.MaxDuration < MinMaxDuration || cfg.MaxDuration > MaxMaxDuration {
		return basenameMapper{}, fmt.Errorf("%w: max duration", ErrInvalidConfig)
	}
	if len(cfg.Password) > MaxPasswordBytes {
		return basenameMapper{}, fmt.Errorf("%w: password length", ErrInvalidConfig)
	}
	if len(cfg.Password) >= 4 {
		password := string(cfg.Password)
		if strings.Contains(cfg.Input, password) ||
			strings.Contains(cfg.ABasename, password) ||
			strings.Contains(cfg.BBasename, password) {
			return basenameMapper{}, fmt.Errorf("%w: credential-like metadata", ErrInvalidConfig)
		}
	}
	if cfg.OutputPath == "" {
		return basenameMapper{}, fmt.Errorf("%w: output path", ErrInvalidConfig)
	}
	return mapper, nil
}

type protocolEnvelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

type helloData struct {
	OBSWebSocketVersion string              `json:"obsWebSocketVersion"`
	RPCVersion          int                 `json:"rpcVersion"`
	Authentication      *authenticationData `json:"authentication,omitempty"`
}

type authenticationData struct {
	Challenge string `json:"challenge"`
	Salt      string `json:"salt"`
}

type identifyData struct {
	RPCVersion         int    `json:"rpcVersion"`
	Authentication     string `json:"authentication,omitempty"`
	EventSubscriptions int    `json:"eventSubscriptions"`
}

type identifiedData struct {
	NegotiatedRPCVersion int `json:"negotiatedRpcVersion"`
}

type eventPayload struct {
	EventType string          `json:"eventType"`
	EventData json.RawMessage `json:"eventData"`
}

type inputSettingsChangedData struct {
	InputName     string `json:"inputName"`
	InputSettings struct {
		LocalFile string `json:"local_file"`
	} `json:"inputSettings"`
}

type mediaActionData struct {
	InputName   string `json:"inputName"`
	MediaAction string `json:"mediaAction"`
}

type mediaEndedData struct {
	InputName string `json:"inputName"`
}

type requestPayload struct {
	RequestType string         `json:"requestType"`
	RequestID   string         `json:"requestId"`
	RequestData map[string]any `json:"requestData"`
}

type requestResponsePayload struct {
	RequestType   string          `json:"requestType"`
	RequestID     string          `json:"requestId"`
	RequestStatus requestStatus   `json:"requestStatus"`
	ResponseData  json.RawMessage `json:"responseData"`
}

type requestStatus struct {
	Result bool `json:"result"`
	Code   int  `json:"code"`
}

type getInputSettingsResponse struct {
	InputSettings struct {
		LocalFile string `json:"local_file"`
	} `json:"inputSettings"`
}

func connectAndIdentify(
	ctx context.Context,
	host string,
	port int,
	password []byte,
	frameLimit int64,
) (*websocket.Conn, helloData, error) {
	endpoint := url.URL{
		Scheme: "ws",
		Host:   net.JoinHostPort(host, strconv.Itoa(port)),
	}
	dialer := websocket.Dialer{HandshakeTimeout: handshakeTimeout}
	conn, _, err := dialer.DialContext(ctx, endpoint.String(), nil)
	if err != nil {
		return nil, helloData{}, ErrConnectionClosed
	}
	contextCloseDone := make(chan struct{})
	stopContextClose := context.AfterFunc(ctx, func() {
		defer close(contextCloseDone)
		_ = conn.Close()
	})
	defer func() {
		if !stopContextClose() {
			<-contextCloseDone
		}
	}()
	conn.SetReadLimit(frameLimit)
	if err := setHandshakeDeadline(ctx, conn); err != nil {
		_ = conn.Close()
		return nil, helloData{}, ErrConnectionClosed
	}

	messageType, raw, err := conn.ReadMessage()
	if err != nil || messageType != websocket.TextMessage {
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	}
	var envelope protocolEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Op != opHello {
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	}
	var hello helloData
	if err := json.Unmarshal(envelope.D, &hello); err != nil ||
		hello.RPCVersion < 1 {
		_ = conn.Close()
		return nil, helloData{}, ErrOBSVersion
	}
	normalizedVersion, ok := normalizeOBSWebSocketVersion(hello.OBSWebSocketVersion)
	if !ok {
		_ = conn.Close()
		return nil, helloData{}, ErrOBSVersion
	}
	hello.OBSWebSocketVersion = normalizedVersion

	identify := identifyData{
		RPCVersion:         1,
		EventSubscriptions: eventSubscriptions,
	}
	switch {
	case hello.Authentication == nil && len(password) > 0:
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	case hello.Authentication != nil && len(password) == 0:
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	case hello.Authentication != nil:
		identify.Authentication = buildAuthentication(
			password,
			hello.Authentication.Salt,
			hello.Authentication.Challenge,
		)
	}
	if err := conn.WriteJSON(protocolEnvelope{
		Op: opIdentify,
		D:  mustJSON(identify),
	}); err != nil {
		_ = conn.Close()
		return nil, helloData{}, ErrConnectionClosed
	}
	messageType, raw, err = conn.ReadMessage()
	if err != nil || messageType != websocket.TextMessage {
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Op != opIdentified {
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	}
	var identified identifiedData
	if err := json.Unmarshal(envelope.D, &identified); err != nil ||
		identified.NegotiatedRPCVersion != 1 {
		_ = conn.Close()
		return nil, helloData{}, ErrProtocol
	}
	_ = conn.SetReadDeadline(time.Time{})
	_ = conn.SetWriteDeadline(time.Time{})
	return conn, hello, nil
}

func setHandshakeDeadline(ctx context.Context, conn *websocket.Conn) error {
	deadline := time.Now().Add(handshakeTimeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	if err := conn.SetReadDeadline(deadline); err != nil {
		return err
	}
	return conn.SetWriteDeadline(deadline)
}

func buildAuthentication(password []byte, salt, challenge string) string {
	first := sha256.New()
	_, _ = first.Write(password)
	_, _ = first.Write([]byte(salt))
	secret := base64.StdEncoding.EncodeToString(first.Sum(nil))
	second := sha256.Sum256([]byte(secret + challenge))
	return base64.StdEncoding.EncodeToString(second[:])
}

func supportedOBSWebSocketVersion(value string) bool {
	_, ok := normalizeOBSWebSocketVersion(value)
	return ok
}

func normalizeOBSWebSocketVersion(value string) (string, bool) {
	coreValue := value
	if idx := strings.IndexAny(coreValue, "-+"); idx >= 0 {
		coreValue = coreValue[:idx]
	}
	core := strings.Split(coreValue, ".")
	if len(core) != 3 {
		return "", false
	}
	numbers := make([]int, 3)
	for idx, part := range core {
		if part == "" {
			return "", false
		}
		value, err := strconv.Atoi(part)
		if err != nil || value < 0 || value > 999 {
			return "", false
		}
		numbers[idx] = value
	}
	if numbers[0] != 5 {
		if numbers[0] < 6 {
			return "", false
		}
	} else if numbers[1] < 4 {
		return "", false
	}
	return fmt.Sprintf("%d.%d.%d", numbers[0], numbers[1], numbers[2]), true
}

func mustJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return encoded
}

type incomingFrame struct {
	at          time.Time
	messageType int
	payload     []byte
}

type pendingSnapshot struct {
	trigger    string
	relatedSeq uint64
	sentAt     time.Time
	sentNS     int64
}

type settleRequest struct {
	relatedSeq uint64
}

type restartState struct {
	seq      uint64
	at       time.Time
	basename string
}

type monitorRuntime struct {
	cfg    CaptureConfig
	opts   runtimeOptions
	mapper basenameMapper
	trace  *TraceWriter
	conn   *websocket.Conn

	pending        map[string]pendingSnapshot
	nextID         uint64
	current        string
	restart        restartState
	settleCh       chan settleRequest
	done           chan struct{}
	pendingSettles int
	readCh         chan incomingFrame
	readDone       chan struct{}
}

func (m *monitorRuntime) run(ctx context.Context) error {
	defer close(m.done)
	stopClose := context.AfterFunc(ctx, func() {
		_ = m.conn.Close()
	})
	defer stopClose()

	go m.readFrames()
	durationTimer := time.NewTimer(m.opts.maxDuration)
	defer durationTimer.Stop()
	timeoutEvery := m.opts.requestTimeout / 4
	if timeoutEvery <= 0 || timeoutEvery > 250*time.Millisecond {
		timeoutEvery = 250 * time.Millisecond
	}
	timeoutTicker := time.NewTicker(timeoutEvery)
	defer timeoutTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-durationTimer.C:
			return ErrDurationLimit
		case <-m.readDone:
			if ctx.Err() != nil {
				return nil
			}
			return ErrConnectionClosed
		case frame := <-m.readCh:
			if err := m.handleFrame(frame); err != nil {
				return err
			}
		case request := <-m.settleCh:
			if err := m.consumePendingSettle(); err != nil {
				return err
			}
			if err := m.sendSnapshot(m.opts.now(), "ended_settle", request.relatedSeq); err != nil {
				return err
			}
		case at := <-timeoutTicker.C:
			for _, pending := range m.pending {
				if at.Sub(pending.sentAt) >= m.opts.requestTimeout {
					return ErrRequestTimeout
				}
			}
		}
	}
}

func (m *monitorRuntime) readFrames() {
	for {
		messageType, payload, err := m.conn.ReadMessage()
		if err != nil {
			select {
			case m.readDone <- struct{}{}:
			default:
			}
			return
		}
		frame := incomingFrame{
			at:          m.opts.now(),
			messageType: messageType,
			payload:     payload,
		}
		select {
		case m.readCh <- frame:
		default:
			select {
			case m.readDone <- struct{}{}:
			default:
			}
			return
		}
	}
}

func (m *monitorRuntime) handleFrame(frame incomingFrame) error {
	if frame.messageType != websocket.TextMessage {
		return ErrProtocol
	}
	var envelope protocolEnvelope
	if err := json.Unmarshal(frame.payload, &envelope); err != nil {
		return ErrProtocol
	}
	switch envelope.Op {
	case opEvent:
		return m.handleEvent(frame.at, envelope.D)
	case opRequestResponse:
		return m.handleResponse(frame.at, envelope.D)
	default:
		return nil
	}
}

func (m *monitorRuntime) handleEvent(at time.Time, raw json.RawMessage) error {
	var event eventPayload
	if err := json.Unmarshal(raw, &event); err != nil {
		return ErrProtocol
	}
	switch event.EventType {
	case "InputSettingsChanged":
		var data inputSettingsChangedData
		if err := json.Unmarshal(event.EventData, &data); err != nil {
			return ErrProtocol
		}
		if data.InputName != m.cfg.Input {
			return nil
		}
		m.current = m.mapper.mapPath(data.InputSettings.LocalFile)
		record, err := m.trace.Write(at, Record{
			Kind:     "settings_changed",
			Event:    "InputSettingsChanged",
			Input:    m.cfg.Input,
			Basename: m.current,
		})
		if err != nil {
			return err
		}
		return m.sendSnapshot(m.opts.now(), "settings_changed", record.Seq)

	case "MediaInputActionTriggered":
		var data mediaActionData
		if err := json.Unmarshal(event.EventData, &data); err != nil {
			return ErrProtocol
		}
		if data.InputName != m.cfg.Input {
			return nil
		}
		action := safeMediaAction(data.MediaAction)
		record, err := m.trace.Write(at, Record{
			Kind:     "media_action",
			Event:    "MediaInputActionTriggered",
			Input:    m.cfg.Input,
			Action:   action,
			Basename: m.current,
		})
		if err != nil {
			return err
		}
		if action != "restart" {
			return nil
		}
		m.restart = restartState{
			seq:      record.Seq,
			at:       at,
			basename: m.current,
		}
		return m.sendSnapshot(m.opts.now(), "restart", record.Seq)

	case "MediaInputPlaybackEnded":
		var data mediaEndedData
		if err := json.Unmarshal(event.EventData, &data); err != nil {
			return ErrProtocol
		}
		if data.InputName != m.cfg.Input {
			return nil
		}
		var since *int64
		if m.restart.seq != 0 {
			value := at.Sub(m.restart.at)
			if value >= 0 {
				nanos := int64(value)
				since = &nanos
			}
		}
		record, err := m.trace.Write(at, Record{
			Kind:           "playback_ended",
			Event:          "MediaInputPlaybackEnded",
			Input:          m.cfg.Input,
			Basename:       m.current,
			LastRestartSeq: m.restart.seq,
			SinceRestartNS: since,
		})
		if err != nil {
			return err
		}
		if err := m.sendSnapshot(m.opts.now(), "ended_immediate", record.Seq); err != nil {
			return err
		}
		return m.scheduleSettle(record.Seq)
	default:
		return nil
	}
}

func (m *monitorRuntime) scheduleSettle(relatedSeq uint64) error {
	if m.pendingSettles >= maxPendingSnapshots {
		return ErrPendingLimit
	}
	m.pendingSettles++
	time.AfterFunc(m.opts.settleWindow, func() {
		select {
		case m.settleCh <- settleRequest{relatedSeq: relatedSeq}:
		case <-m.done:
		}
	})
	return nil
}

func (m *monitorRuntime) consumePendingSettle() error {
	if m.pendingSettles <= 0 {
		return ErrProtocol
	}
	m.pendingSettles--
	return nil
}

func safeMediaAction(value string) string {
	switch value {
	case "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART":
		return "restart"
	case "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_STOP":
		return "stop"
	default:
		return "other"
	}
}

func (m *monitorRuntime) sendSnapshot(at time.Time, trigger string, relatedSeq uint64) error {
	if len(m.pending) >= maxPendingSnapshots {
		return ErrPendingLimit
	}
	m.nextID++
	requestID := "evidence-" + strconv.FormatUint(m.nextID, 10)
	pending := pendingSnapshot{
		trigger:    trigger,
		relatedSeq: relatedSeq,
		sentAt:     at,
		sentNS:     elapsedSince(m.trace.start, at),
	}
	m.pending[requestID] = pending

	if err := m.conn.SetWriteDeadline(time.Now().Add(defaultRequestWait)); err != nil {
		delete(m.pending, requestID)
		return ErrConnectionClosed
	}
	err := m.conn.WriteJSON(protocolEnvelope{
		Op: opRequest,
		D: mustJSON(requestPayload{
			RequestType: "GetInputSettings",
			RequestID:   requestID,
			RequestData: map[string]any{
				"inputName": m.cfg.Input,
			},
		}),
	})
	_ = m.conn.SetWriteDeadline(time.Time{})
	if err != nil {
		delete(m.pending, requestID)
		return ErrConnectionClosed
	}
	return nil
}

func (m *monitorRuntime) handleResponse(at time.Time, raw json.RawMessage) error {
	var response requestResponsePayload
	if err := json.Unmarshal(raw, &response); err != nil {
		return ErrProtocol
	}
	pending, ok := m.pending[response.RequestID]
	if !ok || response.RequestType != "GetInputSettings" {
		return ErrProtocol
	}
	delete(m.pending, response.RequestID)

	result := response.RequestStatus.Result
	record := Record{
		Kind:                 "settings_snapshot",
		Method:               "GetInputSettings",
		Input:                m.cfg.Input,
		Trigger:              pending.trigger,
		RelatedSeq:           pending.relatedSeq,
		RequestSentElapsedNS: pending.sentNS,
		ResponseElapsedNS:    elapsedSince(m.trace.start, at),
		Result:               &result,
		Code:                 response.RequestStatus.Code,
	}
	if result {
		var data getInputSettingsResponse
		if err := json.Unmarshal(response.ResponseData, &data); err != nil {
			return ErrProtocol
		}
		record.Basename = m.mapper.mapPath(data.InputSettings.LocalFile)
		m.current = record.Basename
		if pending.trigger == "restart" &&
			m.restart.seq == pending.relatedSeq {
			m.restart.basename = record.Basename
		}
	}
	if _, err := m.trace.Write(at, record); err != nil {
		return err
	}
	if !result {
		return ErrProtocol
	}
	return nil
}

func (m *monitorRuntime) writeFailure(err error) {
	switch {
	case errors.Is(err, ErrDurationLimit):
		m.trace.WriteIntegrityFailure(m.opts.now(), "duration_limit")
	case errors.Is(err, ErrRequestTimeout):
		m.trace.WriteIntegrityFailure(m.opts.now(), "request_timeout")
	case errors.Is(err, ErrPendingLimit):
		m.trace.WriteIntegrityFailure(m.opts.now(), "pending_limit")
	case errors.Is(err, ErrTraceLimit):
		m.trace.WriteIntegrityFailure(m.opts.now(), "trace_limit")
	case errors.Is(err, ErrProtocol), errors.Is(err, ErrOBSVersion):
		m.trace.WriteIntegrityFailure(m.opts.now(), "protocol")
	default:
		m.trace.WriteIntegrityFailure(m.opts.now(), "connection_closed")
	}
}

func zeroBytes(value []byte) {
	for idx := range value {
		value[idx] = 0
	}
}
