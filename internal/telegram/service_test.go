package telegram

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

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

func TestLibraryReadOnlyCommandsForNonAdmin(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.Library = func(context.Context) (string, error) {
		return "媒體庫", nil
	}
	svc.hooks.Preview = func(context.Context) (string, error) {
		return "預覽", nil
	}

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/library"))
	if err != nil {
		t.Fatalf("library command: %v", err)
	}
	if response.text != "媒體庫" {
		t.Fatalf("library response = %q", response.text)
	}
	assertButton(t, response.markup, "刷新", "library")
	assertButton(t, response.markup, "預覽", "preview")
	assertNoButton(t, response.markup, "掃描")

	response, err = svc.handleCommand(context.Background(), commandMessage(42, "/preview"))
	if err != nil {
		t.Fatalf("preview command: %v", err)
	}
	if response.text != "預覽" {
		t.Fatalf("preview response = %q", response.text)
	}
	assertButton(t, response.markup, "媒體庫", "library")
	assertNoButton(t, response.markup, "略過循環")
}

func TestAdminLibraryCommandsRouteArguments(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	cacheAdmin(svc, 42)
	svc.hooks.Scan = func(context.Context) (string, error) {
		return "scan ok", nil
	}
	svc.hooks.SetTheme = func(_ context.Context, theme string) (string, error) {
		return "theme " + theme, nil
	}
	svc.hooks.SelectLoop = func(_ context.Context, assetID string) (string, error) {
		return "select " + assetID, nil
	}
	svc.hooks.SkipLoop = func(context.Context) (string, error) {
		return "loop skipped", nil
	}
	svc.hooks.SkipMusic = func(context.Context) (string, error) {
		return "music skipped", nil
	}

	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "scan", text: "/scan", want: "scan ok"},
		{name: "theme", text: "/theme random", want: "theme random"},
		{name: "select", text: "/select clear", want: "select clear"},
		{name: "skip loop", text: "/skip loop", want: "loop skipped"},
		{name: "skip music", text: "/skip music", want: "music skipped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := svc.handleCommand(context.Background(), commandMessage(42, tt.text))
			if err != nil {
				t.Fatalf("handle command: %v", err)
			}
			if response.text != tt.want {
				t.Fatalf("response = %q, want %q", response.text, tt.want)
			}
		})
	}
}

func TestAdminOnlyLibraryCommandsRejectedForNonAdmin(t *testing.T) {
	tests := []string{
		"/scan",
		"/theme random",
		"/select asset-1",
		"/skip loop",
		"/skip music",
	}
	for _, text := range tests {
		t.Run(text, func(t *testing.T) {
			bot := &fakeBotAPI{
				adminResponses: []adminResponse{
					{admins: []tgbotapi.ChatMember{}},
					{admins: []tgbotapi.ChatMember{}},
				},
			}
			svc := newTestService(t, bot)

			_, err := svc.handleCommand(context.Background(), commandMessage(42, text))
			if !errors.Is(err, errAdminOnly) {
				t.Fatalf("err = %v, want %v", err, errAdminOnly)
			}
		})
	}
}

func TestLibraryCommandBadArguments(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	cacheAdmin(svc, 42)

	tests := []string{
		"/theme",
		"/theme random extra",
		"/select",
		"/skip track",
	}
	for _, text := range tests {
		t.Run(text, func(t *testing.T) {
			_, err := svc.handleCommand(context.Background(), commandMessage(42, text))
			if !errors.Is(err, errBadCommand) {
				t.Fatalf("err = %v, want %v", err, errBadCommand)
			}
		})
	}
}

func TestLibraryCallbackRefreshEditsMessage(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.Library = func(context.Context) (string, error) {
		return "library refreshed", nil
	}

	svc.handleCallback(context.Background(), callbackQuery(42, "library"))

	if bot.editTextCount != 1 {
		t.Fatalf("edit calls = %d, want 1", bot.editTextCount)
	}
	if bot.sendCount != 0 {
		t.Fatalf("send calls = %d, want 0", bot.sendCount)
	}
}

func TestAdminLibraryCallbacksRouteActions(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	cacheAdmin(svc, 42)
	user := &tgbotapi.User{ID: 42}
	svc.hooks.Scan = func(context.Context) (string, error) {
		return "scan ok", nil
	}
	svc.hooks.SetTheme = func(_ context.Context, theme string) (string, error) {
		return "theme " + theme, nil
	}
	svc.hooks.SelectLoop = func(_ context.Context, assetID string) (string, error) {
		return "select " + assetID, nil
	}
	svc.hooks.SkipLoop = func(context.Context) (string, error) {
		return "loop skipped", nil
	}
	svc.hooks.SkipMusic = func(context.Context) (string, error) {
		return "music skipped", nil
	}

	tests := []struct {
		name string
		data string
		want string
	}{
		{name: "scan", data: "scan", want: "scan ok"},
		{name: "theme", data: "theme:random", want: "theme random"},
		{name: "select", data: "select:asset-1", want: "select asset-1"},
		{name: "skip loop", data: "skip:loop", want: "loop skipped"},
		{name: "skip music", data: "skip:music", want: "music skipped"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := svc.routeAction(context.Background(), testChatID, user, tt.data)
			if err != nil {
				t.Fatalf("route action: %v", err)
			}
			if response.text != tt.want {
				t.Fatalf("response = %q, want %q", response.text, tt.want)
			}
		})
	}
}

func TestAdminOnlyLibraryCallbackRejectedForNonAdmin(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.SkipMusic = func(context.Context) (string, error) {
		t.Fatal("skip music hook should not be called")
		return "", nil
	}

	_, err := svc.routeAction(context.Background(), testChatID, &tgbotapi.User{ID: 42}, "skip:music")
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
}

func TestHelpAndHomeMenuExposeLibraryControls(t *testing.T) {
	publicHelp := helpText(false)
	for _, want := range []string{"/library", "/preview", "/now", "/status", "/help"} {
		if !strings.Contains(publicHelp, want) {
			t.Fatalf("public help missing %q: %q", want, publicHelp)
		}
	}
	for _, notWant := range []string{"/scan", "/theme", "/select", "/skip loop"} {
		if strings.Contains(publicHelp, notWant) {
			t.Fatalf("public help unexpectedly includes %q: %q", notWant, publicHelp)
		}
	}

	adminHelp := helpText(true)
	for _, want := range []string{"/scan", "/theme <theme|random>", "/select <asset_id|clear>", "/skip loop", "/skip music"} {
		if !strings.Contains(adminHelp, want) {
			t.Fatalf("admin help missing %q: %q", want, adminHelp)
		}
	}

	publicMenu := homeKeyboard(false)
	assertButton(t, publicMenu, "媒體庫", "library")
	assertButton(t, publicMenu, "預覽", "preview")
	assertNoButton(t, publicMenu, "掃描")

	adminMenu := homeKeyboard(true)
	assertButton(t, adminMenu, "掃描", "scan")
	assertButton(t, adminMenu, "隨機主題", "theme:random")
	assertButton(t, adminMenu, "清除選取", "select:clear")
	assertButton(t, adminMenu, "略過循環", "skip:loop")
	assertButton(t, adminMenu, "略過音樂", "skip:music")
}

func TestQueueModeHelpAndHomeMenuExposeQueueControls(t *testing.T) {
	publicHelp := helpTextForMode(false, false)
	for _, want := range []string{"/queue", "/now", "/status", "/history"} {
		if !strings.Contains(publicHelp, want) {
			t.Fatalf("queue public help missing %q: %q", want, publicHelp)
		}
	}
	for _, notWant := range []string{"/library", "/preview", "/theme"} {
		if strings.Contains(publicHelp, notWant) {
			t.Fatalf("queue public help unexpectedly includes %q: %q", notWant, publicHelp)
		}
	}
	adminHelp := helpTextForMode(true, false)
	for _, want := range []string{"/move <video_id> <position>", "/remove <video_id>", "/skip"} {
		if !strings.Contains(adminHelp, want) {
			t.Fatalf("queue admin help missing %q: %q", want, adminHelp)
		}
	}

	menu := homeKeyboardForMode(true, false)
	assertButton(t, menu, "Queue", "queue")
	assertButton(t, menu, "History", "history")
	assertButton(t, menu, "Skip", "skip")
	assertNoButton(t, menu, "媒體庫")
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
	if len(bot.setCommands) != 2 {
		t.Fatalf("captured command calls = %d, want 2", len(bot.setCommands))
	}
	publicCommands := bot.setCommands[0].Commands
	for _, command := range []string{"library", "preview", "now", "status", "help"} {
		assertCommand(t, publicCommands, command)
	}
	for _, command := range []string{"scan", "theme", "select", "skip", "queue", "history"} {
		assertNoCommand(t, publicCommands, command)
	}
	adminCommands := bot.setCommands[1].Commands
	for _, command := range []string{"library", "scan", "preview", "theme", "select", "skip", "now", "status", "help"} {
		assertCommand(t, adminCommands, command)
	}
}

func TestQueueModeRegisterCommandsSetsQueueScopes(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)
	svc.cfg.PlayerMode = "queue"

	if err := svc.registerCommands(context.Background()); err != nil {
		t.Fatalf("register commands: %v", err)
	}
	publicCommands := bot.setCommands[0].Commands
	for _, command := range []string{"queue", "now", "status", "history", "help"} {
		assertCommand(t, publicCommands, command)
	}
	for _, command := range []string{"library", "preview", "theme", "select"} {
		assertNoCommand(t, publicCommands, command)
	}
	adminCommands := bot.setCommands[1].Commands
	assertCommand(t, adminCommands, "skip")
}

func TestQueueModeNowStatusHistoryUseQueueKeyboards(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)
	svc.cfg.PlayerMode = "queue"
	svc.hooks.Now = func(context.Context) (string, error) { return "now", nil }
	svc.hooks.Status = func(context.Context) (string, error) { return "status", nil }
	svc.hooks.History = func(context.Context) (string, error) { return "history", nil }

	for _, command := range []string{"/now", "/status", "/history"} {
		response, err := svc.handleCommand(context.Background(), commandMessage(42, command))
		if err != nil {
			t.Fatalf("%s: %v", command, err)
		}
		assertButton(t, response.markup, "Queue", "queue")
		assertNoButton(t, response.markup, "媒體庫")
		assertNoButton(t, response.markup, "預覽")
	}
}

func TestSendErrorLogRedactsTelegramToken(t *testing.T) {
	const token = "123456:ABCdefghi_jklmnop"
	var logs bytes.Buffer
	bot := &fakeBotAPI{
		sendErr: errors.New(`Post "http://127.0.0.1:8081/bot123456:ABCdefghi_jklmnop/sendMessage": EOF`),
	}
	svc := newTestService(t, bot)
	svc.cfg.Token = token
	svc.logger = slog.New(slog.NewTextHandler(&logs, nil))

	svc.reply(context.Background(), testChatID, botResponse{text: "hello"}, nil)

	got := logs.String()
	if strings.Contains(got, token) {
		t.Fatalf("log leaked token: %q", got)
	}
	if !strings.Contains(got, "/bot<redacted>/sendMessage") {
		t.Fatalf("log = %q, want redacted bot URL", got)
	}
}

func TestAdminLookupLogRedactsTelegramToken(t *testing.T) {
	const token = "123456:ABCdefghi_jklmnop"
	var logs bytes.Buffer
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{err: errors.New(`Post "http://127.0.0.1:8081/bot123456:ABCdefghi_jklmnop/getChatAdministrators": EOF`)},
		},
	}
	svc := newTestService(t, bot)
	svc.cfg.Token = token
	svc.logger = slog.New(slog.NewTextHandler(&logs, nil))

	_ = svc.isAdmin(context.Background(), testChatID, &tgbotapi.User{ID: 42})

	got := logs.String()
	if strings.Contains(got, token) {
		t.Fatalf("log leaked token: %q", got)
	}
	if !strings.Contains(got, "/bot<redacted>/getChatAdministrators") {
		t.Fatalf("log = %q, want redacted bot URL", got)
	}
}

func TestFriendlyErrorRedactsPublicMessage(t *testing.T) {
	const token = "123456:ABCdefghi_jklmnop"
	svc := newTestService(t, &fakeBotAPI{})
	svc.cfg.Token = token

	got := svc.friendlyError(testPublicError(`public message includes http://127.0.0.1:8081/bot123456:ABCdefghi_jklmnop/getMe`))

	if strings.Contains(got, token) {
		t.Fatalf("friendly error leaked token: %q", got)
	}
	if !strings.Contains(got, "/bot<redacted>/getMe") {
		t.Fatalf("friendly error = %q, want redacted bot URL", got)
	}
}

func TestNewDefaultsRequestTimeoutExceedsUpdateTimeout(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})

	updateTimeout := time.Duration(svc.cfg.UpdateTimeout) * time.Second
	if svc.cfg.RequestTimeout <= updateTimeout {
		t.Fatalf("request timeout = %s, want greater than update timeout %s", svc.cfg.RequestTimeout, updateTimeout)
	}
}

func TestProductionBotAPIUpdateAndRequestLocksAreIndependent(t *testing.T) {
	api := &productionBotAPI{
		updateClient:  &contextHTTPClient{},
		requestClient: &contextHTTPClient{},
	}

	api.updateMu.Lock()
	requestDone := make(chan struct{})
	go func() {
		_ = api.withRequestContext(context.Background(), func() error {
			close(requestDone)
			return nil
		})
	}()
	select {
	case <-requestDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("request context blocked behind update lock")
	}
	api.updateMu.Unlock()

	api.requestMu.Lock()
	updateDone := make(chan struct{})
	go func() {
		_ = api.withUpdateContext(context.Background(), func() error {
			close(updateDone)
			return nil
		})
	}()
	select {
	case <-updateDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("update context blocked behind request lock")
	}
	api.requestMu.Unlock()
}

func TestContextHTTPClientAttachesCallerContext(t *testing.T) {
	base := &blockingHTTPClient{seen: make(chan context.Context, 1)}
	client := &contextHTTPClient{base: base}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.ctx = ctx
	req, err := http.NewRequest(http.MethodPost, "http://telegram.local/bot/test", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	errCh := make(chan error, 1)

	go func() {
		_, err := client.Do(req)
		errCh <- err
	}()

	select {
	case got := <-base.seen:
		if got != ctx {
			t.Fatalf("request context was not caller context")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("http client did not receive request")
	}
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("http client did not return after context cancellation")
	}
}

func TestSendCanceledContextReturnsQuickly(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	bot := &fakeBotAPI{sendBlock: block}
	svc := newTestService(t, bot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := svc.send(ctx, tgbotapi.NewMessage(testChatID, "hello"))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("send took %s, want quick cancellation", elapsed)
	}
	if bot.sendCount != 0 {
		t.Fatalf("send calls = %d, want 0", bot.sendCount)
	}
}

func TestRequestCanceledContextReturnsQuickly(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	bot := &fakeBotAPI{requestBlock: block}
	svc := newTestService(t, bot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	err := svc.request(ctx, tgbotapi.NewMessage(testChatID, "hello"))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("request took %s, want quick cancellation", elapsed)
	}
	if bot.requestCount != 0 {
		t.Fatalf("request calls = %d, want 0", bot.requestCount)
	}
}

func TestGetFileCanceledContextDoesNotCallBot(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	bot := &fakeBotAPI{fileBlock: block}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	start := time.Now()
	_, err := svc.handleUpload(ctx, videoMessage("file-id", "unique-id", 1))

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("handle upload took %s, want quick cancellation", elapsed)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestGetFileInFlightCanceledContextReturnsQuickly(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{})
	bot := &fakeBotAPI{fileBlock: block, fileStarted: started}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)

	go func() {
		_, err := svc.handleUpload(ctx, videoMessage("file-id", "unique-id", 1))
		errCh <- err
	}()
	<-started
	start := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handle upload did not return after context cancellation")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("handle upload took %s after cancel, want quick cancellation", elapsed)
	}
	if bot.fileCallCount != 1 {
		t.Fatalf("getFile calls = %d, want 1", bot.fileCallCount)
	}
}

func TestAdminCanceledContextDoesNotCallBotAndDeniesCommand(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	bot := &fakeBotAPI{adminBlock: block}
	svc := newTestService(t, bot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	start := time.Now()
	_, err := svc.handleCommand(ctx, commandMessage(42, "/skip"))

	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("admin command took %s, want quick cancellation", elapsed)
	}
	if bot.adminCallCount != 0 {
		t.Fatalf("admin API calls = %d, want 0", bot.adminCallCount)
	}
}

func TestAdminInFlightCanceledContextReturnsQuicklyAndDeniesCommand(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	started := make(chan struct{})
	bot := &fakeBotAPI{adminBlock: block, adminStarted: started}
	svc := newTestService(t, bot)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)

	go func() {
		_, err := svc.handleCommand(ctx, commandMessage(42, "/skip"))
		errCh <- err
	}()
	<-started
	start := time.Now()
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, errAdminOnly) {
			t.Fatalf("err = %v, want %v", err, errAdminOnly)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("admin command did not return after context cancellation")
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("admin command took %s after cancel, want quick cancellation", elapsed)
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want 1", bot.adminCallCount)
	}
}

func TestRunCanceledContextReturnsQuickly(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := svc.Run(ctx)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want %v", err, context.Canceled)
	}
	if bot.updateCallCount != 0 {
		t.Fatalf("get updates calls = %d, want 0", bot.updateCallCount)
	}
}

func TestRunRetriesGetUpdatesError(t *testing.T) {
	getUpdatesErr := errors.New("local bot api unavailable")
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{err: getUpdatesErr},
		},
		updateCalls: make(chan struct{}, 2),
	}
	svc := newTestService(t, bot)
	svc.pollRetryDelay = time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)

	go func() {
		errCh <- svc.Run(ctx)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-bot.updateCalls:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for get updates call %d", i+1)
		}
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after cancellation")
	}
}

func TestRunAdvancesUpdateOffset(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/queue")},
				{UpdateID: 11, Message: commandMessage(42, "/now")},
			}},
		},
		updateCalls: make(chan struct{}, 2),
	}
	svc := newTestService(t, bot)
	svc.hooks.ListQueue = func(context.Context) (string, error) { return "queue", nil }
	svc.hooks.Now = func(context.Context) (string, error) { return "now", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)

	go func() {
		errCh <- svc.Run(ctx)
	}()

	for i := 0; i < 2; i++ {
		select {
		case <-bot.updateCalls:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for get updates call %d", i+1)
		}
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after cancellation")
	}
	if len(bot.updateConfigs) < 2 {
		t.Fatalf("update configs = %d, want at least 2", len(bot.updateConfigs))
	}
	if got := bot.updateConfigs[1].Offset; got != 12 {
		t.Fatalf("second poll offset = %d, want 12", got)
	}
}

func TestRunSkipsStaleUpdates(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 5, Message: commandMessage(42, "/queue")},
			}},
			{updates: []tgbotapi.Update{
				{UpdateID: 5, Message: commandMessage(42, "/queue")},
				{UpdateID: 6, Message: commandMessage(42, "/queue")},
			}},
		},
		updateCalls: make(chan struct{}, 3),
	}
	svc := newTestService(t, bot)
	svc.hooks.ListQueue = func(context.Context) (string, error) { return "queue", nil }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)

	go func() {
		errCh <- svc.Run(ctx)
	}()

	for i := 0; i < 3; i++ {
		select {
		case <-bot.updateCalls:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for get updates call %d", i+1)
		}
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after cancellation")
	}
	if bot.sendCount != 2 {
		t.Fatalf("send calls = %d, want 2", bot.sendCount)
	}
	if got := bot.updateConfigs[2].Offset; got != 7 {
		t.Fatalf("third poll offset = %d, want 7", got)
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
	cacheAdmin(svc, 42)
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

func TestUploadRequiresAdmin(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestUploadAcceptsAudioAndVideoDocuments(t *testing.T) {
	tests := []struct {
		name     string
		message  *tgbotapi.Message
		filePath string
		wantName string
		wantMIME string
	}{
		{
			name:     "audio mime",
			message:  documentMessage("file-id", "unique-id", "song.bin", "audio/mpeg", 12),
			filePath: "/tmp/song.mp3",
			wantName: "song.bin",
			wantMIME: "audio/mpeg",
		},
		{
			name:     "video extension",
			message:  documentMessage("file-id", "unique-id", "clip.mkv", "application/octet-stream", 12),
			filePath: "/tmp/clip.mkv",
			wantName: "clip.mkv",
			wantMIME: "application/octet-stream",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bot := &fakeBotAPI{
				file: tgbotapi.File{
					FilePath: tt.filePath,
					FileSize: 12,
				},
			}
			svc := newTestService(t, bot)
			cacheAdmin(svc, 42)
			var got Upload
			svc.hooks.EnqueueUpload = func(_ context.Context, upload Upload) (string, error) {
				got = upload
				return "imported", nil
			}

			response, err := svc.handleUpload(context.Background(), tt.message)
			if err != nil {
				t.Fatalf("handle upload: %v", err)
			}
			if response.text != "imported" {
				t.Fatalf("response = %q, want imported", response.text)
			}
			if got.Kind != UploadKindDocument {
				t.Fatalf("kind = %q, want %q", got.Kind, UploadKindDocument)
			}
			if got.FileName != tt.wantName {
				t.Fatalf("file name = %q, want %q", got.FileName, tt.wantName)
			}
			if got.MimeType != tt.wantMIME {
				t.Fatalf("mime = %q, want %q", got.MimeType, tt.wantMIME)
			}
			if got.LocalPath != tt.filePath {
				t.Fatalf("local path = %q, want %q", got.LocalPath, tt.filePath)
			}
		})
	}
}

func TestUploadRejectsUnsupportedDocumentsBeforeGetFile(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "/tmp/notes.pdf"},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), documentMessage("file-id", "unique-id", "notes.pdf", "application/pdf", 12))
	if !errors.Is(err, errUnsupportedUpload) {
		t.Fatalf("err = %v, want %v", err, errUnsupportedUpload)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestUploadReturnsGetFileError(t *testing.T) {
	getFileErr := errors.New("telegram getFile failed")
	bot := &fakeBotAPI{fileErr: getFileErr}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))

	if !errors.Is(err, getFileErr) {
		t.Fatalf("err = %v, want wrapped %v", err, getFileErr)
	}
	if bot.fileCallCount != 1 {
		t.Fatalf("getFile calls = %d, want 1", bot.fileCallCount)
	}
}

func TestUploadRejectsDeclaredSizeOverLimit(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.cfg.MaxUploadSizeBytes = 10
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 11))

	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("err = %v, want %v", err, errUploadTooLarge)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestUploadRejectsGetFileSizeOverLimit(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{
			FilePath: "/tmp/video.mp4",
			FileSize: 11,
		},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.cfg.MaxUploadSizeBytes = 10
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("enqueue hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 0))

	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("err = %v, want %v", err, errUploadTooLarge)
	}
}

func TestUploadAcceptsSizeAtLimit(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{
			FilePath: "/tmp/video.mp4",
			FileSize: 10,
		},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.cfg.MaxUploadSizeBytes = 10
	svc.hooks.EnqueueUpload = func(context.Context, Upload) (string, error) {
		return "queued", nil
	}

	response, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 10))

	if err != nil {
		t.Fatalf("handle upload: %v", err)
	}
	if response.text != "queued" {
		t.Fatalf("response = %q, want queued", response.text)
	}
}

func TestUploadRejectsRelativeLocalBotAPIFilePath(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "relative/video.mp4"},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
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

func TestFriendlyErrorHidesInternalDetailsByDefault(t *testing.T) {
	got := friendlyError(errors.New("ffprobe failed for /private/upload.mp4"))
	if strings.Contains(got, "ffprobe") || strings.Contains(got, "/private") {
		t.Fatalf("friendly error leaked internal detail: %q", got)
	}
	if !strings.Contains(got, "/status") {
		t.Fatalf("friendly error = %q, want status guidance", got)
	}
}

func TestTruncateCallbackTextKeepsUTF8Valid(t *testing.T) {
	input := strings.Repeat("開始播放", 80)
	got := truncateCallbackText(input)
	if !utf8.ValidString(got) {
		t.Fatalf("truncated callback text is invalid UTF-8: %q", got)
	}
	if len([]rune(got)) != 180 {
		t.Fatalf("truncated callback rune length = %d, want 180", len([]rune(got)))
	}
	if !strings.HasSuffix(got, "...") {
		t.Fatalf("truncated callback text = %q, want ellipsis suffix", got)
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

func cacheAdmin(svc *Service, userID int64) {
	svc.adminCache[testChatID] = adminCacheEntry{
		adminIDs:  map[int64]struct{}{userID: {}},
		expiresAt: svc.now().Add(adminCacheTTL),
	}
}

func commandMessage(userID int64, text string) *tgbotapi.Message {
	commandLength := len(text)
	if index := strings.IndexByte(text, ' '); index >= 0 {
		commandLength = index
	}
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: testChatID},
		From: &tgbotapi.User{ID: userID},
		Text: text,
		Entities: []tgbotapi.MessageEntity{
			{Type: "bot_command", Offset: 0, Length: commandLength},
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

func documentMessage(fileID string, uniqueID string, name string, mimeType string, size int) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: testChatID},
		From: &tgbotapi.User{ID: 42},
		Document: &tgbotapi.Document{
			FileID:       fileID,
			FileUniqueID: uniqueID,
			FileName:     name,
			MimeType:     mimeType,
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

type updateResponse struct {
	updates []tgbotapi.Update
	err     error
}

type blockingHTTPClient struct {
	seen chan context.Context
}

func (b *blockingHTTPClient) Do(req *http.Request) (*http.Response, error) {
	b.seen <- req.Context()
	<-req.Context().Done()
	return nil, req.Context().Err()
}

type fakeBotAPI struct {
	adminResponses   []adminResponse
	updateResponses  []updateResponse
	adminCallCount   int
	updateCallCount  int
	updateConfigs    []tgbotapi.UpdateConfig
	sendCount        int
	requestCount     int
	editTextCount    int
	setCommandsCount int
	setCommands      []tgbotapi.SetMyCommandsConfig
	fileCallCount    int
	file             tgbotapi.File
	fileErr          error
	sendErr          error
	requestErr       error
	updateCalls      chan struct{}
	sendBlock        <-chan struct{}
	requestBlock     <-chan struct{}
	fileBlock        <-chan struct{}
	adminBlock       <-chan struct{}
	fileStarted      chan struct{}
	adminStarted     chan struct{}
}

type testPublicError string

func (e testPublicError) Error() string {
	return string(e)
}

func (e testPublicError) PublicMessage() string {
	return string(e)
}

func (f *fakeBotAPI) GetUpdates(ctx context.Context, config tgbotapi.UpdateConfig) ([]tgbotapi.Update, error) {
	f.updateCallCount++
	f.updateConfigs = append(f.updateConfigs, config)
	if f.updateCalls != nil {
		select {
		case f.updateCalls <- struct{}{}:
		default:
		}
	}
	if len(f.updateResponses) == 0 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	response := f.updateResponses[0]
	f.updateResponses = f.updateResponses[1:]
	return response.updates, response.err
}

func (f *fakeBotAPI) Send(ctx context.Context, _ tgbotapi.Chattable) (tgbotapi.Message, error) {
	f.sendCount++
	if f.sendBlock != nil {
		select {
		case <-f.sendBlock:
		case <-ctx.Done():
			return tgbotapi.Message{}, ctx.Err()
		}
	}
	return tgbotapi.Message{}, f.sendErr
}

func (f *fakeBotAPI) Request(ctx context.Context, req tgbotapi.Chattable) (*tgbotapi.APIResponse, error) {
	f.requestCount++
	if f.requestBlock != nil {
		select {
		case <-f.requestBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	switch config := req.(type) {
	case tgbotapi.EditMessageTextConfig:
		f.editTextCount++
	case tgbotapi.SetMyCommandsConfig:
		f.setCommandsCount++
		f.setCommands = append(f.setCommands, config)
	}
	return &tgbotapi.APIResponse{}, f.requestErr
}

func (f *fakeBotAPI) GetFile(ctx context.Context, _ tgbotapi.FileConfig) (tgbotapi.File, error) {
	f.fileCallCount++
	if f.fileStarted != nil {
		close(f.fileStarted)
	}
	if f.fileBlock != nil {
		select {
		case <-f.fileBlock:
		case <-ctx.Done():
			return tgbotapi.File{}, ctx.Err()
		}
	}
	return f.file, f.fileErr
}

func (f *fakeBotAPI) GetChatAdministrators(ctx context.Context, _ tgbotapi.ChatAdministratorsConfig) ([]tgbotapi.ChatMember, error) {
	f.adminCallCount++
	if f.adminStarted != nil {
		close(f.adminStarted)
	}
	if f.adminBlock != nil {
		select {
		case <-f.adminBlock:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
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

func assertCommand(t *testing.T, commands []tgbotapi.BotCommand, command string) {
	t.Helper()
	for _, got := range commands {
		if got.Command == command {
			return
		}
	}
	t.Fatalf("missing command %q in %#v", command, commands)
}

func assertNoCommand(t *testing.T, commands []tgbotapi.BotCommand, command string) {
	t.Helper()
	for _, got := range commands {
		if got.Command == command {
			t.Fatalf("unexpected command %q in %#v", command, commands)
		}
	}
}
