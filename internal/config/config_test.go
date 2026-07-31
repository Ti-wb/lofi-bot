package config

import (
	"errors"
	"fmt"
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
ENV_SCHEMA_VERSION=7
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
ALLOWED_CHAT_ID=-1001
DATA_DIR=./state
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
	if cfg.DatabasePath != "state/state.db" && cfg.DatabasePath != "state\\state.db" {
		t.Fatalf("unexpected db path: %q", cfg.DatabasePath)
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
	if cfg.MinFreeDiskBytes != 512*1024*1024 {
		t.Fatalf("unexpected minimum free disk: %d", cfg.MinFreeDiskBytes)
	}
}

func TestLoadDatabasePathFollowsSchemaCompatibility(t *testing.T) {
	tests := []struct {
		name         string
		schema       string
		databasePath string
		want         string
	}{
		{
			name:   "missing schema keeps legacy default",
			schema: "",
			want:   filepath.Join("runtime", "queue.db"),
		},
		{
			name:   "schema six keeps legacy default",
			schema: "6",
			want:   filepath.Join("runtime", "queue.db"),
		},
		{
			name:   "negative legacy schema keeps legacy default",
			schema: "-1",
			want:   filepath.Join("runtime", "queue.db"),
		},
		{
			name:   "schema seven uses state default",
			schema: "7",
			want:   filepath.Join("runtime", "state.db"),
		},
		{
			name:         "schema six preserves explicit path",
			schema:       "6",
			databasePath: "./custom/library.db",
			want:         "./custom/library.db",
		},
		{
			name:         "schema seven preserves explicit path",
			schema:       "7",
			databasePath: "./custom/library.db",
			want:         "./custom/library.db",
		},
		{
			name:         "schema six treats empty path as legacy default",
			schema:       "6",
			databasePath: "",
			want:         filepath.Join("runtime", "queue.db"),
		},
		{
			name:         "schema seven treats empty path as state default",
			schema:       "7",
			databasePath: "",
			want:         filepath.Join("runtime", "state.db"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			chdirTemp(t)
			setValidConfigEnv(t)
			t.Setenv("ENV_SCHEMA_VERSION", test.schema)
			t.Setenv("DATA_DIR", "./runtime")
			t.Setenv("DATABASE_PATH", test.databasePath)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.DatabasePath != test.want {
				t.Fatalf("DatabasePath = %q, want %q", cfg.DatabasePath, test.want)
			}
		})
	}
}

func TestLoadRejectsInvalidEnvSchemaBeforeChoosingDatabase(t *testing.T) {
	tests := []struct {
		name   string
		schema string
		want   string
	}{
		{name: "malformed", schema: "surprise", want: "ENV_SCHEMA_VERSION must be an integer"},
		{name: "leading plus", schema: "+7", want: "ENV_SCHEMA_VERSION must be an integer"},
		{name: "future", schema: "8", want: "newer than this binary supports (7)"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			clearConfigEnv(t)
			chdirTemp(t)
			setValidConfigEnv(t)
			t.Setenv("ENV_SCHEMA_VERSION", test.schema)

			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("err = %v, want error containing %q", err, test.want)
			}
		})
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

func TestLoadRejectsLegacyQueueMode(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("PLAYER_MODE", "queue")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PLAYER_MODE=queue is no longer supported") ||
		!strings.Contains(err.Error(), "OBS_LOOP_SOURCE_NAME") {
		t.Fatalf("err = %v, want actionable legacy queue mode error", err)
	}
}

func TestLoadAcceptsLegacyLibraryMode(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("PLAYER_MODE", "library")

	if _, err := Load(); err != nil {
		t.Fatalf("load compatibility config: %v", err)
	}
}

func TestLoadRejectsUnknownLegacyPlayerMode(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("PLAYER_MODE", "surprise")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "PLAYER_MODE is deprecated") {
		t.Fatalf("err = %v, want deprecated player mode error", err)
	}
}

func TestLoadIgnoresRemovedQueueOnlySettings(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("OBS_MEDIA_SOURCE_NAME", "obsolete")
	t.Setenv("OBS_FALLBACK_FILE", "/obsolete/fallback.mp4")
	t.Setenv("FALLBACK_MODE", "surprise")
	t.Setenv("MAX_QUEUE_LENGTH", "not-an-integer")
	t.Setenv("RETENTION_DAYS", "not-an-integer")
	t.Setenv("RETENTION_MAX_FILES", "not-an-integer")
	t.Setenv("RETENTION_DELETE_LOCAL_FILES", "not-a-boolean")

	if _, err := Load(); err != nil {
		t.Fatalf("removed queue-only settings should be ignored: %v", err)
	}
}

func TestLoadRejectsMalformedNumericEnv(t *testing.T) {
	tests := []string{
		"ALLOWED_CHAT_ID",
		"OBS_PORT",
		"MAX_VIDEO_SIZE_MB",
		"MAX_VIDEO_DURATION_SECONDS",
		"MIN_FREE_DISK_MB",
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

func TestLoadRejectsInvalidNumericRanges(t *testing.T) {
	tests := []struct {
		key     string
		value   string
		wantErr string
	}{
		{key: "OBS_PORT", value: "0", wantErr: "OBS_PORT must be between 1 and 65535"},
		{key: "OBS_PORT", value: "65536", wantErr: "OBS_PORT must be between 1 and 65535"},
		{key: "MAX_VIDEO_SIZE_MB", value: "0", wantErr: "MAX_VIDEO_SIZE_MB must be positive"},
		{key: "MAX_VIDEO_SIZE_MB", value: "8796093022208", wantErr: "MAX_VIDEO_SIZE_MB is too large"},
		{key: "MAX_VIDEO_DURATION_SECONDS", value: "-1", wantErr: "MAX_VIDEO_DURATION_SECONDS must be non-negative"},
		{key: "MIN_FREE_DISK_MB", value: "-1", wantErr: "MIN_FREE_DISK_MB must be non-negative"},
		{key: "MIN_FREE_DISK_MB", value: "8796093022208", wantErr: "MIN_FREE_DISK_MB is too large"},
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

func TestLoadAllowsZeroDurationAndDiskReserve(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("MAX_VIDEO_DURATION_SECONDS", "0")
	t.Setenv("MIN_FREE_DISK_MB", "0")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.MaxVideoDurationSeconds != 0 {
		t.Fatalf("duration = %d, want 0", cfg.MaxVideoDurationSeconds)
	}
	if cfg.MinFreeDiskBytes != 0 {
		t.Fatalf("minimum free disk = %d, want 0", cfg.MinFreeDiskBytes)
	}
}

func TestRunShDoctorNumericRangesMatchGoConfig(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("..", "..", "run.sh"))
	if err != nil {
		t.Fatalf("read run.sh: %v", err)
	}
	for _, want := range []string{
		fmt.Sprintf(`"MAX_VIDEO_SIZE_MB:2000:1:%d"`, maxStorageMiB),
		fmt.Sprintf(`"MIN_FREE_DISK_MB:512:0:%d"`, maxStorageMiB),
	} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("run.sh doctor range does not match Go limit; missing %q", want)
		}
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

func TestLoadRejectsSharedLibrarySourceName(t *testing.T) {
	clearConfigEnv(t)
	chdirTemp(t)
	setValidConfigEnv(t)
	t.Setenv("OBS_LOOP_SOURCE_NAME", " shared-player ")
	t.Setenv("OBS_MUSIC_SOURCE_NAME", "shared-player")

	_, err := Load()
	if err == nil || !strings.Contains(err.Error(), "OBS_LOOP_SOURCE_NAME and OBS_MUSIC_SOURCE_NAME must be different") {
		t.Fatalf("err = %v, want shared library source error", err)
	}
}

func TestLoadDoesNotRemigrateCurrentDotEnv(t *testing.T) {
	clearConfigEnv(t)
	dir := chdirTemp(t)
	envPath := filepath.Join(dir, ".env")
	body := []byte(`
ENV_SCHEMA_VERSION=7
TELEGRAM_BOT_TOKEN=token
TELEGRAM_API_BASE_URL=http://127.0.0.1:8081
TELEGRAM_API_ID=replace-with-telegram-api-id
TELEGRAM_API_HASH=replace-with-telegram-api-hash
TELEGRAM_BOT_API_BIN=telegram-bot-api
TELEGRAM_BOT_API_HOST=127.0.0.1
TELEGRAM_BOT_API_PORT=8081
TELEGRAM_BOT_API_DIR=./data/telegram-bot-api
PLAYER_MODE=library
OBS_LOOP_SOURCE_NAME=tg_loop_player
OBS_MUSIC_SOURCE_NAME=tg_music_player
LOOP_MEDIA_DIR=./data/media/loops
MUSIC_MEDIA_DIR=./data/media/music
MIN_FREE_DISK_MB=512
ALLOWED_CHAT_ID=-1001
`)
	if err := os.WriteFile(envPath, body, 0o600); err != nil {
		t.Fatalf("write env: %v", err)
	}
	if err := os.Chmod(envPath, 0o644); err != nil {
		t.Fatalf("make env permissive: %v", err)
	}

	if _, err := Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	info, err := os.Stat(envPath)
	if err != nil {
		t.Fatalf("stat env: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf(".env mode = %04o, want 0600", got)
	}
	if backups := backupFiles(t, dir); len(backups) != 0 {
		t.Fatalf("backups = %v, want none", backups)
	}
	if got := readFile(t, envPath); got != string(body) {
		t.Fatalf(".env changed:\n%s", got)
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
		"MIN_FREE_DISK_MB",
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

func backupFiles(t *testing.T, dir string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".env.backup.*"))
	if err != nil {
		t.Fatalf("glob backups: %v", err)
	}
	return matches
}
