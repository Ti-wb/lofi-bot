package library

import (
	"context"
	"database/sql"
	"errors"
	"time"

	_ "modernc.org/sqlite"
)

// StateStore persists library playback decisions that should survive restarts.
type StateStore struct {
	db *sql.DB
}

type Override struct {
	Date         string
	Theme        string
	DirectLoopID string
}

type PeriodPlan struct {
	Date   string
	Period Period
	Theme  string
	LoopID string
}

func OpenState(ctx context.Context, path string) (*StateStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &StateStore{db: db}
	if err := store.init(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *StateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *StateStore) init(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
PRAGMA journal_mode=WAL;
PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS library_overrides (
	date_key TEXT PRIMARY KEY,
	theme TEXT NOT NULL DEFAULT '',
	direct_loop_id TEXT NOT NULL DEFAULT '',
	updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS library_period_plans (
	date_key TEXT NOT NULL,
	period TEXT NOT NULL,
	theme TEXT NOT NULL,
	loop_id TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	PRIMARY KEY (date_key, period)
);
CREATE TABLE IF NOT EXISTS library_kv (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL,
	updated_at TEXT NOT NULL
);
`); err != nil {
		return err
	}
	return nil
}

func (s *StateStore) Override(ctx context.Context, date string) (Override, error) {
	var override Override
	err := s.db.QueryRowContext(ctx, `
SELECT date_key, theme, direct_loop_id FROM library_overrides WHERE date_key = ?
`, date).Scan(&override.Date, &override.Theme, &override.DirectLoopID)
	if errors.Is(err, sql.ErrNoRows) {
		return Override{Date: date}, nil
	}
	return override, err
}

func (s *StateStore) SetThemeOverride(ctx context.Context, date string, theme string) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_overrides (date_key, theme, direct_loop_id, updated_at)
VALUES (?, ?, '', ?)
ON CONFLICT(date_key) DO UPDATE SET theme = excluded.theme, direct_loop_id = '', updated_at = excluded.updated_at
`, date, theme, now)
	return err
}

func (s *StateStore) ClearThemeOverride(ctx context.Context, date string) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_overrides (date_key, theme, direct_loop_id, updated_at)
VALUES (?, '', '', ?)
ON CONFLICT(date_key) DO UPDATE SET theme = '', updated_at = excluded.updated_at
`, date, now)
	return err
}

func (s *StateStore) SetDirectLoopOverride(ctx context.Context, date string, loopID string) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_overrides (date_key, theme, direct_loop_id, updated_at)
VALUES (?, '', ?, ?)
ON CONFLICT(date_key) DO UPDATE SET direct_loop_id = excluded.direct_loop_id, updated_at = excluded.updated_at
`, date, loopID, now)
	return err
}

func (s *StateStore) ClearDirectLoopOverride(ctx context.Context, date string) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_overrides (date_key, theme, direct_loop_id, updated_at)
VALUES (?, '', '', ?)
ON CONFLICT(date_key) DO UPDATE SET direct_loop_id = '', updated_at = excluded.updated_at
`, date, now)
	return err
}

func (s *StateStore) ClearOverride(ctx context.Context, date string) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_overrides (date_key, theme, direct_loop_id, updated_at)
VALUES (?, '', '', ?)
ON CONFLICT(date_key) DO UPDATE SET theme = '', direct_loop_id = '', updated_at = excluded.updated_at
`, date, now)
	return err
}

func (s *StateStore) PeriodPlan(ctx context.Context, date string, period Period) (PeriodPlan, bool, error) {
	var plan PeriodPlan
	var rawPeriod string
	err := s.db.QueryRowContext(ctx, `
SELECT date_key, period, theme, loop_id FROM library_period_plans WHERE date_key = ? AND period = ?
`, date, string(period)).Scan(&plan.Date, &rawPeriod, &plan.Theme, &plan.LoopID)
	if errors.Is(err, sql.ErrNoRows) {
		return PeriodPlan{}, false, nil
	}
	if err != nil {
		return PeriodPlan{}, false, err
	}
	parsed, err := ParsePeriod(rawPeriod)
	if err != nil {
		return PeriodPlan{}, false, err
	}
	plan.Period = parsed
	return plan, true, nil
}

func (s *StateStore) SavePeriodPlan(ctx context.Context, plan PeriodPlan) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_period_plans (date_key, period, theme, loop_id, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(date_key, period) DO UPDATE SET theme = excluded.theme, loop_id = excluded.loop_id, updated_at = excluded.updated_at
`, plan.Date, string(plan.Period), plan.Theme, plan.LoopID, now)
	return err
}

func (s *StateStore) ClearPeriodPlan(ctx context.Context, date string, period Period) error {
	_, err := s.db.ExecContext(ctx, `
DELETE FROM library_period_plans WHERE date_key = ? AND period = ?
`, date, string(period))
	return err
}

func (s *StateStore) ClearPlansForDate(ctx context.Context, date string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM library_period_plans WHERE date_key = ?`, date)
	return err
}

func (s *StateStore) LastMusicID(ctx context.Context) (string, error) {
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM library_kv WHERE key = 'last_music_id'`).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return value, err
}

func (s *StateStore) SetLastMusicID(ctx context.Context, id string) error {
	now := formatStateTime(time.Now())
	_, err := s.db.ExecContext(ctx, `
INSERT INTO library_kv (key, value, updated_at) VALUES ('last_music_id', ?, ?)
ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at
`, id, now)
	return err
}

func formatStateTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}
