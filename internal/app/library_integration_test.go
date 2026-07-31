package app

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
	"github.com/tiwb/tg-obs-bot/internal/journalstore"
	medialib "github.com/tiwb/tg-obs-bot/internal/library"
	"github.com/tiwb/tg-obs-bot/internal/obs"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

func TestLibraryTelegramUploadPlaybackAndRestartRecovery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "runtime.db")
	first, firstOBS := newLibraryTestServiceAtDBPath(t, dbPath)
	first.now = fixedNow("2026-06-24T12:00:00+08:00")

	const (
		actorID  int64 = 42
		fileName       = "loop_day_cafe_001.mp4"
	)
	sourcePath := writeBotAPIFile(t, first, fileName)
	sourceInfo, err := osStat(sourcePath)
	if err != nil {
		t.Fatalf("stat Local Bot API upload: %v", err)
	}
	firstBot := newLibraryIntegrationBot(
		[]tgbotapi.Update{
			{
				UpdateID: 10,
				Message: integrationDocumentMessage(
					first.cfg.AllowedChatID,
					actorID,
					"telegram-file-id",
					fileName,
					int(sourceInfo.Size()),
				),
			},
			{
				UpdateID: 11,
				Message: integrationCommandMessage(
					first.cfg.AllowedChatID,
					actorID,
					"/skip loop",
				),
			},
		},
		tgbotapi.File{
			FilePath: sourcePath,
			FileSize: int(sourceInfo.Size()),
		},
		actorID,
	)
	firstJournal, ok := first.journalStore.(*journalstore.Store)
	if !ok {
		t.Fatalf("journal store type = %T, want *journalstore.Store", first.journalStore)
	}
	firstTelegram := newLibraryIntegrationTelegram(t, first, firstBot, firstJournal)

	runCtx, cancelRun := context.WithCancel(ctx)
	runResult := make(chan error, 1)
	go func() {
		runResult <- firstTelegram.Run(runCtx)
	}()
	firstBot.waitForPolls(t, 4)
	cancelRun()
	select {
	case runErr := <-runResult:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("first Telegram run error = %v, want cancellation", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("first Telegram service did not stop after cancellation")
	}

	importedPath := filepath.Join(first.cfg.LoopMediaDir, fileName)
	if !fileExists(importedPath) {
		t.Fatalf("Telegram upload was not imported at %s", importedPath)
	}
	firstLoopID := first.activeLoopID
	if firstLoopID == "" {
		t.Fatal("Telegram /skip loop did not activate an imported library loop")
	}
	if got := firstOBS.sourcePlayed[first.cfg.OBSLoopSourceName]; got != importedPath {
		t.Fatalf("OBS loop path = %q, want imported path %q", got, importedPath)
	}
	if got := firstBot.fileCalls(); got != 1 {
		t.Fatalf("Local Bot API GetFile calls = %d, want 1", got)
	}

	nextOffset, confirmedOffset, err := firstJournal.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load durable Telegram checkpoint: %v", err)
	}
	if nextOffset != 12 || confirmedOffset != 12 {
		t.Fatalf(
			"Telegram checkpoint = next %d / confirmed %d, want 12 / 12",
			nextOffset,
			confirmedOffset,
		)
	}
	plan, found, err := first.libDB.PeriodPlan(ctx, "2026-06-24", medialib.PeriodDay)
	if err != nil {
		t.Fatalf("load durable library period plan: %v", err)
	}
	if !found || plan.LoopID != firstLoopID {
		t.Fatalf("period plan = %#v, found=%v; want loop %q", plan, found, firstLoopID)
	}

	mediaDir := first.cfg.MediaDir
	loopDir := first.cfg.LoopMediaDir
	musicDir := first.cfg.MusicMediaDir
	botAPIDir := first.cfg.TelegramBotAPIDir
	if err := first.libDB.Close(); err != nil {
		t.Fatalf("close first library state store: %v", err)
	}
	if err := firstJournal.Close(); err != nil {
		t.Fatalf("close first Telegram journal: %v", err)
	}

	second, secondOBS := newLibraryTestServiceAtDBPath(t, dbPath)
	second.cfg.MediaDir = mediaDir
	second.cfg.LoopMediaDir = loopDir
	second.cfg.MusicMediaDir = musicDir
	second.cfg.TelegramBotAPIDir = botAPIDir
	second.now = fixedNow("2026-06-24T12:30:00+08:00")
	secondJournal, ok := second.journalStore.(*journalstore.Store)
	if !ok {
		t.Fatalf("restarted journal store type = %T, want *journalstore.Store", second.journalStore)
	}

	restartBot := newLibraryIntegrationBot(nil, tgbotapi.File{}, actorID)
	restartedTelegram := newLibraryIntegrationTelegram(t, second, restartBot, secondJournal)
	restartCtx, cancelRestart := context.WithCancel(ctx)
	restartResult := make(chan error, 1)
	go func() {
		restartResult <- restartedTelegram.Run(restartCtx)
	}()
	restartBot.waitForPolls(t, 1)
	cancelRestart()
	select {
	case runErr := <-restartResult:
		if !errors.Is(runErr, context.Canceled) {
			t.Fatalf("restarted Telegram run error = %v, want cancellation", runErr)
		}
	case <-time.After(time.Second):
		t.Fatal("restarted Telegram service did not stop after cancellation")
	}
	if got := restartBot.firstPollOffset(); got != 12 {
		t.Fatalf("restarted Telegram poll offset = %d, want durable offset 12", got)
	}
	if got := restartBot.fileCalls(); got != 0 {
		t.Fatalf("restarted Telegram replayed completed upload: GetFile calls = %d", got)
	}
	if got := restartBot.sendCalls(); got != 0 {
		t.Fatalf("restarted Telegram replayed completed response: send calls = %d", got)
	}

	if err := second.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback after service restart: %v", err)
	}
	if second.activeLoopID != firstLoopID {
		t.Fatalf("restarted loop ID = %q, want persisted %q", second.activeLoopID, firstLoopID)
	}
	if got := secondOBS.sourcePlayed[second.cfg.OBSLoopSourceName]; got != importedPath {
		t.Fatalf("restarted OBS loop path = %q, want %q", got, importedPath)
	}

	playsBeforeReconnect := secondOBS.sourcePlayCalls[second.cfg.OBSLoopSourceName]
	secondOBS.mediaStatuses[second.cfg.OBSLoopSourceName] = obs.MediaInputStatus{
		State: obs.MediaStateNone,
	}
	if err := second.recoverLibraryPlaybackAfterOBSConnect(ctx); err != nil {
		t.Fatalf("recover playback after OBS reconnect: %v", err)
	}
	if second.activeLoopID != firstLoopID {
		t.Fatalf("OBS reconnect changed persisted loop ID: got %q, want %q", second.activeLoopID, firstLoopID)
	}
	if got := secondOBS.sourcePlayCalls[second.cfg.OBSLoopSourceName]; got != playsBeforeReconnect+1 {
		t.Fatalf("OBS reconnect play calls = %d, want %d", got, playsBeforeReconnect+1)
	}
}

func newLibraryIntegrationTelegram(
	t *testing.T,
	appService *Service,
	bot *libraryIntegrationBot,
	journal telegram.UpdateJournal,
) *telegram.Service {
	t.Helper()
	service, err := telegram.New(
		telegram.Config{
			Token:              "123456789:library-integration",
			APIBaseURL:         "http://127.0.0.1:8081",
			AllowedChatID:      appService.cfg.AllowedChatID,
			MaxUploadSizeBytes: appService.cfg.MaxVideoSizeBytes,
			UpdateTimeout:      1,
		},
		appService.telegramHooks(),
		slog.New(slog.NewTextHandler(io.Discard, nil)),
		telegram.WithBotAPI(bot),
		telegram.WithUpdateJournal(journal),
	)
	if err != nil {
		t.Fatalf("create Telegram integration service: %v", err)
	}
	return service
}

func integrationDocumentMessage(
	chatID int64,
	userID int64,
	fileID string,
	fileName string,
	size int,
) *tgbotapi.Message {
	return &tgbotapi.Message{
		MessageID: 1,
		Chat:      &tgbotapi.Chat{ID: chatID},
		From:      &tgbotapi.User{ID: userID},
		Document: &tgbotapi.Document{
			FileID:       fileID,
			FileUniqueID: "library-integration-file",
			FileName:     fileName,
			MimeType:     "video/mp4",
			FileSize:     size,
		},
	}
}

func integrationCommandMessage(chatID int64, userID int64, text string) *tgbotapi.Message {
	commandLength := len(text)
	for index, char := range text {
		if char == ' ' {
			commandLength = index
			break
		}
	}
	return &tgbotapi.Message{
		MessageID: 2,
		Chat:      &tgbotapi.Chat{ID: chatID},
		From:      &tgbotapi.User{ID: userID},
		Text:      text,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: commandLength},
		},
	}
}

type libraryIntegrationBot struct {
	mu             sync.Mutex
	updates        []tgbotapi.Update
	file           tgbotapi.File
	adminID        int64
	pollConfigs    []tgbotapi.UpdateConfig
	pollCalls      chan struct{}
	getFileCalls   int
	messageSends   int
	requests       int
	deliveredEmpty bool
}

func newLibraryIntegrationBot(
	updates []tgbotapi.Update,
	file tgbotapi.File,
	adminID int64,
) *libraryIntegrationBot {
	return &libraryIntegrationBot{
		updates:     updates,
		file:        file,
		adminID:     adminID,
		pollCalls:   make(chan struct{}, len(updates)+4),
		pollConfigs: make([]tgbotapi.UpdateConfig, 0, len(updates)+2),
	}
}

func (b *libraryIntegrationBot) GetUpdates(
	ctx context.Context,
	config tgbotapi.UpdateConfig,
) ([]tgbotapi.Update, error) {
	b.mu.Lock()
	b.pollConfigs = append(b.pollConfigs, config)
	var response []tgbotapi.Update
	switch {
	case len(b.updates) > 0:
		response = []tgbotapi.Update{b.updates[0]}
		b.updates = b.updates[1:]
	case !b.deliveredEmpty:
		b.deliveredEmpty = true
		response = []tgbotapi.Update{}
	default:
		b.mu.Unlock()
		b.pollCalls <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	b.mu.Unlock()
	b.pollCalls <- struct{}{}
	return response, nil
}

func (b *libraryIntegrationBot) Send(
	context.Context,
	tgbotapi.Chattable,
) (tgbotapi.Message, error) {
	b.mu.Lock()
	b.messageSends++
	b.mu.Unlock()
	return tgbotapi.Message{}, nil
}

func (b *libraryIntegrationBot) Request(
	context.Context,
	tgbotapi.Chattable,
) (*tgbotapi.APIResponse, error) {
	b.mu.Lock()
	b.requests++
	b.mu.Unlock()
	return &tgbotapi.APIResponse{}, nil
}

func (b *libraryIntegrationBot) GetFile(
	context.Context,
	tgbotapi.FileConfig,
) (tgbotapi.File, error) {
	b.mu.Lock()
	b.getFileCalls++
	file := b.file
	b.mu.Unlock()
	return file, nil
}

func (b *libraryIntegrationBot) GetChatAdministrators(
	context.Context,
	tgbotapi.ChatAdministratorsConfig,
) ([]tgbotapi.ChatMember, error) {
	return []tgbotapi.ChatMember{
		{
			User:   &tgbotapi.User{ID: b.adminID},
			Status: "administrator",
		},
	}, nil
}

func (b *libraryIntegrationBot) waitForPolls(t *testing.T, count int) {
	t.Helper()
	for poll := 1; poll <= count; poll++ {
		select {
		case <-b.pollCalls:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for Telegram poll %d/%d", poll, count)
		}
	}
}

func (b *libraryIntegrationBot) firstPollOffset() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.pollConfigs) == 0 {
		return -1
	}
	return b.pollConfigs[0].Offset
}

func (b *libraryIntegrationBot) fileCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.getFileCalls
}

func (b *libraryIntegrationBot) sendCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.messageSends
}
