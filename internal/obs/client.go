package obs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

const (
	opHello           = 0
	opIdentify        = 1
	opIdentified      = 2
	opEvent           = 5
	opRequest         = 6
	opRequestResponse = 7

	defaultRequestTimeout = 10 * time.Second

	mediaActionRestart = "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_RESTART"
	mediaActionStop    = "OBS_WEBSOCKET_MEDIA_INPUT_ACTION_STOP"
)

var (
	ErrAlreadyConnected = errors.New("obs client already connected")
	ErrNotConnected     = errors.New("obs client is not connected")
	ErrClosed           = errors.New("obs client is closed")
)

type State string

const (
	StateDisconnected State = "disconnected"
	StateConnecting   State = "connecting"
	StateConnected    State = "connected"
	StateClosed       State = "closed"
)

type Options struct {
	URL             string
	Password        string
	MediaSourceName string
	EventBuffer     int
	RequestTimeout  time.Duration
	Logger          *slog.Logger
	Dialer          *websocket.Dialer
}

type Status struct {
	State       State
	CurrentFile string
	LastError   string
	ConnectedAt time.Time
}

type EventType string

const EventMediaEnded EventType = "media_ended"

type Event struct {
	Type      EventType
	InputName string
	Path      string
	At        time.Time
}

type Client struct {
	opts Options

	mu          sync.Mutex
	state       State
	conn        *websocket.Conn
	connectedAt time.Time
	currentFile string
	lastErr     error
	pending     map[string]chan requestResponseData
	closed      bool

	writeMu sync.Mutex
	nextID  atomic.Uint64
	events  chan Event
}

func NewClient(opts Options) (*Client, error) {
	if opts.URL == "" {
		return nil, errors.New("obs URL is required")
	}
	if opts.MediaSourceName == "" {
		return nil, errors.New("OBS media source name is required")
	}
	if opts.EventBuffer <= 0 {
		opts.EventBuffer = 8
	}
	if opts.RequestTimeout <= 0 {
		opts.RequestTimeout = defaultRequestTimeout
	}
	if opts.Dialer == nil {
		opts.Dialer = websocket.DefaultDialer
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Client{
		opts:    opts,
		state:   StateDisconnected,
		pending: make(map[string]chan requestResponseData),
		events:  make(chan Event, opts.EventBuffer),
	}, nil
}

func (c *Client) Connect(ctx context.Context) error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return ErrClosed
	}
	if c.conn != nil || c.state == StateConnecting {
		c.mu.Unlock()
		return ErrAlreadyConnected
	}
	c.state = StateConnecting
	c.lastErr = nil
	c.mu.Unlock()

	conn, _, err := c.opts.Dialer.DialContext(ctx, c.opts.URL, http.Header{})
	if err != nil {
		c.setDisconnected(err)
		return fmt.Errorf("connect to OBS: %w", err)
	}
	conn.SetReadLimit(1 << 20)

	if err := c.handshake(ctx, conn); err != nil {
		_ = conn.Close()
		c.setDisconnected(err)
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return ErrClosed
	}
	c.conn = conn
	c.state = StateConnected
	c.connectedAt = time.Now().UTC()
	c.lastErr = nil
	c.mu.Unlock()

	go c.readLoop(conn)
	return nil
}

func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	c.state = StateClosed
	conn := c.conn
	c.conn = nil
	c.failPendingLocked(ErrClosed)
	close(c.events)
	c.mu.Unlock()

	if conn != nil {
		return conn.Close()
	}
	return nil
}

func (c *Client) Events() <-chan Event {
	return c.events
}

func (c *Client) Status() Status {
	c.mu.Lock()
	defer c.mu.Unlock()

	status := Status{
		State:       c.state,
		CurrentFile: c.currentFile,
		ConnectedAt: c.connectedAt,
	}
	if c.lastErr != nil {
		status.LastError = c.lastErr.Error()
	}
	return status
}

func (c *Client) PlayFile(ctx context.Context, path string) error {
	if path == "" {
		return errors.New("media file path is required")
	}
	if err := c.request(ctx, "SetInputSettings", map[string]any{
		"inputName": c.opts.MediaSourceName,
		"inputSettings": map[string]any{
			"local_file": path,
		},
		"overlay": true,
	}); err != nil {
		return err
	}
	if err := c.centerCurrentProgramSceneItem(ctx); err != nil {
		c.opts.Logger.Warn("center OBS media source failed", "source", c.opts.MediaSourceName, "error", err)
	}
	if err := c.request(ctx, "TriggerMediaInputAction", map[string]any{
		"inputName":   c.opts.MediaSourceName,
		"mediaAction": mediaActionRestart,
	}); err != nil {
		return err
	}

	c.mu.Lock()
	c.currentFile = path
	c.mu.Unlock()
	return nil
}

func (c *Client) centerCurrentProgramSceneItem(ctx context.Context) error {
	var scene currentProgramSceneResponse
	if err := c.requestInto(ctx, "GetCurrentProgramScene", nil, &scene); err != nil {
		return err
	}
	sceneName := scene.SceneName
	if sceneName == "" {
		sceneName = scene.CurrentProgramSceneName
	}
	if sceneName == "" {
		return errors.New("OBS current program scene name is empty")
	}

	var video videoSettingsResponse
	if err := c.requestInto(ctx, "GetVideoSettings", nil, &video); err != nil {
		return err
	}
	if video.BaseWidth <= 0 || video.BaseHeight <= 0 {
		return fmt.Errorf("OBS base canvas size is invalid: %.0fx%.0f", video.BaseWidth, video.BaseHeight)
	}

	var item sceneItemIDResponse
	if err := c.requestInto(ctx, "GetSceneItemId", map[string]any{
		"sceneName":  sceneName,
		"sourceName": c.opts.MediaSourceName,
	}, &item); err != nil {
		return err
	}

	return c.request(ctx, "SetSceneItemTransform", map[string]any{
		"sceneName":   sceneName,
		"sceneItemId": item.SceneItemID,
		"sceneItemTransform": map[string]any{
			"alignment": 0,
			"positionX": video.BaseWidth / 2,
			"positionY": video.BaseHeight / 2,
		},
	})
}

func (c *Client) StopCurrent(ctx context.Context) error {
	if err := c.request(ctx, "TriggerMediaInputAction", map[string]any{
		"inputName":   c.opts.MediaSourceName,
		"mediaAction": mediaActionStop,
	}); err != nil {
		return err
	}

	c.mu.Lock()
	c.currentFile = ""
	c.mu.Unlock()
	return nil
}

func (c *Client) handshake(ctx context.Context, conn *websocket.Conn) error {
	var hello envelope
	if err := readJSON(ctx, conn, &hello); err != nil {
		return fmt.Errorf("read OBS hello: %w", err)
	}
	if hello.Op != opHello {
		return fmt.Errorf("expected OBS hello op %d, got %d", opHello, hello.Op)
	}

	var data helloData
	if err := json.Unmarshal(hello.D, &data); err != nil {
		return fmt.Errorf("decode OBS hello: %w", err)
	}

	identify, err := buildIdentify(data, c.opts.Password)
	if err != nil {
		return err
	}

	if err := writeJSON(ctx, conn, envelope{Op: opIdentify, D: mustMarshal(identify)}); err != nil {
		return fmt.Errorf("send OBS identify: %w", err)
	}

	var identified envelope
	if err := readJSON(ctx, conn, &identified); err != nil {
		return fmt.Errorf("read OBS identified: %w", err)
	}
	if identified.Op != opIdentified {
		return fmt.Errorf("expected OBS identified op %d, got %d", opIdentified, identified.Op)
	}
	return nil
}

func (c *Client) request(ctx context.Context, requestType string, requestData map[string]any) error {
	_, err := c.requestResponse(ctx, requestType, requestData)
	return err
}

func (c *Client) requestInto(ctx context.Context, requestType string, requestData map[string]any, out any) error {
	response, err := c.requestResponse(ctx, requestType, requestData)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if len(response.ResponseData) == 0 {
		return fmt.Errorf("OBS %s response data is empty", requestType)
	}
	if err := json.Unmarshal(response.ResponseData, out); err != nil {
		return fmt.Errorf("decode OBS %s response data: %w", requestType, err)
	}
	return nil
}

func (c *Client) requestResponse(ctx context.Context, requestType string, requestData map[string]any) (requestResponseData, error) {
	conn, err := c.connectedConn()
	if err != nil {
		return requestResponseData{}, err
	}
	timeoutCtx, cancelTimeout := context.WithTimeout(context.Background(), c.opts.RequestTimeout)
	defer cancelTimeout()

	id := fmt.Sprintf("%d", c.nextID.Add(1))
	responseCh := make(chan requestResponseData, 1)

	c.mu.Lock()
	if c.conn != conn || c.state != StateConnected {
		c.mu.Unlock()
		return requestResponseData{}, ErrNotConnected
	}
	c.pending[id] = responseCh
	c.mu.Unlock()

	cleanup := true
	defer func() {
		if cleanup {
			c.mu.Lock()
			delete(c.pending, id)
			c.mu.Unlock()
		}
	}()

	select {
	case <-ctx.Done():
		return requestResponseData{}, ctx.Err()
	default:
	}

	err = c.write(ctx, timeoutCtx, conn, envelope{Op: opRequest, D: mustMarshal(requestDataPayload{
		RequestType: requestType,
		RequestID:   id,
		RequestData: requestData,
	})})
	if err != nil {
		c.disconnectIfCurrent(conn, err)
		return requestResponseData{}, err
	}

	select {
	case <-ctx.Done():
		err := ctx.Err()
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		cleanup = false
		return requestResponseData{}, err
	case <-timeoutCtx.Done():
		err := timeoutCtx.Err()
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		cleanup = false
		c.disconnectIfCurrent(conn, err)
		return requestResponseData{}, err
	case response := <-responseCh:
		cleanup = false
		if !response.RequestStatus.Result {
			return requestResponseData{}, fmt.Errorf("OBS %s failed: %s (%d)", requestType, response.RequestStatus.Comment, response.RequestStatus.Code)
		}
		return response, nil
	}
}

func (c *Client) connectedConn() (*websocket.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, ErrClosed
	}
	if c.conn == nil || c.state != StateConnected {
		return nil, ErrNotConnected
	}
	return c.conn, nil
}

func (c *Client) write(ctx context.Context, timeoutCtx context.Context, conn *websocket.Conn, value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return writeJSONWithTimeout(ctx, timeoutCtx, conn, value)
}

func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		var msg envelope
		if err := conn.ReadJSON(&msg); err != nil {
			c.disconnectIfCurrent(conn, err)
			return
		}

		switch msg.Op {
		case opEvent:
			c.handleEvent(msg.D)
		case opRequestResponse:
			c.handleRequestResponse(msg.D)
		}
	}
}

func (c *Client) handleEvent(raw json.RawMessage) {
	var data eventData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.opts.Logger.Warn("decode OBS event", "error", err)
		return
	}
	if data.EventType != "MediaInputPlaybackEnded" || data.EventData.InputName != c.opts.MediaSourceName {
		return
	}

	event := Event{
		Type:      EventMediaEnded,
		InputName: data.EventData.InputName,
		At:        time.Now().UTC(),
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	event.Path = c.currentFile
	c.currentFile = ""
	select {
	case c.events <- event:
	default:
		c.opts.Logger.Warn("drop OBS event because event channel is full", "event", event.Type)
	}
	c.mu.Unlock()
}

func (c *Client) handleRequestResponse(raw json.RawMessage) {
	var data requestResponseData
	if err := json.Unmarshal(raw, &data); err != nil {
		c.opts.Logger.Warn("decode OBS request response", "error", err)
		return
	}

	c.mu.Lock()
	responseCh := c.pending[data.RequestID]
	delete(c.pending, data.RequestID)
	c.mu.Unlock()

	if responseCh != nil {
		responseCh <- data
	}
}

func (c *Client) disconnectIfCurrent(conn *websocket.Conn, err error) {
	c.mu.Lock()
	if c.conn != conn || c.closed {
		c.mu.Unlock()
		return
	}
	c.conn = nil
	c.state = StateDisconnected
	c.connectedAt = time.Time{}
	c.lastErr = err
	c.failPendingLocked(err)
	c.mu.Unlock()
	_ = conn.Close()
}

func (c *Client) setDisconnected(err error) {
	c.mu.Lock()
	if !c.closed {
		c.state = StateDisconnected
		c.connectedAt = time.Time{}
		c.lastErr = err
	}
	c.mu.Unlock()
}

func (c *Client) failPendingLocked(err error) {
	for id, ch := range c.pending {
		delete(c.pending, id)
		ch <- requestResponseData{
			RequestID: id,
			RequestStatus: requestStatus{
				Result:  false,
				Code:    0,
				Comment: err.Error(),
			},
		}
	}
}

func readJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	done := make(chan error, 1)
	go func() {
		done <- conn.ReadJSON(value)
	}()
	select {
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func writeJSON(ctx context.Context, conn *websocket.Conn, value any) error {
	done := make(chan error, 1)
	go func() {
		done <- conn.WriteJSON(value)
	}()
	select {
	case <-ctx.Done():
		_ = conn.Close()
		return ctx.Err()
	case err := <-done:
		return err
	}
}

func writeJSONWithTimeout(ctx context.Context, timeoutCtx context.Context, conn *websocket.Conn, value any) error {
	return waitWriteJSON(ctx, timeoutCtx, func() error {
		return conn.WriteJSON(value)
	}, conn.Close)
}

func waitWriteJSON(ctx context.Context, timeoutCtx context.Context, write func() error, closeConn func() error) error {
	done := make(chan error, 1)
	go func() {
		done <- write()
	}()
	select {
	case <-ctx.Done():
		_ = closeConn()
		return ctx.Err()
	case <-timeoutCtx.Done():
		_ = closeConn()
		return timeoutCtx.Err()
	case err := <-done:
		return err
	}
}

func buildIdentify(data helloData, password string) (identifyData, error) {
	identify := identifyData{RPCVersion: data.RPCVersion}
	if data.Authentication == nil {
		return identify, nil
	}
	auth, err := buildAuthentication(password, data.Authentication.Salt, data.Authentication.Challenge)
	if err != nil {
		return identifyData{}, err
	}
	identify.Authentication = auth
	return identify, nil
}

func buildAuthentication(password, salt, challenge string) (string, error) {
	if password == "" {
		return "", errors.New("OBS authentication required but password is empty")
	}

	secretHash := sha256.Sum256([]byte(password + salt))
	secret := base64.StdEncoding.EncodeToString(secretHash[:])
	authHash := sha256.Sum256([]byte(secret + challenge))
	return base64.StdEncoding.EncodeToString(authHash[:]), nil
}

func mustMarshal(value any) json.RawMessage {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return data
}

type envelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d,omitempty"`
}

type helloData struct {
	RPCVersion     int                 `json:"rpcVersion"`
	Authentication *authenticationData `json:"authentication,omitempty"`
}

type authenticationData struct {
	Challenge string `json:"challenge"`
	Salt      string `json:"salt"`
}

type identifyData struct {
	RPCVersion     int    `json:"rpcVersion"`
	Authentication string `json:"authentication,omitempty"`
}

type requestDataPayload struct {
	RequestType string         `json:"requestType"`
	RequestID   string         `json:"requestId"`
	RequestData map[string]any `json:"requestData,omitempty"`
}

type eventData struct {
	EventType string `json:"eventType"`
	EventData struct {
		InputName string `json:"inputName"`
	} `json:"eventData"`
}

type requestResponseData struct {
	RequestType   string          `json:"requestType"`
	RequestID     string          `json:"requestId"`
	RequestStatus requestStatus   `json:"requestStatus"`
	ResponseData  json.RawMessage `json:"responseData,omitempty"`
}

type requestStatus struct {
	Result  bool   `json:"result"`
	Code    int    `json:"code"`
	Comment string `json:"comment"`
}

type currentProgramSceneResponse struct {
	SceneName               string `json:"sceneName"`
	CurrentProgramSceneName string `json:"currentProgramSceneName"`
}

type videoSettingsResponse struct {
	BaseWidth  float64 `json:"baseWidth"`
	BaseHeight float64 `json:"baseHeight"`
}

type sceneItemIDResponse struct {
	SceneItemID int `json:"sceneItemId"`
}
