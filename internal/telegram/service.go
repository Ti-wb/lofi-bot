package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const defaultUpdateTimeout = 30

type Config struct {
	Token              string
	AllowedChatID      int64
	AdminUserIDs       map[int64]struct{}
	MaxUploadSizeBytes int64
	UpdateTimeout      int
	Debug              bool
}

type Service struct {
	bot    botAPI
	cfg    Config
	hooks  Hooks
	logger *slog.Logger
}

type Hooks struct {
	EnqueueUpload EnqueueUploadFunc
	ListQueue     SimpleFunc
	Move          MoveFunc
	Remove        IDFunc
	Skip          SimpleFunc
	Now           SimpleFunc
	History       SimpleFunc
	Status        SimpleFunc
}

type EnqueueUploadFunc func(context.Context, Upload) (string, error)
type SimpleFunc func(context.Context) (string, error)
type IDFunc func(context.Context, int64) (string, error)
type MoveFunc func(context.Context, int64, int) (string, error)

type Upload struct {
	FileID          string
	FileUniqueID    string
	FileName        string
	MimeType        string
	SizeBytes       int64
	DurationSeconds int
	DownloadURL     string
	Kind            UploadKind
	ChatID          int64
	MessageID       int
	SubmitterID     int64
	SubmitterName   string
	Caption         string
}

type UploadKind string

const (
	UploadKindVideo    UploadKind = "video"
	UploadKindDocument UploadKind = "document"
)

type Option func(*Service)

func New(cfg Config, hooks Hooks, logger *slog.Logger, opts ...Option) (*Service, error) {
	if cfg.Token == "" {
		return nil, errors.New("telegram token is required")
	}
	if cfg.AllowedChatID == 0 {
		return nil, errors.New("telegram allowed chat id is required")
	}
	if cfg.UpdateTimeout <= 0 {
		cfg.UpdateTimeout = defaultUpdateTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Service{
		cfg:    cfg,
		hooks:  hooks,
		logger: logger,
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.bot == nil {
		bot, err := tgbotapi.NewBotAPI(cfg.Token)
		if err != nil {
			return nil, fmt.Errorf("create telegram bot: %w", err)
		}
		bot.Debug = cfg.Debug
		s.bot = bot
	}
	return s, nil
}

func WithBotAPI(bot botAPI) Option {
	return func(s *Service) {
		s.bot = bot
	}
}

func (s *Service) Run(ctx context.Context) error {
	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = s.cfg.UpdateTimeout

	updates := s.bot.GetUpdatesChan(updateConfig)
	defer s.bot.StopReceivingUpdates()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case update, ok := <-updates:
			if !ok {
				return nil
			}
			s.handleUpdate(ctx, update)
		}
	}
}

func (s *Service) SendMessage(ctx context.Context, chatID int64, text string) error {
	if text == "" {
		return nil
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	return s.send(ctx, msg)
}

func (s *Service) handleUpdate(ctx context.Context, update tgbotapi.Update) {
	if update.Message != nil {
		s.handleMessage(ctx, update.Message)
		return
	}
	if update.CallbackQuery != nil {
		s.handleCallback(ctx, update.CallbackQuery)
	}
}

func (s *Service) handleMessage(ctx context.Context, msg *tgbotapi.Message) {
	if msg.Chat == nil || msg.Chat.ID != s.cfg.AllowedChatID {
		return
	}

	if msg.IsCommand() {
		response, err := s.handleCommand(ctx, msg)
		s.reply(ctx, msg.Chat.ID, response, err)
		return
	}

	if msg.Video != nil || msg.Document != nil {
		response, err := s.handleUpload(ctx, msg)
		s.reply(ctx, msg.Chat.ID, response, err)
	}
}

func (s *Service) handleCallback(ctx context.Context, cb *tgbotapi.CallbackQuery) {
	if cb.Message == nil || cb.Message.Chat == nil || cb.Message.Chat.ID != s.cfg.AllowedChatID {
		s.answerCallback(ctx, cb.ID, "")
		return
	}

	response, err := s.routeAction(ctx, cb.From, cb.Data)
	if err != nil {
		response = friendlyError(err)
	}
	s.answerCallback(ctx, cb.ID, truncateCallbackText(response))
	if response != "" {
		_ = s.SendMessage(ctx, cb.Message.Chat.ID, response)
	}
}

func (s *Service) handleCommand(ctx context.Context, msg *tgbotapi.Message) (string, error) {
	command := strings.ToLower(msg.Command())
	args := strings.Fields(msg.CommandArguments())
	switch command {
	case "start", "help":
		return helpText(s.isAdmin(msg.From)), nil
	case "queue", "list":
		return s.callSimple(ctx, "list queue", s.hooks.ListQueue)
	case "now":
		return s.callSimple(ctx, "current video", s.hooks.Now)
	case "history":
		return s.callSimple(ctx, "history", s.hooks.History)
	case "status":
		return s.callSimple(ctx, "status", s.hooks.Status)
	case "remove":
		if !s.isAdmin(msg.From) {
			return "", errAdminOnly
		}
		id, err := parseIDArg(args, "remove")
		if err != nil {
			return "", err
		}
		return s.callID(ctx, "remove", s.hooks.Remove, id)
	case "move":
		if !s.isAdmin(msg.From) {
			return "", errAdminOnly
		}
		id, position, err := parseMoveArgs(args)
		if err != nil {
			return "", err
		}
		return s.callMove(ctx, s.hooks.Move, id, position)
	case "skip":
		if !s.isAdmin(msg.From) {
			return "", errAdminOnly
		}
		return s.callSimple(ctx, "skip", s.hooks.Skip)
	default:
		return "I do not know that command. Try /queue, /now, /status, or /help.", nil
	}
}

func (s *Service) routeAction(ctx context.Context, user *tgbotapi.User, data string) (string, error) {
	parts := strings.Fields(strings.ReplaceAll(data, ":", " "))
	if len(parts) == 0 {
		return "", nil
	}

	action := strings.ToLower(parts[0])
	switch action {
	case "queue", "list":
		return s.callSimple(ctx, "list queue", s.hooks.ListQueue)
	case "now":
		return s.callSimple(ctx, "current video", s.hooks.Now)
	case "history":
		return s.callSimple(ctx, "history", s.hooks.History)
	case "status":
		return s.callSimple(ctx, "status", s.hooks.Status)
	case "remove":
		if !s.isAdmin(user) {
			return "", errAdminOnly
		}
		id, err := parseIDArg(parts[1:], "remove")
		if err != nil {
			return "", err
		}
		return s.callID(ctx, "remove", s.hooks.Remove, id)
	case "move":
		if !s.isAdmin(user) {
			return "", errAdminOnly
		}
		id, position, err := parseMoveArgs(parts[1:])
		if err != nil {
			return "", err
		}
		return s.callMove(ctx, s.hooks.Move, id, position)
	case "skip":
		if !s.isAdmin(user) {
			return "", errAdminOnly
		}
		return s.callSimple(ctx, "skip", s.hooks.Skip)
	default:
		return "", nil
	}
}

func (s *Service) handleUpload(ctx context.Context, msg *tgbotapi.Message) (string, error) {
	if s.hooks.EnqueueUpload == nil {
		return "", errHookNotConfigured("enqueue upload")
	}

	upload, err := uploadFromMessage(msg)
	if err != nil {
		return "", err
	}
	if s.cfg.MaxUploadSizeBytes > 0 && upload.SizeBytes > s.cfg.MaxUploadSizeBytes {
		return "", fmt.Errorf("%w: %s is larger than the limit of %s", errUploadTooLarge, formatBytes(upload.SizeBytes), formatBytes(s.cfg.MaxUploadSizeBytes))
	}

	file, err := s.bot.GetFile(tgbotapi.FileConfig{FileID: upload.FileID})
	if err != nil {
		return "", fmt.Errorf("inspect Telegram file: %w", err)
	}
	if s.cfg.MaxUploadSizeBytes > 0 && file.FileSize > 0 && int64(file.FileSize) > s.cfg.MaxUploadSizeBytes {
		return "", fmt.Errorf("%w: %s is larger than the limit of %s", errUploadTooLarge, formatBytes(int64(file.FileSize)), formatBytes(s.cfg.MaxUploadSizeBytes))
	}
	upload.DownloadURL = file.Link(s.cfg.Token)
	if upload.SizeBytes <= 0 && file.FileSize > 0 {
		upload.SizeBytes = int64(file.FileSize)
	}

	response, err := s.hooks.EnqueueUpload(ctx, upload)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(response) == "" {
		return fmt.Sprintf("Queued %s.", displayName(upload)), nil
	}
	return response, nil
}

func uploadFromMessage(msg *tgbotapi.Message) (Upload, error) {
	upload := Upload{
		ChatID:        msg.Chat.ID,
		MessageID:     msg.MessageID,
		SubmitterID:   userID(msg.From),
		SubmitterName: displayUser(msg.From),
		Caption:       strings.TrimSpace(msg.Caption),
	}

	if msg.Video != nil {
		upload.Kind = UploadKindVideo
		upload.FileID = msg.Video.FileID
		upload.FileUniqueID = msg.Video.FileUniqueID
		upload.FileName = strings.TrimSpace(msg.Video.FileName)
		upload.MimeType = strings.TrimSpace(msg.Video.MimeType)
		upload.SizeBytes = int64(msg.Video.FileSize)
		upload.DurationSeconds = msg.Video.Duration
		if upload.FileName == "" {
			upload.FileName = defaultUploadName(upload.FileUniqueID, ".mp4")
		}
		return upload, nil
	}

	if msg.Document != nil {
		upload.Kind = UploadKindDocument
		upload.FileID = msg.Document.FileID
		upload.FileUniqueID = msg.Document.FileUniqueID
		upload.FileName = strings.TrimSpace(msg.Document.FileName)
		upload.MimeType = strings.TrimSpace(msg.Document.MimeType)
		upload.SizeBytes = int64(msg.Document.FileSize)
		if upload.FileName == "" {
			upload.FileName = defaultUploadName(upload.FileUniqueID, "")
		}
		if !looksLikeVideoDocument(upload.FileName, upload.MimeType) {
			return Upload{}, errUnsupportedUpload
		}
		return upload, nil
	}

	return Upload{}, errUnsupportedUpload
}

func (s *Service) callSimple(ctx context.Context, name string, hook SimpleFunc) (string, error) {
	if hook == nil {
		return "", errHookNotConfigured(name)
	}
	return hook(ctx)
}

func (s *Service) callID(ctx context.Context, name string, hook IDFunc, id int64) (string, error) {
	if hook == nil {
		return "", errHookNotConfigured(name)
	}
	return hook(ctx, id)
}

func (s *Service) callMove(ctx context.Context, hook MoveFunc, id int64, position int) (string, error) {
	if hook == nil {
		return "", errHookNotConfigured("move")
	}
	return hook(ctx, id, position)
}

func (s *Service) reply(ctx context.Context, chatID int64, response string, err error) {
	if err != nil {
		response = friendlyError(err)
	}
	if response == "" {
		return
	}
	if sendErr := s.SendMessage(ctx, chatID, response); sendErr != nil {
		s.logger.Warn("send telegram message", "error", sendErr)
	}
}

func (s *Service) send(ctx context.Context, msg tgbotapi.Chattable) error {
	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := s.bot.Send(msg)
		done <- result{err: err}
	}()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case res := <-done:
		return res.err
	}
}

func (s *Service) answerCallback(ctx context.Context, callbackID string, text string) {
	if callbackID == "" {
		return
	}
	callback := tgbotapi.NewCallback(callbackID, text)
	if _, err := s.bot.Request(callback); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		s.logger.Warn("answer telegram callback", "error", err)
	}
}

func (s *Service) isAdmin(user *tgbotapi.User) bool {
	if user == nil {
		return false
	}
	_, ok := s.cfg.AdminUserIDs[int64(user.ID)]
	return ok
}

func parseIDArg(args []string, command string) (int64, error) {
	if len(args) != 1 {
		return 0, fmt.Errorf("%w: use /%s <video_id>", errBadCommand, command)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: video id must be a positive number", errBadCommand)
	}
	return id, nil
}

func parseMoveArgs(args []string) (int64, int, error) {
	if len(args) != 2 {
		return 0, 0, fmt.Errorf("%w: use /move <video_id> <position>", errBadCommand)
	}
	id, err := strconv.ParseInt(args[0], 10, 64)
	if err != nil || id <= 0 {
		return 0, 0, fmt.Errorf("%w: video id must be a positive number", errBadCommand)
	}
	position, err := strconv.Atoi(args[1])
	if err != nil || position <= 0 {
		return 0, 0, fmt.Errorf("%w: position must be a positive number", errBadCommand)
	}
	return id, position, nil
}

func looksLikeVideoDocument(name string, mimeType string) bool {
	if strings.HasPrefix(strings.ToLower(mimeType), "video/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".m4v", ".mkv", ".webm", ".avi":
		return true
	default:
		return false
	}
}

func defaultUploadName(uniqueID string, ext string) string {
	if uniqueID == "" {
		uniqueID = strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if ext == "" {
		ext = ".video"
	}
	return "telegram-" + uniqueID + ext
}

func displayName(upload Upload) string {
	if upload.FileName != "" {
		return upload.FileName
	}
	return string(upload.Kind)
}

func displayUser(user *tgbotapi.User) string {
	if user == nil {
		return ""
	}
	if user.UserName != "" {
		return "@" + user.UserName
	}
	name := strings.TrimSpace(strings.TrimSpace(user.FirstName + " " + user.LastName))
	if name != "" {
		return name
	}
	return strconv.FormatInt(int64(user.ID), 10)
}

func userID(user *tgbotapi.User) int64 {
	if user == nil {
		return 0
	}
	return int64(user.ID)
}

func helpText(admin bool) string {
	lines := []string{
		"Send a video file here to add it to the OBS queue.",
		"",
		"/queue - show queued videos",
		"/now - show what is playing",
		"/status - show queue status",
		"/history - show recent completed items",
	}
	if admin {
		lines = append(lines,
			"",
			"Admin:",
			"/move <video_id> <position> - reorder a queued video",
			"/remove <video_id> - remove a queued video",
			"/skip - skip the current video",
		)
	}
	return strings.Join(lines, "\n")
}

func formatBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}

func truncateCallbackText(text string) string {
	text = strings.TrimSpace(text)
	const limit = 180
	if len(text) <= limit {
		return text
	}
	return text[:limit-3] + "..."
}

func friendlyError(err error) string {
	switch {
	case errors.Is(err, errAdminOnly):
		return "Only admins can use that command."
	case errors.Is(err, errBadCommand):
		return strings.TrimPrefix(err.Error(), errBadCommand.Error()+": ")
	case errors.Is(err, errUnsupportedUpload):
		return "Please send a video upload or a video document."
	case errors.Is(err, errUploadTooLarge):
		return strings.TrimPrefix(err.Error(), errUploadTooLarge.Error()+": ")
	default:
		return "Sorry, I could not complete that: " + err.Error()
	}
}

var (
	errAdminOnly         = errors.New("admin only")
	errBadCommand        = errors.New("bad command")
	errUnsupportedUpload = errors.New("unsupported upload")
	errUploadTooLarge    = errors.New("upload too large")
)

type errHookNotConfigured string

func (e errHookNotConfigured) Error() string {
	return fmt.Sprintf("%s handler is not configured", string(e))
}

type botAPI interface {
	GetUpdatesChan(tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel
	StopReceivingUpdates()
	Send(tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
	GetFile(tgbotapi.FileConfig) (tgbotapi.File, error)
}
