package obs

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestBuildIdentifyAllowsEmptyPasswordWhenAuthenticationDisabled(t *testing.T) {
	identify, err := buildIdentify(helloData{RPCVersion: 1}, "")
	if err != nil {
		t.Fatalf("build identify without OBS password: %v", err)
	}
	if identify.RPCVersion != 1 {
		t.Fatalf("rpc version = %d, want 1", identify.RPCVersion)
	}
	if identify.Authentication != "" {
		t.Fatalf("expected empty authentication when OBS auth is disabled, got %q", identify.Authentication)
	}
}

func TestBuildIdentifyFailsWhenAuthenticationRequiresEmptyPassword(t *testing.T) {
	_, err := buildIdentify(helloData{
		RPCVersion: 1,
		Authentication: &authenticationData{
			Challenge: "challenge",
			Salt:      "salt",
		},
	}, "")
	if err == nil {
		t.Fatal("expected identify build to fail when OBS requires auth and password is empty")
	}
	if !strings.Contains(err.Error(), "OBS authentication required but password is empty") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestClientConnectHandshakeSuccess(t *testing.T) {
	identified := make(chan struct{}, 1)
	server := newOBSTestServer(t, func(conn *websocket.Conn) error {
		if err := readIdentify(conn); err != nil {
			return err
		}
		if err := writeEnvelope(conn, envelope{Op: opIdentified}); err != nil {
			return err
		}
		identified <- struct{}{}
		return nil
	})
	defer server.Close()

	client := newTestClient(t, server.URL, 100*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error: %v", err)
		}
	}()

	select {
	case <-identified:
	case <-time.After(time.Second):
		t.Fatal("server did not receive identify")
	}
	if got := client.Status().State; got != StateConnected {
		t.Fatalf("state = %s, want %s", got, StateConnected)
	}
}

func TestClientPlayFileSendsMediaRequestsAndCentersSource(t *testing.T) {
	requests := make(chan requestDataPayload, 6)
	server := newOBSTestServer(t, func(conn *websocket.Conn) error {
		if err := readIdentify(conn); err != nil {
			return err
		}
		if err := writeEnvelope(conn, envelope{Op: opIdentified}); err != nil {
			return err
		}
		for i := 0; i < 6; i++ {
			req, err := readRequest(conn)
			if err != nil {
				return err
			}
			requests <- req
			if err := writeEnvelope(conn, successfulRequestResponse(req)); err != nil {
				return err
			}
		}
		return nil
	})
	defer server.Close()

	client := newTestClient(t, server.URL, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error: %v", err)
		}
	}()

	if err := client.PlayFile(context.Background(), "/tmp/media.mp4"); err != nil {
		t.Fatalf("PlayFile: %v", err)
	}

	first := receiveRequest(t, requests)
	if first.RequestType != "SetInputSettings" {
		t.Fatalf("first request type = %q, want SetInputSettings", first.RequestType)
	}
	if got := first.RequestData["inputName"]; got != "media" {
		t.Fatalf("SetInputSettings inputName = %v, want media", got)
	}
	settings, ok := first.RequestData["inputSettings"].(map[string]any)
	if !ok {
		t.Fatalf("SetInputSettings inputSettings = %T, want map[string]any", first.RequestData["inputSettings"])
	}
	if got := settings["local_file"]; got != "/tmp/media.mp4" {
		t.Fatalf("local_file = %v, want /tmp/media.mp4", got)
	}

	second := receiveRequest(t, requests)
	if second.RequestType != "GetCurrentProgramScene" {
		t.Fatalf("second request type = %q, want GetCurrentProgramScene", second.RequestType)
	}

	third := receiveRequest(t, requests)
	if third.RequestType != "GetVideoSettings" {
		t.Fatalf("third request type = %q, want GetVideoSettings", third.RequestType)
	}

	fourth := receiveRequest(t, requests)
	if fourth.RequestType != "GetSceneItemId" {
		t.Fatalf("fourth request type = %q, want GetSceneItemId", fourth.RequestType)
	}
	if got := fourth.RequestData["sceneName"]; got != "Main" {
		t.Fatalf("GetSceneItemId sceneName = %v, want Main", got)
	}
	if got := fourth.RequestData["sourceName"]; got != "media" {
		t.Fatalf("GetSceneItemId sourceName = %v, want media", got)
	}

	fifth := receiveRequest(t, requests)
	if fifth.RequestType != "SetSceneItemTransform" {
		t.Fatalf("fifth request type = %q, want SetSceneItemTransform", fifth.RequestType)
	}
	if got := fifth.RequestData["sceneName"]; got != "Main" {
		t.Fatalf("SetSceneItemTransform sceneName = %v, want Main", got)
	}
	if got := fifth.RequestData["sceneItemId"]; got != float64(42) {
		t.Fatalf("SetSceneItemTransform sceneItemId = %v, want 42", got)
	}
	transform, ok := fifth.RequestData["sceneItemTransform"].(map[string]any)
	if !ok {
		t.Fatalf("sceneItemTransform = %T, want map[string]any", fifth.RequestData["sceneItemTransform"])
	}
	if got := transform["alignment"]; got != float64(0) {
		t.Fatalf("alignment = %v, want 0", got)
	}
	if got := transform["positionX"]; got != float64(960) {
		t.Fatalf("positionX = %v, want 960", got)
	}
	if got := transform["positionY"]; got != float64(540) {
		t.Fatalf("positionY = %v, want 540", got)
	}
	for _, forbidden := range []string{"scaleX", "scaleY", "boundsType", "boundsWidth", "boundsHeight"} {
		if _, ok := transform[forbidden]; ok {
			t.Fatalf("sceneItemTransform should not set %s: %#v", forbidden, transform)
		}
	}

	sixth := receiveRequest(t, requests)
	if sixth.RequestType != "TriggerMediaInputAction" {
		t.Fatalf("sixth request type = %q, want TriggerMediaInputAction", sixth.RequestType)
	}
	if got := sixth.RequestData["inputName"]; got != "media" {
		t.Fatalf("TriggerMediaInputAction inputName = %v, want media", got)
	}
	if got := sixth.RequestData["mediaAction"]; got != mediaActionRestart {
		t.Fatalf("mediaAction = %v, want %s", got, mediaActionRestart)
	}
}

func TestClientPlayFileRestartsWhenCenteringFails(t *testing.T) {
	requests := make(chan requestDataPayload, 3)
	server := newOBSTestServer(t, func(conn *websocket.Conn) error {
		if err := readIdentify(conn); err != nil {
			return err
		}
		if err := writeEnvelope(conn, envelope{Op: opIdentified}); err != nil {
			return err
		}
		for i := 0; i < 3; i++ {
			req, err := readRequest(conn)
			if err != nil {
				return err
			}
			requests <- req
			if req.RequestType == "GetCurrentProgramScene" {
				if err := writeEnvelope(conn, failedRequestResponse(req, "no scene", 608)); err != nil {
					return err
				}
				continue
			}
			if err := writeEnvelope(conn, successfulRequestResponse(req)); err != nil {
				return err
			}
		}
		return nil
	})
	defer server.Close()

	client := newTestClient(t, server.URL, time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error: %v", err)
		}
	}()

	if err := client.PlayFile(context.Background(), "/tmp/media.mp4"); err != nil {
		t.Fatalf("PlayFile should ignore centering failure: %v", err)
	}

	if first := receiveRequest(t, requests); first.RequestType != "SetInputSettings" {
		t.Fatalf("first request type = %q, want SetInputSettings", first.RequestType)
	}
	if second := receiveRequest(t, requests); second.RequestType != "GetCurrentProgramScene" {
		t.Fatalf("second request type = %q, want GetCurrentProgramScene", second.RequestType)
	}
	third := receiveRequest(t, requests)
	if third.RequestType != "TriggerMediaInputAction" {
		t.Fatalf("third request type = %q, want TriggerMediaInputAction", third.RequestType)
	}
	if got := third.RequestData["mediaAction"]; got != mediaActionRestart {
		t.Fatalf("mediaAction = %v, want %s", got, mediaActionRestart)
	}
}

func TestClientRequestTimeoutClearsPendingAndDisconnects(t *testing.T) {
	requestReceived := make(chan struct{}, 1)
	server := newOBSTestServer(t, func(conn *websocket.Conn) error {
		if err := readIdentify(conn); err != nil {
			return err
		}
		if err := writeEnvelope(conn, envelope{Op: opIdentified}); err != nil {
			return err
		}
		if _, err := readRequest(conn); err != nil {
			return err
		}
		requestReceived <- struct{}{}
		var msg envelope
		_ = conn.ReadJSON(&msg)
		return nil
	})
	defer server.Close()

	client := newTestClient(t, server.URL, 20*time.Millisecond)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := client.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error: %v", err)
		}
	}()

	err := client.request(context.Background(), "GetVersion", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request error = %v, want context deadline exceeded", err)
	}
	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("server did not receive timed-out request")
	}

	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}
	if got := client.Status().State; got != StateDisconnected {
		t.Fatalf("state = %s, want %s", got, StateDisconnected)
	}
}

func TestClientRequestCallerCancelClearsPendingWithoutDisconnect(t *testing.T) {
	requestReceived := make(chan struct{}, 1)
	releaseResponse := make(chan struct{})
	responseWritten := make(chan struct{})
	server := newOBSTestServer(t, func(conn *websocket.Conn) error {
		if err := readIdentify(conn); err != nil {
			return err
		}
		if err := writeEnvelope(conn, envelope{Op: opIdentified}); err != nil {
			return err
		}
		req, err := readRequest(conn)
		if err != nil {
			return err
		}
		requestReceived <- struct{}{}
		<-releaseResponse
		err = writeEnvelope(conn, envelope{Op: opRequestResponse, D: mustMarshal(requestResponseData{
			RequestType: req.RequestType,
			RequestID:   req.RequestID,
			RequestStatus: requestStatus{
				Result: true,
			},
		})})
		close(responseWritten)
		return err
	})
	defer server.Close()

	client := newTestClient(t, server.URL, time.Second)
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), time.Second)
	defer cancelConnect()
	if err := client.Connect(connectCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error: %v", err)
		}
	}()

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.request(requestCtx, "GetVersion", nil)
	}()

	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
	cancelRequest()

	if err := receiveError(t, errCh); !errors.Is(err, context.Canceled) {
		t.Fatalf("request error = %v, want context canceled", err)
	}
	assertNoPending(t, client)
	if got := client.Status().State; got != StateConnected {
		t.Fatalf("state = %s, want %s", got, StateConnected)
	}
	close(releaseResponse)
	waitForSignal(t, responseWritten, "server did not write delayed response")
}

func TestClientRequestCallerDeadlineClearsPendingWithoutDisconnect(t *testing.T) {
	requestReceived := make(chan struct{}, 1)
	releaseResponse := make(chan struct{})
	responseWritten := make(chan struct{})
	server := newOBSTestServer(t, func(conn *websocket.Conn) error {
		if err := readIdentify(conn); err != nil {
			return err
		}
		if err := writeEnvelope(conn, envelope{Op: opIdentified}); err != nil {
			return err
		}
		req, err := readRequest(conn)
		if err != nil {
			return err
		}
		requestReceived <- struct{}{}
		<-releaseResponse
		err = writeEnvelope(conn, envelope{Op: opRequestResponse, D: mustMarshal(requestResponseData{
			RequestType: req.RequestType,
			RequestID:   req.RequestID,
			RequestStatus: requestStatus{
				Result: true,
			},
		})})
		close(responseWritten)
		return err
	})
	defer server.Close()

	client := newTestClient(t, server.URL, time.Second)
	connectCtx, cancelConnect := context.WithTimeout(context.Background(), time.Second)
	defer cancelConnect()
	if err := client.Connect(connectCtx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		if err := client.Close(); err != nil {
			t.Fatalf("client.Close() error: %v", err)
		}
	}()

	requestCtx := newControlledDeadlineContext(time.Now().Add(time.Hour))
	errCh := make(chan error, 1)
	go func() {
		errCh <- client.request(requestCtx, "GetVersion", nil)
	}()

	select {
	case <-requestReceived:
	case <-time.After(time.Second):
		t.Fatal("server did not receive request")
	}
	requestCtx.expire()

	if err := receiveError(t, errCh); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("request error = %v, want context deadline exceeded", err)
	}
	assertNoPending(t, client)
	if got := client.Status().State; got != StateConnected {
		t.Fatalf("state = %s, want %s", got, StateConnected)
	}
	close(releaseResponse)
	waitForSignal(t, responseWritten, "server did not write delayed response")
}

type obsTestServer struct {
	URL   string
	Close func()
}

func newOBSTestServer(t *testing.T, handle func(*websocket.Conn) error) obsTestServer {
	t.Helper()
	upgrader := websocket.Upgrader{}
	errCh := make(chan error, 8)
	var wg sync.WaitGroup
	var connsMu sync.Mutex
	conns := make(map[*websocket.Conn]struct{})
	reportErr := func(err error) {
		if err == nil {
			return
		}
		select {
		case errCh <- err:
		default:
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			reportErr(fmt.Errorf("upgrade websocket: %w", err))
			return
		}
		wg.Add(1)
		connsMu.Lock()
		conns[conn] = struct{}{}
		connsMu.Unlock()
		defer func() {
			connsMu.Lock()
			delete(conns, conn)
			connsMu.Unlock()
			_ = conn.Close()
			wg.Done()
		}()
		if err := writeEnvelope(conn, envelope{Op: opHello, D: mustMarshal(helloData{RPCVersion: 1})}); err != nil {
			reportErr(err)
			return
		}
		reportErr(handle(conn))
	}))
	return obsTestServer{
		URL: "ws" + strings.TrimPrefix(server.URL, "http"),
		Close: func() {
			connsMu.Lock()
			for conn := range conns {
				_ = conn.Close()
			}
			connsMu.Unlock()
			server.Close()
			wg.Wait()
			close(errCh)
			for err := range errCh {
				t.Errorf("OBS test server: %v", err)
			}
		},
	}
}

func newTestClient(t *testing.T, url string, requestTimeout time.Duration) *Client {
	t.Helper()
	client, err := NewClient(Options{
		URL:             url,
		MediaSourceName: "media",
		RequestTimeout:  requestTimeout,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

func readIdentify(conn *websocket.Conn) error {
	var msg envelope
	if err := conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("read identify: %w", err)
	}
	if msg.Op != opIdentify {
		return fmt.Errorf("identify op = %d, want %d", msg.Op, opIdentify)
	}
	return nil
}

func readRequest(conn *websocket.Conn) (requestDataPayload, error) {
	var msg envelope
	if err := conn.ReadJSON(&msg); err != nil {
		return requestDataPayload{}, fmt.Errorf("read request: %w", err)
	}
	if msg.Op != opRequest {
		return requestDataPayload{}, fmt.Errorf("request op = %d, want %d", msg.Op, opRequest)
	}
	var req requestDataPayload
	if err := json.Unmarshal(msg.D, &req); err != nil {
		return requestDataPayload{}, fmt.Errorf("decode request: %w", err)
	}
	return req, nil
}

func successfulRequestResponse(req requestDataPayload) envelope {
	response := requestResponseData{
		RequestType: req.RequestType,
		RequestID:   req.RequestID,
		RequestStatus: requestStatus{
			Result: true,
		},
	}
	switch req.RequestType {
	case "GetCurrentProgramScene":
		response.ResponseData = mustMarshal(map[string]any{"sceneName": "Main"})
	case "GetVideoSettings":
		response.ResponseData = mustMarshal(map[string]any{"baseWidth": 1920, "baseHeight": 1080})
	case "GetSceneItemId":
		response.ResponseData = mustMarshal(map[string]any{"sceneItemId": 42})
	}
	return envelope{Op: opRequestResponse, D: mustMarshal(response)}
}

func failedRequestResponse(req requestDataPayload, comment string, code int) envelope {
	return envelope{Op: opRequestResponse, D: mustMarshal(requestResponseData{
		RequestType: req.RequestType,
		RequestID:   req.RequestID,
		RequestStatus: requestStatus{
			Result:  false,
			Code:    code,
			Comment: comment,
		},
	})}
}

func receiveRequest(t *testing.T, requests <-chan requestDataPayload) requestDataPayload {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request")
		return requestDataPayload{}
	}
}

func receiveError(t *testing.T, errCh <-chan error) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for request error")
		return nil
	}
}

func waitForSignal(t *testing.T, signal <-chan struct{}, timeoutMessage string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(timeoutMessage)
	}
}

func assertNoPending(t *testing.T, client *Client) {
	t.Helper()
	client.mu.Lock()
	pending := len(client.pending)
	client.mu.Unlock()
	if pending != 0 {
		t.Fatalf("pending request count = %d, want 0", pending)
	}
}

type controlledDeadlineContext struct {
	done     chan struct{}
	deadline time.Time
	once     sync.Once
}

func newControlledDeadlineContext(deadline time.Time) *controlledDeadlineContext {
	return &controlledDeadlineContext{
		done:     make(chan struct{}),
		deadline: deadline,
	}
}

func (c *controlledDeadlineContext) Deadline() (time.Time, bool) {
	return c.deadline, true
}

func (c *controlledDeadlineContext) Done() <-chan struct{} {
	return c.done
}

func (c *controlledDeadlineContext) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}

func (c *controlledDeadlineContext) Value(key any) any {
	return nil
}

func (c *controlledDeadlineContext) expire() {
	c.once.Do(func() {
		close(c.done)
	})
}

func writeEnvelope(conn *websocket.Conn, msg envelope) error {
	if err := conn.WriteJSON(msg); err != nil {
		return fmt.Errorf("write websocket message: %w", err)
	}
	return nil
}
