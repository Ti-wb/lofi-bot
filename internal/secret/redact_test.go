package secret

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestRedactStringMasksExplicitSecrets(t *testing.T) {
	got := RedactString(
		"token=123456:ABCdefghi_jklmnop password=obs-secret hash=telegram-hash",
		"123456:ABCdefghi_jklmnop",
		"obs-secret",
		"telegram-hash",
	)

	for _, leaked := range []string{"123456:ABCdefghi_jklmnop", "obs-secret", "telegram-hash"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted string leaked %q: %q", leaked, got)
		}
	}
	if strings.Count(got, redacted) != 3 {
		t.Fatalf("redacted string = %q, want three redactions", got)
	}
}

func TestRedactStringMasksTelegramBotURLToken(t *testing.T) {
	for _, input := range []string{
		`Post "http://127.0.0.1:8081/bot123456:ABCdefghi_jklmnop/getMe": connection refused`,
		`Get "https://api.telegram.org/bot123456:ABCdefghi_jklmnop/getUpdates?timeout=30": EOF`,
	} {
		got := RedactString(input)
		if strings.Contains(got, "123456:ABCdefghi_jklmnop") {
			t.Fatalf("redacted string leaked token: %q", got)
		}
		if !strings.Contains(got, "/bot"+redacted) {
			t.Fatalf("redacted string = %q, want redacted bot URL", got)
		}
	}
}

func TestRedactErrorPreservesErrorsIs(t *testing.T) {
	err := fmt.Errorf("request failed for /bot123456:ABCdefghi_jklmnop/getMe: %w", context.Canceled)
	got := RedactError(err)

	if !errors.Is(got, context.Canceled) {
		t.Fatalf("errors.Is(redacted, context.Canceled) = false")
	}
	if strings.Contains(got.Error(), "123456:ABCdefghi_jklmnop") {
		t.Fatalf("redacted error leaked token: %q", got.Error())
	}
}

func TestRedactStringUsesSensitiveEnvironmentValues(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", "123456:ENVtoken_secret")
	t.Setenv("TELEGRAM_API_HASH", "env-api-hash")
	t.Setenv("OBS_PASSWORD", "env-obs-password")

	got := RedactString("123456:ENVtoken_secret env-api-hash env-obs-password")
	for _, leaked := range []string{"123456:ENVtoken_secret", "env-api-hash", "env-obs-password"} {
		if strings.Contains(got, leaked) {
			t.Fatalf("redacted string leaked %q: %q", leaked, got)
		}
	}
}
