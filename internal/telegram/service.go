package telegram

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/tiwb/tg-obs-bot/internal/liveness"
	"github.com/tiwb/tg-obs-bot/internal/secret"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	defaultUpdateTimeout           = 30
	defaultRequestTimeout          = time.Duration(defaultUpdateTimeout)*time.Second + 5*time.Second
	defaultPollRetryDelay          = 3 * time.Second
	defaultUpdateProcessingTimeout = 5 * time.Minute
	defaultUpdateHandlerStopGrace  = 5 * time.Second
	defaultJournalWriteTimeout     = 4 * time.Second
	defaultUpdateLeaseBuffer       = 30 * time.Second
	defaultStuckOwnerLease         = 10 * time.Second
	defaultAdminLookupTimeout      = 5 * time.Second
	adminCacheTTL                  = 60 * time.Second

	// MaxThemeRunes and MaxThemeUTF8Bytes keep operator-provided themes
	// comfortably below Telegram's 4096-character message limit while still
	// accepting the longest theme that fits the repository's tested 255-byte
	// loop filename boundary (loop_day_<theme>_v.mp4).
	MaxThemeRunes     = 240
	MaxThemeUTF8Bytes = 240

	// RecommendedParentDrainGrace reserves enough time for a handler that
	// ignores cancellation to consume its stop grace and for the update
	// journal to persist Abort/Fail in an independent bounded context, plus a
	// small scheduling cushion. The app coordinator uses this as its outer
	// worker-drain budget.
	RecommendedParentDrainGrace = defaultUpdateHandlerStopGrace + defaultJournalWriteTimeout + time.Second
)

var ErrUpdateHandlerStuck = errors.New("telegram update handler did not stop after cancellation")

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
	bot                     botAPI
	cfg                     Config
	hooks                   Hooks
	logger                  *slog.Logger
	now                     func() time.Time
	pollRetryDelay          time.Duration
	pollRetryMaxDelay       time.Duration
	pollProviderHintMax     time.Duration
	retryRandom             func() uint64
	pollSleep               func(context.Context, time.Duration) error
	updateProcessingTimeout time.Duration
	updateHandlerStopGrace  time.Duration
	journalWriteTimeout     time.Duration
	updateLeaseBuffer       time.Duration
	stuckOwnerLease         time.Duration
	adminLookupTimeout      time.Duration
	adminCacheMutex         sync.Mutex
	adminCache              map[int64]adminCacheEntry
	updateJournal           UpdateJournal
	updateHandler           func(context.Context, tgbotapi.Update) error
}

type adminCacheEntry struct {
	adminIDs  map[int64]struct{}
	expiresAt time.Time
}

type Hooks struct {
	PreflightUpload func(context.Context, Upload) error
	ImportUpload    ImportUploadFunc
	LibraryPage     PageFunc
	Scan            SimpleFunc
	Preview         SimpleFunc
	SetTheme        TextFunc
	SelectLoop      TextFunc
	SkipLoop        SimpleFunc
	SkipMusic       SimpleFunc
	Now             SimpleFunc
	Status          SimpleFunc
}

type ImportUploadFunc func(context.Context, Upload) (string, error)
type SimpleFunc func(context.Context) (string, error)
type TextFunc func(context.Context, string) (string, error)
type PageFunc func(context.Context, int) (LibraryPageResult, error)

type LibraryPageResult struct {
	Text       string
	Page       int
	TotalPages int
}

type botResponse struct {
	text   string
	markup *tgbotapi.InlineKeyboardMarkup
}

type actionableUpdate struct {
	update     tgbotapi.Update
	nextOffset int
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
	UploadKindAudio    UploadKind = "audio"
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
		cfg:                     cfg,
		hooks:                   hooks,
		logger:                  logger,
		now:                     time.Now,
		pollRetryDelay:          defaultPollRetryDelay,
		pollRetryMaxDelay:       defaultPollRetryMaxDelay,
		pollProviderHintMax:     defaultPollProviderHintMax,
		updateProcessingTimeout: defaultUpdateProcessingTimeout,
		updateHandlerStopGrace:  defaultUpdateHandlerStopGrace,
		journalWriteTimeout:     defaultJournalWriteTimeout,
		updateLeaseBuffer:       defaultUpdateLeaseBuffer,
		stuckOwnerLease:         defaultStuckOwnerLease,
		adminLookupTimeout:      defaultAdminLookupTimeout,
		adminCache:              make(map[int64]adminCacheEntry),
	}
	for _, opt := range opts {
		opt(s)
	}
	if s.updateJournal == nil {
		return nil, errors.New("telegram update journal is required")
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

func WithUpdateJournal(journal UpdateJournal) Option {
	return func(s *Service) {
		s.updateJournal = journal
	}
}

func (s *Service) registerCommands(ctx context.Context) error {
	publicCommands := []tgbotapi.BotCommand{
		{Command: "library", Description: "查看媒體庫"},
		{Command: "preview", Description: "預覽目前候選"},
		{Command: "now", Description: "查看目前播放"},
		{Command: "status", Description: "查看服務狀態"},
		{Command: "help", Description: "顯示說明"},
	}
	adminCommands := []tgbotapi.BotCommand{
		{Command: "library", Description: "查看媒體庫"},
		{Command: "scan", Description: "掃描媒體庫"},
		{Command: "preview", Description: "預覽目前候選"},
		{Command: "theme", Description: "設定主題"},
		{Command: "select", Description: "選擇循環素材"},
		{Command: "skip", Description: "略過循環或音樂"},
		{Command: "now", Description: "查看目前播放"},
		{Command: "status", Description: "查看服務狀態"},
		{Command: "help", Description: "顯示說明"},
	}

	if err := s.request(ctx, tgbotapi.NewSetMyCommandsWithScope(tgbotapi.NewBotCommandScopeChat(s.cfg.AllowedChatID), publicCommands...)); err != nil {
		return err
	}
	return s.request(ctx, tgbotapi.NewSetMyCommandsWithScope(tgbotapi.NewBotCommandScopeChatAdministrators(s.cfg.AllowedChatID), adminCommands...))
}

func (s *Service) Run(ctx context.Context) error {
	tracker := liveness.WorkerFromContext(ctx)
	tracker.Advance(liveness.PhaseOperation)
	if err := ctx.Err(); err != nil {
		tracker.Advance(liveness.PhaseCancelWait)
		return err
	}
	ownerToken, err := newUpdateOwnerToken()
	if err != nil {
		return journalFailure("create update owner token", err)
	}
	journalCtx, cancelJournal := s.journalContext(ctx)
	nextOffset, confirmedOffset, err := s.updateJournal.LoadUpdateCheckpoint(journalCtx)
	cancelJournal()
	if err != nil {
		return journalOperationError(ctx, "load polling checkpoint", err)
	}
	if nextOffset < 0 || confirmedOffset < 0 || confirmedOffset > nextOffset {
		return journalFailure(
			"load polling checkpoint",
			fmt.Errorf(
				"invalid offsets: next_offset=%d confirmed_offset=%d",
				nextOffset,
				confirmedOffset,
			),
		)
	}

	if err := s.registerCommands(ctx); err != nil {
		s.logger.Warn("register telegram commands", "error", s.redactError(err))
	}

	updateConfig := tgbotapi.NewUpdate(nextOffset)
	updateConfig.Timeout = s.cfg.UpdateTimeout
	updateConfig.Limit = 1
	pollRetry := s.newPollRetryState()

	for {
		if err := ctx.Err(); err != nil {
			tracker.Advance(liveness.PhaseCancelWait)
			return ctx.Err()
		}

		tracker.Advance(liveness.PhaseOperation)
		updates, err := s.bot.GetUpdates(ctx, updateConfig)
		var actionableUpdates []actionableUpdate
		if err == nil {
			tracker.Advance(liveness.PhaseOperation)
			actionableUpdates, err = validateActionableUpdates(updates, updateConfig.Offset)
		}
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				tracker.Advance(liveness.PhaseCancelWait)
				return ctxErr
			}
			hint := telegramRetryHint(err, s.pollProviderHintMax)
			attempt, sample := pollRetry.failure(hint)
			s.logPollFailure(err, attempt, sample)
			if err := s.waitPollRetry(ctx, tracker, attempt.Delay); err != nil {
				return err
			}
			continue
		}
		s.logPollRecovery(pollRetry.recovery())
		journalCtx, cancelJournal = s.journalContext(ctx)
		err = s.updateJournal.ConfirmUpdateOffset(journalCtx, updateConfig.Offset)
		cancelJournal()
		if err != nil {
			return journalOperationError(ctx, "confirm polling offset", err)
		}

		for _, candidate := range actionableUpdates {
			update := candidate.update
			nextOffset := candidate.nextOffset
			if err := ctx.Err(); err != nil {
				tracker.Advance(liveness.PhaseCancelWait)
				return err
			}
			tracker.Advance(liveness.PhaseOperation)
			if update.UpdateID < updateConfig.Offset {
				continue
			}
			metadata := metadataForUpdate(update)
			journalCtx, cancelJournal = s.journalContext(ctx)
			disposition, attemptCount, failureCount, err := s.updateJournal.BeginUpdateAttempt(
				journalCtx,
				update.UpdateID,
				metadata.kind,
				metadata.action,
				metadata.chatID,
				metadata.messageID,
				metadata.actorID,
				ownerToken,
				s.updateLeaseUntil(),
				maxUpdateHandlerFailures,
			)
			cancelJournal()
			if err != nil {
				return journalOperationError(ctx, "begin update attempt", err)
			}
			switch disposition {
			case updateBeginAlreadyTerminal, updateBeginDead:
				updateConfig.Offset = nextOffset
				continue
			case updateBeginBusy:
				return fmt.Errorf("%w: update_id=%d", ErrUpdateAttemptBusy, update.UpdateID)
			case updateBeginExecute:
			default:
				return journalFailure(
					"begin update attempt",
					fmt.Errorf("unknown disposition %q", disposition),
				)
			}

			timedOut, err := s.handleUpdateBounded(ctx, update)
			if err != nil {
				cause := boundedJournalError(s.redactError(err))
				switch {
				case errors.Is(err, ErrUpdateHandlerStuck):
					dead, journalErr := s.recordUpdateFailure(
						update.UpdateID,
						nextOffset,
						ownerToken,
						cause,
						s.now().Add(s.effectiveStuckOwnerLease()),
					)
					if journalErr != nil {
						return errors.Join(err, journalErr)
					}
					if dead {
						s.logPoisonUpdate(update.UpdateID, attemptCount, failureCount+1, metadata, cause)
					}
					// A stuck handler may still own application locks. Even
					// after the third failure dead-letters the update, this
					// process generation must stop and never poll again.
					return err
				case ctx.Err() != nil:
					if journalErr := s.abortUpdateAttempt(update.UpdateID, ownerToken); journalErr != nil {
						return errors.Join(err, journalErr)
					}
					return err
				default:
					dead, journalErr := s.recordUpdateFailure(
						update.UpdateID,
						nextOffset,
						ownerToken,
						cause,
						time.Time{},
					)
					if journalErr != nil {
						return errors.Join(err, journalErr)
					}
					if !dead {
						return err
					}
					s.logPoisonUpdate(update.UpdateID, attemptCount, failureCount+1, metadata, cause)
					if errors.Is(err, ErrUpdateHandlerPanic) {
						// A recovered panic may have left unrelated in-process
						// state inconsistent. Persist quarantine atomically,
						// then let the supervisor replace this generation;
						// only the next generation may poll past it.
						return err
					}
					updateConfig.Offset = nextOffset
					continue
				}
			}
			if timedOut {
				s.logger.Warn("telegram update processing timed out", "update_id", update.UpdateID)
			}
			if err := s.completeUpdateAttempt(update.UpdateID, nextOffset, ownerToken); err != nil {
				return err
			}
			// The done journal row and durable next offset commit atomically.
			// Telegram itself confirms that offset only on the next successful
			// GetUpdates request, when confirmed_offset advances separately.
			updateConfig.Offset = nextOffset
		}
	}
}

func validateActionableUpdates(
	updates []tgbotapi.Update,
	currentOffset int,
) ([]actionableUpdate, error) {
	actionable := make([]actionableUpdate, 0, len(updates))
	for _, update := range updates {
		nextOffset, err := nextTelegramOffset(update.UpdateID)
		if err != nil {
			return nil, fmt.Errorf("invalid Telegram update response: %w", err)
		}
		if update.UpdateID < currentOffset {
			continue
		}
		actionable = append(actionable, actionableUpdate{
			update:     update,
			nextOffset: nextOffset,
		})
	}
	return actionable, nil
}

func nextTelegramOffset(updateID int) (int, error) {
	if updateID < 0 {
		return 0, fmt.Errorf("Telegram update ID must be non-negative: %d", updateID)
	}
	if updateID == math.MaxInt {
		return 0, fmt.Errorf("Telegram update ID has no representable successor: %d", updateID)
	}
	return updateID + 1, nil
}

func (s *Service) updateLeaseUntil() time.Time {
	processingTimeout := s.updateProcessingTimeout
	if processingTimeout <= 0 {
		processingTimeout = defaultUpdateProcessingTimeout
	}
	stopGrace := s.updateHandlerStopGrace
	if stopGrace <= 0 {
		stopGrace = defaultUpdateHandlerStopGrace
	}
	buffer := s.updateLeaseBuffer
	if buffer <= 0 {
		buffer = defaultUpdateLeaseBuffer
	}
	return s.now().Add(processingTimeout + stopGrace + buffer)
}

func (s *Service) effectiveStuckOwnerLease() time.Duration {
	if s.stuckOwnerLease <= 0 {
		return defaultStuckOwnerLease
	}
	return s.stuckOwnerLease
}

func (s *Service) journalContext(parent context.Context) (context.Context, context.CancelFunc) {
	timeout := s.journalWriteTimeout
	if timeout <= 0 {
		timeout = defaultJournalWriteTimeout
	}
	return context.WithTimeout(parent, timeout)
}

func (s *Service) independentJournalContext() (context.Context, context.CancelFunc) {
	return s.journalContext(context.Background())
}

func (s *Service) abortUpdateAttempt(updateID int, ownerToken string) error {
	journalCtx, cancel := s.independentJournalContext()
	defer cancel()
	if err := s.updateJournal.AbortUpdateAttempt(journalCtx, updateID, ownerToken); err != nil {
		return journalFailure("abort canceled update attempt", err)
	}
	return nil
}

func (s *Service) completeUpdateAttempt(updateID int, nextOffset int, ownerToken string) error {
	journalCtx, cancel := s.independentJournalContext()
	err := s.updateJournal.CompleteUpdateAttempt(
		journalCtx,
		updateID,
		nextOffset,
		ownerToken,
	)
	cancel()
	if err == nil {
		return nil
	}

	// A failed terminal transaction must remain at-least-once replayable, but
	// it must not retain the normal multi-minute processing lease. Release the
	// still-owned claim in a fresh bounded context so a supervisor restart can
	// replay immediately. If the commit actually succeeded despite an
	// ambiguous driver error, Abort will safely fail its owner/status guard and
	// the terminal row will repair the checkpoint on the next Begin.
	completeErr := journalFailure("complete update attempt", err)
	if abortErr := s.abortUpdateAttempt(updateID, ownerToken); abortErr != nil {
		return errors.Join(completeErr, abortErr)
	}
	return completeErr
}

func (s *Service) recordUpdateFailure(
	updateID int,
	nextOffset int,
	ownerToken string,
	cause string,
	holdLeaseUntil time.Time,
) (bool, error) {
	journalCtx, cancel := s.independentJournalContext()
	defer cancel()
	dead, err := s.updateJournal.FailUpdateAttempt(
		journalCtx,
		updateID,
		nextOffset,
		ownerToken,
		maxUpdateHandlerFailures,
		cause,
		holdLeaseUntil,
	)
	if err != nil {
		return false, journalFailure("record failed update attempt", err)
	}
	return dead, nil
}

func (s *Service) logPoisonUpdate(
	updateID int,
	attemptCount int,
	failureCount int,
	metadata updateMetadata,
	cause string,
) {
	s.logger.Error(
		"Telegram update quarantined after repeated handler failures",
		"update_id", updateID,
		"attempt_count", attemptCount,
		"failure_count", failureCount,
		"action", metadata.action,
		"error", cause,
	)
}

func (s *Service) handleUpdateBounded(ctx context.Context, update tgbotapi.Update) (bool, error) {
	updateTimeout := s.updateProcessingTimeout
	if updateTimeout <= 0 {
		updateTimeout = defaultUpdateProcessingTimeout
	}
	stopGrace := s.updateHandlerStopGrace
	if stopGrace <= 0 {
		stopGrace = defaultUpdateHandlerStopGrace
	}

	updateCtx, cancel := context.WithTimeout(ctx, updateTimeout)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		var handlerErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				handlerErr = fmt.Errorf("%w: %v", ErrUpdateHandlerPanic, recovered)
			}
			done <- handlerErr
		}()
		if s.updateHandler != nil {
			handlerErr = s.updateHandler(updateCtx, update)
			return
		}
		s.handleUpdate(updateCtx, update)
	}()

	select {
	case handlerErr := <-done:
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return errors.Is(updateCtx.Err(), context.DeadlineExceeded), handlerErr
	case <-updateCtx.Done():
	}

	liveness.WorkerFromContext(ctx).Advance(liveness.PhaseCancelWait)
	stopTimer := time.NewTimer(stopGrace)
	defer stopTimer.Stop()
	select {
	case handlerErr := <-done:
		if err := ctx.Err(); err != nil {
			return false, err
		}
		return errors.Is(updateCtx.Err(), context.DeadlineExceeded), handlerErr
	case <-stopTimer.C:
		cause := updateCtx.Err()
		if cause == nil {
			cause = context.Canceled
		}
		return false, errors.Join(
			cause,
			fmt.Errorf("%w: update_id=%d grace=%s", ErrUpdateHandlerStuck, update.UpdateID, stopGrace),
		)
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

	if msg.Video != nil || msg.Document != nil || msg.Audio != nil {
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
	switch command {
	case "start", "help", "library", "preview", "scan", "theme", "select", "now", "status", "skip":
	default:
		return botResponse{
			text:   "我不認得這個指令。請試 /library、/preview、/now、/status 或 /help。",
			markup: homeKeyboard(false),
		}, nil
	}

	admin := false
	if requiresFreshAdmin(command) {
		admin = s.isAdminFresh(ctx, msg.Chat.ID, msg.From)
	} else {
		admin = s.isAdmin(ctx, msg.Chat.ID, msg.From)
	}
	switch command {
	case "start", "help":
		return botResponse{text: helpText(admin), markup: homeKeyboard(admin)}, nil
	case "library":
		page, err := parseOptionalPageArg(args, "library")
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromLibrary(ctx, admin, page)
	case "preview":
		return s.responseFromSimple(ctx, "preview", s.hooks.Preview, previewKeyboard(admin))
	case "scan":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		return s.responseFromSimple(ctx, "scan library", s.hooks.Scan, libraryKeyboard(admin))
	case "theme":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		theme, err := parseRawTextArg(msg.CommandArguments(), "theme", "<theme|random>")
		if err != nil {
			return botResponse{}, err
		}
		if err := validateThemeArgument(theme); err != nil {
			return botResponse{}, err
		}
		return s.responseFromText(ctx, "set theme", s.hooks.SetTheme, theme, libraryKeyboard(admin))
	case "select":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		assetID, err := parseTextArg(args, "select", "<asset_id|clear>")
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromText(ctx, "select loop", s.hooks.SelectLoop, assetID, libraryKeyboard(admin))
	case "now":
		return s.responseFromSimple(ctx, "current video", s.hooks.Now, nowKeyboard(admin))
	case "status":
		return s.responseFromSimple(ctx, "status", s.hooks.Status, statusKeyboard(admin))
	case "skip":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		return s.responseFromSkip(ctx, args, admin)
	}
	return botResponse{}, nil
}

func (s *Service) routeAction(ctx context.Context, chatID int64, user *tgbotapi.User, data string) (botResponse, error) {
	parts := strings.Fields(strings.ReplaceAll(data, ":", " "))
	if len(parts) == 0 {
		return botResponse{}, nil
	}

	action := strings.ToLower(parts[0])
	switch action {
	case "library", "preview", "scan", "theme", "select", "now", "status":
	case "skip":
		// The removed queue UI used the bare "skip" callback. The library UI
		// emits only skip:loop or skip:music, so an argument-free callback is
		// stale and must have no authorization lookup or playback side effect.
		if len(parts) == 1 {
			return botResponse{}, nil
		}
	default:
		return botResponse{}, nil
	}

	admin := false
	if requiresFreshAdmin(action) {
		admin = s.isAdminFresh(ctx, chatID, user)
	} else {
		admin = s.isAdmin(ctx, chatID, user)
	}
	switch action {
	case "library":
		page, err := parseOptionalPageArg(parts[1:], "library")
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromLibrary(ctx, admin, page)
	case "preview":
		return s.responseFromSimple(ctx, "preview", s.hooks.Preview, previewKeyboard(admin))
	case "scan":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		return s.responseFromSimple(ctx, "scan library", s.hooks.Scan, libraryKeyboard(admin))
	case "theme":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		theme, err := parseJoinedTextArg(parts[1:], "theme", "<theme|random>")
		if err != nil {
			return botResponse{}, err
		}
		if err := validateThemeArgument(theme); err != nil {
			return botResponse{}, err
		}
		return s.responseFromText(ctx, "set theme", s.hooks.SetTheme, theme, libraryKeyboard(admin))
	case "select":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		assetID, err := parseTextArg(parts[1:], "select", "<asset_id|clear>")
		if err != nil {
			return botResponse{}, err
		}
		return s.responseFromText(ctx, "select loop", s.hooks.SelectLoop, assetID, libraryKeyboard(admin))
	case "now":
		return s.responseFromSimple(ctx, "current video", s.hooks.Now, nowKeyboard(admin))
	case "status":
		return s.responseFromSimple(ctx, "status", s.hooks.Status, statusKeyboard(admin))
	case "skip":
		if !admin {
			return botResponse{}, errAdminOnly
		}
		return s.responseFromSkip(ctx, parts[1:], admin)
	}
	return botResponse{}, nil
}

func (s *Service) handleUpload(ctx context.Context, msg *tgbotapi.Message) (botResponse, error) {
	if !s.isAdminFresh(ctx, msg.Chat.ID, msg.From) {
		if err := ctx.Err(); err != nil {
			return botResponse{}, err
		}
		return botResponse{}, errAdminOnly
	}
	if s.hooks.ImportUpload == nil {
		return botResponse{}, errHookNotConfigured("import upload")
	}

	upload, err := uploadFromMessage(msg)
	if err != nil {
		return botResponse{}, err
	}
	if s.cfg.MaxUploadSizeBytes > 0 && upload.SizeBytes > s.cfg.MaxUploadSizeBytes {
		return botResponse{}, fmt.Errorf("%w: %s is larger than the limit of %s", errUploadTooLarge, formatBytes(upload.SizeBytes), formatBytes(s.cfg.MaxUploadSizeBytes))
	}
	if s.hooks.PreflightUpload == nil {
		return botResponse{}, errHookNotConfigured("preflight upload")
	}
	if err := s.hooks.PreflightUpload(ctx, upload); err != nil {
		return botResponse{}, err
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

	response, err := s.hooks.ImportUpload(ctx, upload)
	if err != nil {
		return botResponse{}, err
	}
	if strings.TrimSpace(response) == "" {
		response = fmt.Sprintf("已匯入媒體庫：%s。", displayName(upload))
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
		if !looksLikeMediaDocument(upload.FileName, upload.MimeType) {
			return Upload{}, errUnsupportedUpload
		}
		return upload, nil
	}

	if msg.Audio != nil {
		upload.Kind = UploadKindAudio
		upload.FileID = msg.Audio.FileID
		upload.FileUniqueID = msg.Audio.FileUniqueID
		upload.FileName = strings.TrimSpace(msg.Audio.FileName)
		upload.MimeType = strings.TrimSpace(msg.Audio.MimeType)
		upload.SizeBytes = int64(msg.Audio.FileSize)
		upload.DurationSeconds = msg.Audio.Duration
		if upload.FileName == "" {
			upload.FileName = defaultUploadName(upload.FileUniqueID, ".mp3")
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

func (s *Service) callText(ctx context.Context, name string, hook TextFunc, value string) (string, error) {
	if hook == nil {
		return "", errHookNotConfigured(name)
	}
	return hook(ctx, value)
}

func (s *Service) callPage(ctx context.Context, name string, hook PageFunc, page int) (LibraryPageResult, error) {
	if hook == nil {
		return LibraryPageResult{}, errHookNotConfigured(name)
	}
	result, err := hook(ctx, page)
	if err != nil {
		return LibraryPageResult{}, err
	}
	if err := validateLibraryPageResult(result, page); err != nil {
		return LibraryPageResult{}, fmt.Errorf("%s: %w", name, err)
	}
	return result, nil
}

func (s *Service) responseFromSimple(ctx context.Context, name string, hook SimpleFunc, markup *tgbotapi.InlineKeyboardMarkup) (botResponse, error) {
	text, err := s.callSimple(ctx, name, hook)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: text, markup: markup}, nil
}

func (s *Service) responseFromText(ctx context.Context, name string, hook TextFunc, value string, markup *tgbotapi.InlineKeyboardMarkup) (botResponse, error) {
	text, err := s.callText(ctx, name, hook, value)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: text, markup: markup}, nil
}

func (s *Service) responseFromLibrary(ctx context.Context, admin bool, page int) (botResponse, error) {
	result, err := s.callPage(ctx, "library page", s.hooks.LibraryPage, page)
	if err != nil {
		return botResponse{}, err
	}
	return botResponse{text: result.Text, markup: libraryPageKeyboard(admin, result)}, nil
}

func (s *Service) responseFromSkip(ctx context.Context, args []string, admin bool) (botResponse, error) {
	if len(args) != 1 {
		return botResponse{}, fmt.Errorf("%w: use /skip loop or /skip music", errBadCommand)
	}
	switch strings.ToLower(args[0]) {
	case "loop":
		return s.responseFromSimple(ctx, "skip loop", s.hooks.SkipLoop, libraryKeyboard(admin))
	case "music":
		return s.responseFromSimple(ctx, "skip music", s.hooks.SkipMusic, libraryKeyboard(admin))
	default:
		return botResponse{}, fmt.Errorf("%w: use /skip loop or /skip music", errBadCommand)
	}
}

func validateLibraryPageResult(result LibraryPageResult, requestedPage int) error {
	switch {
	case strings.TrimSpace(result.Text) == "":
		return errors.New("page text is empty")
	case result.Page <= 0:
		return fmt.Errorf("page must be positive, got %d", result.Page)
	case result.TotalPages <= 0:
		return fmt.Errorf("total pages must be positive, got %d", result.TotalPages)
	case result.Page > result.TotalPages:
		return fmt.Errorf("page %d exceeds total pages %d", result.Page, result.TotalPages)
	case result.Page != requestedPage:
		return fmt.Errorf("returned page %d does not match requested page %d", result.Page, requestedPage)
	default:
		return nil
	}
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
	_, ok := adminIDs[userID]
	return ok
}

func (s *Service) isAdminFresh(ctx context.Context, chatID int64, user *tgbotapi.User) bool {
	if user == nil {
		return false
	}
	timeout := s.adminLookupTimeout
	if timeout <= 0 {
		timeout = defaultAdminLookupTimeout
	}
	lookupCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	adminIDs, _, err := s.getAdminIDs(lookupCtx, chatID, true)
	if err != nil {
		s.logger.Warn("refresh telegram chat administrators", "chat_id", chatID, "error", s.redactError(err))
		return false
	}
	_, ok := adminIDs[int64(user.ID)]
	return ok
}

func requiresFreshAdmin(action string) bool {
	switch action {
	case "scan", "theme", "select", "skip":
		return true
	default:
		return false
	}
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

func parseTextArg(args []string, command string, placeholder string) (string, error) {
	if len(args) != 1 || strings.TrimSpace(args[0]) == "" {
		return "", fmt.Errorf("%w: use /%s %s", errBadCommand, command, placeholder)
	}
	return strings.TrimSpace(args[0]), nil
}

func parseRawTextArg(raw string, command string, placeholder string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("%w: use /%s %s", errBadCommand, command, placeholder)
	}
	return value, nil
}

func parseJoinedTextArg(args []string, command string, placeholder string) (string, error) {
	return parseRawTextArg(strings.Join(args, " "), command, placeholder)
}

func validateThemeArgument(theme string) error {
	if !utf8.ValidString(theme) {
		return fmt.Errorf("%w: theme must be valid UTF-8", errBadCommand)
	}
	if utf8.RuneCountInString(theme) > MaxThemeRunes || len(theme) > MaxThemeUTF8Bytes {
		return fmt.Errorf(
			"%w: theme must be at most %d characters and %d UTF-8 bytes",
			errBadCommand,
			MaxThemeRunes,
			MaxThemeUTF8Bytes,
		)
	}
	return nil
}

func parseOptionalPageArg(args []string, command string) (int, error) {
	if len(args) == 0 {
		return 1, nil
	}
	if len(args) != 1 {
		return 0, fmt.Errorf("%w: use /%s [page]", errBadCommand, command)
	}
	page, err := strconv.Atoi(args[0])
	if err != nil || page <= 0 {
		return 0, fmt.Errorf("%w: page must be a positive number", errBadCommand)
	}
	return page, nil
}

func looksLikeMediaDocument(name string, mimeType string) bool {
	mimeType = strings.ToLower(strings.TrimSpace(mimeType))
	if strings.HasPrefix(mimeType, "video/") || strings.HasPrefix(mimeType, "audio/") {
		return true
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp4", ".mov", ".m4v", ".mkv", ".webm", ".avi", ".mp3", ".m4a", ".aac", ".wav", ".flac", ".ogg", ".oga", ".opus":
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
		ext = ".media"
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
		"傳送影片或音訊檔給我，管理員可匯入媒體庫。",
		"",
		"/library [page] - 分頁查看媒體庫",
		"/preview - 預覽目前候選",
		"/now - 目前播放",
		"/status - 系統狀態",
		"/help - 顯示說明",
	}
	if admin {
		lines = append(lines,
			"",
			"Admin:",
			"/scan - 掃描媒體庫",
			"/theme <theme|random> - 設定主題",
			"/select <asset_id|clear> - 選擇循環素材",
			"/skip loop - 略過循環",
			"/skip music - 略過音樂",
		)
	}
	return strings.Join(lines, "\n")
}

func homeKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("媒體庫", "library"),
			tgbotapi.NewInlineKeyboardButtonData("預覽", "preview"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("現在", "now"),
			tgbotapi.NewInlineKeyboardButtonData("狀態", "status"),
		),
	}
	if admin {
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("掃描", "scan"),
				tgbotapi.NewInlineKeyboardButtonData("隨機主題", "theme:random"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("清除選取", "select:clear"),
				tgbotapi.NewInlineKeyboardButtonData("略過循環", "skip:loop"),
				tgbotapi.NewInlineKeyboardButtonData("略過音樂", "skip:music"),
			),
		)
	}
	return inlineKeyboard(rows...)
}

func libraryKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	return libraryPageKeyboard(admin, LibraryPageResult{Page: 1, TotalPages: 1})
}

func libraryPageKeyboard(admin bool, result LibraryPageResult) *tgbotapi.InlineKeyboardMarkup {
	currentPage, totalPages := result.Page, result.TotalPages
	refreshData := "library"
	if currentPage > 1 {
		refreshData = fmt.Sprintf("library:%d", currentPage)
	}
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("刷新", refreshData),
			tgbotapi.NewInlineKeyboardButtonData("預覽", "preview"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("現在", "now"),
			tgbotapi.NewInlineKeyboardButtonData("狀態", "status"),
		),
	}
	if totalPages > 1 {
		pageRow := make([]tgbotapi.InlineKeyboardButton, 0, 2)
		if currentPage > 1 {
			pageRow = append(pageRow, tgbotapi.NewInlineKeyboardButtonData("上一頁", fmt.Sprintf("library:%d", currentPage-1)))
		}
		if currentPage < totalPages {
			pageRow = append(pageRow, tgbotapi.NewInlineKeyboardButtonData("下一頁", fmt.Sprintf("library:%d", currentPage+1)))
		}
		if len(pageRow) > 0 {
			rows = append(rows, pageRow)
		}
	}
	if admin {
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("掃描", "scan"),
				tgbotapi.NewInlineKeyboardButtonData("隨機主題", "theme:random"),
			),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("清除選取", "select:clear"),
				tgbotapi.NewInlineKeyboardButtonData("略過循環", "skip:loop"),
				tgbotapi.NewInlineKeyboardButtonData("略過音樂", "skip:music"),
			),
		)
	}
	return inlineKeyboard(rows...)
}

func previewKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("刷新", "preview"),
			tgbotapi.NewInlineKeyboardButtonData("媒體庫", "library"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("現在", "now"),
			tgbotapi.NewInlineKeyboardButtonData("狀態", "status"),
		),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("略過循環", "skip:loop"),
			tgbotapi.NewInlineKeyboardButtonData("略過音樂", "skip:music"),
		))
	}
	return inlineKeyboard(rows...)
}

func nowKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("媒體庫", "library"),
			tgbotapi.NewInlineKeyboardButtonData("狀態", "status"),
		),
		tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("預覽", "preview")),
	}
	if admin {
		rows = append(rows, tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("略過循環", "skip:loop"),
			tgbotapi.NewInlineKeyboardButtonData("略過音樂", "skip:music"),
		))
	}
	return inlineKeyboard(rows...)
}

func statusKeyboard(admin bool) *tgbotapi.InlineKeyboardMarkup {
	rows := [][]tgbotapi.InlineKeyboardButton{
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("刷新", "status"),
			tgbotapi.NewInlineKeyboardButtonData("媒體庫", "library"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("預覽", "preview"),
			tgbotapi.NewInlineKeyboardButtonData("現在", "now"),
		),
	}
	if admin {
		rows = append(rows,
			tgbotapi.NewInlineKeyboardRow(tgbotapi.NewInlineKeyboardButtonData("掃描", "scan")),
			tgbotapi.NewInlineKeyboardRow(
				tgbotapi.NewInlineKeyboardButtonData("略過循環", "skip:loop"),
				tgbotapi.NewInlineKeyboardButtonData("略過音樂", "skip:music"),
			),
		)
	}
	return inlineKeyboard(rows...)
}

func uploadAcceptedKeyboard() *tgbotapi.InlineKeyboardMarkup {
	return inlineKeyboard(
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("媒體庫", "library"),
			tgbotapi.NewInlineKeyboardButtonData("預覽", "preview"),
		),
		tgbotapi.NewInlineKeyboardRow(
			tgbotapi.NewInlineKeyboardButtonData("狀態", "status"),
			tgbotapi.NewInlineKeyboardButtonData("掃描", "scan"),
		),
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
	case "library", "preview", "now", "status":
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
		return "這個操作只有管理員可以使用。"
	case errors.Is(err, errBadCommand):
		return strings.TrimPrefix(err.Error(), errBadCommand.Error()+": ")
	case errors.Is(err, errUnsupportedUpload):
		return "請傳影片或音訊檔。"
	case errors.Is(err, errUploadTooLarge):
		return strings.TrimPrefix(err.Error(), errUploadTooLarge.Error()+": ")
	case errors.As(err, &public):
		return public.PublicMessage()
	default:
		return "抱歉，這次沒有完成。可用 /status 查看狀態，細節請看服務日誌。"
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
	updates        *tgbotapi.BotAPI
	requests       *tgbotapi.BotAPI
	updateClient   *contextHTTPClient
	updateGate     chan struct{}
	requestClient  *contextHTTPClient
	requestGate    chan struct{}
	requestTimeout time.Duration
}

type contextHTTPClient struct {
	base    tgbotapi.HTTPClient
	mu      sync.RWMutex
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
		updates:        updates,
		requests:       &requests,
		updateClient:   updateClient,
		updateGate:     newContextGate(),
		requestClient:  requestClient,
		requestGate:    newContextGate(),
		requestTimeout: cfg.RequestTimeout,
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
	return withContextGate(ctx, b.requestTimeout, b.updateGate, b.updateClient, call)
}

func (b *productionBotAPI) withRequestContext(ctx context.Context, call func() error) error {
	return withContextGate(ctx, b.requestTimeout, b.requestGate, b.requestClient, call)
}

func newContextGate() chan struct{} {
	gate := make(chan struct{}, 1)
	gate <- struct{}{}
	return gate
}

func withContextGate(ctx context.Context, timeout time.Duration, gate chan struct{}, client *contextHTTPClient, call func() error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if gate == nil {
		return errors.New("telegram context gate is not initialized")
	}
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	boundedCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-boundedCtx.Done():
		return boundedCtx.Err()
	case <-gate:
	}
	defer func() {
		gate <- struct{}{}
	}()
	if err := boundedCtx.Err(); err != nil {
		return err
	}

	client.setContext(boundedCtx)
	defer client.setContext(nil)
	return call()
}

func (c *contextHTTPClient) Do(req *http.Request) (*http.Response, error) {
	c.mu.RLock()
	ctx := c.ctx
	c.mu.RUnlock()
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	resp, err := c.base.Do(req)
	return resp, secret.RedactError(err, c.secrets...)
}

func (c *contextHTTPClient) setContext(ctx context.Context) {
	c.mu.Lock()
	c.ctx = ctx
	c.mu.Unlock()
}

func (s *Service) redactError(err error) error {
	return secret.RedactError(err, s.cfg.Token)
}
