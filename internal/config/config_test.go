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
ENV_SCHEMA_VERSION=4
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
	if cfg.PlayerMode != "library" {
		t.Fatalf("unexpected player mode: %q", cfg.PlayerMode)
	}
	if cfg.OBSLoopSourceName != "tg_loop_player" {
		t.Fatalf("unexpected loop source: %q", cfg.OBSLoopSourceName)
	}
	if cfg.OBSMusicSourceName != "tg_music_player" {
		t.Fatalf("unexpected music source: %q", cfg.OBSMusicSourceName)
	}
	if cfg.LoopMediaDir != filepath.Join("state", "media", "loops") {
		t.Fatalf("unexpected loop media dir: %q", cfg.LoopMediaDir)
	}
	if cfg.MusicMediaDir != filepath.Join("state", "media", "music") {
		t.Fatalf("unexpected music media dir: %q", cfg.MusicMediaDir)
	}
	if cfg.TelegramBotAPIDir != "./data/telegram-bot-api" {
		t.Fatalf("unexpected telegram bot api dir: %q", cfg.TelegramBotAPIDir)
	}
	if cfg.MaxVideoSizeBytes != 2000*1024*1024 {
		t.Fatalf("unexpected max video size: %d", cfg.MaxVideoSizeBytes)
	}
	if cfg.RetentionDeleteLocalFiles {
		t.Fatalf("retention should keep local files by default")
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
	if backups := backupFiles(t, dir); len(backups) != 0 {
		t.Fatalf("Load should not migrate .env, backups = %v", backups)
	}
}

func TestLoadRejectsInvalidTelegramAPIBaseURL(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("TELEGRAM_API_BASE_URL", "127.0.0.1:8081")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "TELEGRAM_API_BASE_URL must be a valid URL") {
		t.Fatalf("err = %v, want invalid URL error", err)
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
ENV_SCHEMA_VERSION=4
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

func TestLoadRejectsInvalidPlayerMode(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("PLAYER_MODE", "surprise")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PLAYER_MODE must be one of library, queue") {
		t.Fatalf("err = %v, want invalid player mode error", err)
	}
}

func TestLoadRejectsMalformedNumericEnv(t *testing.T) {
	tests := []string{
		"ALLOWED_CHAT_ID",
		"OBS_PORT",
		"MAX_VIDEO_SIZE_MB",
		"MAX_VIDEO_DURATION_SECONDS",
		"MAX_QUEUE_LENGTH",
		"RETENTION_DAYS",
		"RETENTION_MAX_FILES",
	}

	for _, key := range tests {
		t.Run(key, func(t *testing.T) {
			clearConfigEnv(t)
			chdirTemp(t)
			setValidConfigEnv(t)
			t.Setenv(key, "abc")

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), key+" must be an integer") {
				t.Fatalf("err = %v, want invalid integer error for %s", err, key)
			}
		})
	}
}

func TestLoadRejectsMalformedBooleanEnv(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("RETENTION_DELETE_LOCAL_FILES", "sometimes")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "RETENTION_DELETE_LOCAL_FILES must be a boolean") {
		t.Fatalf("err = %v, want invalid boolean error", err)
	}
}

func TestLoadReadsRetentionDeleteLocalFiles(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("RETENTION_DELETE_LOCAL_FILES", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !cfg.RetentionDeleteLocalFiles {
		t.Fatal("retention delete local files should be enabled")
	}
}

func TestLoadRejectsInvalidNumericRanges(t *testing.T) {
	tests := []struct {
		key     string
		value   string
		wantErr string
	}{
		{key: "OBS_PORT", value: "0", wantErr: "OBS_PORT must be between 1 and 65535"},
		{key: "OBS_PORT", value: "65536", wantErr: "OBS_PORT must be between 1 and 65535"},
		{key: "MAX_VIDEO_SIZE_MB", value: "0", wantErr: "MAX_VIDEO_SIZE_MB must be positive"},
		{key: "MAX_VIDEO_DURATION_SECONDS", value: "-1", wantErr: "MAX_VIDEO_DURATION_SECONDS must be non-negative"},
		{key: "MAX_QUEUE_LENGTH", value: "0", wantErr: "MAX_QUEUE_LENGTH must be positive"},
		{key: "RETENTION_DAYS", value: "-1", wantErr: "RETENTION_DAYS must be non-negative"},
		{key: "RETENTION_MAX_FILES", value: "-1", wantErr: "RETENTION_MAX_FILES must be non-negative"},
	}

	for _, tt := range tests {
		t.Run(tt.key+"="+tt.value, func(t *testing.T) {
			clearConfigEnv(t)
			chdirTemp(t)
			setValidConfigEnv(t)
			t.Setenv(tt.key, tt.value)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadAllowsZeroDurationAndRetentionLimits(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("MAX_VIDEO_DURATION_SECONDS", "0")
	t.Setenv("RETENTION_DAYS", "0")
	t.Setenv("RETENTION_MAX_FILES", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxVideoDurationSeconds != 0 {
		t.Fatalf("duration = %d, want 0", cfg.MaxVideoDurationSeconds)
	}
	if cfg.RetentionDays != 0 {
		t.Fatalf("retention days = %d, want 0", cfg.RetentionDays)
	}
	if cfg.RetentionMaxFiles != 0 {
		t.Fatalf("retention max files = %d, want 0", cfg.RetentionMaxFiles)
	}
}

func TestLoadRejectsBlankMediaSourceName(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("OBS_MEDIA_SOURCE_NAME", " \t ")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OBS_MEDIA_SOURCE_NAME is required") {
		t.Fatalf("err = %v, want blank media source error", err)
	}
}

func TestLoadRejectsBlankLibrarySourceNames(t *testing.T) {
	tests := []struct {
		key     string
		wantErr string
	}{
		{key: "OBS_LOOP_SOURCE_NAME", wantErr: "OBS_LOOP_SOURCE_NAME is required"},
		{key: "OBS_MUSIC_SOURCE_NAME", wantErr: "OBS_MUSIC_SOURCE_NAME is required"},
	}

	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			clearConfigEnv(t)
			chdirTemp(t)
			setValidConfigEnv(t)
			t.Setenv(tt.key, " \t ")

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
		})
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

	if err := migrateDotEnv(envPath); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=4")
	assertContains(t, migrated, "TELEGRAM_API_BASE_URL=http://127.0.0.1:8081")
	assertContains(t, migrated, "MAX_VIDEO_SIZE_MB=2000")
	assertContains(t, migrated, "TELEGRAM_API_ID=replace-with-telegram-api-id")
	assertContains(t, migrated, "TELEGRAM_API_HASH=replace-with-telegram-api-hash")
	assertContains(t, migrated, "TELEGRAM_BOT_API_BIN=telegram-bot-api")
	assertContains(t, migrated, "TELEGRAM_BOT_API_HOST=127.0.0.1")
	assertContains(t, migrated, "TELEGRAM_BOT_API_PORT=8081")
	assertContains(t, migrated, "TELEGRAM_BOT_API_DIR=./data/telegram-bot-api")
	assertContains(t, migrated, "RETENTION_DELETE_LOCAL_FILES=false")
	assertContains(t, migrated, "PLAYER_MODE=library")
	assertContains(t, migrated, "OBS_LOOP_SOURCE_NAME=tg_loop_player")
	assertContains(t, migrated, "OBS_MUSIC_SOURCE_NAME=tg_music_player")
	assertContains(t, migrated, "LOOP_MEDIA_DIR=./data/media/loops")
	assertContains(t, migrated, "MUSIC_MEDIA_DIR=./data/media/music")
	if backups := backupFiles(t, dir); len(backups) != 1 {
		t.Fatalf("backups = %v, want 1 backup", backups)
	}
}

func TestLoadMigratesVersionOneDotEnvToVersionTwo(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=1
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
MAX_VIDEO_SIZE_MB=2000
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if err := migrateDotEnv(envPath); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=4")
	assertContains(t, migrated, "TELEGRAM_API_ID=replace-with-telegram-api-id")
	assertContains(t, migrated, "TELEGRAM_API_HASH=replace-with-telegram-api-hash")
	assertContains(t, migrated, "TELEGRAM_BOT_API_BIN=telegram-bot-api")
	assertContains(t, migrated, "TELEGRAM_BOT_API_HOST=127.0.0.1")
	assertContains(t, migrated, "TELEGRAM_BOT_API_PORT=8081")
	assertContains(t, migrated, "TELEGRAM_BOT_API_DIR=./data/telegram-bot-api")
	assertContains(t, migrated, "RETENTION_DELETE_LOCAL_FILES=false")
	assertContains(t, migrated, "PLAYER_MODE=library")
	assertContains(t, migrated, "OBS_LOOP_SOURCE_NAME=tg_loop_player")
	assertContains(t, migrated, "OBS_MUSIC_SOURCE_NAME=tg_music_player")
	assertContains(t, migrated, "LOOP_MEDIA_DIR=./data/media/loops")
	assertContains(t, migrated, "MUSIC_MEDIA_DIR=./data/media/music")
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
TELEGRAM_API_ID=custom-id
TELEGRAM_BOT_API_PORT=9090
ALLOWED_CHAT_ID=-1001
MAX_VIDEO_SIZE_MB=123
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if err := migrateDotEnv(envPath); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=4")
	if countSubstring(migrated, "TELEGRAM_API_BASE_URL=") != 1 {
		t.Fatalf("telegram api base url should not be duplicated:\n%s", migrated)
	}
	if countSubstring(migrated, "TELEGRAM_API_ID=") != 1 {
		t.Fatalf("telegram api id should not be duplicated:\n%s", migrated)
	}
	assertContains(t, migrated, "TELEGRAM_API_ID=custom-id")
	if countSubstring(migrated, "TELEGRAM_BOT_API_PORT=") != 1 {
		t.Fatalf("telegram bot api port should not be duplicated:\n%s", migrated)
	}
	assertContains(t, migrated, "TELEGRAM_BOT_API_PORT=9090")
	if countSubstring(migrated, "MAX_VIDEO_SIZE_MB=") != 1 {
		t.Fatalf("max video size should not be duplicated:\n%s", migrated)
	}
	assertContains(t, migrated, "RETENTION_DELETE_LOCAL_FILES=false")
	assertContains(t, migrated, "PLAYER_MODE=library")
	assertContains(t, migrated, "OBS_LOOP_SOURCE_NAME=tg_loop_player")
	assertContains(t, migrated, "OBS_MUSIC_SOURCE_NAME=tg_music_player")
	assertContains(t, migrated, "LOOP_MEDIA_DIR=./data/media/loops")
	assertContains(t, migrated, "MUSIC_MEDIA_DIR=./data/media/music")
}

func TestLoadMigrationDerivesLibraryDirsFromCustomMediaDir(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=3
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
MEDIA_DIR=/srv/lofi/media
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if err := migrateDotEnv(envPath); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=4")
	assertContains(t, migrated, "MEDIA_DIR=/srv/lofi/media")
	assertContains(t, migrated, "LOOP_MEDIA_DIR=/srv/lofi/media/loops")
	assertContains(t, migrated, "MUSIC_MEDIA_DIR=/srv/lofi/media/music")
	if strings.Contains(migrated, "LOOP_MEDIA_DIR=./data/media/loops") || strings.Contains(migrated, "MUSIC_MEDIA_DIR=./data/media/music") {
		t.Fatalf("migration should not hard-code default media dirs when MEDIA_DIR is set:\n%s", migrated)
	}
}

func TestLoadMigrationDerivesLibraryDirsFromCustomDataDir(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=3
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
DATA_DIR=/srv/lofi/data
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}

	if err := migrateDotEnv(envPath); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	migrated := readFile(t, envPath)
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=4")
	assertContains(t, migrated, "DATA_DIR=/srv/lofi/data")
	assertContains(t, migrated, "LOOP_MEDIA_DIR=/srv/lofi/data/media/loops")
	assertContains(t, migrated, "MUSIC_MEDIA_DIR=/srv/lofi/data/media/music")
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

	if err := migrateDotEnv(envPath); err != nil {
		t.Fatalf("migrate env: %v", err)
	}
	migrated := readFile(t, envPath)
	if countSubstring(migrated, "ENV_SCHEMA_VERSION=") != 1 {
		t.Fatalf("schema version should not be duplicated:\n%s", migrated)
	}
	assertContains(t, migrated, "ENV_SCHEMA_VERSION=4")
	assertContains(t, migrated, "TELEGRAM_API_BASE_URL=http://127.0.0.1:8081")
	assertContains(t, migrated, "TELEGRAM_BOT_API_DIR=./data/telegram-bot-api")
	assertContains(t, migrated, "RETENTION_DELETE_LOCAL_FILES=false")
	assertContains(t, migrated, "PLAYER_MODE=library")
	assertContains(t, migrated, "OBS_LOOP_SOURCE_NAME=tg_loop_player")
	assertContains(t, migrated, "OBS_MUSIC_SOURCE_NAME=tg_music_player")
	assertContains(t, migrated, "LOOP_MEDIA_DIR=./data/media/loops")
	assertContains(t, migrated, "MUSIC_MEDIA_DIR=./data/media/music")
}

func TestLoadDoesNotRemigrateCurrentDotEnv(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=4
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
TELEGRAM_API_ID=replace-with-telegram-api-id
TELEGRAM_API_HASH=replace-with-telegram-api-hash
TELEGRAM_BOT_API_BIN=telegram-bot-api
TELEGRAM_BOT_API_HOST=127.0.0.1
TELEGRAM_BOT_API_PORT=8081
TELEGRAM_BOT_API_DIR=./data/telegram-bot-api
RETENTION_DELETE_LOCAL_FILES=false
PLAYER_MODE=library
OBS_LOOP_SOURCE_NAME=tg_loop_player
OBS_MUSIC_SOURCE_NAME=tg_music_player
LOOP_MEDIA_DIR=./data/media/loops
MUSIC_MEDIA_DIR=./data/media/music
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

func TestMigrateRejectsNewerDotEnvVersion(t *testing.T) {
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

	err := migrateDotEnv(envPath)
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
		"TELEGRAM_API_ID",
		"TELEGRAM_API_HASH",
		"TELEGRAM_BOT_API_BIN",
		"TELEGRAM_BOT_API_HOST",
		"TELEGRAM_BOT_API_PORT",
		"TELEGRAM_BOT_API_DIR",
		"ALLOWED_CHAT_ID",
		"OBS_HOST",
		"OBS_PORT",
		"OBS_PASSWORD",
		"OBS_MEDIA_SOURCE_NAME",
		"OBS_LOOP_SOURCE_NAME",
		"OBS_MUSIC_SOURCE_NAME",
		"OBS_FALLBACK_FILE",
		"FALLBACK_MODE",
		"PLAYER_MODE",
		"DATA_DIR",
		"MEDIA_DIR",
		"LOOP_MEDIA_DIR",
		"MUSIC_MEDIA_DIR",
		"DATABASE_PATH",
		"MAX_VIDEO_SIZE_MB",
		"MAX_VIDEO_DURATION_SECONDS",
		"MAX_QUEUE_LENGTH",
		"RETENTION_DAYS",
		"RETENTION_MAX_FILES",
		"RETENTION_DELETE_LOCAL_FILES",
		"FFPROBE_PATH",
		"LOG_LEVEL",
	}
	for _, key := range keys {
		t.Setenv(key, "")
	}
}

func setValidConfigEnv(t *testing.T) {
	t.Helper()
	t.Setenv("TELEGRAM_BOT_TOKEN", "token")
	t.Setenv("TELEGRAM_API_BASE_URL", "http://127.0.0.1:8081")
	t.Setenv("ALLOWED_CHAT_ID", "-1001")
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
