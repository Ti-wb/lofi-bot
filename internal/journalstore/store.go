// Package journalstore persists Telegram polling checkpoints and update
// attempts. It deliberately owns no media queue schema.
package journalstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Store is a durable Telegram update journal backed by SQLite.
//
// Store serializes access through a single database connection so the
// read/claim/write transactions in the journal remain process-safe.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens or creates the SQLite journal at path and applies the Telegram
// journal migration. Existing unrelated tables are left unchanged.
func Open(ctx context.Context, path string) (*Store, error) {
	if err := ensureParent(path); err != nil {
		return nil, err
	}
	if err := prepareDatabaseFile(path); err != nil {
		return nil, err
	}
	if err := secureDatabaseFiles(path); err != nil {
		return nil, err
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	store := &Store{db: db, now: time.Now}
	if err := store.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	if err := secureDatabaseFiles(path); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func ensureParent(path string) error {
	parent := filepath.Dir(path)
	if parent == "." || parent == "" {
		return nil
	}
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return fmt.Errorf("create SQLite parent directory %q: %w", parent, err)
	}
	return nil
}

func prepareDatabaseFile(path string) error {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("prepare SQLite database %q: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return fmt.Errorf("secure SQLite database %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close prepared SQLite database %q: %w", path, err)
	}
	return nil
}

func secureDatabaseFiles(path string) error {
	for index, candidate := range []string{path, path + "-wal", path + "-shm"} {
		optional := index > 0
		if err := os.Chmod(candidate, 0o600); err != nil {
			if optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("secure SQLite file %q: %w", candidate, err)
		}
		info, err := os.Stat(candidate)
		if err != nil {
			if optional && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("verify SQLite file %q: %w", candidate, err)
		}
		if got := info.Mode().Perm(); got != 0o600 {
			return fmt.Errorf("verify SQLite file %q: mode is %04o, want 0600", candidate, got)
		}
	}
	return nil
}

// Close releases the SQLite database resources.
func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;
PRAGMA busy_timeout=5000;
`); err != nil {
		return err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	if err := migrateTelegramUpdateJournal(ctx, tx, s.nowUTC()); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) nowUTC() time.Time {
	if s.now == nil {
		return time.Now().UTC()
	}
	return s.now().UTC()
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}
