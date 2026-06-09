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
	if response != "skipped" {
		t.Fatalf("response = %q, want skipped", response)
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
	if response != "skipped" {
		t.Fatalf("response = %q, want skipped", response)
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
	if response != "skipped" {
		t.Fatalf("prime response = %q, want skipped", response)
	}

	response, err = svc.handleCommand(context.Background(), commandMessage(42, "/skip"))
	if err != nil {
		t.Fatalf("second handle command: %v", err)
	}
	if response != "skipped" {
		t.Fatalf("response = %q, want skipped", response)
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

func newTestService(t *testing.T, bot *fakeBotAPI) *Service {
	t.Helper()

	svc, err := New(Config{
		Token:         "token",
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
	adminResponses []adminResponse
	adminCallCount int
}

func (f *fakeBotAPI) GetUpdatesChan(tgbotapi.UpdateConfig) tgbotapi.UpdatesChannel {
	return make(tgbotapi.UpdatesChannel)
}

func (f *fakeBotAPI) StopReceivingUpdates() {}

func (f *fakeBotAPI) Send(tgbotapi.Chattable) (tgbotapi.Message, error) {
	return tgbotapi.Message{}, nil
}

func (f *fakeBotAPI) Request(tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	return &tgbotapi.APIResponse{}, nil
}

func (f *fakeBotAPI) GetFile(tgbotapi.FileConfig) (tgbotapi.File, error) {
	return tgbotapi.File{}, nil
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
