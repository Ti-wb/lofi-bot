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
)

const (
	currentEnvSchemaVersion       = 7
	stateDatabaseEnvSchemaVersion = 7
	bytesPerMiB                   = int64(1024 * 1024)
	maxStorageMiB                 = int64(^uint64(0)>>1) / bytesPerMiB
)

type Config struct {
	TelegramBotToken   string
	TelegramAPIBaseURL string
	TelegramBotAPIDir  string
	AllowedChatID      int64

	OBSHost            string
	OBSPort            int
	OBSPassword        string
	OBSLoopSourceName  string
	OBSMusicSourceName string

	DataDir                 string
	MediaDir                string
	LoopMediaDir            string
	MusicMediaDir           string
	DatabasePath            string
	MaxVideoSizeBytes       int64
	MaxVideoDurationSeconds int
	MinFreeDiskBytes        int64

	FFProbePath string
	LogLevel    slog.Level
}

func Load() (Config, error) {
	_ = loadDotEnv(".env")

	if err := validateLegacyPlayerMode(os.Getenv("PLAYER_MODE")); err != nil {
		return Config{}, err
	}
	databaseName, err := defaultDatabaseName(os.Getenv("ENV_SCHEMA_VERSION"))
	if err != nil {
		return Config{}, err
	}

	allowedChatID, err := getenvInt64("ALLOWED_CHAT_ID", 0)
	if err != nil {
		return Config{}, err
	}
	obsPort, err := getenvInt("OBS_PORT", 4455)
	if err != nil {
		return Config{}, err
	}
	maxVideoSizeMB, err := getenvInt64("MAX_VIDEO_SIZE_MB", 2000)
	if err != nil {
		return Config{}, err
	}
	maxVideoDurationSeconds, err := getenvInt("MAX_VIDEO_DURATION_SECONDS", 7200)
	if err != nil {
		return Config{}, err
	}
	minFreeDiskMB, err := getenvInt64("MIN_FREE_DISK_MB", 512)
	if err != nil {
		return Config{}, err
	}
	if maxVideoSizeMB <= 0 {
		return Config{}, errors.New("MAX_VIDEO_SIZE_MB must be positive")
	}
	if maxVideoSizeMB > maxStorageMiB {
		return Config{}, errors.New("MAX_VIDEO_SIZE_MB is too large")
	}
	if minFreeDiskMB < 0 {
		return Config{}, errors.New("MIN_FREE_DISK_MB must be non-negative")
	}
	if minFreeDiskMB > maxStorageMiB {
		return Config{}, errors.New("MIN_FREE_DISK_MB is too large")
	}

	cfg := Config{
		TelegramBotToken:        strings.TrimSpace(getenv("TELEGRAM_BOT_TOKEN", "")),
		TelegramAPIBaseURL:      strings.TrimRight(strings.TrimSpace(getenv("TELEGRAM_API_BASE_URL", "")), "/"),
		TelegramBotAPIDir:       strings.TrimSpace(getenv("TELEGRAM_BOT_API_DIR", "./data/telegram-bot-api")),
		AllowedChatID:           allowedChatID,
		OBSHost:                 strings.TrimSpace(getenv("OBS_HOST", "127.0.0.1")),
		OBSPort:                 obsPort,
		OBSPassword:             getenv("OBS_PASSWORD", ""),
		OBSLoopSourceName:       strings.TrimSpace(getenv("OBS_LOOP_SOURCE_NAME", "tg_loop_player")),
		OBSMusicSourceName:      strings.TrimSpace(getenv("OBS_MUSIC_SOURCE_NAME", "tg_music_player")),
		DataDir:                 strings.TrimSpace(getenv("DATA_DIR", "./data")),
		MaxVideoSizeBytes:       maxVideoSizeMB * bytesPerMiB,
		MaxVideoDurationSeconds: maxVideoDurationSeconds,
		MinFreeDiskBytes:        minFreeDiskMB * bytesPerMiB,
		FFProbePath:             strings.TrimSpace(getenv("FFPROBE_PATH", "ffprobe")),
		LogLevel:                parseLogLevel(strings.TrimSpace(getenv("LOG_LEVEL", "info"))),
	}
	cfg.MediaDir = strings.TrimSpace(getenv("MEDIA_DIR", filepath.Join(cfg.DataDir, "media")))
	cfg.LoopMediaDir = strings.TrimSpace(getenv("LOOP_MEDIA_DIR", filepath.Join(cfg.MediaDir, "loops")))
	cfg.MusicMediaDir = strings.TrimSpace(getenv("MUSIC_MEDIA_DIR", filepath.Join(cfg.MediaDir, "music")))
	cfg.DatabasePath = strings.TrimSpace(os.Getenv("DATABASE_PATH"))
	if cfg.DatabasePath == "" {
		cfg.DatabasePath = filepath.Join(cfg.DataDir, databaseName)
	}

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
	if cfg.OBSLoopSourceName == "" {
		return cfg, errors.New("OBS_LOOP_SOURCE_NAME is required")
	}
	if cfg.OBSMusicSourceName == "" {
		return cfg, errors.New("OBS_MUSIC_SOURCE_NAME is required")
	}
	if cfg.OBSLoopSourceName == cfg.OBSMusicSourceName {
		return cfg, errors.New("OBS_LOOP_SOURCE_NAME and OBS_MUSIC_SOURCE_NAME must be different")
	}
	if cfg.MaxVideoDurationSeconds < 0 {
		return cfg, errors.New("MAX_VIDEO_DURATION_SECONDS must be non-negative")
	}
	return cfg, nil
}

func validateLegacyPlayerMode(raw string) error {
	switch mode := strings.TrimSpace(raw); mode {
	case "", "library":
		return nil
	case "queue":
		return errors.New("PLAYER_MODE=queue is no longer supported; configure distinct OBS_LOOP_SOURCE_NAME and OBS_MUSIC_SOURCE_NAME sources, add library media, then set PLAYER_MODE=library or remove PLAYER_MODE")
	default:
		return fmt.Errorf("PLAYER_MODE is deprecated; remove it or set PLAYER_MODE=library (got %q)", mode)
	}
}

func defaultDatabaseName(rawSchemaVersion string) (string, error) {
	rawSchemaVersion = strings.TrimSpace(rawSchemaVersion)
	if rawSchemaVersion == "" {
		// Configs without a schema marker predate the state.db default. Keep the
		// historical path unless migration pins it explicitly.
		return "queue.db", nil
	}
	digits := rawSchemaVersion
	if strings.HasPrefix(digits, "-") {
		digits = strings.TrimPrefix(digits, "-")
	}
	if digits == "" || strings.IndexFunc(digits, func(r rune) bool {
		return r < '0' || r > '9'
	}) >= 0 {
		return "", errors.New("ENV_SCHEMA_VERSION must be an integer")
	}
	version, err := strconv.Atoi(rawSchemaVersion)
	if err != nil {
		return "", errors.New("ENV_SCHEMA_VERSION must be an integer")
	}
	if version > currentEnvSchemaVersion {
		return "", fmt.Errorf(
			"ENV_SCHEMA_VERSION %d is newer than this binary supports (%d)",
			version,
			currentEnvSchemaVersion,
		)
	}
	if version < stateDatabaseEnvSchemaVersion {
		return "queue.db", nil
	}
	return "state.db", nil
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

func decodeDotEnvValue(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return strings.ReplaceAll(value[1:len(value)-1], "'\\''", "'")
	}
	return strings.Trim(value, `"'`)
}

func loadDotEnv(path string) error {
	body, err := readPrivateDotEnv(path)
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
		value = decodeDotEnvValue(value)
		if key != "" && os.Getenv(key) == "" {
			_ = os.Setenv(key, value)
		}
	}
	return nil
}

func readPrivateDotEnv(path string) ([]byte, error) {
	if err := os.Chmod(path, 0o600); err != nil {
		return nil, err
	}
	return os.ReadFile(path)
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
