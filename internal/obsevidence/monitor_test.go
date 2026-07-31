package obsevidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	testPassword  = "obs-password-canary"
	testToken     = "123456789:ABCdefghi_jklmnop_secret"
	testAPIHash   = "deadbeefdeadbeefdeadbeefdeadbeef"
	testUUID      = "private-uuid-canary"
	testChallenge = "challenge-canary"
	testSalt      = "salt-canary"
)

func TestCaptureProducesOnlyAllowlistedCredentialRedactedEvidence(t *testing.T) {
	server, host, port, sequenceDone, serverErr := newMonitorTestServer(
		t,
		"5.7.3-"+testToken,
	)
	defer server.Close()

	output := filepath.Join(t.TempDir(), "trace.jsonl")
	ready := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	captureDone := make(chan error, 1)
	go func() {
		captureDone <- captureWithOptions(ctx, validCaptureConfig(
			host,
			port,
			output,
			func() { close(ready) },
		), runtimeOptions{
			now:            time.Now,
			settleWindow:   20 * time.Millisecond,
			requestTimeout: time.Second,
			maxDuration:    time.Minute,
			frameLimit:     MaxFrameBytes,
		})
	}()
	waitSignal(t, ready, "monitor did not become ready")
	waitSignal(t, sequenceDone, "fake OBS did not complete stale-event sequence")
	waitForFileText(t, output, `"trigger":"ended_settle"`)
	cancel()
	select {
	case err := <-captureDone:
		if err != nil {
			t.Fatalf("captureWithOptions: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("capture did not stop after cancellation")
	}
	select {
	case err := <-serverErr:
		if err != nil {
			t.Fatalf("fake OBS: %v", err)
		}
	default:
	}

	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	text := string(data)
	for _, forbidden := range []string{
		testPassword,
		testToken,
		testAPIHash,
		testUUID,
		testChallenge,
		testSalt,
		"/private/cache",
		"local_file",
		"requestId",
		"authentication",
		"response-comment-canary",
	} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("trace leaked forbidden value %q:\n%s", forbidden, text)
		}
	}
	for _, required := range []string{
		`"subscriptions":264`,
		`"obs_websocket_version":"5.7.3"`,
		`"event":"InputSettingsChanged"`,
		`"event":"MediaInputActionTriggered"`,
		`"event":"MediaInputPlaybackEnded"`,
		`"trigger":"ended_immediate"`,
		`"trigger":"ended_settle"`,
		`"basename":"music_stale-short-a.m4a"`,
		`"basename":"music_stale-long-b.m4a"`,
	} {
		if !strings.Contains(text, required) {
			t.Fatalf("trace missing %s:\n%s", required, text)
		}
	}
	for lineNumber, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if _, _, err := decodeStrictRecord([]byte(line)); err != nil {
			t.Fatalf("trace line %d: %v", lineNumber+1, err)
		}
	}
}

func TestCaptureRejectsOBSWebSocketBefore54WithoutLeakingHandshake(t *testing.T) {
	server, host, port := newHelloOnlyServer(t, "5.3.0")
	defer server.Close()

	output := filepath.Join(t.TempDir(), "trace.jsonl")
	err := Capture(context.Background(), validCaptureConfig(host, port, output, nil))
	if !errors.Is(err, ErrOBSVersion) {
		t.Fatalf("Capture error = %v, want %v", err, ErrOBSVersion)
	}
	data, readErr := os.ReadFile(output)
	if readErr != nil {
		t.Fatalf("ReadFile: %v", readErr)
	}
	for _, forbidden := range []string{
		testPassword, testChallenge, testSalt, testToken,
	} {
		if bytes.Contains(data, []byte(forbidden)) {
			t.Fatalf("failed trace leaked %q", forbidden)
		}
	}
}

func TestAuthenticationVectorAndSupportedVersions(t *testing.T) {
	got := buildAuthentication([]byte("configured-password"), "salt", "challenge")
	const want = "IC+CckPMyp9jOz74iVmxn7TWxInldEvbuJvqFtkHaYU="
	if got != want {
		t.Fatalf("authentication = %q, want %q", got, want)
	}
	for _, version := range []string{
		"5.4.0",
		"5.7.3",
		"5.7.3-" + testToken,
		"6.0.0",
	} {
		if !supportedOBSWebSocketVersion(version) {
			t.Fatalf("version %q unexpectedly rejected", version)
		}
	}
	for _, version := range []string{"", "5.3.9", "4.9.1", "5.4", testToken} {
		if supportedOBSWebSocketVersion(version) {
			t.Fatalf("version %q unexpectedly accepted", version)
		}
	}
	if normalized, ok := normalizeOBSWebSocketVersion("5.7.3-" + testToken); !ok ||
		normalized != "5.7.3" {
		t.Fatalf("normalized version = %q ok=%v", normalized, ok)
	}
}

func TestSettleDeliveryIsBoundedAndNeverSilentlyDropped(t *testing.T) {
	runtime := &monitorRuntime{
		opts:     runtimeOptions{settleWindow: 10 * time.Millisecond},
		settleCh: make(chan settleRequest, 1),
		done:     make(chan struct{}),
	}
	runtime.settleCh <- settleRequest{relatedSeq: 1}
	if err := runtime.scheduleSettle(2); err != nil {
		t.Fatalf("scheduleSettle: %v", err)
	}
	time.Sleep(30 * time.Millisecond)

	first := <-runtime.settleCh
	if first.relatedSeq != 1 {
		t.Fatalf("first related seq = %d", first.relatedSeq)
	}
	select {
	case second := <-runtime.settleCh:
		if second.relatedSeq != 2 {
			t.Fatalf("second related seq = %d", second.relatedSeq)
		}
	case <-time.After(time.Second):
		t.Fatal("settle request was silently dropped when the bounded channel was full")
	}
	if err := runtime.consumePendingSettle(); err != nil {
		t.Fatalf("consumePendingSettle: %v", err)
	}
	if runtime.pendingSettles != 0 {
		t.Fatalf("pending settles = %d, want 0", runtime.pendingSettles)
	}
	close(runtime.done)
}

func TestSettleSchedulingFailsClosedAtConcurrentLimit(t *testing.T) {
	runtime := &monitorRuntime{
		opts:     runtimeOptions{settleWindow: time.Hour},
		settleCh: make(chan settleRequest, maxPendingSnapshots),
		done:     make(chan struct{}),
	}
	for idx := 0; idx < maxPendingSnapshots; idx++ {
		if err := runtime.scheduleSettle(uint64(idx + 1)); err != nil {
			t.Fatalf("schedule %d: %v", idx+1, err)
		}
	}
	if err := runtime.scheduleSettle(99); !errors.Is(err, ErrPendingLimit) {
		t.Fatalf("over-limit error = %v, want %v", err, ErrPendingLimit)
	}
	if runtime.pendingSettles != maxPendingSnapshots {
		t.Fatalf(
			"pending settles = %d, want %d",
			runtime.pendingSettles,
			maxPendingSnapshots,
		)
	}
	close(runtime.done)
}

func TestPendingSettleLimitIsRecordedAsIntegrityFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "trace.jsonl")
	start := time.Now()
	trace, err := NewTraceWriter(path, start, MinMaxTraceBytes)
	if err != nil {
		t.Fatalf("NewTraceWriter: %v", err)
	}
	if _, err := trace.WriteHeader(start, validTestHeader(MinMaxTraceBytes)); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	runtime := &monitorRuntime{
		opts:  runtimeOptions{now: func() time.Time { return start.Add(time.Second) }},
		trace: trace,
	}
	runtime.writeFailure(ErrPendingLimit)
	if err := trace.Close(start.Add(2*time.Second), "failure"); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !bytes.Contains(data, []byte(`"reason":"pending_limit"`)) {
		t.Fatalf("trace missing pending-limit integrity failure:\n%s", data)
	}
	if _, err := VerifyTrace(path); !errors.Is(err, ErrInvalidTrace) {
		t.Fatalf("VerifyTrace error = %v, want %v", err, ErrInvalidTrace)
	}
}

func validCaptureConfig(
	host string,
	port int,
	output string,
	ready func(),
) CaptureConfig {
	return CaptureConfig{
		Host:        host,
		Port:        port,
		Password:    []byte(testPassword),
		Input:       "tg_music_player",
		ABasename:   "music_stale-short-a.m4a",
		BBasename:   "music_stale-long-b.m4a",
		OutputPath:  output,
		MaxBytes:    DefaultMaxTraceBytes,
		MaxDuration: MinMaxDuration,
		Revision:    "0123456789abcdef0123456789abcdef01234567",
		Ready:       ready,
	}
}

func newMonitorTestServer(
	t *testing.T,
	version string,
) (
	*httptest.Server,
	string,
	int,
	<-chan struct{},
	<-chan error,
) {
	t.Helper()
	sequenceDone := make(chan struct{})
	serverErr := make(chan error, 1)
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			serverErr <- errors.New("upgrade")
			return
		}
		defer conn.Close()
		if err := writeServerEnvelope(conn, opHello, map[string]any{
			"obsWebSocketVersion": version,
			"rpcVersion":          1,
			"authentication": map[string]any{
				"challenge": testChallenge,
				"salt":      testSalt,
			},
		}); err != nil {
			serverErr <- err
			return
		}
		var identify protocolEnvelope
		if err := conn.ReadJSON(&identify); err != nil {
			serverErr <- errors.New("read identify")
			return
		}
		if identify.Op != opIdentify {
			serverErr <- errors.New("unexpected identify op")
			return
		}
		var data identifyData
		if err := json.Unmarshal(identify.D, &data); err != nil {
			serverErr <- errors.New("decode identify")
			return
		}
		if data.EventSubscriptions != eventSubscriptions ||
			data.Authentication != buildAuthentication(
				[]byte(testPassword),
				testSalt,
				testChallenge,
			) {
			serverErr <- errors.New("unsafe or incorrect identify")
			return
		}
		if err := writeServerEnvelope(conn, opIdentified, map[string]any{
			"negotiatedRpcVersion": 1,
		}); err != nil {
			serverErr <- err
			return
		}

		aPath := "/private/cache/" + testToken + "/music_stale-short-a.m4a"
		bPath := "/private/cache/" + testAPIHash + "/music_stale-long-b.m4a"
		if err := respondToSettingsRequest(conn, aPath); err != nil {
			serverErr <- err
			return
		}
		if err := writeServerEvent(conn, "UnrelatedEvent", map[string]any{
			"inputName": testToken,
			"inputUuid": testUUID,
		}); err != nil {
			serverErr <- err
			return
		}
		if err := writeServerEvent(conn, "InputSettingsChanged", map[string]any{
			"inputName": "tg_music_player",
			"inputUuid": testUUID,
			"inputSettings": map[string]any{
				"local_file": aPath,
			},
		}); err != nil {
			serverErr <- err
			return
		}
		if err := respondToSettingsRequest(conn, aPath); err != nil {
			serverErr <- err
			return
		}
		if err := writeServerEvent(conn, "MediaInputActionTriggered", map[string]any{
			"inputName":   "tg_music_player",
			"inputUuid":   testUUID,
			"mediaAction": "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART",
		}); err != nil {
			serverErr <- err
			return
		}
		if err := respondToSettingsRequest(conn, aPath); err != nil {
			serverErr <- err
			return
		}
		if err := writeServerEvent(conn, "InputSettingsChanged", map[string]any{
			"inputName": "tg_music_player",
			"inputUuid": testUUID,
			"inputSettings": map[string]any{
				"local_file": bPath,
			},
		}); err != nil {
			serverErr <- err
			return
		}
		if err := respondToSettingsRequest(conn, bPath); err != nil {
			serverErr <- err
			return
		}
		if err := writeServerEvent(conn, "MediaInputActionTriggered", map[string]any{
			"inputName":   "tg_music_player",
			"inputUuid":   testUUID,
			"mediaAction": "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART",
		}); err != nil {
			serverErr <- err
			return
		}
		if err := respondToSettingsRequest(conn, bPath); err != nil {
			serverErr <- err
			return
		}
		if err := writeServerEvent(conn, "MediaInputPlaybackEnded", map[string]any{
			"inputName": "tg_music_player",
			"inputUuid": testUUID,
		}); err != nil {
			serverErr <- err
			return
		}
		if err := respondToSettingsRequest(conn, bPath); err != nil {
			serverErr <- err
			return
		}
		if err := respondToSettingsRequest(conn, bPath); err != nil {
			serverErr <- err
			return
		}
		close(sequenceDone)
		_, _, _ = conn.ReadMessage()
		serverErr <- nil
	})
	server := httptest.NewServer(handler)
	host, port := testServerHostPort(t, server.URL)
	return server, host, port, sequenceDone, serverErr
}

func newHelloOnlyServer(t *testing.T, version string) (*httptest.Server, string, int) {
	t.Helper()
	handler := http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
		conn, err := upgrader.Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = writeServerEnvelope(conn, opHello, map[string]any{
			"obsWebSocketVersion": version,
			"rpcVersion":          1,
			"authentication": map[string]any{
				"challenge": testChallenge,
				"salt":      testSalt,
				"ignored":   testToken,
			},
		})
		_, _, _ = conn.ReadMessage()
	})
	server := httptest.NewServer(handler)
	host, port := testServerHostPort(t, server.URL)
	return server, host, port
}

func testServerHostPort(t *testing.T, rawURL string) (string, int) {
	t.Helper()
	parsed, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("Parse server URL: %v", err)
	}
	host, rawPort, err := net.SplitHostPort(parsed.Host)
	if err != nil {
		t.Fatalf("SplitHostPort: %v", err)
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		t.Fatalf("Atoi port: %v", err)
	}
	return host, port
}

func writeServerEnvelope(conn *websocket.Conn, op int, data any) error {
	return conn.WriteJSON(protocolEnvelope{Op: op, D: mustJSON(data)})
}

func writeServerEvent(conn *websocket.Conn, eventType string, eventData any) error {
	return writeServerEnvelope(conn, opEvent, map[string]any{
		"eventType":   eventType,
		"eventIntent": eventSubscriptions,
		"eventData":   eventData,
	})
}

func respondToSettingsRequest(conn *websocket.Conn, path string) error {
	var envelope protocolEnvelope
	if err := conn.ReadJSON(&envelope); err != nil {
		return errors.New("read settings request")
	}
	if envelope.Op != opRequest {
		return errors.New("unexpected settings request op")
	}
	var request requestPayload
	if err := json.Unmarshal(envelope.D, &request); err != nil {
		return errors.New("decode settings request")
	}
	if request.RequestType != "GetInputSettings" ||
		request.RequestData["inputName"] != "tg_music_player" {
		return errors.New("unexpected settings request")
	}
	return writeServerEnvelope(conn, opRequestResponse, map[string]any{
		"requestType": request.RequestType,
		"requestId":   request.RequestID,
		"requestStatus": map[string]any{
			"result":  true,
			"code":    100,
			"comment": "response-comment-canary " + testPassword + " " + testToken,
		},
		"responseData": map[string]any{
			"inputSettings": map[string]any{
				"local_file": path,
				"private":    testAPIHash,
			},
			"inputKind": "ffmpeg_source",
		},
	})
}

func waitSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func waitForFileText(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && bytes.Contains(data, []byte(want)) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, _ := os.ReadFile(path)
	t.Fatalf("trace never contained %q:\n%s", want, data)
}
