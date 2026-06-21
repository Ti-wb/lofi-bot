package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/secret"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	defaultUpdateTimeout  = 30
	defaultRequestTimeout = time.Duration(defaultUpdateTimeout)*time.Second + 5*time.Second
	defaultPollRetryDelay = 3 * time.Second
	adminCacheTTL         = 60 * time.Second
)

var queueItemPattern = regexp.MustCompile(`#([0-9]+).*第 ([0-9]+) 位`)

type Config struct {
	Token              string
	APIBaseURL         string
	AllowedChatID      int64
	MaxUploadSizeBytes int64
	UpdateTimeout      int
	RequestTimeout     time.Duration
	Debug              bool
}

type Service struct {
	bot             botAPI
	cfg             Config
	hooks           Hooks
	logger          *slog.Logger
	now             func() time.Time
	pollRetryDelay  time.Duration
	adminCacheMutex sync.Mutex
	adminCache      map[int64]adminCacheEntry
}

type adminCacheEntry struct {
	adminIDs  map[int64]struct{}
	expiresAt time.Time
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

type botResponse struct {
	text   string
	markup *tgbotapi.InlineKeyboardMarkup
}

type Upload struct {
	FileID          string
	FileUniqueID    string
	FileName        string
	MimeType        string
	SizeBytes       int64
	DurationSeconds int
	LocalPath       string
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
	if strings.TrimSpace(cfg.APIBaseURL) == "" {
		return nil, errors.New("telegram api base url is required")
	}
	if cfg.AllowedChatID == 0 {
		return nil, errors.New("telegram allowed chat id is required")
	}
	cfg.APIBaseURL = strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/")
	if cfg.UpdateTimeout <= 0 {
		cfg.UpdateTimeout = defaultUpdateTimeout
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = defaultRequestTimeout
	}
	minRequestTimeout := time.Duration(cfg.UpdateTimeout)*time.Second + 5*time.Second
	if cfg.RequestTimeout < minRequestTimeout {
		cfg.RequestTimeout = minRequestTimeout
	}
	if logger == nil {
		logger = slog.Default()
	}

	s := &Service{
		cfg:            cfg,
		hooks:          hooks,
		logger:         logger,
		now:            time.Now,
		pollRetryDelay: defaultPollRetryDelay,
		adminCache:     make(map[int64]adminCacheEntry),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.bot == nil {
		bot, err := newProductionBotAPI(cfg)
		if err != nil {
			return nil, fmt.Errorf("create telegram bot: %w", secret.RedactError(err, cfg.Token))
		}
		s.bot = bot
	}
	return s, nil
}

func WithBotAPI(bot botAPI) Option {
	return func(s *Service) {
		s.bot = bot
	}
}

func (s *Service) registerCommands(ctx context.Context) error {
	publicCommands := []tgbotapi.BotCommand{
		{Command: "queue", Description: "Show queued videos"},
		{Command: "now", Description: "Show what is playing"},
		{Command: "status", Description: "Show bot, OBS, queue, and disk status"},
		{Command: "history", Description: "Show recent completed items"},
		{Command: "help", Description: "Show help"},
	}
	adminCommands := []tgbotapi.BotCommand{
		{Command: "queue", Description: "Show queued videos"},
		{Command: "now", Description: "Show what is playing"},
		{Command: "status", Description: "Show bot, OBS, queue, and disk status"},
		{Command: "history", Description: "Show recent completed items"},
		{Command: "skip", Description: "Skip current playback"},
		{Command: "help", Description: "Show help"},
	}

	if err := s.request(ctx, tgbotapi.NewSetMyCommandsWithScope(tgbotapi.NewBotCommandScopeChat(s.cfg.AllowedChatID), publicCommands...)); err != nil {
		return err
	}
	return s.request(ctx, tgbotapi.NewSetMyCommandsWithScope(tgbotapi.NewBotCommandScopeChatAdministrators(s.cfg.AllowedChatID), adminCommands...))
}

func (s *Service) Run(ctx context.Context) error {
	if err := s.registerCommands(ctx); err != nil {
		s.logger.Warn("register telegram commands", "error", s.redactError(err))
	}

	updateConfig := tgbotapi.NewUpdate(0)
	updateConfig.Timeout = s.cfg.UpdateTimeout

	for {
		if err := ctx.Err(); err != nil {
			return ctx.Err()
		}

		updates, err := s.bot.GetUpdates(ctx, updateConfig)
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			s.logger.Warn("get telegram updates", "error", s.redactError(err))
			if err := sleepContext(ctx, s.pollRetryDelay); err != nil {
				return err
			}
			continue
		}

		for _, update := range updates {
			if err := ctx.Err(); err != nil {
				return err
			}
			if update.UpdateID < updateConfig.Offset {
				continue
			}
			updateConfig.Offset = update.UpdateID + 1
			s.handleUpdate(ctx, update)
		}
	}
}

func (s *Service) SendMessage(ctx context.Context, chatID int64, text string) error {
	return s.SendMessageWithMarkup(ctx, chatID, text, nil)
}

func (s *Service) SendMessageWithMarkup(ctx context.Context, chatID int64, text string, markup *tgbotapi.InlineKeyboardMarkup) error {
	if text == "" {
		return nil
	}
	msg := tgbotapi.NewMessage(chatID, text)
	msg.DisableWebPagePreview = true
	msg.ReplyMarkup = markup
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

	response, err := s.routeAction(ctx, cb.Message.Chat.ID, cb.From, cb.Data)
	if err != nil {
		response = botResponse{text: s.friendlyError(err)}
	}
	s.answerCallback(ctx, cb.ID, truncateCallbackText(response.text))
	if response.text != "" {
		if isRefreshAction(cb.Data) {
			if err := s.editCallbackMessage(ctx, cb.Message.Chat.ID, cb.Message.MessageID, response); err != nil {
				s.logger.Warn("edit telegram callback message", "error", s.redactError(err))
				_ = s.SendMessageWithMarkup(ctx, cb.Message.Chat.ID, response.text, response.markup)
			}
			return
		}
		_ = s.SendMessageWithMarkup(ctx, cb.Message.Chat.ID, response.text, response.markup)
	}
}

func (s *Service) handleCommand(ctx context.Context, msg *tgbotapi.Message) (botResponse, error) {
	command := strings.ToLower(msg.Command())
	args := strings.Fields(msg.CommandArguments())
	admin := s.isAdmin(ctx, msg.Chat.ID, msg.From)
	switch command {
	case "start", "help":
		return botResponse{text: helpText(admin), markup: homeKeyboard(admin)}, nil
	case "queue", "list":
		return s.responseFromQueue(ctx, admin)
	case "now":
		return s.responseFromSimple(ctx, "current video", s.hooks.Now, nowKeyboard(admin))
	case "history":
		return s.responseFromSimple(ctx, "history", s.hooks.History, historyKeyboard(admin))
	case "status":
		return s.responseFromSimple(ctx, "status", s.hooks.Status, statusKeyboard(admin))
	case "remove":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		id, err := parseIDArg(args, "remove")
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromID(ctx, "remove", s.hooks.Remove, id, queueKeyboard(admin, ""))
	case "move":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		id, position, err := parseMoveArgs(args)
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromMove(ctx, s.hooks.Move, id, position, queueKeyboard(admin, ""))
	case "skip":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		return s.responseFromSimple(ctx, "skip", s.hooks.Skip, queueKeyboard(admin, ""))
	default:
		return botResponse{text: "I do not know that command. Try /queue, /now, /status, or /help.", markup: homeKeyboard(admin)}, nil
	}
}

func (s *Service) routeAction(ctx context.Context, chatID int64, user *tgbotapi.User, data string) (botResponse, error) {
	parts := strings.Fields(strings.ReplaceAll(data, ":", " "))
	if len(parts) == 0 {
		return botResponse{}, nil
	}

	action := strings.ToLower(parts[0])
	admin := s.isAdmin(ctx, chatID, user)
	switch action {
	case "queue", "list":
		return s.responseFromQueue(ctx, admin)
	case "now":
		return s.responseFromSimple(ctx, "current video", s.hooks.Now, nowKeyboard(admin))
	case "history":
		return s.responseFromSimple(ctx, "history", s.hooks.History, historyKeyboard(admin))
	case "status":
		return s.responseFromSimple(ctx, "status", s.hooks.Status, statusKeyboard(admin))
	case "remove":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		id, err := parseIDArg(parts[1:], "remove")
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromID(ctx, "remove", s.hooks.Remove, id, queueKeyboard(admin, ""))
	case "move":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		id, position, err := parseMoveArgs(parts[1:])
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromMove(ctx, s.hooks.Move, id, position, queueKeyboard(admin, ""))
	case "skip":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		return s.responseFromSimple(ctx, "skip", s.hooks.Skip, queueKeyboard(admin, ""))
	default:
		return botResponse{}, nil
	}
}

func (s *Service) handleUpload(ctx context.Context, msg *tgbotapi.Message) (botResponse, error) {
	if s.hooks.EnqueueUpload == nil {
		return botResponse{}, errHookNotConfigured("enqueue upload")
	}

	upload, err := uploadFromMessage(msg)
	if err != nil {
		return botResponse{}, err
	}
	if s.cfg.MaxUploadSizeBytes > 0 && upload.SizeBytes > s.cfg.MaxUploadSizeBytes {
		return botResponse{}, fmt.Errorf("%w: %s is larger than the limit of %s", errUploadTooLarge, formatBytes(upload.SizeBytes), formatBytes(s.cfg.MaxUploadSizeBytes))
	}

	file, err := s.getFile(ctx, tgbotapi.FileConfig{FileID: upload.FileID})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return botResponse{}, err
		}
		return botResponse{}, fmt.Errorf("inspect Telegram file: %w", err)
	}
	if s.cfg.MaxUploadSizeBytes > 0 && file.FileSize > 0 && int64(file.FileSize) > s.cfg.MaxUploadSizeBytes {
		return botResponse{}, fmt.Errorf("%w: %s is larger than the limit of %s", errUploadTooLarge, formatBytes(int64(file.FileSize)), formatBytes(s.cfg.MaxUploadSizeBytes))
	}
	if strings.TrimSpace(file.FilePath) == "" || !filepath.IsAbs(file.FilePath) {
		return botResponse{}, fmt.Errorf("Local Bot API Server must run with --local and return an absolute file path")
	}
	upload.LocalPath = file.FilePath
	if upload.SizeBytes <= 0 && file.FileSize > 0 {
		upload.SizeBytes = int64(file.FileSize)
	}

	response, err := s.hooks.EnqueueUpload(ctx, upload)
	if err != nil {
		return botResponse{}, err
	}
	if strings.TrimSpace(response) == "" {
		response = fmt.Sprintf("Queued %s.", displayName(upload))
	}
	return botResponse{text: response, markup: uploadAcceptedKeyboard()}, nil
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

func (s *Service) responseFromSimple(ctx context.Context, name string, hook SimpleFunc, markup *tgbotapi.InlineKeyboardMarkup) (botResponse, error) {
	text, err := s.callSimple(ctx, name, hook)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: text, markup: markup}, nil
}

func (s *Service) responseFromQueue(ctx context.Context, admin bool) (botResponse, error) {
	text, err := s.callSimple(ctx, "list queue", s.hooks.ListQueue)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: text, markup: queueKeyboard(admin, text)}, nil
}

func (s *Service) responseFromID(ctx context.Context, name string, hook IDFunc, id int64, markup *tgbotapi.InlineKeyboardMarkup) (botResponse, error) {
	text, err := s.callID(ctx, name, hook, id)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: text, markup: markup}, nil
}

func (s *Service) responseFromMove(ctx context.Context, hook MoveFunc, id int64, position int, markup *tgbotapi.InlineKeyboardMarkup) (botResponse, error) {
	text, err := s.callMove(ctx, hook, id, position)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: text, markup: markup}, nil
}

func (s *Service) reply(ctx context.Context, chatID int64, response botResponse, err error) {
	if err != nil {
		response = botResponse{text: s.friendlyError(err)}
	}
	if response.text == "" {
		return
	}
	if sendErr := s.SendMessageWithMarkup(ctx, chatID, response.text, response.markup); sendErr != nil {
		s.logger.Warn("send telegram message", "error", s.redactError(sendErr))
	}
}

func (s *Service) send(ctx context.Context, msg tgbotapi.Chattable) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.bot.Send(ctx, msg)
	return s.redactError(err)
}

func (s *Service) request(ctx context.Context, req tgbotapi.Chattable) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := s.bot.Request(ctx, req)
	return s.redactError(err)
}

func (s *Service) getFile(ctx context.Context, config tgbotapi.FileConfig) (tgbotapi.File, error) {
	if err := ctx.Err(); err != nil {
		return tgbotapi.File{}, err
	}
	file, err := s.bot.GetFile(ctx, config)
	return file, s.redactError(err)
}

func (s *Service) getChatAdministrators(ctx context.Context, config tgbotapi.ChatAdministratorsConfig) ([]tgbotapi.ChatMember, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	admins, err := s.bot.GetChatAdministrators(ctx, config)
	return admins, s.redactError(err)
}

func (s *Service) answerCallback(ctx context.Context, callbackID string, text string) {
	if callbackID == "" {
		return
	}
	callback := tgbotapi.NewCallback(callbackID, text)
	if err := s.request(ctx, callback); err != nil && !errors.Is(ctx.Err(), context.Canceled) {
		s.logger.Warn("answer telegram callback", "error", s.redactError(err))
	}
}

func (s *Service) editCallbackMessage(ctx context.Context, chatID int64, messageID int, response botResponse) error {
	if response.text == "" {
		return nil
	}
	if response.markup != nil {
		edit := tgbotapi.NewEditMessageTextAndMarkup(chatID, messageID, response.text, *response.markup)
		edit.DisableWebPagePreview = true
		return s.request(ctx, edit)
	}
	edit := tgbotapi.NewEditMessageText(chatID, messageID, response.text)
	edit.DisableWebPagePreview = true
	return s.request(ctx, edit)
}

func (s *Service) isAdmin(ctx context.Context, chatID int64, user *tgbotapi.User) bool {
	if user == nil {
		return false
	}
	userID := int64(user.ID)
	adminIDs, _, err := s.getAdminIDs(ctx, chatID, false)
	if err != nil {
		s.logger.Warn("get telegram chat administrators", "chat_id", chatID, "error", s.redactError(err))
		return false
	}
	if _, ok := adminIDs[userID]; ok {
		return true
	}

	adminIDs, _, err = s.getAdminIDs(ctx, chatID, true)
	if err != nil {
		s.logger.Warn("refresh telegram chat administrators", "chat_id", chatID, "error", s.redactError(err))
		return false
	}
	_, ok := adminIDs[userID]
	return ok
}

func (s *Service) getAdminIDs(ctx context.Context, chatID int64, force bool) (map[int64]struct{}, bool, error) {
	if !force {
		s.adminCacheMutex.Lock()
		entry, ok := s.adminCache[chatID]
		if ok && s.now().Before(entry.expiresAt) {
			s.adminCacheMutex.Unlock()
			return entry.adminIDs, true, nil
		}
		s.adminCacheMutex.Unlock()
	}

	admins, err := s.getChatAdministrators(ctx, tgbotapi.ChatAdministratorsConfig{
		ChatConfig: tgbotapi.ChatConfig{ChatID: chatID},
	})
	if err != nil {
		return nil, false, err
	}
	adminIDs := make(map[int64]struct{}, len(admins))
	for _, admin := range admins {
		if admin.User == nil || admin.User.IsBot {
			continue
		}
		if admin.IsCreator() || admin.IsAdministrator() {
			adminIDs[int64(admin.User.ID)] = struct{}{}
		}
	}

	s.adminCacheMutex.Lock()
	s.adminCache[chatID] = adminCacheEntry{
		adminIDs:  adminIDs,
		expiresAt: s.now().Add(adminCacheTTL),
	}
	s.adminCacheMutex.Unlock()
	return adminIDs, false, nil
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

func homeKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Queue", "queue"),
			tgbotapi.NewInlineKeyboardButtonData("Now", "now"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Status", "status"),
			tgbotapi.NewInlineKeyboardButtonData("History", "history"),
		),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Skip", "skip")))
	}
	return inlineKeyboard(rows...)
}

func queueKeyboard(admin bool, queueText string) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Refresh", "queue"),
			tgbotapi.NewInlineKeyboardButtonData("Now", "now"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Status", "status"),
			tgbotapi.NewInlineKeyboardButtonData("History", "history"),
		),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Skip", "skip")))
		for _, item := range queueItems(queueText) {
			row := []tgbotapi.InlineKeyboardButton{
				tgbotapi.NewInlineKeyboardButtonData(fmt.Sprintf("Remove #%d", item.id), fmt.Sprintf("remove:%d", item.id)),
			}
			if item.position > 1 {
				row = append(row, tgbotapi.NewInlineKeyboardButtonData("Up", fmt.Sprintf("move:%d:%d", item.id, item.position-1)))
			}
			row = append(row, tgbotapi.NewInlineKeyboardButtonData("Down", fmt.Sprintf("move:%d:%d", item.id, item.position+1)))
			rows = append(rows, row)
		}
	}
	return inlineKeyboard(rows...)
}

type queueItem struct {
	id       int64
	position int
}

func queueItems(text string) []queueItem {
	var items []queueItem
	for _, match := range queueItemPattern.FindAllStringSubmatch(text, -1) {
		if len(match) != 3 {
			continue
		}
		id, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			continue
		}
		position, err := strconv.Atoi(match[2])
		if err != nil {
			continue
		}
		items = append(items, queueItem{id: id, position: position})
		if len(items) >= 5 {
			break
		}
	}
	return items
}

func nowKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Queue", "queue"),
			tgbotapi.NewInlineKeyboardButtonData("Status", "status"),
		),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Skip", "skip")))
	}
	return inlineKeyboard(rows...)
}

func statusKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Refresh", "status"),
			tgbotapi.NewInlineKeyboardButtonData("Queue", "queue"),
		),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Now", "now")),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Skip", "skip")))
	}
	return inlineKeyboard(rows...)
}

func historyKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Queue", "queue"),
			tgbotapi.NewInlineKeyboardButtonData("Status", "status"),
		),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Skip", "skip")))
	}
	return inlineKeyboard(rows...)
}

func uploadAcceptedKeyboard() *tgbotapi.InlineKeyboardMarkup {
	return inlineKeyboard(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("Queue", "queue"),
			tgbotapi.NewInlineKeyboardButtonData("Now", "now"),
		),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("Status", "status")),
	)
}

func inlineKeyboard(rows ...[]tgbotapi.InlineKeyboardButton) *tgbotapi.InlineKeyboardMarkup {
	markup := tgbotapi.NewInlineKeyboardMarkup(rows...)
	return &markup
}

func isRefreshAction(data string) bool {
	parts := strings.Fields(strings.ReplaceAll(data, ":", " "))
	if len(parts) == 0 {
		return false
	}
	switch strings.ToLower(parts[0]) {
	case "queue", "list", "now", "status", "history":
		return true
	default:
		return false
	}
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
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit-3]) + "..."
}

func friendlyError(err error) string {
	var public publicError
	switch {
	case errors.Is(err, errAdminOnly):
		return "Only admins can use that command."
	case errors.Is(err, errBadCommand):
		return strings.TrimPrefix(err.Error(), errBadCommand.Error()+": ")
	case errors.Is(err, errUnsupportedUpload):
		return "Please send a video upload or a video document."
	case errors.Is(err, errUploadTooLarge):
		return strings.TrimPrefix(err.Error(), errUploadTooLarge.Error()+": ")
	case errors.As(err, &public):
		return public.PublicMessage()
	default:
		return "Sorry, I could not complete that. Check /status or the service logs for details."
	}
}

func (s *Service) friendlyError(err error) string {
	return secret.RedactString(friendlyError(s.redactError(err)), s.cfg.Token)
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

type publicError interface {
	error
	PublicMessage() string
}

type botAPI interface {
	GetUpdates(context.Context, tgbotapi.UpdateConfig) ([]tgbotapi.Update, error)
	Send(context.Context, tgbotapi.Chattable) (tgbotapi.Message, error)
	Request(context.Context, tgbotapi.Chattable) (*tgbotapi.APIResponse, error)
	GetFile(context.Context, tgbotapi.FileConfig) (tgbotapi.File, error)
	GetChatAdministrators(context.Context, tgbotapi.ChatAdministratorsConfig) ([]tgbotapi.ChatMember, error)
}

type productionBotAPI struct {
	updates       *tgbotapi.BotAPI
	requests      *tgbotapi.BotAPI
	updateClient  *contextHTTPClient
	updateMu      sync.Mutex
	requestClient *contextHTTPClient
	requestMu     sync.Mutex
}

type contextHTTPClient struct {
	base    tgbotapi.HTTPClient
	ctx     context.Context
	secrets []string
}

func newProductionBotAPI(cfg Config) (*productionBotAPI, error) {
	endpoint := cfg.APIBaseURL + "/bot%s/%s"
	updateClient := &contextHTTPClient{
		base:    &http.Client{Timeout: cfg.RequestTimeout},
		secrets: []string{cfg.Token},
	}
	updates, err := tgbotapi.NewBotAPIWithClient(cfg.Token, endpoint, updateClient)
	if err != nil {
		return nil, secret.RedactError(err, cfg.Token)
	}

	requestClient := &contextHTTPClient{
		base:    &http.Client{Timeout: cfg.RequestTimeout},
		secrets: []string{cfg.Token},
	}
	requests := *updates
	requests.Client = requestClient

	updates.Debug = cfg.Debug
	requests.Debug = cfg.Debug
	return &productionBotAPI{
		updates:       updates,
		requests:      &requests,
		updateClient:  updateClient,
		requestClient: requestClient,
	}, nil
}

func (b *productionBotAPI) GetUpdates(ctx context.Context, config tgbotapi.UpdateConfig) ([]tgbotapi.Update, error) {
	var updates []tgbotapi.Update
	err := b.withUpdateContext(ctx, func() error {
		var err error
		updates, err = b.updates.GetUpdates(config)
		return err
	})
	return updates, err
}

func (b *productionBotAPI) Send(ctx context.Context, msg tgbotapi.Chattable) (tgbotapi.Message, error) {
	var message tgbotapi.Message
	err := b.withRequestContext(ctx, func() error {
		var err error
		message, err = b.requests.Send(msg)
		return err
	})
	return message, err
}

func (b *productionBotAPI) Request(ctx context.Context, req tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	var response *tgbotapi.APIResponse
	err := b.withRequestContext(ctx, func() error {
		var err error
		response, err = b.requests.Request(req)
		return err
	})
	return response, err
}

func (b *productionBotAPI) GetFile(ctx context.Context, config tgbotapi.FileConfig) (tgbotapi.File, error) {
	var file tgbotapi.File
	err := b.withRequestContext(ctx, func() error {
		var err error
		file, err = b.requests.GetFile(config)
		return err
	})
	return file, err
}

func (b *productionBotAPI) GetChatAdministrators(ctx context.Context, config tgbotapi.ChatAdministratorsConfig) ([]tgbotapi.ChatMember, error) {
	var admins []tgbotapi.ChatMember
	err := b.withRequestContext(ctx, func() error {
		var err error
		admins, err = b.requests.GetChatAdministrators(config)
		return err
	})
	return admins, err
}

func (b *productionBotAPI) withUpdateContext(ctx context.Context, call func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	b.updateMu.Lock()
	defer b.updateMu.Unlock()
	b.updateClient.ctx = ctx
	defer func() {
		b.updateClient.ctx = nil
	}()
	return call()
}

func (b *productionBotAPI) withRequestContext(ctx context.Context, call func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	b.requestMu.Lock()
	defer b.requestMu.Unlock()
	b.requestClient.ctx = ctx
	defer func() {
		b.requestClient.ctx = nil
	}()
	return call()
}

func (c *contextHTTPClient) Do(req *http.Request) (*http.Response, error) {
	if c.ctx != nil {
		req = req.WithContext(c.ctx)
	}
	resp, err := c.base.Do(req)
	return resp, secret.RedactError(err, c.secrets...)
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		delay = defaultPollRetryDelay
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) redactError(err error) error {
	return secret.RedactError(err, s.cfg.Token)
}
