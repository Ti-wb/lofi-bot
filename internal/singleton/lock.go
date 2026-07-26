package singleton

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

const (
	databaseLockFileSuffix = ".tg-obs-bot.lock"
	userLockRootPrefix     = "tg-obs-bot-"
	userLockDirectoryName  = "locks"
	botLockFilePrefix      = "telegram-bot-"
	botLockFileSuffix      = ".lock"
)

var (
	ErrAlreadyRunning      = errors.New("tg-obs-bot backend is already running")
	ErrUnsupportedPlatform = errors.New("tg-obs-bot singleton locking is unsupported on this platform")
	errPlatformContended   = errors.New("singleton lock is held")
)

// ContentionError describes the kernel lock that prevented a second backend
// generation from starting. The lock file is persistent metadata; ownership
// is determined only by the live kernel lock, never by its contents.
type ContentionError struct {
	Resource     string
	DatabasePath string
	LockPath     string
}

func (e *ContentionError) Error() string {
	if e.Resource == "telegram_bot" {
		return fmt.Sprintf(
			"%s for this Telegram bot identity while opening database %q (lock %q); stop the existing backend before retrying",
			ErrAlreadyRunning,
			e.DatabasePath,
			e.LockPath,
		)
	}
	return fmt.Sprintf(
		"%s for database %q (lock %q); stop the existing backend before retrying",
		ErrAlreadyRunning,
		e.DatabasePath,
		e.LockPath,
	)
}

func (e *ContentionError) Unwrap() error {
	return ErrAlreadyRunning
}

// Lock is an exclusive, process-lifetime backend lock. Close is idempotent.
// If the process crashes or is killed, the OS closes the descriptor and
// releases ownership automatically.
type Lock struct {
	mu           sync.Mutex
	files        []*os.File
	databasePath string
	databaseLock string
	botLock      string
}

// Acquire obtains both process-lifetime resource fences in deterministic
// order: canonical database first, logical Telegram bot identity second. It
// returns only after both locks are held and releases the database lock if the
// bot lock cannot be acquired.
func Acquire(databasePath string, telegramToken string) (*Lock, error) {
	if err := checkPlatformSupport(); err != nil {
		return nil, err
	}
	telegramToken = strings.TrimSpace(telegramToken)
	if telegramToken == "" {
		return nil, errors.New("Telegram bot token is required for singleton identity")
	}
	canonicalDatabasePath, err := canonicalizeDatabasePath(databasePath)
	if err != nil {
		return nil, fmt.Errorf("canonicalize singleton database path: %w", err)
	}
	databaseLockPath := canonicalDatabasePath + databaseLockFileSuffix
	databaseFile, err := openPlatformLock(databaseLockPath)
	if err != nil {
		if errors.Is(err, errPlatformContended) {
			return nil, &ContentionError{
				Resource:     "database",
				DatabasePath: canonicalDatabasePath,
				LockPath:     databaseLockPath,
			}
		}
		return nil, fmt.Errorf("acquire database lock %q: %w", databaseLockPath, err)
	}

	botLockPath, err := telegramBotLockPath(telegramToken)
	if err != nil {
		return nil, errors.Join(
			err,
			wrapPartialReleaseError(releaseFile(databaseFile)),
		)
	}
	botFile, err := openPlatformLock(botLockPath)
	if err != nil {
		releaseErr := wrapPartialReleaseError(releaseFile(databaseFile))
		if errors.Is(err, errPlatformContended) {
			return nil, errors.Join(
				&ContentionError{
					Resource:     "telegram_bot",
					DatabasePath: canonicalDatabasePath,
					LockPath:     botLockPath,
				},
				releaseErr,
			)
		}
		return nil, errors.Join(
			fmt.Errorf("acquire Telegram bot identity lock %q: %w", botLockPath, err),
			releaseErr,
		)
	}
	return &Lock{
		files:        []*os.File{databaseFile, botFile},
		databasePath: canonicalDatabasePath,
		databaseLock: databaseLockPath,
		botLock:      botLockPath,
	}, nil
}

func telegramBotLockPath(token string) (string, error) {
	lockDirectory, err := secureUserLockDirectory()
	if err != nil {
		return "", fmt.Errorf("prepare per-user singleton directory: %w", err)
	}
	digest := sha256.Sum256([]byte(token))
	return filepath.Join(
		lockDirectory,
		botLockFilePrefix+hex.EncodeToString(digest[:])+botLockFileSuffix,
	), nil
}

func secureUserLockDirectory() (string, error) {
	return securePlatformUserLockDirectory()
}

func canonicalizeDatabasePath(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("database path is required")
	}
	absolute, err := filepath.Abs(filepath.Clean(raw))
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(absolute)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create database directory %q: %w", parent, err)
	}

	if _, err := os.Lstat(absolute); err == nil {
		resolved, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return "", fmt.Errorf("resolve database path %q: %w", absolute, err)
		}
		resolved, err = filepath.Abs(resolved)
		if err != nil {
			return "", err
		}
		return filepath.Clean(resolved), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("inspect database path %q: %w", absolute, err)
	}

	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve database directory %q: %w", parent, err)
	}
	resolvedParent, err = filepath.Abs(resolvedParent)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Clean(resolvedParent), filepath.Base(absolute)), nil
}

// DatabasePath is the canonical path used to derive this lock. Callers should
// open SQLite through this exact path so path aliases cannot diverge from the
// lock identity.
func (l *Lock) DatabasePath() string {
	if l == nil {
		return ""
	}
	return l.databasePath
}

// Path returns the persistent lock-file path. The file must not be manually
// removed while any backend generation may still be running.
func (l *Lock) Path() string {
	return l.DatabaseLockPath()
}

// DatabaseLockPath returns the persistent canonical-database lock path.
func (l *Lock) DatabaseLockPath() string {
	if l == nil {
		return ""
	}
	return l.databaseLock
}

// BotLockPath returns the SHA-256-derived per-user Telegram identity lock path.
// It never contains the Telegram token.
func (l *Lock) BotLockPath() string {
	if l == nil {
		return ""
	}
	return l.botLock
}

func (l *Lock) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.files) == 0 {
		return nil
	}
	files := l.files
	l.files = nil
	var closeErr error
	for index := len(files) - 1; index >= 0; index-- {
		closeErr = errors.Join(closeErr, releaseFile(files[index]))
	}
	return closeErr
}

func releaseFile(file *os.File) error {
	if file == nil {
		return nil
	}
	return errors.Join(unlockPlatform(file), file.Close())
}

func wrapPartialReleaseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("release partial database lock acquisition: %w", err)
}
