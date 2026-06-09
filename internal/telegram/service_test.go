package telegram

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const testChatID int64 = -100123

func TestAdminCreatorAuthorized(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(42, "creator")}},
		},
	}
	svc := newTestService(t, bot)

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if err != nil {
		t.Fatalf("handle command: %v", err)
	}
	if response.text != "skipped" {
		t.Fatalf("response = %q, want skipped", response.text)
	}
}

func TestAdminAdministratorAuthorized(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if err != nil {
		t.Fatalf("handle command: %v", err)
	}
	if response.text != "skipped" {
		t.Fatalf("response = %q, want skipped", response.text)
	}
}

func TestAdminRegularMemberRejected(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(42, "member")}},
			{admins: []tgbotapi.ChatMember{chatMember(42, "member")}},
		},
	}
	svc := newTestService(t, bot)

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if bot.adminCallCount != 2 {
		t.Fatalf("admin API calls = %d, want 2", bot.adminCallCount)
	}
}

func TestAdminStaleDenyForceRefreshes(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(7, "administrator")}},
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)

	response, err := svc.handleCommand(context.Background(), commandMessage(7, "/skip"))
	if err != nil {
		t.Fatalf("prime cache command: %v", err)
	}
	if response.text != "skipped" {
		t.Fatalf("prime response = %q, want skipped", response.text)
	}

	response, err = svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if err != nil {
		t.Fatalf("second handle command: %v", err)
	}
	if response.text != "skipped" {
		t.Fatalf("response = %q, want skipped", response.text)
	}
	if bot.adminCallCount != 2 {
		t.Fatalf("admin API calls = %d, want 2", bot.adminCallCount)
	}
}

func TestAdminBotAdministratorIgnored(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{botChatMember(42, "administrator")}},
			{admins: []tgbotapi.ChatMember{botChatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
}

func TestAdminLookupErrorDenies(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{err: errors.New("telegram unavailable")},
		},
	}
	svc := newTestService(t, bot)

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
}

func TestQueueCommandIncludesPublicKeyboard(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.ListQueue = func(context.Context) (string, error) {
		return "queue", nil
	}

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/queue"))
	if err != nil {
		t.Fatalf("handle command: %v", err)
	}
	assertButton(t, response.markup, "Refresh", "queue")
	assertButton(t, response.markup, "Now", "now")
	assertNoButton(t, response.markup, "Skip")
}

func TestQueueCommandIncludesAdminKeyboard(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.ListQueue = func(context.Context) (string, error) {
		return "目前佇列：\n#10 [第 2 位] song.mp4", nil
	}

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/queue"))
	if err != nil {
		t.Fatalf("handle command: %v", err)
	}
	assertButton(t, response.markup, "Skip", "skip")
	assertButton(t, response.markup, "Remove #10", "remove:10")
	assertButton(t, response.markup, "Up", "move:10:1")
	assertButton(t, response.markup, "Down", "move:10:3")
}

func TestCallbackRefreshEditsMessage(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.ListQueue = func(context.Context) (string, error) {
		return "queue refreshed", nil
	}

	svc.handleCallback(context.Background(), callbackQuery(42, "queue"))

	if bot.editTextCount != 1 {
		t.Fatalf("edit calls = %d, want 1", bot.editTextCount)
	}
	if bot.sendCount != 0 {
		t.Fatalf("send calls = %d, want 0", bot.sendCount)
	}
}

func TestRegisterCommandsSetsPublicAndAdminScopes(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)

	if err := svc.registerCommands(context.Background()); err != nil {
		t.Fatalf("register commands: %v", err)
	}
	if bot.setCommandsCount != 2 {
		t.Fatalf("set command calls = %d, want 2", bot.setCommandsCount)
	}
}

func TestUploadUsesLocalBotAPIFilePath(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{
			FilePath: "/tmp/video.mp4",
			FileSize: 42,
		},
	}
	svc := newTestService(t, bot)
	var got Upload
	svc.hooks.EnqueueUpload = func(_ context.Context, upload Upload) (string, error) {
		got = upload
		return "queued", nil
	}

	response, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 0))
	if err != nil {
		t.Fatalf("handle upload: %v", err)
	}
	if response.text != "queued" {
		t.Fatalf("response = %q, want queued", response.text)
	}
	if got.LocalPath != "/tmp/video.mp4" {
		t.Fatalf("local path = %q, want /tmp/video.mp4", got.LocalPath)
	}
	if got.SizeBytes != 42 {
		t.Fatalf("size = %d, want 42", got.SizeBytes)
	}
}

func TestUploadRejectsRelativeLocalBotAPIFilePath(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "relative/video.mp4"},
	}
	svc := newTestService(t, bot)
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))
	if err == nil {
		t.Fatal("expected relative path error")
	}
	if got := err.Error(); got != "Local Bot API Server must run with --local and return an absolute file path" {
		t.Fatalf("err = %q", got)
	}
}

func newTestService(t *testing.T, bot *fakeBotAPI) *Service {
	t.Helper()

	svc, err := New(Config{
		Token:         "token",
		APIBaseURL:    "http://127.0.0.1:8081",
		AllowedChatID: testChatID,
	}, Hooks{
		Skip: func(context.Context) (string, error) {
			return "skipped", nil
		},
	}, slog.Default(), WithBotAPI(bot))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time {
		return now
	}
	return svc
}

func commandMessage(userID int64, text string) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: testChatID},
		From: &tgbotapi.User{ID: userID},
		Text: text,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: len(text)},
		},
	}
}

func callbackQuery(userID int64, data string) *tgbotapi.CallbackQuery {
	return &tgbotapi.CallbackQuery{
		ID:   "callback-id",
		From: &tgbotapi.User{ID: userID},
		Message: &tgbotapi.Message{
			MessageID: 99,
			Chat:      &tgbotapi.Chat{ID: testChatID},
		},
		Data: data,
	}
}

func videoMessage(fileID string, uniqueID string, size int) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: testChatID},
		From: &tgbotapi.User{ID: 42},
		Video: &tgbotapi.Video{
			FileID:       fileID,
			FileUniqueID: uniqueID,
			FileName:     "video.mp4",
			MimeType:     "video/mp4",
			FileSize:     size,
		},
	}
}

func chatMember(userID int64, status string) tgbotapi.ChatMember {
	return tgbotapi.ChatMember{
		User:   &tgbotapi.User{ID: userID},
		Status: status,
	}
}

func botChatMember(userID int64, status string) tgbotapi.ChatMember {
	return tgbotapi.ChatMember{
		User:   &tgbotapi.User{ID: userID, IsBot: true},
		Status: status,
	}
}

type adminResponse struct {
	admins []tgbotapi.ChatMember
	err    error
}

type fakeBotAPI struct {
	adminResponses   []adminResponse
	adminCallCount   int
	sendCount        int
	editTextCount    int
	setCommandsCount int
	file             tgbotapi.File
	fileErr          error
}

func (f *fakeBotAPI) GetUpdatesChan(tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	return make(tgbotapi.UpdatesChannel)
}

func (f *fakeBotAPI) StopReceivingUpdates() {}

func (f *fakeBotAPI) Send(tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.sendCount++
	return tgbotapi.Message{}, nil
}

func (f *fakeBotAPI) Request(req tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	switch req.(type) {
	case tgbotapi.EditMessageTextConfig:
		f.editTextCount++
	case tgbotapi.SetMyCommandsConfig:
		f.setCommandsCount++
	}
	return &tgbotapi.APIResponse{}, nil
}

func (f *fakeBotAPI) GetFile(tgbotapi.FileConfig) (tgbotapi.File, error) {
	return f.file, f.fileErr
}

func (f *fakeBotAPI) GetChatAdministrators(tgbotapi.ChatAdministratorsConfig) ([]tgbotapi.ChatMember, error) {
	f.adminCallCount++
	if len(f.adminResponses) == 0 {
		return nil, nil
	}
	response := f.adminResponses[0]
	f.adminResponses = f.adminResponses[1:]
	return response.admins, response.err
}

func assertButton(t *testing.T, markup *tgbotapi.InlineKeyboardMarkup, text, data string) {
	t.Helper()
	if markup == nil {
		t.Fatalf("expected markup with button %q", text)
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.Text == text && button.CallbackData != nil && *button.CallbackData == data {
				return
			}
		}
	}
	t.Fatalf("missing button %q with data %q in %#v", text, data, markup.InlineKeyboard)
}

func assertNoButton(t *testing.T, markup *tgbotapi.InlineKeyboardMarkup, text string) {
	t.Helper()
	if markup == nil {
		return
	}
	for _, row := range markup.InlineKeyboard {
		for _, button := range row {
			if button.Text == text {
				t.Fatalf("unexpected button %q in %#v", text, markup.InlineKeyboard)
			}
		}
	}
}
