package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

const (
	telegramAttemptPending = "pending"
	telegramAttemptRunning = "running"
	telegramAttemptStuck   = "stuck"
	telegramAttemptFailed  = "failed"
	telegramAttemptDone    = "done"
	telegramAttemptDead    = "dead"

	telegramBeginExecute         = "execute"
	telegramBeginAlreadyTerminal = "already_terminal"
	telegramBeginBusy            = "busy"
	telegramBeginDead            = "dead"

	telegramDoneRetentionLimit = 10_000
	telegramDoneRetentionAge   = 30 * 24 * time.Hour
	telegramDeadRetentionLimit = 1_000
	telegramDeadRetentionAge   = 90 * 24 * time.Hour
	telegramPruneBatchSize     = 256
	telegramMaxKindBytes       = 32
	telegramMaxActionBytes     = 64
	telegramMaxOwnerTokenBytes = 128
	telegramMaxErrorBytes      = 1024

	telegramAbandonedUpdateError = "update abandoned after Telegram confirmed a higher offset"
)

func migrateTelegramUpdateJournal(ctx context.Context, tx *sql.Tx, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS telegram_poll_checkpoint (
	singleton INTEGER PRIMARY KEY CHECK (singleton = 1),
	next_offset INTEGER NOT NULL CHECK (next_offset >= 0),
	confirmed_offset INTEGER NOT NULL CHECK (
		confirmed_offset >= 0 AND confirmed_offset <= next_offset
	),
	updated_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS telegram_update_attempts (
	update_id INTEGER PRIMARY KEY CHECK (update_id >= 0),
	update_kind TEXT NOT NULL,
	action TEXT NOT NULL DEFAULT '',
	chat_id INTEGER NOT NULL DEFAULT 0,
	message_id INTEGER NOT NULL DEFAULT 0,
	actor_id INTEGER NOT NULL DEFAULT 0,
	attempt_count INTEGER NOT NULL CHECK (attempt_count > 0),
	failure_count INTEGER NOT NULL DEFAULT 0 CHECK (failure_count >= 0),
	status TEXT NOT NULL CHECK (
		status IN ('pending', 'running', 'stuck', 'failed', 'done', 'dead')
	),
	owner_token TEXT NOT NULL DEFAULT '',
	lease_until TEXT,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL,
	updated_at TEXT NOT NULL,
	finished_at TEXT
);

CREATE INDEX IF NOT EXISTS idx_telegram_attempts_status_finished
ON telegram_update_attempts(status, finished_at DESC, update_id DESC);
CREATE INDEX IF NOT EXISTS idx_telegram_attempts_status_lease
ON telegram_update_attempts(status, lease_until, update_id);
`); err != nil {
		return err
	}

	_, err := tx.ExecContext(ctx, `
INSERT INTO telegram_poll_checkpoint (
	singleton, next_offset, confirmed_offset, updated_at
) VALUES (1, 0, 0, ?)
ON CONFLICT(singleton) DO NOTHING
`, formatTime(now))
	return err
}

func (s *Store) LoadUpdateCheckpoint(ctx context.Context) (nextOffset int, confirmedOffset int, err error) {
	err = s.db.QueryRowContext(ctx, `
SELECT next_offset, confirmed_offset
FROM telegram_poll_checkpoint
WHERE singleton = 1
`).Scan(&nextOffset, &confirmedOffset)
	if err != nil {
		return 0, 0, err
	}
	if nextOffset < 0 || confirmedOffset < 0 || confirmedOffset > nextOffset {
		return 0, 0, fmt.Errorf(
			"invalid Telegram polling checkpoint: next_offset=%d confirmed_offset=%d",
			nextOffset,
			confirmedOffset,
		)
	}
	return nextOffset, confirmedOffset, nil
}

func (s *Store) ConfirmUpdateOffset(ctx context.Context, offset int) error {
	if offset < 0 {
		return fmt.Errorf("Telegram confirmed offset must be non-negative: %d", offset)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	var nextOffset, confirmedOffset int
	if err := tx.QueryRowContext(ctx, `
SELECT next_offset, confirmed_offset
FROM telegram_poll_checkpoint
WHERE singleton = 1
`).Scan(&nextOffset, &confirmedOffset); err != nil {
		return err
	}
	if offset > nextOffset {
		return fmt.Errorf(
			"cannot confirm Telegram offset %d beyond durable next_offset %d",
			offset,
			nextOffset,
		)
	}
	if offset <= confirmedOffset {
		return tx.Commit()
	}

	now := s.nowUTC()
	if _, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint
SET confirmed_offset = ?, updated_at = ?
WHERE singleton = 1 AND confirmed_offset < ?
`, offset, formatTime(now), offset); err != nil {
		return err
	}
	if _, err := reconcileTelegramAbandonedAttempts(ctx, tx, offset, now); err != nil {
		return err
	}
	if _, err := pruneTelegramTerminalAttempts(
		ctx,
		tx,
		telegramAttemptDone,
		offset,
		telegramDoneRetentionLimit,
		now.Add(-telegramDoneRetentionAge),
	); err != nil {
		return err
	}
	if _, err := pruneTelegramTerminalAttempts(
		ctx,
		tx,
		telegramAttemptDead,
		offset,
		telegramDeadRetentionLimit,
		now.Add(-telegramDeadRetentionAge),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) PruneTelegramUpdateJournal(ctx context.Context) (done int64, dead int64, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer rollback(tx)

	var confirmedOffset int
	if err := tx.QueryRowContext(ctx, `
SELECT confirmed_offset
FROM telegram_poll_checkpoint
WHERE singleton = 1
`).Scan(&confirmedOffset); err != nil {
		return 0, 0, err
	}
	now := s.nowUTC()
	if _, err := reconcileTelegramAbandonedAttempts(ctx, tx, confirmedOffset, now); err != nil {
		return 0, 0, err
	}
	done, err = pruneTelegramTerminalAttempts(
		ctx,
		tx,
		telegramAttemptDone,
		confirmedOffset,
		telegramDoneRetentionLimit,
		now.Add(-telegramDoneRetentionAge),
	)
	if err != nil {
		return 0, 0, err
	}
	dead, err = pruneTelegramTerminalAttempts(
		ctx,
		tx,
		telegramAttemptDead,
		confirmedOffset,
		telegramDeadRetentionLimit,
		now.Add(-telegramDeadRetentionAge),
	)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, err
	}
	return done, dead, nil
}

func reconcileTelegramAbandonedAttempts(
	ctx context.Context,
	tx *sql.Tx,
	confirmedOffset int,
	now time.Time,
) (int64, error) {
	nowText := formatTime(now)
	result, err := tx.ExecContext(ctx, `
UPDATE telegram_update_attempts
SET status = ?, owner_token = '', lease_until = NULL, last_error = ?,
	updated_at = ?, finished_at = ?
WHERE update_id IN (
	SELECT update_id
	FROM telegram_update_attempts
	WHERE update_id < ?
		AND (
			status IN (?, ?)
			OR (
				status IN (?, ?)
				AND (
					lease_until IS NULL
					OR julianday(lease_until) IS NULL
					OR julianday(lease_until) <= julianday(?)
				)
			)
		)
	ORDER BY update_id ASC
	LIMIT ?
)
`,
		telegramAttemptDead,
		telegramAbandonedUpdateError,
		nowText,
		nowText,
		confirmedOffset,
		telegramAttemptPending,
		telegramAttemptFailed,
		telegramAttemptRunning,
		telegramAttemptStuck,
		nowText,
		telegramPruneBatchSize,
	)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func pruneTelegramTerminalAttempts(
	ctx context.Context,
	tx *sql.Tx,
	status string,
	confirmedOffset int,
	limit int,
	cutoff time.Time,
) (int64, error) {
	result, err := tx.ExecContext(ctx, `
DELETE FROM telegram_update_attempts
WHERE update_id IN (
	SELECT update_id
	FROM telegram_update_attempts
	WHERE status = ?
		AND update_id < ?
		AND (
			finished_at < ?
			OR update_id NOT IN (
				SELECT update_id
				FROM telegram_update_attempts
				WHERE status = ? AND update_id < ?
				ORDER BY finished_at DESC, update_id DESC
				LIMIT ?
			)
		)
	ORDER BY finished_at ASC, update_id ASC
	LIMIT ?
)
`, status, confirmedOffset, formatTime(cutoff), status, confirmedOffset, limit, telegramPruneBatchSize)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

func (s *Store) BeginUpdateAttempt(
	ctx context.Context,
	updateID int,
	updateKind string,
	action string,
	chatID int64,
	messageID int,
	actorID int64,
	ownerToken string,
	leaseUntil time.Time,
	maxFailures int,
) (disposition string, attemptCount int, failureCount int, err error) {
	nextOffset, err := telegramUpdateSuccessor(updateID)
	if err != nil {
		return "", 0, 0, err
	}
	if err := validateTelegramJournalLabel("kind", updateKind, telegramMaxKindBytes, false); err != nil {
		return "", 0, 0, err
	}
	if err := validateTelegramJournalLabel("action", action, telegramMaxActionBytes, true); err != nil {
		return "", 0, 0, err
	}
	if ownerToken == "" || len(ownerToken) > telegramMaxOwnerTokenBytes {
		return "", 0, 0, errors.New("Telegram update owner token is missing or too long")
	}
	if maxFailures <= 0 {
		return "", 0, 0, fmt.Errorf("Telegram poison threshold must be positive: %d", maxFailures)
	}
	now := s.nowUTC()
	if !leaseUntil.After(now) {
		return "", 0, 0, fmt.Errorf("Telegram update lease must be in the future: %s", leaseUntil)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", 0, 0, err
	}
	defer rollback(tx)

	// This no-op write obtains SQLite's single-writer reservation before the
	// read/claim sequence. Separate Store instances therefore cannot both
	// observe and claim the same update as executable.
	result, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint
SET updated_at = updated_at
WHERE singleton = 1
`)
	if err != nil {
		return "", 0, 0, err
	}
	if err := requireSingleCheckpoint(result); err != nil {
		return "", 0, 0, err
	}

	nowText := formatTime(now)
	leaseText := formatTime(leaseUntil)
	result, err = tx.ExecContext(ctx, `
INSERT INTO telegram_update_attempts (
	update_id, update_kind, action, chat_id, message_id, actor_id,
	attempt_count, failure_count, status, owner_token, lease_until,
	last_error, created_at, updated_at, finished_at
) VALUES (?, ?, ?, ?, ?, ?, 1, 0, ?, ?, ?, '', ?, ?, NULL)
ON CONFLICT(update_id) DO NOTHING
`,
		updateID,
		updateKind,
		action,
		chatID,
		messageID,
		actorID,
		telegramAttemptRunning,
		ownerToken,
		leaseText,
		nowText,
		nowText,
	)
	if err != nil {
		return "", 0, 0, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return "", 0, 0, err
	}
	if inserted == 1 {
		if err := tx.Commit(); err != nil {
			return "", 0, 0, err
		}
		return telegramBeginExecute, 1, 0, nil
	}

	var status, lastError string
	var existingLease sql.NullString
	if err := tx.QueryRowContext(ctx, `
SELECT status, attempt_count, failure_count, lease_until, last_error
FROM telegram_update_attempts
WHERE update_id = ?
`, updateID).Scan(
		&status,
		&attemptCount,
		&failureCount,
		&existingLease,
		&lastError,
	); err != nil {
		return "", 0, 0, err
	}

	switch status {
	case telegramAttemptDone:
		if err := advanceTelegramNextOffset(ctx, tx, nextOffset, nowText); err != nil {
			return "", 0, 0, err
		}
		if err := tx.Commit(); err != nil {
			return "", 0, 0, err
		}
		return telegramBeginAlreadyTerminal, attemptCount, failureCount, nil
	case telegramAttemptDead:
		if err := advanceTelegramNextOffset(ctx, tx, nextOffset, nowText); err != nil {
			return "", 0, 0, err
		}
		if err := tx.Commit(); err != nil {
			return "", 0, 0, err
		}
		return telegramBeginDead, attemptCount, failureCount, nil
	case telegramAttemptRunning, telegramAttemptStuck:
		expiresAt, err := parseTelegramLease(existingLease)
		if err != nil {
			return "", 0, 0, fmt.Errorf("parse Telegram update %d lease: %w", updateID, err)
		}
		if expiresAt.After(now) {
			if err := tx.Commit(); err != nil {
				return "", 0, 0, err
			}
			return telegramBeginBusy, attemptCount, failureCount, nil
		}
		if status == telegramAttemptRunning {
			failureCount++
			lastError = "update owner lease expired before completion"
			if failureCount >= maxFailures {
				if err := markTelegramAttemptDead(
					ctx,
					tx,
					updateID,
					failureCount,
					"update owner lease expired before completion",
					nowText,
				); err != nil {
					return "", 0, 0, err
				}
				if err := advanceTelegramNextOffset(ctx, tx, nextOffset, nowText); err != nil {
					return "", 0, 0, err
				}
				if err := tx.Commit(); err != nil {
					return "", 0, 0, err
				}
				return telegramBeginDead, attemptCount, failureCount, nil
			}
		}
	case telegramAttemptPending, telegramAttemptFailed:
		// A cooperative abort or a previously counted handler failure is
		// replayable without consuming another poison-budget slot.
		if status == telegramAttemptPending {
			lastError = ""
		}
	default:
		return "", 0, 0, fmt.Errorf(
			"Telegram update %d has unknown journal status %q",
			updateID,
			status,
		)
	}

	attemptCount++
	result, err = tx.ExecContext(ctx, `
UPDATE telegram_update_attempts
	SET update_kind = ?, action = ?, chat_id = ?, message_id = ?, actor_id = ?,
		attempt_count = ?, failure_count = ?, status = ?, owner_token = ?,
		lease_until = ?, last_error = ?,
		updated_at = ?, finished_at = NULL
WHERE update_id = ? AND status NOT IN (?, ?)
`,
		updateKind,
		action,
		chatID,
		messageID,
		actorID,
		attemptCount,
		failureCount,
		telegramAttemptRunning,
		ownerToken,
		leaseText,
		lastError,
		nowText,
		updateID,
		telegramAttemptDone,
		telegramAttemptDead,
	)
	if err != nil {
		return "", 0, 0, err
	}
	if err := requireSingleTelegramAttempt(result, updateID); err != nil {
		return "", 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return "", 0, 0, err
	}
	return telegramBeginExecute, attemptCount, failureCount, nil
}

func (s *Store) CompleteUpdateAttempt(
	ctx context.Context,
	updateID int,
	nextOffset int,
	ownerToken string,
) error {
	if err := validateTelegramUpdateTransition(updateID, nextOffset); err != nil {
		return err
	}
	if ownerToken == "" || len(ownerToken) > telegramMaxOwnerTokenBytes {
		return errors.New("Telegram update owner token is missing or too long")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer rollback(tx)

	now := formatTime(s.nowUTC())
	result, err := tx.ExecContext(ctx, `
UPDATE telegram_update_attempts
SET status = ?, owner_token = '', lease_until = NULL, last_error = '',
	updated_at = ?, finished_at = ?
WHERE update_id = ? AND status = ? AND owner_token = ?
`, telegramAttemptDone, now, now, updateID, telegramAttemptRunning, ownerToken)
	if err != nil {
		return err
	}
	if err := requireSingleTelegramAttempt(result, updateID); err != nil {
		return err
	}
	if err := advanceTelegramNextOffset(ctx, tx, nextOffset, now); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) AbortUpdateAttempt(ctx context.Context, updateID int, ownerToken string) error {
	if updateID < 0 {
		return fmt.Errorf("Telegram update ID must be non-negative: %d", updateID)
	}
	if ownerToken == "" || len(ownerToken) > telegramMaxOwnerTokenBytes {
		return errors.New("Telegram update owner token is missing or too long")
	}
	result, err := s.db.ExecContext(ctx, `
UPDATE telegram_update_attempts
SET status = ?, owner_token = '', lease_until = NULL, last_error = '',
	updated_at = ?, finished_at = NULL
WHERE update_id = ? AND status = ? AND owner_token = ?
`, telegramAttemptPending, formatTime(s.nowUTC()), updateID, telegramAttemptRunning, ownerToken)
	if err != nil {
		return err
	}
	return requireSingleTelegramAttempt(result, updateID)
}

func (s *Store) FailUpdateAttempt(
	ctx context.Context,
	updateID int,
	nextOffset int,
	ownerToken string,
	maxFailures int,
	cause string,
	holdLeaseUntil time.Time,
) (dead bool, err error) {
	if err := validateTelegramUpdateTransition(updateID, nextOffset); err != nil {
		return false, err
	}
	if maxFailures <= 0 {
		return false, fmt.Errorf("Telegram poison threshold must be positive: %d", maxFailures)
	}
	if ownerToken == "" || len(ownerToken) > telegramMaxOwnerTokenBytes {
		return false, errors.New("Telegram update owner token is missing or too long")
	}
	if len(cause) > telegramMaxErrorBytes {
		return false, fmt.Errorf("Telegram update failure is too long: %d bytes", len(cause))
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer rollback(tx)

	result, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint
SET updated_at = updated_at
WHERE singleton = 1
`)
	if err != nil {
		return false, err
	}
	if err := requireSingleCheckpoint(result); err != nil {
		return false, err
	}

	var failureCount int
	if err := tx.QueryRowContext(ctx, `
SELECT failure_count
FROM telegram_update_attempts
WHERE update_id = ? AND status = ? AND owner_token = ?
`, updateID, telegramAttemptRunning, ownerToken).Scan(&failureCount); err != nil {
		return false, err
	}
	failureCount++
	now := s.nowUTC()
	nowText := formatTime(now)
	if failureCount >= maxFailures {
		if err := markTelegramAttemptDead(
			ctx,
			tx,
			updateID,
			failureCount,
			cause,
			nowText,
		); err != nil {
			return false, err
		}
		if err := advanceTelegramNextOffset(ctx, tx, nextOffset, nowText); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}

	status := telegramAttemptFailed
	nextOwner := ""
	var lease any
	if !holdLeaseUntil.IsZero() {
		if !holdLeaseUntil.After(now) {
			return false, errors.New("Telegram stuck-owner lease must be in the future")
		}
		status = telegramAttemptStuck
		nextOwner = ownerToken
		lease = formatTime(holdLeaseUntil)
	}
	result, err = tx.ExecContext(ctx, `
UPDATE telegram_update_attempts
SET failure_count = ?, status = ?, owner_token = ?, lease_until = ?,
	last_error = ?, updated_at = ?, finished_at = NULL
WHERE update_id = ? AND status = ? AND owner_token = ?
`,
		failureCount,
		status,
		nextOwner,
		lease,
		cause,
		nowText,
		updateID,
		telegramAttemptRunning,
		ownerToken,
	)
	if err != nil {
		return false, err
	}
	if err := requireSingleTelegramAttempt(result, updateID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
}

func markTelegramAttemptDead(
	ctx context.Context,
	tx *sql.Tx,
	updateID int,
	failureCount int,
	cause string,
	now string,
) error {
	result, err := tx.ExecContext(ctx, `
UPDATE telegram_update_attempts
SET failure_count = ?, status = ?, owner_token = '', lease_until = NULL,
	last_error = ?, updated_at = ?, finished_at = ?
WHERE update_id = ? AND status NOT IN (?, ?)
`,
		failureCount,
		telegramAttemptDead,
		cause,
		now,
		now,
		updateID,
		telegramAttemptDone,
		telegramAttemptDead,
	)
	if err != nil {
		return err
	}
	return requireSingleTelegramAttempt(result, updateID)
}

func parseTelegramLease(raw sql.NullString) (time.Time, error) {
	if !raw.Valid || raw.String == "" {
		return time.Time{}, errors.New("active attempt has no lease")
	}
	return time.Parse(time.RFC3339Nano, raw.String)
}

func validateTelegramJournalLabel(name string, value string, limit int, allowEmpty bool) error {
	if value == "" {
		if allowEmpty {
			return nil
		}
		return fmt.Errorf("Telegram update %s is required", name)
	}
	if len(value) > limit {
		return fmt.Errorf("Telegram update %s exceeds %d bytes", name, limit)
	}
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9':
		case r == '_', r == '-':
		default:
			return fmt.Errorf("Telegram update %s contains an unsupported character", name)
		}
	}
	return nil
}

func validateTelegramUpdateTransition(updateID int, nextOffset int) error {
	if updateID < 0 {
		return fmt.Errorf("Telegram update ID must be non-negative: %d", updateID)
	}
	if updateID == math.MaxInt {
		return fmt.Errorf("Telegram update ID has no representable successor: %d", updateID)
	}
	if nextOffset <= 0 || updateID != nextOffset-1 {
		return fmt.Errorf(
			"Telegram next offset %d does not follow update %d",
			nextOffset,
			updateID,
		)
	}
	return nil
}

func telegramUpdateSuccessor(updateID int) (int, error) {
	if updateID < 0 {
		return 0, fmt.Errorf("Telegram update ID must be non-negative: %d", updateID)
	}
	if updateID == math.MaxInt {
		return 0, fmt.Errorf("Telegram update ID has no representable successor: %d", updateID)
	}
	return updateID + 1, nil
}

func requireSingleCheckpoint(result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("Telegram polling checkpoint is missing")
	}
	return nil
}

func requireSingleTelegramAttempt(result sql.Result, updateID int) error {
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf(
			"Telegram update %d has no matching active attempt",
			updateID,
		)
	}
	return nil
}

func advanceTelegramNextOffset(ctx context.Context, tx *sql.Tx, nextOffset int, now string) error {
	result, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint
SET next_offset = CASE
		WHEN next_offset < ? THEN ?
		ELSE next_offset
	END,
	updated_at = ?
WHERE singleton = 1 AND confirmed_offset <= ?
`, nextOffset, nextOffset, now, nextOffset)
	if err != nil {
		return err
	}
	return requireSingleCheckpoint(result)
}
