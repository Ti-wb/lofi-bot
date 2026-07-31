package telegram

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/tiwb/tg-obs-bot/internal/journalstore"
	"github.com/tiwb/tg-obs-bot/internal/liveness"

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

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
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

	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
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
		},
	}
	svc := newTestService(t, bot)

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want 1", bot.adminCallCount)
	}
}

func TestReadOnlyAdminCheckHonorsNegativeCache(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)

	for i := 0; i < 2; i++ {
		response, err := svc.handleCommand(context.Background(), commandMessage(42, "/library"))
		if err != nil {
			t.Fatalf("library command %d: %v", i+1, err)
		}
		assertNoButton(t, response.markup, "略過循環")
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want 1", bot.adminCallCount)
	}
}

func TestMutationFreshAdminLookupAllowsPromotionAndUpdatesCache(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)

	if _, err := svc.handleCommand(context.Background(), commandMessage(42, "/library")); err != nil {
		t.Fatalf("prime negative cache: %v", err)
	}
	response, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
	if err != nil {
		t.Fatalf("fresh mutation command: %v", err)
	}
	if response.text != "skipped" {
		t.Fatalf("response = %q, want skipped", response.text)
	}
	if bot.adminCallCount != 2 {
		t.Fatalf("admin API calls = %d, want 2", bot.adminCallCount)
	}
	if !svc.isAdmin(context.Background(), testChatID, &tgbotapi.User{ID: 42}) {
		t.Fatal("successful fresh lookup did not update positive cache")
	}
	if bot.adminCallCount != 2 {
		t.Fatalf("cached admin check made another API call: got %d, want 2", bot.adminCallCount)
	}
}

func TestMutationFreshAdminLookupDeniesRevocationAndUpdatesCache(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{chatMember(42, "administrator")}},
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	svc.hooks.SkipLoop = func(context.Context) (string, error) {
		t.Fatal("skip hook should not be called after admin revocation")
		return "", nil
	}

	if _, err := svc.handleCommand(context.Background(), commandMessage(42, "/library")); err != nil {
		t.Fatalf("prime positive cache: %v", err)
	}
	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if bot.adminCallCount != 2 {
		t.Fatalf("admin API calls = %d, want 2", bot.adminCallCount)
	}
	if svc.isAdmin(context.Background(), testChatID, &tgbotapi.User{ID: 42}) {
		t.Fatal("successful fresh lookup did not update negative cache")
	}
	if bot.adminCallCount != 2 {
		t.Fatalf("cached negative check made another API call: got %d, want 2", bot.adminCallCount)
	}
}

func TestAdminBotAdministratorIgnored(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{botChatMember(42, "administrator")}},
		},
	}
	svc := newTestService(t, bot)

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
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

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
}

func TestMutationAdminLookupErrorDoesNotTrustStalePositiveCache(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{err: errors.New("telegram unavailable")},
		},
	}
	svc := newTestService(t, bot)
	cacheOnlyAdmin(svc, 42)
	svc.hooks.SkipLoop = func(context.Context) (string, error) {
		t.Fatal("skip hook should not be called when fresh admin lookup fails")
		return "", nil
	}

	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want 1", bot.adminCallCount)
	}
}

func TestMutationAdminLookupTimeoutFailsClosed(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	bot := &fakeBotAPI{adminBlock: block}
	svc := newTestService(t, bot)
	cacheOnlyAdmin(svc, 42)
	svc.adminLookupTimeout = 20 * time.Millisecond
	svc.hooks.SkipLoop = func(context.Context) (string, error) {
		t.Fatal("skip hook should not be called when fresh admin lookup times out")
		return "", nil
	}

	start := time.Now()
	_, err := svc.handleCommand(context.Background(), commandMessage(42, "/skip loop"))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("admin lookup took %s, want bounded timeout", elapsed)
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want 1", bot.adminCallCount)
	}
}

func TestRemovedQueueAndListCommandsUseUnknownCommandWithoutLibrarySideEffects(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)
	calls := 0
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		calls++
		return LibraryPageResult{Text: "媒體庫", Page: page, TotalPages: 1}, nil
	}

	for _, command := range []string{"/queue", "/list ignored"} {
		t.Run(command, func(t *testing.T) {
			response, err := svc.handleCommand(context.Background(), commandMessage(42, command))
			if err != nil {
				t.Fatalf("handle command: %v", err)
			}
			if !strings.Contains(response.text, "我不認得這個指令") {
				t.Fatalf("response = %q, want unknown command message", response.text)
			}
			assertButton(t, response.markup, "媒體庫", "library")
		})
	}
	if calls != 0 {
		t.Fatalf("library page calls = %d, want 0", calls)
	}
	if bot.adminCallCount != 0 {
		t.Fatalf("admin API calls = %d, want 0 for removed commands", bot.adminCallCount)
	}
}

func TestRemovedQueueCallbacksAreNoOpsWithoutAdminLookup(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)
	calls := 0
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		calls++
		return LibraryPageResult{Text: "媒體庫", Page: page, TotalPages: 1}, nil
	}

	for _, action := range []string{
		"queue",
		"list:legacy-argument",
		"history",
		"remove:1",
		"move:1:2",
		"skip",
	} {
		svc.handleCallback(context.Background(), callbackQuery(42, action))
	}
	if bot.adminCallCount != 0 {
		t.Fatalf("admin API calls = %d, want 0 for removed callbacks", bot.adminCallCount)
	}
	if bot.sendCount != 0 {
		t.Fatalf("send calls = %d, want 0", bot.sendCount)
	}
	if bot.editTextCount != 0 {
		t.Fatalf("edit calls = %d, want 0", bot.editTextCount)
	}
	if calls != 0 {
		t.Fatalf("library page calls = %d, want 0", calls)
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
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		return LibraryPageResult{Text: "媒體庫", Page: page, TotalPages: 1}, nil
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

func TestLibraryPaginationUsesTypedMetadata(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	cacheAdmin(svc, 42)
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		return LibraryPageResult{
			Text:       "文案刻意包含假的頁面：99/100",
			Page:       page,
			TotalPages: 3,
		}, nil
	}

	tests := []struct {
		name       string
		command    string
		refresh    string
		wantPrev   string
		wantNext   string
		noPrevNext string
	}{
		{name: "first", command: "/library", refresh: "library", wantNext: "library:2", noPrevNext: "上一頁"},
		{name: "middle", command: "/library 2", refresh: "library:2", wantPrev: "library:1", wantNext: "library:3"},
		{name: "last", command: "/library 3", refresh: "library:3", wantPrev: "library:2", noPrevNext: "下一頁"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			response, err := svc.handleCommand(context.Background(), commandMessage(42, tt.command))
			if err != nil {
				t.Fatalf("handle command: %v", err)
			}
			assertButton(t, response.markup, "刷新", tt.refresh)
			if tt.wantPrev != "" {
				assertButton(t, response.markup, "上一頁", tt.wantPrev)
			}
			if tt.wantNext != "" {
				assertButton(t, response.markup, "下一頁", tt.wantNext)
			}
			if tt.noPrevNext != "" {
				assertNoButton(t, response.markup, tt.noPrevNext)
			}
		})
	}
}

func TestLibraryPaginationCallbackRequestsTypedPage(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	cacheAdmin(svc, 42)
	requestedPage := 0
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		requestedPage = page
		return LibraryPageResult{Text: "第二頁", Page: page, TotalPages: 4}, nil
	}

	response, err := svc.routeAction(context.Background(), testChatID, &tgbotapi.User{ID: 42}, "library:2")
	if err != nil {
		t.Fatalf("route action: %v", err)
	}
	if requestedPage != 2 {
		t.Fatalf("requested page = %d, want 2", requestedPage)
	}
	assertButton(t, response.markup, "刷新", "library:2")
	assertButton(t, response.markup, "上一頁", "library:1")
	assertButton(t, response.markup, "下一頁", "library:3")
}

func TestLibraryPaginationStaleSecondPageCallbackAfterShrink(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)

	shrunk := false
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		totalPages := 2
		if shrunk {
			totalPages = 1
		}
		if page > totalPages {
			return LibraryPageResult{}, testPublicError(
				fmt.Sprintf("頁碼超出範圍；媒體庫目前共有 %d 頁。", totalPages),
			)
		}
		return LibraryPageResult{
			Text:       fmt.Sprintf("媒體庫第 %d 頁", page),
			Page:       page,
			TotalPages: totalPages,
		}, nil
	}

	original, err := svc.routeAction(context.Background(), testChatID, &tgbotapi.User{ID: 42}, "library:2")
	if err != nil {
		t.Fatalf("open original page 2: %v", err)
	}
	assertButton(t, original.markup, "刷新", "library:2")
	assertButton(t, original.markup, "上一頁", "library:1")
	assertNoButton(t, original.markup, "下一頁")

	shrunk = true
	svc.handleCallback(context.Background(), callbackQuery(42, "library:2"))

	const wantError = "頁碼超出範圍；媒體庫目前共有 1 頁。"
	if bot.editTextCount != 1 {
		t.Fatalf("edit calls = %d, want 1", bot.editTextCount)
	}
	if bot.sendCount != 0 {
		t.Fatalf("send calls = %d, want 0", bot.sendCount)
	}

	var answer *tgbotapi.CallbackConfig
	var edit *tgbotapi.EditMessageTextConfig
	for _, request := range bot.requests {
		switch request := request.(type) {
		case tgbotapi.CallbackConfig:
			captured := request
			answer = &captured
		case tgbotapi.EditMessageTextConfig:
			captured := request
			edit = &captured
		}
	}
	if answer == nil || answer.Text != wantError {
		t.Fatalf("callback answer = %#v, want %q", answer, wantError)
	}
	if edit == nil || edit.Text != wantError {
		t.Fatalf("edited message = %#v, want %q", edit, wantError)
	}
	if edit.ReplyMarkup != nil {
		t.Fatalf("stale callback edit generated pagination metadata: %#v", edit.ReplyMarkup)
	}
}

func TestLibraryPaginationRejectsInvalidHookMetadata(t *testing.T) {
	tests := []struct {
		name   string
		result LibraryPageResult
	}{
		{name: "empty text", result: LibraryPageResult{Page: 2, TotalPages: 3}},
		{name: "zero page", result: LibraryPageResult{Text: "page", Page: 0, TotalPages: 3}},
		{name: "zero total", result: LibraryPageResult{Text: "page", Page: 2, TotalPages: 0}},
		{name: "page exceeds total", result: LibraryPageResult{Text: "page", Page: 4, TotalPages: 3}},
		{name: "page mismatch", result: LibraryPageResult{Text: "page", Page: 1, TotalPages: 3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := newTestService(t, &fakeBotAPI{})
			cacheAdmin(svc, 42)
			svc.hooks.LibraryPage = func(context.Context, int) (LibraryPageResult, error) {
				return tt.result, nil
			}

			_, err := svc.handleCommand(context.Background(), commandMessage(42, "/library 2"))
			if err == nil || !strings.Contains(err.Error(), "library page") {
				t.Fatalf("err = %v, want invalid library page metadata error", err)
			}
		})
	}
}

func TestLibraryPageArgumentBoundaries(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	cacheAdmin(svc, 42)

	for _, command := range []string{"/library 0", "/library -1", "/library nope", "/library 1 2"} {
		t.Run(command, func(t *testing.T) {
			_, err := svc.handleCommand(context.Background(), commandMessage(42, command))
			if !errors.Is(err, errBadCommand) {
				t.Fatalf("err = %v, want bad command", err)
			}
		})
	}
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
		{name: "multi-word theme", text: "/theme Cozy Cafe", want: "theme Cozy Cafe"},
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

func TestThemeArgumentBoundsBeforeHook(t *testing.T) {
	maxTheme := strings.Repeat("t", MaxThemeUTF8Bytes)
	if got := utf8.RuneCountInString(maxTheme); got != MaxThemeRunes {
		t.Fatalf("maximum fixture runes = %d, want %d", got, MaxThemeRunes)
	}

	entrypoints := []struct {
		name   string
		invoke func(*Service, string) (botResponse, error)
	}{
		{
			name: "text command",
			invoke: func(svc *Service, theme string) (botResponse, error) {
				return svc.handleCommand(
					context.Background(),
					commandMessage(42, "/theme "+theme),
				)
			},
		},
		{
			name: "callback action",
			invoke: func(svc *Service, theme string) (botResponse, error) {
				return svc.routeAction(
					context.Background(),
					testChatID,
					&tgbotapi.User{ID: 42},
					"theme:"+theme,
				)
			},
		},
	}

	for _, entrypoint := range entrypoints {
		t.Run(entrypoint.name+"/maximum accepted", func(t *testing.T) {
			svc := newTestService(t, &fakeBotAPI{})
			cacheAdmin(svc, 42)
			calls := 0
			svc.hooks.SetTheme = func(_ context.Context, theme string) (string, error) {
				calls++
				if theme != maxTheme {
					t.Fatalf("hook theme length = %d bytes, want maximum fixture", len(theme))
				}
				return "theme " + theme, nil
			}

			response, err := entrypoint.invoke(svc, maxTheme)
			if err != nil {
				t.Fatalf("maximum theme: %v", err)
			}
			if calls != 1 {
				t.Fatalf("hook calls = %d, want 1", calls)
			}
			if got := utf8.RuneCountInString(response.text); got > 4096 {
				t.Fatalf("response runes = %d, want <= Telegram limit 4096", got)
			}
			if got := len(response.text); got > 4096 {
				t.Fatalf("response UTF-8 bytes = %d, want conservative bound <= 4096", got)
			}
		})

		overlongThemes := []struct {
			name  string
			theme string
		}{
			{
				name:  "rune and byte limit",
				theme: strings.Repeat("t", MaxThemeRunes+1),
			},
			{
				name:  "UTF-8 byte limit",
				theme: strings.Repeat("界", MaxThemeUTF8Bytes/len("界")+1),
			},
		}
		for _, overlong := range overlongThemes {
			t.Run(entrypoint.name+"/rejects "+overlong.name, func(t *testing.T) {
				svc := newTestService(t, &fakeBotAPI{})
				cacheAdmin(svc, 42)
				svc.hooks.SetTheme = func(context.Context, string) (string, error) {
					t.Fatal("oversized theme reached persistence hook")
					return "", nil
				}

				_, err := entrypoint.invoke(svc, overlong.theme)
				if !errors.Is(err, errBadCommand) {
					t.Fatalf("error = %v, want bad command", err)
				}
				if got := friendlyError(err); !strings.Contains(got, "240") {
					t.Fatalf("friendly error = %q, want explicit theme bound", got)
				}
			})
		}
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
		"/select",
		"/skip",
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
	svc.hooks.LibraryPage = func(_ context.Context, page int) (LibraryPageResult, error) {
		return LibraryPageResult{Text: "library refreshed", Page: page, TotalPages: 1}, nil
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
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want 1", bot.adminCallCount)
	}
}

func TestMutationCommandsAlwaysUseOneFreshAdminLookup(t *testing.T) {
	for _, command := range []string{
		"/scan",
		"/theme random",
		"/select clear",
		"/skip loop",
	} {
		t.Run(command, func(t *testing.T) {
			bot := &fakeBotAPI{
				adminResponses: []adminResponse{
					{admins: []tgbotapi.ChatMember{}},
				},
			}
			svc := newTestService(t, bot)
			cacheOnlyAdmin(svc, 42)

			_, err := svc.handleCommand(context.Background(), commandMessage(42, command))
			if !errors.Is(err, errAdminOnly) {
				t.Fatalf("err = %v, want %v", err, errAdminOnly)
			}
			if bot.adminCallCount != 1 {
				t.Fatalf("admin API calls = %d, want exactly 1", bot.adminCallCount)
			}
		})
	}
}

func TestMutationCallbacksAlwaysUseOneFreshAdminLookup(t *testing.T) {
	for _, action := range []string{
		"scan",
		"theme:random",
		"select:clear",
		"skip:loop",
	} {
		t.Run(action, func(t *testing.T) {
			bot := &fakeBotAPI{
				adminResponses: []adminResponse{
					{admins: []tgbotapi.ChatMember{}},
				},
			}
			svc := newTestService(t, bot)
			cacheOnlyAdmin(svc, 42)

			_, err := svc.routeAction(context.Background(), testChatID, &tgbotapi.User{ID: 42}, action)
			if !errors.Is(err, errAdminOnly) {
				t.Fatalf("err = %v, want %v", err, errAdminOnly)
			}
			if bot.adminCallCount != 1 {
				t.Fatalf("admin API calls = %d, want exactly 1", bot.adminCallCount)
			}
		})
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
	for _, command := range []string{"queue", "list", "history", "remove", "move"} {
		assertNoCommand(t, adminCommands, command)
	}
}

func TestRemovedQueueCommandsUseUnknownCommandResponse(t *testing.T) {
	bot := &fakeBotAPI{}
	svc := newTestService(t, bot)

	for _, command := range []string{"/history", "/remove 1", "/move 1 2"} {
		t.Run(command, func(t *testing.T) {
			response, err := svc.handleCommand(context.Background(), commandMessage(42, command))
			if err != nil {
				t.Fatalf("handle command: %v", err)
			}
			if !strings.Contains(response.text, "我不認得這個指令") {
				t.Fatalf("response = %q, want unknown command message", response.text)
			}
			assertButton(t, response.markup, "媒體庫", "library")
			assertNoButton(t, response.markup, "Queue")
			assertNoButton(t, response.markup, "History")
		})
	}
	if bot.adminCallCount != 0 {
		t.Fatalf("admin API calls = %d, want 0 for removed commands", bot.adminCallCount)
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

func TestNewRequiresUpdateJournal(t *testing.T) {
	_, err := New(Config{
		Token:         "token",
		APIBaseURL:    "http://127.0.0.1:8081",
		AllowedChatID: testChatID,
	}, Hooks{}, slog.Default(), WithBotAPI(&fakeBotAPI{}))
	if err == nil || !strings.Contains(err.Error(), "update journal is required") {
		t.Fatalf("New error = %v, want required update journal", err)
	}
}

func TestMetadataForUpdateInfersOnlyCoarseActionAndTelegramIDs(t *testing.T) {
	command := commandMessage(42, "/library")
	command.MessageID = 91
	tests := []struct {
		name   string
		update tgbotapi.Update
		want   updateMetadata
	}{
		{
			name:   "command",
			update: tgbotapi.Update{Message: command},
			want: updateMetadata{
				kind:      "message",
				action:    "library",
				chatID:    testChatID,
				messageID: 91,
				actorID:   42,
			},
		},
		{
			name: "callback",
			update: tgbotapi.Update{CallbackQuery: &tgbotapi.CallbackQuery{
				Data: "remove:12345 extra arguments are not journaled",
				From: &tgbotapi.User{ID: 43},
				Message: &tgbotapi.Message{
					MessageID: 92,
					Chat:      &tgbotapi.Chat{ID: testChatID},
				},
			}},
			want: updateMetadata{
				kind:      "callback_query",
				action:    "remove",
				chatID:    testChatID,
				messageID: 92,
				actorID:   43,
			},
		},
		{
			name: "video upload",
			update: tgbotapi.Update{Message: &tgbotapi.Message{
				MessageID: 93,
				Chat:      &tgbotapi.Chat{ID: testChatID},
				From:      &tgbotapi.User{ID: 44},
				Video:     &tgbotapi.Video{FileID: "file"},
			}},
			want: updateMetadata{
				kind:      "message",
				action:    "upload_video",
				chatID:    testChatID,
				messageID: 93,
				actorID:   44,
			},
		},
		{
			name: "audio upload",
			update: tgbotapi.Update{Message: &tgbotapi.Message{
				MessageID: 95,
				Chat:      &tgbotapi.Chat{ID: testChatID},
				From:      &tgbotapi.User{ID: 46},
				Audio:     &tgbotapi.Audio{FileID: "file"},
			}},
			want: updateMetadata{
				kind:      "message",
				action:    "upload_audio",
				chatID:    testChatID,
				messageID: 95,
				actorID:   46,
			},
		},
		{
			name: "malformed command entity",
			update: tgbotapi.Update{Message: &tgbotapi.Message{
				MessageID: 94,
				Chat:      &tgbotapi.Chat{ID: testChatID},
				From:      &tgbotapi.User{ID: 45},
				Text:      "/q",
				Entities: []tgbotapi.MessageEntity{
					{Type: "bot_command", Offset: 0, Length: 999},
				},
			}},
			want: updateMetadata{
				kind:      "message",
				chatID:    testChatID,
				messageID: 94,
				actorID:   45,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := metadataForUpdate(tt.update); got != tt.want {
				t.Fatalf("metadata = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestProductionBotAPIUpdateAndRequestGatesAreIndependent(t *testing.T) {
	api := &productionBotAPI{
		updateClient:   &contextHTTPClient{},
		updateGate:     newContextGate(),
		requestClient:  &contextHTTPClient{},
		requestGate:    newContextGate(),
		requestTimeout: time.Second,
	}

	<-api.updateGate
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
		t.Fatal("request context blocked behind update gate")
	}
	api.updateGate <- struct{}{}

	<-api.requestGate
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
		t.Fatal("update context blocked behind request gate")
	}
	api.requestGate <- struct{}{}
}

func TestProductionBotAPIRequestGateWaitUsesTotalTimeoutAndRecovers(t *testing.T) {
	api := &productionBotAPI{
		requestClient:  &contextHTTPClient{},
		requestGate:    newContextGate(),
		requestTimeout: 20 * time.Millisecond,
	}
	<-api.requestGate

	called := false
	start := time.Now()
	err := api.withRequestContext(context.Background(), func() error {
		called = true
		return nil
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("gate wait took %s, want configured total timeout", elapsed)
	}
	if called {
		t.Fatal("timed-out gate waiter executed its callback")
	}

	api.requestGate <- struct{}{}
	if err := api.withRequestContext(context.Background(), func() error {
		called = true
		return nil
	}); err != nil {
		t.Fatalf("request after timeout: %v", err)
	}
	if !called {
		t.Fatal("request after timeout did not execute")
	}
	if got := len(api.requestGate); got != 1 {
		t.Fatalf("request gate tokens = %d, want 1", got)
	}
}

func TestProductionBotAPIWaitingRequestCanCancelAndGateRecovers(t *testing.T) {
	var sendCalls atomic.Int32
	firstStarted := make(chan struct{})
	releaseFirst := make(chan struct{})
	defer func() {
		select {
		case <-releaseFirst:
		default:
			close(releaseFirst)
		}
	}()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/getMe"):
			_, _ = io.WriteString(w, `{"ok":true,"result":{"id":1,"is_bot":true,"first_name":"test","username":"test_bot"}}`)
		case strings.HasSuffix(req.URL.Path, "/sendMessage"):
			call := sendCalls.Add(1)
			if call == 1 {
				close(firstStarted)
				select {
				case <-releaseFirst:
				case <-req.Context().Done():
					return
				}
			}
			_, _ = io.WriteString(w, `{"ok":true,"result":{"message_id":1,"date":0,"chat":{"id":1,"type":"private"},"text":"ok"}}`)
		default:
			http.NotFound(w, req)
		}
	}))
	defer server.Close()

	api, err := newProductionBotAPI(Config{
		Token:          "test-token",
		APIBaseURL:     server.URL,
		UpdateTimeout:  1,
		RequestTimeout: 6 * time.Second,
	})
	if err != nil {
		t.Fatalf("new production bot API: %v", err)
	}

	firstDone := make(chan error, 1)
	go func() {
		_, err := api.Send(context.Background(), tgbotapi.NewMessage(1, "first"))
		firstDone <- err
	}()
	select {
	case <-firstStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first request did not reach blocking server")
	}

	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		_, err := api.Send(secondCtx, tgbotapi.NewMessage(1, "second"))
		secondDone <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancelSecond()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("second request err = %v, want %v", err, context.Canceled)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("canceled gate waiter did not return promptly")
	}
	if got := sendCalls.Load(); got != 1 {
		t.Fatalf("HTTP send calls = %d, want 1 while first request holds gate", got)
	}

	close(releaseFirst)
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatalf("first request: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first request did not finish after release")
	}
	if _, err := api.Send(context.Background(), tgbotapi.NewMessage(1, "third")); err != nil {
		t.Fatalf("third request after cancellation: %v", err)
	}
	if got := sendCalls.Load(); got != 2 {
		t.Fatalf("HTTP send calls = %d, want 2 after third request", got)
	}
	if got := len(api.requestGate); got != 1 {
		t.Fatalf("request gate tokens = %d, want 1", got)
	}
}

func TestContextHTTPClientAttachesCallerContext(t *testing.T) {
	base := &blockingHTTPClient{seen: make(chan context.Context, 1)}
	client := &contextHTTPClient{base: base}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client.setContext(ctx)
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
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
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
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
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
	_, err := svc.handleCommand(ctx, commandMessage(42, "/skip loop"))

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
		_, err := svc.handleCommand(ctx, commandMessage(42, "/skip loop"))
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

func TestNextTelegramOffsetBoundaries(t *testing.T) {
	tests := []struct {
		name     string
		updateID int
		want     int
		wantErr  bool
	}{
		{name: "negative", updateID: -1, wantErr: true},
		{name: "zero", updateID: 0, want: 1},
		{name: "largest representable successor", updateID: math.MaxInt - 1, want: math.MaxInt},
		{name: "unrepresentable successor", updateID: math.MaxInt, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := nextTelegramOffset(test.updateID)
			if test.wantErr {
				if err == nil {
					t.Fatalf("nextTelegramOffset(%d) succeeded with %d", test.updateID, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("nextTelegramOffset(%d): %v", test.updateID, err)
			}
			if got != test.want {
				t.Fatalf("nextTelegramOffset(%d) = %d, want %d", test.updateID, got, test.want)
			}
		})
	}
}

func TestRunRetriesMalformedUpdateBatchBeforeJournalMutation(t *testing.T) {
	journal := newFakeUpdateJournal()
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
				{UpdateID: math.MaxInt, Message: commandMessage(42, "/library")},
			}},
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
		},
		updateCalls: make(chan struct{}, 3),
	}
	svc := newTestServiceWithJournal(t, bot, journal)
	svc.pollRetryDelay = time.Millisecond
	var handledMu sync.Mutex
	var handled []int
	svc.updateHandler = func(_ context.Context, update tgbotapi.Update) error {
		handledMu.Lock()
		defer handledMu.Unlock()
		handled = append(handled, update.UpdateID)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()

	waitForUpdateCalls(t, bot.updateCalls, 3)
	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after cancellation")
	}

	if len(bot.updateConfigs) < 3 {
		t.Fatalf("update configs = %d, want at least 3", len(bot.updateConfigs))
	}
	for poll, want := range []int{0, 0, 11} {
		if got := bot.updateConfigs[poll].Offset; got != want {
			t.Fatalf("poll %d offset = %d, want %d", poll+1, got, want)
		}
	}
	handledMu.Lock()
	defer handledMu.Unlock()
	if len(handled) != 1 || handled[0] != 10 {
		t.Fatalf("handled updates = %v, want [10]", handled)
	}
	next, confirmed, attempt, ok := journal.snapshot(10)
	if !ok || next != 11 || confirmed != 0 || attempt.status != "done" {
		t.Fatalf(
			"update 10 state = next=%d confirmed=%d attempt=%#v ok=%v",
			next,
			confirmed,
			attempt,
			ok,
		)
	}
	if _, _, _, ok := journal.snapshot(math.MaxInt); ok {
		t.Fatal("malformed MaxInt update reached the journal")
	}
}

func TestRunLivenessAdvancesDuringCappedPollRetryOutage(t *testing.T) {
	getUpdatesErr := errors.New("local bot api unavailable")
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{{err: getUpdatesErr}},
		updateCalls:     make(chan struct{}, 1),
	}
	svc := newTestService(t, bot)
	// Initial == max makes the first dependency failure a capped-backoff wait.
	// The deterministic jitter result remains long enough to observe several
	// worker-owned progress pulses without a successful Telegram response.
	svc.pollRetryDelay = time.Second
	svc.pollRetryMaxDelay = time.Second
	svc.pollProviderHintMax = time.Second
	svc.retryRandom = func() uint64 { return 0 }

	registry := liveness.NewRegistry(liveness.Options{ProgressInterval: 2 * time.Millisecond})
	var tracker *liveness.Worker
	for _, binding := range []struct {
		id    liveness.WorkerID
		owner liveness.Owner
	}{
		{id: liveness.WorkerTelegram, owner: liveness.OwnerTelegram},
		{id: liveness.WorkerOBSReconnect, owner: liveness.OwnerOBSReconnect},
		{id: liveness.WorkerOBSEvents, owner: liveness.OwnerOBSEvents},
		{id: liveness.WorkerMaintenance, owner: liveness.OwnerMaintenance},
		{id: liveness.WorkerPlayback, owner: liveness.OwnerLibraryScheduler},
	} {
		worker, err := registry.Bind(binding.id, binding.owner)
		if err != nil {
			t.Fatalf("Bind(%s): %v", binding.id, err)
		}
		if binding.id == liveness.WorkerTelegram {
			tracker = worker
		}
	}
	if err := registry.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}

	ctx, cancel := context.WithCancel(liveness.WithWorker(context.Background(), tracker))
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()

	select {
	case <-bot.updateCalls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Telegram outage was not exercised")
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for tracker.Snapshot().Phase != liveness.PhaseRetryWait {
		if time.Now().After(deadline) {
			t.Fatalf("worker phase = %s, want retry wait", tracker.Snapshot().Phase)
		}
		time.Sleep(time.Millisecond)
	}
	start := tracker.Snapshot().Sequence
	for tracker.Snapshot().Sequence < start+3 {
		if time.Now().After(deadline) {
			t.Fatalf(
				"retry sequence = %d, want at least %d without external success",
				tracker.Snapshot().Sequence,
				start+3,
			)
		}
		time.Sleep(time.Millisecond)
	}

	cancel()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after cancellation")
	}
	if bot.updateCallCount != 1 {
		t.Fatalf("GetUpdates calls = %d, want one failed operation and no success", bot.updateCallCount)
	}
}

func TestRunRestartsFromDurableOffsetAndConfirmsOnSuccessfulNextPoll(t *testing.T) {
	journal := newFakeUpdateJournal()
	handled := atomic.Int32{}

	firstBot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
		},
		updateCalls: make(chan struct{}, 2),
	}
	first := newTestServiceWithJournal(t, firstBot, journal)
	first.updateHandler = func(context.Context, tgbotapi.Update) error {
		handled.Add(1)
		return nil
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- first.Run(firstCtx)
	}()
	waitForUpdateCalls(t, firstBot.updateCalls, 2)
	cancelFirst()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run error = %v, want cancellation", err)
	}

	next, confirmed, attempt, ok := journal.snapshot(10)
	if !ok {
		t.Fatal("durable attempt for update 10 is missing")
	}
	if next != 11 || confirmed != 0 || attempt.status != "done" {
		t.Fatalf(
			"after first process: next=%d confirmed=%d status=%q, want 11/0/done",
			next,
			confirmed,
			attempt.status,
		)
	}

	secondBot := &fakeBotAPI{
		updateResponses: []updateResponse{{}},
		updateCalls:     make(chan struct{}, 2),
	}
	second := newTestServiceWithJournal(t, secondBot, journal)
	second.updateHandler = func(context.Context, tgbotapi.Update) error {
		t.Fatal("confirmed update was replayed after restart")
		return nil
	}
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondErr := make(chan error, 1)
	go func() {
		secondErr <- second.Run(secondCtx)
	}()
	waitForUpdateCalls(t, secondBot.updateCalls, 2)
	cancelSecond()
	if err := <-secondErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("second Run error = %v, want cancellation", err)
	}

	if got := secondBot.updateConfigs[0].Offset; got != 11 {
		t.Fatalf("restart poll offset = %d, want durable offset 11", got)
	}
	next, confirmed, _, _ = journal.snapshot(10)
	if next != 11 || confirmed != 11 {
		t.Fatalf("confirmed checkpoint = (%d, %d), want (11, 11)", next, confirmed)
	}
	if got := handled.Load(); got != 1 {
		t.Fatalf("handler executions = %d, want exactly 1", got)
	}
}

func TestRunRestartAcrossTelegramGapAbandonsOnlySafeOldAttempts(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	store, err := journalstore.Open(ctx, dbPath)
	if err != nil {
		t.Fatalf("open real update journal: %v", err)
	}
	defer store.Close()
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open fixture database: %v", err)
	}
	defer db.Close()

	now := time.Now().UTC()
	created := now.Add(-2 * time.Hour).Format(time.RFC3339Nano)
	fixtures := []struct {
		id         int
		status     string
		owner      string
		leaseUntil any
		lastError  string
	}{
		{id: 5, status: "failed", lastError: "earlier handler failure"},
		{id: 6, status: "running", owner: "expired-owner", leaseUntil: now.Add(-time.Hour).Format(time.RFC3339Nano)},
		{id: 7, status: "running", owner: "active-old-owner", leaseUntil: now.Add(time.Hour).Format(time.RFC3339Nano)},
		{id: 11, status: "running", owner: "boundary-owner", leaseUntil: now.Add(time.Hour).Format(time.RFC3339Nano)},
		{id: 12, status: "pending"},
	}
	for _, fixture := range fixtures {
		if _, err := db.ExecContext(ctx, `
INSERT INTO telegram_update_attempts (
	update_id, update_kind, action, chat_id, message_id, actor_id,
	attempt_count, failure_count, status, owner_token, lease_until,
	last_error, created_at, updated_at, finished_at
) VALUES (?, 'message', 'library', ?, 1, 42, 1, 0, ?, ?, ?, ?, ?, ?, NULL)
`,
			fixture.id,
			testChatID,
			fixture.status,
			fixture.owner,
			fixture.leaseUntil,
			fixture.lastError,
			created,
			created,
		); err != nil {
			t.Fatalf("insert old attempt %d: %v", fixture.id, err)
		}
	}

	firstBot := &fakeBotAPI{
		updateResponses: []updateResponse{{updates: []tgbotapi.Update{
			{UpdateID: 10, Message: commandMessage(42, "/library")},
		}}},
		updateCalls: make(chan struct{}, 2),
	}
	first := newTestServiceWithJournal(t, firstBot, store)
	first.now = time.Now
	var handled atomic.Int32
	first.updateHandler = func(context.Context, tgbotapi.Update) error {
		handled.Add(1)
		return nil
	}
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstErr := make(chan error, 1)
	go func() {
		firstErr <- first.Run(firstCtx)
	}()
	waitForUpdateCalls(t, firstBot.updateCalls, 2)
	cancelFirst()
	if err := <-firstErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("first Run error = %v, want cancellation", err)
	}
	next, confirmed, err := store.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load checkpoint before restart: %v", err)
	}
	if next != 11 || confirmed != 0 {
		t.Fatalf("checkpoint before restart = (%d, %d), want (11, 0)", next, confirmed)
	}

	secondBot := &fakeBotAPI{
		updateResponses: []updateResponse{{}},
		updateCalls:     make(chan struct{}, 2),
	}
	second := newTestServiceWithJournal(t, secondBot, store)
	second.now = time.Now
	second.updateHandler = func(context.Context, tgbotapi.Update) error {
		t.Fatal("restart unexpectedly executed an update")
		return nil
	}
	secondCtx, cancelSecond := context.WithCancel(context.Background())
	secondErr := make(chan error, 1)
	go func() {
		secondErr <- second.Run(secondCtx)
	}()
	waitForUpdateCalls(t, secondBot.updateCalls, 2)
	cancelSecond()
	if err := <-secondErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("second Run error = %v, want cancellation", err)
	}
	if got := handled.Load(); got != 1 {
		t.Fatalf("higher update handler executions = %d, want 1", got)
	}

	const abandonedError = "update abandoned after Telegram confirmed a higher offset"
	for _, fixture := range []struct {
		id        int
		status    string
		owner     string
		lastError string
	}{
		{id: 5, status: "dead", lastError: abandonedError},
		{id: 6, status: "dead", lastError: abandonedError},
		{id: 7, status: "running", owner: "active-old-owner"},
		{id: 11, status: "running", owner: "boundary-owner"},
		{id: 12, status: "pending"},
	} {
		var status, owner, lastError string
		var lease, finished sql.NullString
		if err := db.QueryRowContext(ctx, `
SELECT status, owner_token, lease_until, last_error, finished_at
FROM telegram_update_attempts
WHERE update_id = ?
`, fixture.id).Scan(&status, &owner, &lease, &lastError, &finished); err != nil {
			t.Fatalf("inspect reconciled update %d: %v", fixture.id, err)
		}
		if status != fixture.status || owner != fixture.owner || lastError != fixture.lastError {
			t.Fatalf(
				"update %d = status=%q owner=%q error=%q, want %q/%q/%q",
				fixture.id,
				status,
				owner,
				lastError,
				fixture.status,
				fixture.owner,
				fixture.lastError,
			)
		}
		if fixture.status == "dead" && (lease.Valid || !finished.Valid) {
			t.Fatalf(
				"abandoned update %d lease=%#v finished=%#v",
				fixture.id,
				lease,
				finished,
			)
		}
	}
	next, confirmed, err = store.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load checkpoint after restart: %v", err)
	}
	if next != 11 || confirmed != 11 {
		t.Fatalf("checkpoint after restart = (%d, %d), want (11, 11)", next, confirmed)
	}

	oldFinished := now.Add(-100 * 24 * time.Hour).Format(time.RFC3339Nano)
	if _, err := db.ExecContext(ctx, `
UPDATE telegram_update_attempts
SET finished_at = ?
WHERE update_id IN (5, 6)
`, oldFinished); err != nil {
		t.Fatalf("age abandoned attempts: %v", err)
	}
	_, prunedDead, err := store.PruneTelegramUpdateJournal(ctx)
	if err != nil {
		t.Fatalf("prune abandoned attempts: %v", err)
	}
	if prunedDead != 2 || prunedDead > 256 {
		t.Fatalf("pruned dead rows = %d, want 2 within batch", prunedDead)
	}
	for _, id := range []int{5, 6} {
		var count int
		if err := db.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM telegram_update_attempts WHERE update_id = ?`,
			id,
		).Scan(&count); err != nil {
			t.Fatalf("inspect pruned update %d: %v", id, err)
		}
		if count != 0 {
			t.Fatalf("abandoned update %d was not pruned", id)
		}
	}
}

func TestRunReplaysAfterTerminalJournalWriteFailureOnRestart(t *testing.T) {
	journal := newFakeUpdateJournal()
	journal.completeErr = errors.New("injected commit failure")
	var handled atomic.Int32

	firstBot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
		},
	}
	first := newTestServiceWithJournal(t, firstBot, journal)
	first.updateHandler = func(context.Context, tgbotapi.Update) error {
		handled.Add(1)
		return nil
	}
	err := first.Run(context.Background())
	if !errors.Is(err, ErrUpdateJournalFailure) || !strings.Contains(err.Error(), "injected commit failure") {
		t.Fatalf("first Run error = %v, want fail-closed journal error", err)
	}
	next, _, attempt, ok := journal.snapshot(10)
	if !ok ||
		next != 0 ||
		attempt.status != "pending" ||
		!attempt.leaseUntil.IsZero() {
		t.Fatalf("failed commit state = next=%d attempt=%#v ok=%v", next, attempt, ok)
	}

	journal.mu.Lock()
	journal.completeErr = nil
	journal.mu.Unlock()
	secondBot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
		},
		updateCalls: make(chan struct{}, 2),
	}
	second := newTestServiceWithJournal(t, secondBot, journal)
	second.updateHandler = func(context.Context, tgbotapi.Update) error {
		handled.Add(1)
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- second.Run(ctx)
	}()
	waitForUpdateCalls(t, secondBot.updateCalls, 2)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("restart Run error = %v, want cancellation", err)
	}

	next, _, attempt, ok = journal.snapshot(10)
	if !ok ||
		next != 11 ||
		attempt.status != "done" ||
		attempt.attemptCount != 2 ||
		attempt.failureCount != 0 {
		t.Fatalf("restart state = next=%d attempt=%#v ok=%v", next, attempt, ok)
	}
	if got := handled.Load(); got != 2 {
		t.Fatalf("handler executions = %d, want replay after uncertain commit", got)
	}
}

func TestRunQuarantinesPanickingHandlerAfterThreeRestarts(t *testing.T) {
	journal := newFakeUpdateJournal()
	var handled atomic.Int32

	for attemptNumber := 1; attemptNumber <= maxUpdateHandlerFailures; attemptNumber++ {
		bot := &fakeBotAPI{
			updateResponses: []updateResponse{
				{updates: []tgbotapi.Update{
					{UpdateID: 10, Message: commandMessage(42, "/library")},
				}},
			},
		}
		svc := newTestServiceWithJournal(t, bot, journal)
		svc.updateHandler = func(context.Context, tgbotapi.Update) error {
			handled.Add(1)
			panic("deterministic panic")
		}

		err := svc.Run(context.Background())
		if !errors.Is(err, ErrUpdateHandlerPanic) {
			t.Fatalf("attempt %d error = %v, want panic sentinel", attemptNumber, err)
		}
		if got := len(bot.updateConfigs); got != 1 {
			t.Fatalf("attempt %d GetUpdates calls = %d, want 1", attemptNumber, got)
		}
	}

	next, _, attempt, ok := journal.snapshot(10)
	if !ok ||
		next != 11 ||
		attempt.status != "dead" ||
		attempt.attemptCount != maxUpdateHandlerFailures ||
		attempt.failureCount != maxUpdateHandlerFailures {
		t.Fatalf("poison state = next=%d attempt=%#v ok=%v", next, attempt, ok)
	}
	if got := handled.Load(); got != maxUpdateHandlerFailures {
		t.Fatalf("handler executions = %d, want %d", got, maxUpdateHandlerFailures)
	}

	// Even the generation that atomically dead-letters the third panic must
	// stop. Only a clean process generation may poll past the quarantined row.
	restartBot := &fakeBotAPI{
		updateResponses: []updateResponse{{}},
		updateCalls:     make(chan struct{}, 2),
	}
	restart := newTestServiceWithJournal(t, restartBot, journal)
	restart.updateHandler = func(context.Context, tgbotapi.Update) error {
		t.Fatal("dead update was executed in the clean restart")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- restart.Run(ctx)
	}()
	waitForUpdateCalls(t, restartBot.updateCalls, 2)
	if got := restartBot.updateConfigs[0].Offset; got != 11 {
		t.Fatalf("clean restart offset = %d, want 11", got)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("clean restart error = %v, want cancellation", err)
	}
}

func TestRunRecoversHandlerPanicWithoutAcknowledgingUpdate(t *testing.T) {
	journal := newFakeUpdateJournal()
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
		},
	}
	svc := newTestServiceWithJournal(t, bot, journal)
	svc.updateHandler = func(context.Context, tgbotapi.Update) error {
		panic("boom")
	}

	err := svc.Run(context.Background())
	if !errors.Is(err, ErrUpdateHandlerPanic) {
		t.Fatalf("Run error = %v, want panic sentinel", err)
	}
	next, _, attempt, ok := journal.snapshot(10)
	if !ok || next != 0 || attempt.status != "failed" || attempt.attemptCount != 1 {
		t.Fatalf("panic state = next=%d attempt=%#v ok=%v", next, attempt, ok)
	}
}

func TestRunJournalFailuresAreFailClosedBeforeAcknowledgement(t *testing.T) {
	journalErr := errors.New("injected journal failure")
	tests := []struct {
		name      string
		configure func(*fakeUpdateJournal)
		wantCalls int
		wantBegin bool
	}{
		{
			name: "load",
			configure: func(journal *fakeUpdateJournal) {
				journal.loadErr = journalErr
			},
			wantCalls: 0,
		},
		{
			name: "confirm",
			configure: func(journal *fakeUpdateJournal) {
				journal.confirmErr = journalErr
			},
			wantCalls: 1,
		},
		{
			name: "begin",
			configure: func(journal *fakeUpdateJournal) {
				journal.beginErr = journalErr
			},
			wantCalls: 1,
		},
		{
			name: "complete",
			configure: func(journal *fakeUpdateJournal) {
				journal.completeErr = journalErr
			},
			wantCalls: 1,
			wantBegin: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			journal := newFakeUpdateJournal()
			tt.configure(journal)
			bot := &fakeBotAPI{
				updateResponses: []updateResponse{
					{updates: []tgbotapi.Update{
						{UpdateID: 10, Message: commandMessage(42, "/library")},
					}},
				},
			}
			svc := newTestServiceWithJournal(t, bot, journal)
			var handled atomic.Bool
			svc.updateHandler = func(context.Context, tgbotapi.Update) error {
				handled.Store(true)
				return nil
			}

			err := svc.Run(context.Background())
			if !errors.Is(err, ErrUpdateJournalFailure) || !errors.Is(err, journalErr) {
				t.Fatalf("Run error = %v, want fail-closed journal error", err)
			}
			if bot.updateCallCount != tt.wantCalls {
				t.Fatalf("GetUpdates calls = %d, want %d", bot.updateCallCount, tt.wantCalls)
			}
			if handled.Load() != tt.wantBegin {
				t.Fatalf("handler called = %v, want %v", handled.Load(), tt.wantBegin)
			}
			next, _, _, _ := journal.snapshot(10)
			if next != 0 {
				t.Fatalf("durable next offset = %d, want 0", next)
			}
		})
	}
}

func TestRunCancellationDuringJournalConfirmationRemainsGraceful(t *testing.T) {
	journal := &blockingConfirmJournal{
		fakeUpdateJournal: newFakeUpdateJournal(),
		started:           make(chan struct{}),
	}
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{{}},
		updateCalls:     make(chan struct{}, 1),
	}
	svc := newTestServiceWithJournal(t, bot, journal)
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-journal.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("journal confirmation did not start")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want cancellation", err)
		}
		if errors.Is(err, ErrUpdateJournalFailure) {
			t.Fatalf("normal cancellation was classified as journal failure: %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after confirmation cancellation")
	}
}

func TestRunBoundsEveryPollingJournalOperation(t *testing.T) {
	tests := []struct {
		operation   string
		wantPolls   int
		wantHandled bool
	}{
		{operation: "load"},
		{operation: "confirm", wantPolls: 1},
		{operation: "begin", wantPolls: 1},
		{operation: "complete", wantPolls: 1, wantHandled: true},
	}

	for _, tt := range tests {
		t.Run(tt.operation, func(t *testing.T) {
			journal := &contextBlockingJournal{
				fakeUpdateJournal: newFakeUpdateJournal(),
				blockOperation:    tt.operation,
			}
			responses := []updateResponse{{}}
			if tt.operation == "begin" || tt.operation == "complete" {
				responses = []updateResponse{{updates: []tgbotapi.Update{
					{UpdateID: 10, Message: commandMessage(42, "/library")},
				}}}
			}
			bot := &fakeBotAPI{updateResponses: responses}
			svc := newTestServiceWithJournal(t, bot, journal)
			svc.journalWriteTimeout = 20 * time.Millisecond
			var handled atomic.Bool
			svc.updateHandler = func(context.Context, tgbotapi.Update) error {
				handled.Store(true)
				return nil
			}

			started := time.Now()
			err := svc.Run(context.Background())
			if !errors.Is(err, ErrUpdateJournalFailure) ||
				!errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("Run error = %v, want bounded journal deadline", err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("journal operation blocked for %s", elapsed)
			}
			if got := bot.updateCallCount; got != tt.wantPolls {
				t.Fatalf("GetUpdates calls = %d, want %d", got, tt.wantPolls)
			}
			if got := handled.Load(); got != tt.wantHandled {
				t.Fatalf("handler called = %v, want %v", got, tt.wantHandled)
			}
		})
	}
}

func TestRunCancellationAfterHandlerSuccessStillCommitsTerminalAttempt(t *testing.T) {
	journal := &blockingCompleteJournal{
		fakeUpdateJournal: newFakeUpdateJournal(),
		started:           make(chan struct{}),
		release:           make(chan struct{}),
	}
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{{updates: []tgbotapi.Update{
			{UpdateID: 10, Message: commandMessage(42, "/library")},
		}}},
	}
	svc := newTestServiceWithJournal(t, bot, journal)
	svc.updateHandler = func(context.Context, tgbotapi.Update) error {
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-journal.started:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("terminal journal write did not start")
	}
	cancel()
	close(journal.release)

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run error = %v, want parent cancellation", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not stop after terminal write")
	}
	next, _, attempt, ok := journal.snapshot(10)
	if !ok ||
		next != 11 ||
		attempt.status != "done" ||
		attempt.failureCount != 0 {
		t.Fatalf("terminal cancellation race = next=%d attempt=%#v ok=%v", next, attempt, ok)
	}
}

func TestRunThreeCooperativeShutdownsDoNotPoisonUpdate(t *testing.T) {
	journal := newFakeUpdateJournal()

	for generation := 1; generation <= 3; generation++ {
		bot := &fakeBotAPI{
			updateResponses: []updateResponse{{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}}},
		}
		svc := newTestServiceWithJournal(t, bot, journal)
		started := make(chan struct{})
		svc.updateHandler = func(ctx context.Context, _ tgbotapi.Update) error {
			close(started)
			<-ctx.Done()
			return ctx.Err()
		}
		ctx, cancel := context.WithCancel(context.Background())
		errCh := make(chan error, 1)
		go func() {
			errCh <- svc.Run(ctx)
		}()
		select {
		case <-started:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("generation %d handler did not start", generation)
		}
		cancel()
		select {
		case err := <-errCh:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("generation %d Run error = %v, want cancellation", generation, err)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("generation %d did not stop", generation)
		}

		next, _, attempt, ok := journal.snapshot(10)
		if !ok ||
			next != 0 ||
			attempt.status != "pending" ||
			attempt.failureCount != 0 ||
			attempt.attemptCount != generation {
			t.Fatalf(
				"generation %d state = next=%d attempt=%#v ok=%v",
				generation,
				next,
				attempt,
				ok,
			)
		}
	}
}

func TestRunAdvancesUpdateOffset(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
				{UpdateID: 11, Message: commandMessage(42, "/now")},
			}},
		},
		updateCalls: make(chan struct{}, 2),
	}
	svc := newTestService(t, bot)
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
	if got := bot.updateConfigs[0].Limit; got != 1 {
		t.Fatalf("update limit = %d, want 1", got)
	}
}

func TestRunCooperativeUpdateTimeoutPreservesOrderAndNextOffsets(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
			{updates: []tgbotapi.Update{
				{UpdateID: 11, Message: commandMessage(42, "/now")},
			}},
		},
		updateCalls: make(chan struct{}, 3),
	}
	svc := newTestService(t, bot)
	cacheOnlyAdmins(svc)
	svc.updateProcessingTimeout = 20 * time.Millisecond
	svc.updateHandlerStopGrace = 500 * time.Millisecond
	events := make(chan string, 3)
	releaseFirstHook := make(chan struct{})
	var activeHandlers atomic.Int32
	var maxActiveHandlers atomic.Int32
	recordActive := func() func() {
		active := activeHandlers.Add(1)
		for {
			maxActive := maxActiveHandlers.Load()
			if active <= maxActive || maxActiveHandlers.CompareAndSwap(maxActive, active) {
				break
			}
		}
		return func() {
			activeHandlers.Add(-1)
		}
	}
	svc.hooks.LibraryPage = func(ctx context.Context, page int) (LibraryPageResult, error) {
		defer recordActive()()
		events <- "first-start"
		<-ctx.Done()
		events <- "first-deadline"
		<-releaseFirstHook
		return LibraryPageResult{}, ctx.Err()
	}
	svc.hooks.Now = func(context.Context) (string, error) {
		defer recordActive()()
		events <- "second"
		return "now", nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()

	select {
	case <-bot.updateCalls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first poll did not start")
	}
	for _, want := range []string{"first-start", "first-deadline"} {
		select {
		case got := <-events:
			if got != want {
				t.Fatalf("event = %q, want %q", got, want)
			}
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for %q", want)
		}
	}
	select {
	case <-bot.updateCalls:
		t.Fatal("second poll started before first update handler returned")
	default:
	}
	close(releaseFirstHook)

	select {
	case <-bot.updateCalls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second poll did not start after first handler returned")
	}
	select {
	case got := <-events:
		if got != "second" {
			t.Fatalf("event = %q, want second", got)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("second update was not processed")
	}
	select {
	case <-bot.updateCalls:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("third poll did not start after second update")
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
	if len(bot.updateConfigs) < 3 {
		t.Fatalf("update configs = %d, want at least 3", len(bot.updateConfigs))
	}
	for i, wantOffset := range []int{0, 11, 12} {
		if got := bot.updateConfigs[i].Offset; got != wantOffset {
			t.Fatalf("poll %d offset = %d, want %d", i+1, got, wantOffset)
		}
		if got := bot.updateConfigs[i].Limit; got != 1 {
			t.Fatalf("poll %d limit = %d, want 1", i+1, got)
		}
	}
	if got := maxActiveHandlers.Load(); got != 1 {
		t.Fatalf("maximum active handlers = %d, want 1", got)
	}
}

func TestRunStuckUpdateHandlerReturnsSentinelWithoutNextPoll(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
			{updates: []tgbotapi.Update{
				{UpdateID: 11, Message: commandMessage(42, "/now")},
			}},
		},
		updateCalls: make(chan struct{}, 2),
	}
	svc := newTestService(t, bot)
	cacheOnlyAdmins(svc)
	svc.updateProcessingTimeout = 20 * time.Millisecond
	svc.updateHandlerStopGrace = 20 * time.Millisecond

	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	releaseHandler := make(chan struct{})
	svc.hooks.LibraryPage = func(context.Context, int) (LibraryPageResult, error) {
		close(handlerStarted)
		defer close(handlerDone)
		<-releaseHandler
		return LibraryPageResult{Text: "library", Page: 1, TotalPages: 1}, nil
	}
	svc.hooks.Now = func(context.Context) (string, error) {
		t.Error("second update handler must not run after a stuck handler")
		return "", nil
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(context.Background())
	}()

	select {
	case <-handlerStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stuck handler did not start")
	}
	select {
	case err := <-errCh:
		if !errors.Is(err, ErrUpdateHandlerStuck) {
			t.Fatalf("err = %v, want %v", err, ErrUpdateHandlerStuck)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("err = %v, want deadline cause", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not return after update deadline and stop grace")
	}

	if got := len(bot.updateConfigs); got != 1 {
		t.Fatalf("GetUpdates calls = %d, want exactly 1", got)
	}
	if got := bot.updateConfigs[0].Offset; got != 0 {
		t.Fatalf("first poll offset = %d, want 0", got)
	}
	select {
	case <-bot.updateCalls:
	default:
		t.Fatal("first GetUpdates call was not observed")
	}
	select {
	case <-bot.updateCalls:
		t.Fatal("next GetUpdates started after a stuck handler")
	default:
	}

	close(releaseHandler)
	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("stuck test handler did not drain after release")
	}
	journal := svc.updateJournal.(*fakeUpdateJournal)
	next, _, attempt, ok := journal.snapshot(10)
	if !ok || next != 0 || attempt.status != "stuck" || attempt.failureCount != 1 {
		t.Fatalf("stuck handler journal = next=%d attempt=%#v ok=%v", next, attempt, ok)
	}
}

func TestRunStuckHandlerConvergesToDeadAcrossProcessGenerations(t *testing.T) {
	journal := newFakeUpdateJournal()
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)

	for generation := 1; generation <= maxUpdateHandlerFailures; generation++ {
		journal.mu.Lock()
		journal.now = now
		journal.mu.Unlock()
		bot := &fakeBotAPI{
			updateResponses: []updateResponse{{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}}},
		}
		svc := newTestServiceWithJournal(t, bot, journal)
		svc.now = func() time.Time { return now }
		svc.updateProcessingTimeout = 10 * time.Millisecond
		svc.updateHandlerStopGrace = 10 * time.Millisecond
		svc.stuckOwnerLease = time.Second
		release := make(chan struct{})
		svc.updateHandler = func(context.Context, tgbotapi.Update) error {
			<-release
			return nil
		}

		err := svc.Run(context.Background())
		close(release)
		if !errors.Is(err, ErrUpdateHandlerStuck) {
			t.Fatalf("generation %d error = %v, want stuck sentinel", generation, err)
		}
		if got := len(bot.updateConfigs); got != 1 {
			t.Fatalf("generation %d polls = %d, want 1", generation, got)
		}

		next, _, attempt, ok := journal.snapshot(10)
		wantStatus := "stuck"
		wantNext := 0
		if generation == maxUpdateHandlerFailures {
			wantStatus = "dead"
			wantNext = 11
		}
		if !ok ||
			next != wantNext ||
			attempt.status != wantStatus ||
			attempt.attemptCount != generation ||
			attempt.failureCount != generation {
			t.Fatalf(
				"generation %d state = next=%d attempt=%#v ok=%v",
				generation,
				next,
				attempt,
				ok,
			)
		}
		now = now.Add(2 * time.Second)
	}

	restartBot := &fakeBotAPI{
		updateResponses: []updateResponse{{}},
		updateCalls:     make(chan struct{}, 2),
	}
	restart := newTestServiceWithJournal(t, restartBot, journal)
	restart.now = func() time.Time { return now }
	restart.updateHandler = func(context.Context, tgbotapi.Update) error {
		t.Fatal("dead stuck update was executed after restart")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- restart.Run(ctx)
	}()
	waitForUpdateCalls(t, restartBot.updateCalls, 2)
	if got := restartBot.updateConfigs[0].Offset; got != 11 {
		t.Fatalf("restart poll offset = %d, want 11", got)
	}
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("restart error = %v, want cancellation", err)
	}
}

func TestRunBusyAttemptDoesNotExecuteHandler(t *testing.T) {
	journal := newFakeUpdateJournal()
	journal.attempts[10] = &fakeJournalAttempt{
		kind:         "message",
		action:       "library",
		attemptCount: 1,
		status:       "running",
		ownerToken:   "other-process",
		leaseUntil:   journal.now.Add(time.Minute),
	}
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{{updates: []tgbotapi.Update{
			{UpdateID: 10, Message: commandMessage(42, "/library")},
		}}},
	}
	svc := newTestServiceWithJournal(t, bot, journal)
	var handled atomic.Bool
	svc.updateHandler = func(context.Context, tgbotapi.Update) error {
		handled.Store(true)
		return nil
	}

	err := svc.Run(context.Background())
	if !errors.Is(err, ErrUpdateAttemptBusy) {
		t.Fatalf("Run error = %v, want busy sentinel", err)
	}
	if handled.Load() {
		t.Fatal("busy update executed handler")
	}
	if got := len(bot.updateConfigs); got != 1 {
		t.Fatalf("GetUpdates calls = %d, want 1", got)
	}
}

func TestRunTerminalReplayRepairsCheckpointWithoutHandler(t *testing.T) {
	journal := newFakeUpdateJournal()
	journal.attempts[10] = &fakeJournalAttempt{
		kind:         "message",
		action:       "library",
		attemptCount: 1,
		status:       "done",
	}
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
			{},
		},
		updateCalls: make(chan struct{}, 2),
	}
	svc := newTestServiceWithJournal(t, bot, journal)
	svc.updateHandler = func(context.Context, tgbotapi.Update) error {
		t.Fatal("terminal replay executed handler")
		return nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	waitForUpdateCalls(t, bot.updateCalls, 2)
	cancel()
	if err := <-errCh; !errors.Is(err, context.Canceled) {
		t.Fatalf("Run error = %v, want cancellation", err)
	}
	next, confirmed, attempt, ok := journal.snapshot(10)
	if !ok ||
		next != 11 ||
		confirmed != 11 ||
		attempt.status != "done" ||
		attempt.attemptCount != 1 {
		t.Fatalf(
			"terminal replay state = next=%d confirmed=%d attempt=%#v ok=%v",
			next,
			confirmed,
			attempt,
			ok,
		)
	}
}

func TestRunParentCancellationWaitsOnlyForHandlerStopGrace(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 10, Message: commandMessage(42, "/library")},
			}},
		},
		updateCalls: make(chan struct{}, 1),
	}
	svc := newTestService(t, bot)
	cacheOnlyAdmins(svc)
	svc.updateProcessingTimeout = time.Hour
	svc.updateHandlerStopGrace = 20 * time.Millisecond

	handlerStarted := make(chan struct{})
	handlerDone := make(chan struct{})
	releaseHandler := make(chan struct{})
	svc.hooks.LibraryPage = func(context.Context, int) (LibraryPageResult, error) {
		close(handlerStarted)
		defer close(handlerDone)
		<-releaseHandler
		return LibraryPageResult{Text: "library", Page: 1, TotalPages: 1}, nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- svc.Run(ctx)
	}()
	select {
	case <-handlerStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("handler did not start")
	}
	cancel()

	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want parent cancellation", err)
		}
		if !errors.Is(err, ErrUpdateHandlerStuck) {
			t.Fatalf("err = %v, want %v", err, ErrUpdateHandlerStuck)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Run did not bound parent-cancel handler drain")
	}
	if got := len(bot.updateConfigs); got != 1 {
		t.Fatalf("GetUpdates calls = %d, want exactly 1", got)
	}

	close(releaseHandler)
	select {
	case <-handlerDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("test handler did not drain after release")
	}
}

func TestRunSkipsStaleUpdates(t *testing.T) {
	bot := &fakeBotAPI{
		updateResponses: []updateResponse{
			{updates: []tgbotapi.Update{
				{UpdateID: 5, Message: commandMessage(42, "/library")},
			}},
			{updates: []tgbotapi.Update{
				{UpdateID: 5, Message: commandMessage(42, "/library")},
				{UpdateID: 6, Message: commandMessage(42, "/library")},
			}},
		},
		updateCalls: make(chan struct{}, 3),
	}
	svc := newTestService(t, bot)
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
	svc.hooks.ImportUpload = func(_ context.Context, upload Upload) (string, error) {
		got = upload
		return "imported", nil
	}

	response, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 0))
	if err != nil {
		t.Fatalf("handle upload: %v", err)
	}
	if response.text != "imported" {
		t.Fatalf("response = %q, want imported", response.text)
	}
	if got.LocalPath != "/tmp/video.mp4" {
		t.Fatalf("local path = %q, want /tmp/video.mp4", got.LocalPath)
	}
	if got.SizeBytes != 42 {
		t.Fatalf("size = %d, want 42", got.SizeBytes)
	}
}

func TestUploadDefaultResponseAndKeyboardAreLibraryOnly(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{
			FilePath: "/tmp/video.mp4",
			FileSize: 42,
		},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		return "", nil
	}

	response, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 42))
	if err != nil {
		t.Fatalf("handle upload: %v", err)
	}
	if !strings.Contains(response.text, "已匯入媒體庫") {
		t.Fatalf("response = %q, want library import wording", response.text)
	}
	assertButton(t, response.markup, "媒體庫", "library")
	assertButton(t, response.markup, "預覽", "preview")
	assertNoButton(t, response.markup, "Queue")
	assertNoButton(t, response.markup, "History")
}

func TestUploadPreflightRejectionAvoidsGetFile(t *testing.T) {
	preflightErr := testPublicError("upload is not admissible")
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "/tmp/video.mp4", FileSize: 1},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	var got Upload
	svc.hooks.PreflightUpload = func(_ context.Context, upload Upload) error {
		got = upload
		return preflightErr
	}
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))
	if !errors.Is(err, preflightErr) {
		t.Fatalf("err = %v, want %v", err, preflightErr)
	}
	if got.FileID != "file-id" || got.SizeBytes != 1 || got.LocalPath != "" {
		t.Fatalf("preflight upload = %#v, want parsed metadata without a local path", got)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestUploadMissingPreflightFailsClosedBeforeGetFile(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "/tmp/video.mp4", FileSize: 1},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.PreflightUpload = nil
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))
	var hookErr errHookNotConfigured
	if !errors.As(err, &hookErr) || hookErr != "preflight upload" {
		t.Fatalf("err = %v, want missing preflight hook", err)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestUploadMissingImportHookFailsClosedBeforePreflightOrGetFile(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "/tmp/video.mp4", FileSize: 1},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.ImportUpload = nil
	preflightCalls := 0
	svc.hooks.PreflightUpload = func(context.Context, Upload) error {
		preflightCalls++
		return nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))
	var hookErr errHookNotConfigured
	if !errors.As(err, &hookErr) || hookErr != "import upload" {
		t.Fatalf("err = %v, want missing import hook", err)
	}
	if preflightCalls != 0 {
		t.Fatalf("preflight calls = %d, want 0", preflightCalls)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
}

func TestUploadRequiresAdmin(t *testing.T) {
	bot := &fakeBotAPI{
		adminResponses: []adminResponse{
			{admins: []tgbotapi.ChatMember{}},
		},
	}
	svc := newTestService(t, bot)
	cacheOnlyAdmin(svc, 42)
	preflightCalls := 0
	svc.hooks.PreflightUpload = func(context.Context, Upload) error {
		preflightCalls++
		return nil
	}
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 1))
	if !errors.Is(err, errAdminOnly) {
		t.Fatalf("err = %v, want %v", err, errAdminOnly)
	}
	if bot.fileCallCount != 0 {
		t.Fatalf("getFile calls = %d, want 0", bot.fileCallCount)
	}
	if preflightCalls != 0 {
		t.Fatalf("preflight calls = %d, want 0 before fresh-admin authorization", preflightCalls)
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("admin API calls = %d, want exactly 1 fresh lookup", bot.adminCallCount)
	}
	if svc.isAdmin(context.Background(), testChatID, &tgbotapi.User{ID: 42}) {
		t.Fatal("library upload fresh lookup did not replace stale positive cache")
	}
	if bot.adminCallCount != 1 {
		t.Fatalf("cached negative check made another API call: got %d, want 1", bot.adminCallCount)
	}
}

func TestUploadAcceptsAudioAndVideoDocuments(t *testing.T) {
	tests := []struct {
		name     string
		message  *tgbotapi.Message
		filePath string
		wantName string
		wantMIME string
		wantKind UploadKind
	}{
		{
			name:     "audio mime",
			message:  documentMessage("file-id", "unique-id", "song.bin", "audio/mpeg", 12),
			filePath: "/tmp/song.mp3",
			wantName: "song.bin",
			wantMIME: "audio/mpeg",
			wantKind: UploadKindDocument,
		},
		{
			name:     "video extension",
			message:  documentMessage("file-id", "unique-id", "clip.mkv", "application/octet-stream", 12),
			filePath: "/tmp/clip.mkv",
			wantName: "clip.mkv",
			wantMIME: "application/octet-stream",
			wantKind: UploadKindDocument,
		},
		{
			name:     "telegram audio",
			message:  audioMessage("file-id", "unique-id", "music_lofi.mp3", "audio/mpeg", 12),
			filePath: "/tmp/music_lofi.mp3",
			wantName: "music_lofi.mp3",
			wantMIME: "audio/mpeg",
			wantKind: UploadKindAudio,
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
			svc.hooks.ImportUpload = func(_ context.Context, upload Upload) (string, error) {
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
			if got.Kind != tt.wantKind {
				t.Fatalf("kind = %q, want %q", got.Kind, tt.wantKind)
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

func TestHandleMessageRoutesAudioUploads(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{
			FilePath: "/tmp/music_lofi.mp3",
			FileSize: 12,
		},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	var got Upload
	svc.hooks.ImportUpload = func(_ context.Context, upload Upload) (string, error) {
		got = upload
		return "imported", nil
	}

	svc.handleMessage(context.Background(), audioMessage("file-id", "unique-id", "music_lofi.mp3", "audio/mpeg", 12))

	if got.Kind != UploadKindAudio {
		t.Fatalf("kind = %q, want %q", got.Kind, UploadKindAudio)
	}
	if got.LocalPath != "/tmp/music_lofi.mp3" {
		t.Fatalf("local path = %q, want /tmp/music_lofi.mp3", got.LocalPath)
	}
	if bot.sendCount != 1 {
		t.Fatalf("send calls = %d, want 1", bot.sendCount)
	}
}

func TestUploadRejectsUnsupportedDocumentsBeforeGetFile(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "/tmp/notes.pdf"},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
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
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
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
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
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
	preflightCalls := 0
	svc.hooks.PreflightUpload = func(_ context.Context, upload Upload) error {
		preflightCalls++
		if upload.SizeBytes != 10 || upload.LocalPath != "" {
			t.Fatalf("preflight upload = %#v, want declared size and no local path", upload)
		}
		return nil
	}
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
		return "", nil
	}

	_, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 10))

	if !errors.Is(err, errUploadTooLarge) {
		t.Fatalf("err = %v, want %v", err, errUploadTooLarge)
	}
	if preflightCalls != 1 {
		t.Fatalf("preflight calls = %d, want 1", preflightCalls)
	}
	if bot.fileCallCount != 1 {
		t.Fatalf("getFile calls = %d, want 1 for post-download size recheck", bot.fileCallCount)
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
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		return "imported", nil
	}

	response, err := svc.handleUpload(context.Background(), videoMessage("file-id", "unique-id", 10))

	if err != nil {
		t.Fatalf("handle upload: %v", err)
	}
	if response.text != "imported" {
		t.Fatalf("response = %q, want imported", response.text)
	}
}

func TestUploadRejectsRelativeLocalBotAPIFilePath(t *testing.T) {
	bot := &fakeBotAPI{
		file: tgbotapi.File{FilePath: "relative/video.mp4"},
	}
	svc := newTestService(t, bot)
	cacheAdmin(svc, 42)
	svc.hooks.ImportUpload = func(context.Context, Upload) (string, error) {
		t.Fatal("import hook should not be called")
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
	return newTestServiceWithJournal(t, bot, newFakeUpdateJournal())
}

func newTestServiceWithJournal(t *testing.T, bot *fakeBotAPI, journal UpdateJournal) *Service {
	t.Helper()
	svc, err := New(Config{
		Token:         "token",
		APIBaseURL:    "http://127.0.0.1:8081",
		AllowedChatID: testChatID,
	}, Hooks{
		PreflightUpload: func(ctx context.Context, _ Upload) error {
			return ctx.Err()
		},
		LibraryPage: func(_ context.Context, page int) (LibraryPageResult, error) {
			return LibraryPageResult{Text: "媒體庫", Page: page, TotalPages: page}, nil
		},
		SkipLoop: func(context.Context) (string, error) {
			return "skipped", nil
		},
	}, slog.Default(), WithBotAPI(bot), WithUpdateJournal(journal))
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	svc.now = func() time.Time {
		return now
	}
	return svc
}

type fakeJournalAttempt struct {
	kind         string
	action       string
	chatID       int64
	messageID    int
	actorID      int64
	attemptCount int
	failureCount int
	status       string
	ownerToken   string
	leaseUntil   time.Time
	lastError    string
}

type fakeUpdateJournal struct {
	mu sync.Mutex

	nextOffset      int
	confirmedOffset int
	attempts        map[int]*fakeJournalAttempt
	now             time.Time

	loadErr     error
	confirmErr  error
	beginErr    error
	completeErr error
	abortErr    error
	failErr     error
}

type blockingConfirmJournal struct {
	*fakeUpdateJournal
	started chan struct{}
}

func (j *blockingConfirmJournal) ConfirmUpdateOffset(ctx context.Context, _ int) error {
	close(j.started)
	<-ctx.Done()
	return ctx.Err()
}

type blockingCompleteJournal struct {
	*fakeUpdateJournal
	started chan struct{}
	release chan struct{}
}

func (j *blockingCompleteJournal) CompleteUpdateAttempt(
	ctx context.Context,
	updateID int,
	nextOffset int,
	ownerToken string,
) error {
	close(j.started)
	select {
	case <-j.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	return j.fakeUpdateJournal.CompleteUpdateAttempt(ctx, updateID, nextOffset, ownerToken)
}

type contextBlockingJournal struct {
	*fakeUpdateJournal
	blockOperation string
}

func (j *contextBlockingJournal) wait(ctx context.Context, operation string) error {
	if operation != j.blockOperation {
		return nil
	}
	<-ctx.Done()
	return ctx.Err()
}

func (j *contextBlockingJournal) LoadUpdateCheckpoint(ctx context.Context) (int, int, error) {
	if err := j.wait(ctx, "load"); err != nil {
		return 0, 0, err
	}
	return j.fakeUpdateJournal.LoadUpdateCheckpoint(ctx)
}

func (j *contextBlockingJournal) ConfirmUpdateOffset(ctx context.Context, offset int) error {
	if err := j.wait(ctx, "confirm"); err != nil {
		return err
	}
	return j.fakeUpdateJournal.ConfirmUpdateOffset(ctx, offset)
}

func (j *contextBlockingJournal) BeginUpdateAttempt(
	ctx context.Context,
	updateID int,
	kind string,
	action string,
	chatID int64,
	messageID int,
	actorID int64,
	ownerToken string,
	leaseUntil time.Time,
	maxFailures int,
) (string, int, int, error) {
	if err := j.wait(ctx, "begin"); err != nil {
		return "", 0, 0, err
	}
	return j.fakeUpdateJournal.BeginUpdateAttempt(
		ctx,
		updateID,
		kind,
		action,
		chatID,
		messageID,
		actorID,
		ownerToken,
		leaseUntil,
		maxFailures,
	)
}

func (j *contextBlockingJournal) CompleteUpdateAttempt(
	ctx context.Context,
	updateID int,
	nextOffset int,
	ownerToken string,
) error {
	if err := j.wait(ctx, "complete"); err != nil {
		return err
	}
	return j.fakeUpdateJournal.CompleteUpdateAttempt(ctx, updateID, nextOffset, ownerToken)
}

func newFakeUpdateJournal() *fakeUpdateJournal {
	return &fakeUpdateJournal{
		attempts: make(map[int]*fakeJournalAttempt),
		now:      time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC),
	}
}

func (j *fakeUpdateJournal) LoadUpdateCheckpoint(context.Context) (int, int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.nextOffset, j.confirmedOffset, j.loadErr
}

func (j *fakeUpdateJournal) ConfirmUpdateOffset(_ context.Context, offset int) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.confirmErr != nil {
		return j.confirmErr
	}
	if offset > j.nextOffset {
		return errors.New("confirmed offset exceeds next offset")
	}
	if offset > j.confirmedOffset {
		j.confirmedOffset = offset
	}
	return nil
}

func (j *fakeUpdateJournal) BeginUpdateAttempt(
	_ context.Context,
	updateID int,
	kind string,
	action string,
	chatID int64,
	messageID int,
	actorID int64,
	ownerToken string,
	leaseUntil time.Time,
	maxFailures int,
) (string, int, int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.beginErr != nil {
		return "", 0, 0, j.beginErr
	}
	nextOffset, err := nextTelegramOffset(updateID)
	if err != nil {
		return "", 0, 0, err
	}
	attempt := j.attempts[updateID]
	if attempt == nil {
		attempt = &fakeJournalAttempt{
			attemptCount: 1,
			status:       "running",
			ownerToken:   ownerToken,
			leaseUntil:   leaseUntil,
		}
		j.attempts[updateID] = attempt
		j.setMetadata(attempt, kind, action, chatID, messageID, actorID)
		return updateBeginExecute, 1, 0, nil
	}
	switch attempt.status {
	case "done":
		j.advance(nextOffset)
		return updateBeginAlreadyTerminal, attempt.attemptCount, attempt.failureCount, nil
	case "dead":
		j.advance(nextOffset)
		return updateBeginDead, attempt.attemptCount, attempt.failureCount, nil
	case "running", "stuck":
		if attempt.leaseUntil.After(j.now) {
			return updateBeginBusy, attempt.attemptCount, attempt.failureCount, nil
		}
		if attempt.status == "running" {
			attempt.failureCount++
			attempt.lastError = "update owner lease expired before completion"
			if attempt.failureCount >= maxFailures {
				attempt.status = "dead"
				attempt.ownerToken = ""
				attempt.leaseUntil = time.Time{}
				j.advance(nextOffset)
				return updateBeginDead, attempt.attemptCount, attempt.failureCount, nil
			}
		}
	case "pending":
		attempt.lastError = ""
	case "failed":
	default:
		return "", 0, 0, errors.New("unknown attempt status")
	}
	attempt.attemptCount++
	j.setMetadata(attempt, kind, action, chatID, messageID, actorID)
	attempt.status = "running"
	attempt.ownerToken = ownerToken
	attempt.leaseUntil = leaseUntil
	return updateBeginExecute, attempt.attemptCount, attempt.failureCount, nil
}

func (j *fakeUpdateJournal) CompleteUpdateAttempt(
	_ context.Context,
	updateID int,
	nextOffset int,
	ownerToken string,
) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.completeErr != nil {
		return j.completeErr
	}
	attempt := j.attempts[updateID]
	if attempt == nil || attempt.status != "running" || attempt.ownerToken != ownerToken {
		return errors.New("attempt is not running")
	}
	attempt.status = "done"
	attempt.ownerToken = ""
	attempt.leaseUntil = time.Time{}
	j.advance(nextOffset)
	return nil
}

func (j *fakeUpdateJournal) AbortUpdateAttempt(
	_ context.Context,
	updateID int,
	ownerToken string,
) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.abortErr != nil {
		return j.abortErr
	}
	attempt := j.attempts[updateID]
	if attempt == nil || attempt.status != "running" || attempt.ownerToken != ownerToken {
		return errors.New("attempt is not owned")
	}
	attempt.status = "pending"
	attempt.ownerToken = ""
	attempt.leaseUntil = time.Time{}
	attempt.lastError = ""
	return nil
}

func (j *fakeUpdateJournal) FailUpdateAttempt(
	_ context.Context,
	updateID int,
	nextOffset int,
	ownerToken string,
	maxFailures int,
	cause string,
	holdLeaseUntil time.Time,
) (bool, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.failErr != nil {
		return false, j.failErr
	}
	attempt := j.attempts[updateID]
	if attempt == nil || attempt.status != "running" || attempt.ownerToken != ownerToken {
		return false, errors.New("attempt is not running")
	}
	attempt.failureCount++
	attempt.lastError = cause
	if attempt.failureCount >= maxFailures {
		attempt.status = "dead"
		attempt.ownerToken = ""
		attempt.leaseUntil = time.Time{}
		j.advance(nextOffset)
		return true, nil
	}
	if holdLeaseUntil.IsZero() {
		attempt.status = "failed"
		attempt.ownerToken = ""
		attempt.leaseUntil = time.Time{}
	} else {
		attempt.status = "stuck"
		attempt.leaseUntil = holdLeaseUntil
	}
	return false, nil
}

func (j *fakeUpdateJournal) setMetadata(
	attempt *fakeJournalAttempt,
	kind string,
	action string,
	chatID int64,
	messageID int,
	actorID int64,
) {
	attempt.kind = kind
	attempt.action = action
	attempt.chatID = chatID
	attempt.messageID = messageID
	attempt.actorID = actorID
}

func (j *fakeUpdateJournal) advance(nextOffset int) {
	if nextOffset > j.nextOffset {
		j.nextOffset = nextOffset
	}
}

func (j *fakeUpdateJournal) snapshot(updateID int) (int, int, fakeJournalAttempt, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	attempt, ok := j.attempts[updateID]
	if !ok {
		return j.nextOffset, j.confirmedOffset, fakeJournalAttempt{}, false
	}
	return j.nextOffset, j.confirmedOffset, *attempt, true
}

func waitForUpdateCalls(t *testing.T, calls <-chan struct{}, count int) {
	t.Helper()
	for call := 1; call <= count; call++ {
		select {
		case <-calls:
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("timed out waiting for GetUpdates call %d", call)
		}
	}
}

func cacheAdmin(svc *Service, userID int64) {
	cacheOnlyAdmin(svc, userID)
	if bot, ok := svc.bot.(*fakeBotAPI); ok {
		bot.defaultAdmins = []tgbotapi.ChatMember{chatMember(userID, "administrator")}
	}
}

func cacheOnlyAdmin(svc *Service, userID int64) {
	cacheOnlyAdmins(svc, userID)
}

func cacheOnlyAdmins(svc *Service, userIDs ...int64) {
	adminIDs := make(map[int64]struct{}, len(userIDs))
	for _, userID := range userIDs {
		adminIDs[userID] = struct{}{}
	}
	svc.adminCache[testChatID] = adminCacheEntry{
		adminIDs:  adminIDs,
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

func audioMessage(fileID string, uniqueID string, name string, mimeType string, size int) *tgbotapi.Message {
	return &tgbotapi.Message{
		Chat: &tgbotapi.Chat{ID: testChatID},
		From: &tgbotapi.User{ID: 42},
		Audio: &tgbotapi.Audio{
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
	defaultAdmins    []tgbotapi.ChatMember
	updateResponses  []updateResponse
	adminCallCount   int
	updateCallCount  int
	updateConfigs    []tgbotapi.UpdateConfig
	sendCount        int
	requestCount     int
	editTextCount    int
	setCommandsCount int
	setCommands      []tgbotapi.SetMyCommandsConfig
	requests         []tgbotapi.Chattable
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
	f.requests = append(f.requests, req)
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
		return f.defaultAdmins, nil
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
