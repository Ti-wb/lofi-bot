package config

import (
	"os"
	"path/filepath"
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
ALLOWED_CHAT_ID=-1001
ADMIN_USER_IDS=11,22
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
	if cfg.AllowedChatID != -1001 {
		t.Fatalf("unexpected chat id: %d", cfg.AllowedChatID)
	}
	if _, ok := cfg.AdminUserIDs[11]; !ok {
		t.Fatalf("missing admin id 11")
	}
	if cfg.MediaDir != "state/media" && cfg.MediaDir != "state\\media" {
		t.Fatalf("unexpected media dir: %q", cfg.MediaDir)
	}
	if cfg.DatabasePath != "state/queue.db" && cfg.DatabasePath != "state\\queue.db" {
		t.Fatalf("unexpected db path: %q", cfg.DatabasePath)
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

func clearConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"TELEGRAM_BOT_TOKEN",
		"ALLOWED_CHAT_ID",
		"ADMIN_USER_IDS",
		"OBS_HOST",
		"OBS_PORT",
		"OBS_PASSWORD",
		"OBS_MEDIA_SOURCE_NAME",
		"OBS_FALLBACK_FILE",
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
