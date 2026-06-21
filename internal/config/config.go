package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const currentEnvSchemaVersion = 2

type Config struct {
	TelegramBotToken   string
	TelegramAPIBaseURL string
	TelegramBotAPIDir  string
	AllowedChatID      int64

	OBSHost            string
	OBSPort            int
	OBSPassword        string
	OBSMediaSourceName string
	OBSFallbackFile    string
	FallbackMode       string

	DataDir                 string
	MediaDir                string
	DatabasePath            string
	MaxVideoSizeBytes       int64
	MaxVideoDurationSeconds int
	MaxQueueLength          int
	RetentionDays           int
	RetentionMaxFiles       int

	FFProbePath string
	LogLevel    slog.Level
}

func Load() (Config, error) {
	_ = loadDotEnv(".env")

	allowedChatID, err := getenvInt64("ALLOWED_CHAT_ID", 0)
	if err != nil {
		return Config{}, err
	}
	obsPort, err := getenvInt("OBS_PORT", 4455)
	if err != nil {
		return Config{}, err
	}
	maxVideoSizeMB, err := getenvInt("MAX_VIDEO_SIZE_MB", 2000)
	if err != nil {
		return Config{}, err
	}
	maxVideoDurationSeconds, err := getenvInt("MAX_VIDEO_DURATION_SECONDS", 7200)
	if err != nil {
		return Config{}, err
	}
	maxQueueLength, err := getenvInt("MAX_QUEUE_LENGTH", 50)
	if err != nil {
		return Config{}, err
	}
	retentionDays, err := getenvInt("RETENTION_DAYS", 7)
	if err != nil {
		return Config{}, err
	}
	retentionMaxFiles, err := getenvInt("RETENTION_MAX_FILES", 100)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		TelegramBotToken:        strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN", "")),
		TelegramAPIBaseURL:      strings.TrimRight(strings.TrimSpace(getenv("TELEGRAM_API_BASE_URL", "")), "/"),
		TelegramBotAPIDir:       strings.TrimSpace(getenv("TELEGRAM_BOT_API_DIR", "./data/telegram-bot-api")),
		AllowedChatID:           allowedChatID,
		OBSHost:                 strings.TrimSpace(getenv("OBS_HOST", "127.0.0.1")),
		OBSPort:                 obsPort,
		OBSPassword:             getenv("OBS_PASSWORD", ""),
		OBSMediaSourceName:      strings.TrimSpace(getenv("OBS_MEDIA_SOURCE_NAME", "tg_queue_player")),
		OBSFallbackFile:         strings.TrimSpace(getenv("OBS_FALLBACK_FILE", "")),
		FallbackMode:            strings.TrimSpace(getenv("FALLBACK_MODE", "random_played")),
		DataDir:                 strings.TrimSpace(getenv("DATA_DIR", "./data")),
		MaxVideoSizeBytes:       int64(maxVideoSizeMB) * 1024 * 1024,
		MaxVideoDurationSeconds: maxVideoDurationSeconds,
		MaxQueueLength:          maxQueueLength,
		RetentionDays:           retentionDays,
		RetentionMaxFiles:       retentionMaxFiles,
		FFProbePath:             strings.TrimSpace(getenv("FFPROBE_PATH", "ffprobe")),
		LogLevel:                parseLogLevel(strings.TrimSpace(getenv("LOG_LEVEL", "info"))),
	}
	cfg.MediaDir = strings.TrimSpace(getenv("MEDIA_DIR", filepath.Join(cfg.DataDir, "media")))
	cfg.DatabasePath = strings.TrimSpace(getenv("DATABASE_PATH", filepath.Join(cfg.DataDir, "queue.db")))

	if cfg.TelegramBotToken == "" {
		return cfg, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.TelegramAPIBaseURL == "" {
		return cfg, errors.New("TELEGRAM_API_BASE_URL is required")
	}
	if err := validateHTTPURL("TELEGRAM_API_BASE_URL", cfg.TelegramAPIBaseURL); err != nil {
		return cfg, err
	}
	if cfg.TelegramBotAPIDir == "" {
		return cfg, errors.New("TELEGRAM_BOT_API_DIR is required")
	}
	if cfg.AllowedChatID == 0 {
		return cfg, errors.New("ALLOWED_CHAT_ID is required")
	}
	if cfg.OBSPort < 1 || cfg.OBSPort > 65535 {
		return cfg, errors.New("OBS_PORT must be between 1 and 65535")
	}
	if cfg.OBSMediaSourceName == "" {
		return cfg, errors.New("OBS_MEDIA_SOURCE_NAME is required")
	}
	if !validFallbackMode(cfg.FallbackMode) {
		return cfg, fmt.Errorf("FALLBACK_MODE must be one of random_played, file, off")
	}
	if cfg.MaxVideoSizeBytes <= 0 {
		return cfg, errors.New("MAX_VIDEO_SIZE_MB must be positive")
	}
	if cfg.MaxVideoDurationSeconds < 0 {
		return cfg, errors.New("MAX_VIDEO_DURATION_SECONDS must be non-negative")
	}
	if cfg.MaxQueueLength <= 0 {
		return cfg, errors.New("MAX_QUEUE_LENGTH must be positive")
	}
	if cfg.RetentionDays < 0 {
		return cfg, errors.New("RETENTION_DAYS must be non-negative")
	}
	if cfg.RetentionMaxFiles < 0 {
		return cfg, errors.New("RETENTION_MAX_FILES must be non-negative")
	}

	return cfg, nil
}

func validateHTTPURL(key, raw string) error {
	parsed, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("%s must be a valid URL: %w", key, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("%s must use http or https", key)
	}
	if parsed.Host == "" {
		return fmt.Errorf("%s must include a host", key)
	}
	return nil
}

func migrateDotEnv(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}

	values := parseDotEnv(body)
	version := 0
	if rawVersion, ok := values["ENV_SCHEMA_VERSION"]; ok {
		parsed, err := strconv.Atoi(strings.TrimSpace(rawVersion))
		if err != nil {
			return fmt.Errorf("ENV_SCHEMA_VERSION must be an integer")
		}
		version = parsed
	}
	if version > currentEnvSchemaVersion {
		return fmt.Errorf("ENV_SCHEMA_VERSION %d is newer than this binary supports (%d)", version, currentEnvSchemaVersion)
	}
	if version == currentEnvSchemaVersion {
		return nil
	}

	updatedBody := append([]byte(nil), body...)
	if _, ok := values["ENV_SCHEMA_VERSION"]; ok {
		updatedBody = setDotEnvValue(updatedBody, "ENV_SCHEMA_VERSION", strconv.Itoa(currentEnvSchemaVersion))
		values["ENV_SCHEMA_VERSION"] = strconv.Itoa(currentEnvSchemaVersion)
	}
	additions := envMigrationAdditions(version, values)
	if len(additions) == 0 && string(updatedBody) == string(body) {
		return nil
	}

	backupPath := fmt.Sprintf("%s.backup.%d", path, time.Now().Unix())
	if err := os.WriteFile(backupPath, body, 0o600); err != nil {
		return fmt.Errorf("backup .env: %w", err)
	}

	updated := updatedBody
	if len(updated) > 0 && updated[len(updated)-1] != '\n' {
		updated = append(updated, '\n')
	}
	if len(additions) > 0 {
		updated = append(updated, "\n# Added automatically by tg-obs-bot env migration.\n"...)
		for _, addition := range additions {
			updated = append(updated, addition...)
			updated = append(updated, '\n')
		}
	}
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		return fmt.Errorf("write migrated .env: %w", err)
	}
	return nil
}

func setDotEnvValue(body []byte, key string, value string) []byte {
	lines := strings.Split(string(body), "\n")
	for idx, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		rawKey, _, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(rawKey) != key {
			continue
		}
		lines[idx] = key + "=" + value
		return []byte(strings.Join(lines, "\n"))
	}
	return body
}

func envMigrationAdditions(version int, values map[string]string) []string {
	var additions []string
	if version < 1 {
		if _, ok := values["ENV_SCHEMA_VERSION"]; !ok {
			additions = append(additions, "ENV_SCHEMA_VERSION=2")
		}
		if _, ok := values["TELEGRAM_API_BASE_URL"]; !ok {
			additions = append(additions, "TELEGRAM_API_BASE_URL=http://127.0.0.1:8081")
		}
		if _, ok := values["MAX_VIDEO_SIZE_MB"]; !ok {
			additions = append(additions, "MAX_VIDEO_SIZE_MB=2000")
		}
	}
	if version < 2 {
		if _, ok := values["TELEGRAM_API_ID"]; !ok {
			additions = append(additions, "TELEGRAM_API_ID=replace-with-telegram-api-id")
		}
		if _, ok := values["TELEGRAM_API_HASH"]; !ok {
			additions = append(additions, "TELEGRAM_API_HASH=replace-with-telegram-api-hash")
		}
		if _, ok := values["TELEGRAM_BOT_API_BIN"]; !ok {
			additions = append(additions, "TELEGRAM_BOT_API_BIN=telegram-bot-api")
		}
		if _, ok := values["TELEGRAM_BOT_API_HOST"]; !ok {
			additions = append(additions, "TELEGRAM_BOT_API_HOST=127.0.0.1")
		}
		if _, ok := values["TELEGRAM_BOT_API_PORT"]; !ok {
			additions = append(additions, "TELEGRAM_BOT_API_PORT=8081")
		}
		if _, ok := values["TELEGRAM_BOT_API_DIR"]; !ok {
			additions = append(additions, "TELEGRAM_BOT_API_DIR=./data/telegram-bot-api")
		}
	}
	return additions
}

func parseDotEnv(body []byte) map[string]string {
	values := make(map[string]string)
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" {
			values[key] = value
		}
	}
	return values
}

func validFallbackMode(mode string) bool {
	switch mode {
	case "random_played", "file", "off":
		return true
	default:
		return false
	}
}

func loadDotEnv(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	for _, rawLine := range strings.Split(string(body), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}

func getenv(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func getenvInt(key string, fallback int) (int, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func getenvInt64(key string, fallback int64) (int64, error) {
	value := os.Getenv(key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", key)
	}
	return parsed, nil
}

func parseLogLevel(raw string) slog.Level {
	switch strings.ToLower(raw) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

func (c Config) OBSURL() string {
	return fmt.Sprintf("ws://%s:%d", c.OBSHost, c.OBSPort)
}

func (c Config) SensitiveValues() []string {
	return []string{c.TelegramBotToken, c.OBSPassword}
}

func (c Config) RetentionMaxAge() time.Duration {
	if c.RetentionDays <= 0 {
		return 0
	}
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}
