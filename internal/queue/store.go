package queue

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

type Store struct {
	db *sql.DB
}

const videoColumns = `
	id, telegram_file_id, telegram_unique_id, submitter_id, submitter_name,
	chat_id, message_id, file_name, local_path, mime_type, size_bytes,
	duration_seconds, queue_position, status, error, created_at, updated_at,
	started_at, finished_at
`

func Open(ctx context.Context, path string) (*Store, error) {
	if err := ensureParent(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
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
	return os.MkdirAll(parent, 0o755)
}

func (s *Store) Close() error {
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

	if _, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS videos (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	telegram_file_id TEXT NOT NULL,
	telegram_unique_id TEXT NOT NULL,
	submitter_id INTEGER NOT NULL,
	submitter_name TEXT NOT NULL,
	chat_id INTEGER NOT NULL,
	message_id INTEGER NOT NULL,
	file_name TEXT NOT NULL,
	local_path TEXT NOT NULL,
	mime_type TEXT NOT NULL,
	size_bytes INTEGER NOT NULL DEFAULT 0,
	duration_seconds INTEGER NOT NULL DEFAULT 0,
	queue_position INTEGER NOT NULL DEFAULT 0,
	status TEXT NOT NULL,
	error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	started_at TEXT,
	finished_at TEXT
);
`); err != nil {
		return err
	}

	if err := s.ensureVideoColumn(ctx, "local_path", `ALTER TABLE videos ADD COLUMN local_path TEXT NOT NULL DEFAULT ''`); err != nil {
		return err
	}

	_, err := s.db.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_videos_status_position ON videos(status, queue_position);
CREATE INDEX IF NOT EXISTS idx_videos_created ON videos(created_at);
`)
	return err
}

func (s *Store) ensureVideoColumn(ctx context.Context, name, alter string) error {
	hasColumn, err := s.videoColumnExists(ctx, name)
	if err != nil {
		return err
	}
	if hasColumn {
		return nil
	}
	_, err = s.db.ExecContext(ctx, alter)
	return err
}

func (s *Store) videoColumnExists(ctx context.Context, name string) (bool, error) {
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(videos)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()

	for rows.Next() {
		var cid int
		var columnName, columnType string
		var notNull int
		var defaultValue sql.NullString
		var primaryKey int
		if err := rows.Scan(&cid, &columnName, &columnType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if columnName == name {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) AddDownloading(ctx context.Context, v Video) (Video, error) {
	now := time.Now().UTC()
	v.CreatedAt = now
	v.UpdatedAt = now
	v.Status = StatusDownloading

	err := s.db.QueryRowContext(ctx, `
INSERT INTO videos (
	telegram_file_id, telegram_unique_id, submitter_id, submitter_name, chat_id, message_id,
	file_name, local_path, mime_type, size_bytes, duration_seconds, queue_position, status,
	error, created_at, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, '', ?, ?)
RETURNING id
`, v.TelegramFileID, v.TelegramUniqueID, v.SubmitterID, v.SubmitterName, v.ChatID, v.MessageID,
		v.FileName, v.LocalPath, v.MimeType, v.SizeBytes, v.DurationSeconds, string(v.Status),
		formatTime(v.CreatedAt), formatTime(v.UpdatedAt)).Scan(&v.ID)
	if err != nil {
		return v, err
	}
	return v, nil
}

func (s *Store) MarkReady(ctx context.Context, id int64, localPath string, sizeBytes int64, durationSeconds int) (Video, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Video{}, err
	}
	defer rollback(tx)

	position, err := nextQueuePosition(ctx, tx)
	if err != nil {
		return Video{}, err
	}
	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
UPDATE videos
SET status = ?, local_path = ?, size_bytes = ?, duration_seconds = ?, queue_position = ?, updated_at = ?
WHERE id = ? AND status = ?
`, string(StatusReady), localPath, sizeBytes, durationSeconds, position, formatTime(now), id, string(StatusDownloading))
	if err != nil {
		return Video{}, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return Video{}, err
	}
	if changed == 0 {
		return Video{}, fmt.Errorf("video %d is no longer downloading", id)
	}
	if err := compactReadyPositions(ctx, tx); err != nil {
		return Video{}, err
	}
	if err := tx.Commit(); err != nil {
		return Video{}, err
	}
	return s.Get(ctx, id)
}

func (s *Store) MarkFailed(ctx context.Context, id int64, cause string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE videos SET status = ?, error = ?, updated_at = ? WHERE id = ?
`, string(StatusFailed), cause, formatTime(time.Now().UTC()), id)
	return err
}

func (s *Store) FailStaleDownloading(ctx context.Context, olderThan time.Duration, cause string) (int64, error) {
	if olderThan <= 0 {
		return 0, nil
	}
	now := time.Now().UTC()
	cutoff := now.Add(-olderThan)
	res, err := s.db.ExecContext(ctx, `
UPDATE videos
SET status = ?, error = ?, updated_at = ?, finished_at = ?
WHERE status = ? AND created_at < ?
`, string(StatusFailed), cause, formatTime(now), formatTime(now), string(StatusDownloading), formatTime(cutoff))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *Store) QueueLength(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM videos WHERE status IN (?, ?)
`, string(StatusReady), string(StatusDownloading)).Scan(&count)
	return count, err
}

func (s *Store) Current(ctx context.Context) (*Video, error) {
	return s.firstByStatus(ctx, StatusPlaying)
}

func (s *Store) NextReady(ctx context.Context) (*Video, error) {
	return s.firstByStatus(ctx, StatusReady)
}

func (s *Store) MarkPlaying(ctx context.Context, id int64) (Video, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Video{}, err
	}
	defer rollback(tx)

	now := time.Now().UTC()
	res, err := tx.ExecContext(ctx, `
UPDATE videos SET status = ?, started_at = ?, updated_at = ?, queue_position = 0
WHERE id = ? AND status = ?
`, string(StatusPlaying), formatTime(now), formatTime(now), id, string(StatusReady))
	if err != nil {
		return Video{}, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return Video{}, err
	}
	if changed == 0 {
		return Video{}, fmt.Errorf("video %d is no longer ready", id)
	}
	if err := compactReadyPositions(ctx, tx); err != nil {
		return Video{}, err
	}
	if err := tx.Commit(); err != nil {
		return Video{}, err
	}
	return s.Get(ctx, id)
}

func (s *Store) RestartPlaying(ctx context.Context, id int64) (Video, error) {
	now := time.Now().UTC()
	res, err := s.db.ExecContext(ctx, `
UPDATE videos SET started_at = ?, updated_at = ?
WHERE id = ? AND status = ?
`, formatTime(now), formatTime(now), id, string(StatusPlaying))
	if err != nil {
		return Video{}, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return Video{}, err
	}
	if changed == 0 {
		return Video{}, fmt.Errorf("video %d is not playing", id)
	}
	return s.Get(ctx, id)
}

func (s *Store) StartNext(ctx context.Context) (*Video, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer rollback(tx)

	now := time.Now().UTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE videos SET status = ?, finished_at = ?, updated_at = ? WHERE status = ?
`, string(StatusPlayed), formatTime(now), formatTime(now), string(StatusPlaying)); err != nil {
		return nil, err
	}

	row := tx.QueryRowContext(ctx, `
SELECT id FROM videos WHERE status = ? ORDER BY queue_position ASC, created_at ASC LIMIT 1
`, string(StatusReady))
	var id int64
	if err := row.Scan(&id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return nil, nil
		}
		return nil, err
	}

	if _, err := tx.ExecContext(ctx, `
UPDATE videos SET status = ?, started_at = ?, updated_at = ?, queue_position = 0 WHERE id = ?
`, string(StatusPlaying), formatTime(now), formatTime(now), id); err != nil {
		return nil, err
	}
	if err := compactReadyPositions(ctx, tx); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	video, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &video, nil
}

func (s *Store) FinishCurrent(ctx context.Context) error {
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
UPDATE videos SET status = ?, finished_at = ?, updated_at = ? WHERE status = ?
`, string(StatusPlayed), formatTime(now), formatTime(now), string(StatusPlaying))
	return err
}

func (s *Store) Cancel(ctx context.Context, id int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	res, err := tx.ExecContext(ctx, `
UPDATE videos SET status = ?, updated_at = ?, queue_position = 0
WHERE id = ? AND status IN (?, ?)
`, string(StatusCanceled), formatTime(time.Now().UTC()), id, string(StatusReady), string(StatusDownloading))
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return fmt.Errorf("video %d is not queued", id)
	}
	if err := compactReadyPositions(ctx, tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Move(ctx context.Context, id int64, position int) error {
	if position < 1 {
		position = 1
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	rows, err := tx.QueryContext(ctx, `
SELECT id FROM videos WHERE status = ? ORDER BY queue_position ASC, created_at ASC
`, string(StatusReady))
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []int64
	found := false
	for rows.Next() {
		var rowID int64
		if err := rows.Scan(&rowID); err != nil {
			return err
		}
		if rowID == id {
			found = true
			continue
		}
		ids = append(ids, rowID)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("video %d is not ready in queue", id)
	}

	if position > len(ids)+1 {
		position = len(ids) + 1
	}
	insertAt := position - 1
	ids = append(ids[:insertAt], append([]int64{id}, ids[insertAt:]...)...)
	for idx, rowID := range ids {
		if _, err := tx.ExecContext(ctx, `
UPDATE videos SET queue_position = ?, updated_at = ? WHERE id = ?
`, idx+1, formatTime(time.Now().UTC()), rowID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) ListQueue(ctx context.Context, limit int) ([]Video, error) {
	if limit <= 0 {
		limit = 20
	}
	return s.list(ctx, `
SELECT `+videoColumns+` FROM videos
WHERE status IN (?, ?)
ORDER BY CASE status WHEN ? THEN 0 ELSE 1 END, queue_position ASC, created_at ASC
LIMIT ?
`, string(StatusPlaying), string(StatusReady), string(StatusPlaying), limit)
}

func (s *Store) History(ctx context.Context, limit int) ([]Video, error) {
	if limit <= 0 {
		limit = 10
	}
	return s.list(ctx, `
SELECT `+videoColumns+` FROM videos WHERE status IN (?, ?, ?)
ORDER BY updated_at DESC LIMIT ?
`, string(StatusPlayed), string(StatusCanceled), string(StatusFailed), limit)
}

func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var stats Stats
	err := s.db.QueryRowContext(ctx, `
SELECT
	COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0),
	COALESCE(SUM(size_bytes), 0)
FROM videos
`, string(StatusReady), string(StatusDownloading), string(StatusPlayed), string(StatusFailed)).Scan(
		&stats.ReadyCount, &stats.DownloadingCount, &stats.PlayedCount, &stats.FailedCount, &stats.TotalBytes)
	return stats, err
}

func (s *Store) Played(ctx context.Context) ([]Video, error) {
	return s.list(ctx, `
SELECT `+videoColumns+` FROM videos
WHERE status = ?
ORDER BY finished_at ASC, updated_at ASC
`, string(StatusPlayed))
}

func (s *Store) PlayedFallbackCandidates(ctx context.Context, limit int) ([]Video, error) {
	query := `
SELECT ` + videoColumns + ` FROM videos
WHERE status = ? AND local_path <> ''
ORDER BY finished_at DESC, updated_at DESC
`
	if limit <= 0 {
		return s.list(ctx, query, string(StatusPlayed))
	}
	return s.list(ctx, query+`
LIMIT ?
`, string(StatusPlayed), limit)
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM videos WHERE id = ?`, id)
	return err
}

func (s *Store) LocalPathReferenced(ctx context.Context, path string, excludeID int64) (bool, error) {
	if path == "" {
		return false, nil
	}
	var count int
	err := s.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM videos WHERE local_path = ? AND id <> ?
`, path, excludeID).Scan(&count)
	return count > 0, err
}

func (s *Store) Get(ctx context.Context, id int64) (Video, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+videoColumns+` FROM videos WHERE id = ?`, id)
	if err != nil {
		return Video{}, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Video{}, sql.ErrNoRows
	}
	return scanVideo(rows)
}

func (s *Store) firstByStatus(ctx context.Context, status Status) (*Video, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT `+videoColumns+` FROM videos WHERE status = ? ORDER BY queue_position ASC, created_at ASC LIMIT 1
`, string(status))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	video, err := scanVideo(rows)
	if err != nil {
		return nil, err
	}
	return &video, nil
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]Video, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var videos []Video
	for rows.Next() {
		video, err := scanVideo(rows)
		if err != nil {
			return nil, err
		}
		videos = append(videos, video)
	}
	return videos, rows.Err()
}

func nextQueuePosition(ctx context.Context, tx *sql.Tx) (int, error) {
	var position int
	err := tx.QueryRowContext(ctx, `
SELECT COALESCE(MAX(queue_position), 0) + 1 FROM videos WHERE status = ?
`, string(StatusReady)).Scan(&position)
	return position, err
}

func compactReadyPositions(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
SELECT id FROM videos WHERE status = ? ORDER BY queue_position ASC, created_at ASC
`, string(StatusReady))
	if err != nil {
		return err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}

	for idx, id := range ids {
		if _, err := tx.ExecContext(ctx, `
UPDATE videos SET queue_position = ? WHERE id = ?
`, idx+1, id); err != nil {
			return err
		}
	}
	return nil
}

func scanVideo(rows *sql.Rows) (Video, error) {
	var v Video
	var createdAt, updatedAt string
	var startedAt, finishedAt sql.NullString
	err := rows.Scan(
		&v.ID, &v.TelegramFileID, &v.TelegramUniqueID, &v.SubmitterID, &v.SubmitterName,
		&v.ChatID, &v.MessageID, &v.FileName, &v.LocalPath, &v.MimeType, &v.SizeBytes,
		&v.DurationSeconds, &v.QueuePosition, &v.Status, &v.Error, &createdAt, &updatedAt,
		&startedAt, &finishedAt,
	)
	if err != nil {
		return v, err
	}
	v.CreatedAt = parseTime(createdAt)
	v.UpdatedAt = parseTime(updatedAt)
	if startedAt.Valid {
		t := parseTime(startedAt.String)
		v.StartedAt = &t
	}
	if finishedAt.Valid {
		t := parseTime(finishedAt.String)
		v.FinishedAt = &t
	}
	return v, nil
}

func formatTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(raw string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, raw)
	return t
}

func rollback(tx *sql.Tx) {
	_ = tx.Rollback()
}
