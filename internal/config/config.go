package config

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	TelegramBotToken string
	AllowedChatID    int64

	OBSHost            string
	OBSPort            int
	OBSPassword        string
	OBSMediaSourceName string
	OBSFallbackFile    string

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

	cfg := Config{
		TelegramBotToken:        getenv("TELEGRAM_BOT_TOKEN", ""),
		AllowedChatID:           getenvInt64("ALLOWED_CHAT_ID", 0),
		OBSHost:                 getenv("OBS_HOST", "127.0.0.1"),
		OBSPort:                 getenvInt("OBS_PORT", 4455),
		OBSPassword:             getenv("OBS_PASSWORD", ""),
		OBSMediaSourceName:      getenv("OBS_MEDIA_SOURCE_NAME", "tg_queue_player"),
		OBSFallbackFile:         getenv("OBS_FALLBACK_FILE", ""),
		DataDir:                 getenv("DATA_DIR", "./data"),
		MaxVideoSizeBytes:       int64(getenvInt("MAX_VIDEO_SIZE_MB", 500)) * 1024 * 1024,
		MaxVideoDurationSeconds: getenvInt("MAX_VIDEO_DURATION_SECONDS", 7200),
		MaxQueueLength:          getenvInt("MAX_QUEUE_LENGTH", 50),
		RetentionDays:           getenvInt("RETENTION_DAYS", 7),
		RetentionMaxFiles:       getenvInt("RETENTION_MAX_FILES", 100),
		FFProbePath:             getenv("FFPROBE_PATH", "ffprobe"),
		LogLevel:                parseLogLevel(getenv("LOG_LEVEL", "info")),
	}
	cfg.MediaDir = getenv("MEDIA_DIR", filepath.Join(cfg.DataDir, "media"))
	cfg.DatabasePath = getenv("DATABASE_PATH", filepath.Join(cfg.DataDir, "queue.db"))

	if cfg.TelegramBotToken == "" {
		return cfg, errors.New("TELEGRAM_BOT_TOKEN is required")
	}
	if cfg.AllowedChatID == 0 {
		return cfg, errors.New("ALLOWED_CHAT_ID is required")
	}
	if cfg.OBSMediaSourceName == "" {
		return cfg, errors.New("OBS_MEDIA_SOURCE_NAME is required")
	}
	if cfg.MaxVideoSizeBytes <= 0 {
		return cfg, errors.New("MAX_VIDEO_SIZE_MB must be positive")
	}
	if cfg.MaxQueueLength <= 0 {
		return cfg, errors.New("MAX_QUEUE_LENGTH must be positive")
	}

	return cfg, nil
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

func getenvInt(key string, fallback int) int {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func getenvInt64(key string, fallback int64) int64 {
	value := os.Getenv(key)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return fallback
	}
	return parsed
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

func (c Config) RetentionMaxAge() time.Duration {
	if c.RetentionDays <= 0 {
		return 0
	}
	return time.Duration(c.RetentionDays) * 24 * time.Hour
}
