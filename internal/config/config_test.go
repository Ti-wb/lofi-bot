package config

import (
	"errors"
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
ENV_SCHEMA_VERSION=1
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
	if cfg.MaxVideoSizeBytes != 2000*1024*1024 {
		t.Fatalf("unexpected max video size: %d", cfg.MaxVideoSizeBytes)
	}
}

func TestLoadRequiresCoreSettings(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)

	if _, err := Load(); err == nil {
		t.Fatal("expected missing env error")
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf(".env should not be created, stat err = %v", err)
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
ENV_SCHEMA_VERSION=1
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
ENV_SCHEMA_VERSION=1
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

func TestLoadMigratesOldDotEnv(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
TELEGRAM_BOT_TOKEN=token
ALLOWED_CHAT_ID=-1001
OBS_MEDIA_SOURCE_NAME=player
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TelegramAPIBaseURL != "http://127.0.0.1:8081" {
		t.Fatalf("telegram api base url = %q", cfg.TelegramAPIBaseURL)
	}
	if cfg.MaxVideoSizeBytes != 2000*1024*1024 {
		t.Fatalf("max video size = %d", cfg.MaxVideoSizeBytes)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=1")
	assertContains(t, migrated, "TELEGRAM_API_BASE_URL=http://127.0.0.1:8081")
	assertContains(t, migrated, "MAX_VIDEO_SIZE_MB=2000")
	if backups := backupFiles(t, dir); len(backups) != 1 {
		t.Fatalf("backups = %v, want 1 backup", backups)
	}
}

func TestLoadMigrationDoesNotOverwriteExistingValues(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://10.0.0.1:9000
ALLOWED_CHAT_ID=-1001
MAX_VIDEO_SIZE_MB=123
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.TelegramAPIBaseURL != "http://10.0.0.1:9000" {
		t.Fatalf("telegram api base url = %q", cfg.TelegramAPIBaseURL)
	}
	if cfg.MaxVideoSizeBytes != 123*1024*1024 {
		t.Fatalf("max video size = %d", cfg.MaxVideoSizeBytes)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=1")
	if countSubstring(migrated, "TELEGRAM_API_BASE_URL=") != 1 {
		t.Fatalf("telegram api base url should not be duplicated:\n%s", migrated)
	}
	if countSubstring(migrated, "MAX_VIDEO_SIZE_MB=") != 1 {
		t.Fatalf("max video size should not be duplicated:\n%s", migrated)
	}
}

func TestLoadMigrationUpdatesExplicitOldVersion(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=0
TELEGRAM_BOT_TOKEN=token
ALLOWED_CHAT_ID=-1001
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	migrated := readFile(t, envPath)
	if countSubstring(migrated, "ENV_SCHEMA_VERSION=") != 1 {
		t.Fatalf("schema version should not be duplicated:\n%s", migrated)
	}
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=1")
	assertContains(t, migrated, "TELEGRAM_API_BASE_URL=http://127.0.0.1:8081")
}

func TestLoadDoesNotRemigrateCurrentDotEnv(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=1
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	if backups := backupFiles(t, dir); len(backups) != 0 {
		t.Fatalf("backups = %v, want none", backups)
	}
	if got := readFile(t, envPath); got != string(body) {
		t.Fatalf(".env changed:\n%s", got)
	}
}

func TestLoadRejectsNewerDotEnvVersion(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=99
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "newer than this binary supports") {
		t.Fatalf("err = %v, want newer version error", err)
	}
}

func clearConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"ENV_SCHEMA_VERSION",
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

func chdirTemp(t *testing.T) string {
	t.Helper()
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
	return dir
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return string(body)
}

func assertContains(t *testing.T, body string, want string) {
	t.Helper()
	if !strings.Contains(body, want) {
		t.Fatalf("expected %q in:\n%s", want, body)
	}
}

func countSubstring(body string, pattern string) int {
	return strings.Count(body, pattern)
}

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".env.backup.*"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}
