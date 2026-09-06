package telegram

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	maxUpdateHandlerFailures = 3
	maxJournalKindBytes      = 32
	maxJournalActionBytes    = 64
	maxJournalErrorBytes     = 1024

	updateBeginExecute         = "execute"
	updateBeginAlreadyTerminal = "already_terminal"
	updateBeginBusy            = "busy"
	updateBeginDead            = "dead"
)

var (
	ErrUpdateJournalFailure = errors.New("telegram update journal failure")
	ErrUpdateHandlerPanic   = errors.New("telegram update handler panicked")
	ErrUpdateAttemptBusy    = errors.New("telegram update attempt is owned by another process")
)

type UpdateJournal interface {
	LoadUpdateCheckpoint(context.Context) (nextOffset int, confirmedOffset int, err error)
	ConfirmUpdateOffset(context.Context, int) error
	BeginUpdateAttempt(
		ctx context.Context,
		updateID int,
		updateKind string,
		action string,
		chatID int64,
		messageID int,
		actorID int64,
		ownerToken string,
		leaseUntil time.Time,
		maxFailures int,
	) (disposition string, attemptCount int, failureCount int, err error)
	CompleteUpdateAttempt(
		ctx context.Context,
		updateID int,
		nextOffset int,
		ownerToken string,
	) error
	AbortUpdateAttempt(ctx context.Context, updateID int, ownerToken string) error
	FailUpdateAttempt(
		ctx context.Context,
		updateID int,
		nextOffset int,
		ownerToken string,
		maxFailures int,
		cause string,
		holdLeaseUntil time.Time,
	) (dead bool, err error)
}

type updateMetadata struct {
	kind      string
	action    string
	chatID    int64
	messageID int
	actorID   int64
}

func metadataForUpdate(update tgbotapi.Update) (metadata updateMetadata) {
	// Action inference is optional journal context, not a reason to lose the
	// update. The Telegram dependency slices command text using entity lengths,
	// so malformed upstream entities must degrade to an empty action.
	defer func() {
		if recover() != nil {
			metadata.action = ""
		}
		metadata.kind = normalizeJournalLabel(metadata.kind, maxJournalKindBytes)
		metadata.action = normalizeJournalLabel(metadata.action, maxJournalActionBytes)
	}()

	if message := update.Message; message != nil {
		metadata = updateMetadata{
			kind:      "message",
			messageID: message.MessageID,
			actorID:   userID(message.From),
		}
		if message.Chat != nil {
			metadata.chatID = message.Chat.ID
		}
		switch {
		case message.IsCommand():
			metadata.action = message.Command()
		case message.Video != nil:
			metadata.action = "upload_video"
		case message.Document != nil:
			metadata.action = "upload_document"
		case message.Audio != nil:
			metadata.action = "upload_audio"
		}
		return metadata
	}
	if callback := update.CallbackQuery; callback != nil {
		metadata = updateMetadata{
			kind:    "callback_query",
			action:  inferCallbackAction(callback.Data),
			actorID: userID(callback.From),
		}
		if callback.Message != nil {
			metadata.messageID = callback.Message.MessageID
			if callback.Message.Chat != nil {
				metadata.chatID = callback.Message.Chat.ID
			}
		}
		return metadata
	}
	return updateMetadata{kind: "unknown"}
}

func inferCallbackAction(data string) string {
	parts := strings.Fields(strings.ReplaceAll(data, ":", " "))
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

func normalizeJournalLabel(raw string, limit int) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" || len(raw) > limit {
		return ""
	}
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_', r == '-':
		default:
			return ""
		}
	}
	return raw
}

func newUpdateOwnerToken() (string, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(token[:]), nil
}

func journalFailure(operation string, err error) error {
	return fmt.Errorf("%w: %s: %w", ErrUpdateJournalFailure, operation, err)
}

func journalOperationError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	return journalFailure(operation, err)
}

func boundedJournalError(err error) string {
	if err == nil {
		return ""
	}
	raw := err.Error()
	if len(raw) <= maxJournalErrorBytes {
		return raw
	}
	for len(raw) > maxJournalErrorBytes {
		_, width := utf8.DecodeLastRuneInString(raw)
		if width == 0 {
			break
		}
		raw = raw[:len(raw)-width]
	}
	return raw
}
