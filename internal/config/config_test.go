package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadReadsDotEnvAndDefaults(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	body := []byte(`
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
DATA_DIR=./state
OBS_MEDIA_SOURCE_NAME=player
`)
	if err := os.WriteFile(filepath.Join(dir, ".env"), body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TelegramBotToken != "token" {
		t.Fatalf("unexpected token: %q", cfg.TelegramBotToken)
	}
	if cfg.TelegramAPIBaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("unexpected telegram api base url: %q", cfg.TelegramAPIBaseURL)
	}
	if cfg.AllowedChatID != -1001 {
		t.Fatalf("unexpected chat id: %d", cfg.AllowedChatID)
	}
	if cfg.MediaDir != "state/media" && cfg.MediaDir != "state\\media" {
		t.Fatalf("unexpected media dir: %q", cfg.MediaDir)
	}
	if cfg.DatabasePath != "state/queue.db" && cfg.DatabasePath != "state\\queue.db" {
		t.Fatalf("unexpected db path: %q", cfg.DatabasePath)
	}
	if cfg.FallbackMode != "random_played" {
		t.Fatalf("unexpected fallback mode: %q", cfg.FallbackMode)
	}
}

func TestLoadRequiresCoreSettings(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected missing env error")
	}
}

func TestLoadRequiresTelegramAPIBaseURL(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	body := []byte(`
TELEGRAM_BOT_TOKEN=token
ALLOWED_CHAT_ID=-1001
`)
	if err := os.WriteFile(filepath.Join(dir, ".env"), body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	_, err = Load()
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_API_BASE_URL") {
		t.Fatalf("err = %v, want TELEGRAM_API_BASE_URL error", err)
	}
}

func TestLoadRejectsInvalidFallbackMode(t *testing.T) {
	clearConfigEnv(t)
	dir := t.TempDir()
	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("get wd: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(oldwd)
	})
	if err := os.Chdir(dir); err != nil {
		t.Fatalf("chdir: %v", err)
	}

	body := []byte(`
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
FALLBACK_MODE=surprise
`)
	if err := os.WriteFile(filepath.Join(dir, ".env"), body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if _, err := Load(); err == nil {
		t.Fatal("expected invalid fallback mode error")
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"TELEGRAM_BOT_TOKEN",
		"TELEGRAM_API_BASE_URL",
		"ALLOWED_CHAT_ID",
		"OBS_HOST",
		"OBS_PORT",
		"OBS_PASSWORD",
		"OBS_MEDIA_SOURCE_NAME",
		"OBS_FALLBACK_FILE",
		"FALLBACK_MODE",
		"DATA_DIR",
		"MEDIA_DIR",
		"DATABASE_PATH",
		"MAX_VIDEO_SIZE_MB",
		"MAX_VIDEO_DURATION_SECONDS",
		"MAX_QUEUE_LENGTH",
		"RETENTION_DAYS",
		"RETENTION_MAX_FILES",
		"FFPROBE_PATH",
		"LOG_LEVEL",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
}
