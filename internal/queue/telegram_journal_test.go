package queue

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTelegramJournalMigrationIsAtomic(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open incompatible database: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
CREATE TABLE telegram_update_attempts (
	update_id INTEGER PRIMARY KEY
)
`); err != nil {
		t.Fatalf("create incompatible journal table: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close incompatible database: %v", err)
	}

	if store, err := Open(ctx, dbPath); err == nil {
		_ = store.Close()
		t.Fatal("Open succeeded with an incompatible journal table")
	}

	db, err = sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen incompatible database: %v", err)
	}
	defer db.Close()
	for _, table := range []string{"videos", "telegram_poll_checkpoint"} {
		var count int
		if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?
`, table).Scan(&count); err != nil {
			t.Fatalf("inspect table %s: %v", table, err)
		}
		if count != 0 {
			t.Fatalf("table %s survived failed migration transaction", table)
		}
	}
}

func TestTelegramJournalPersistsCheckpointAndPoisonAttemptsAcrossReopen(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")

	store := openStoreAtPath(t, ctx, dbPath)
	next, confirmed, err := store.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load initial checkpoint: %v", err)
	}
	if next != 0 || confirmed != 0 {
		t.Fatalf("initial checkpoint = (%d, %d), want (0, 0)", next, confirmed)
	}

	for attempt := 1; attempt <= 3; attempt++ {
		owner := "owner-" + string(rune('0'+attempt))
		disposition, count, failures, err := store.BeginUpdateAttempt(
			ctx,
			10,
			"message",
			"queue",
			-100123,
			55,
			42,
			owner,
			store.nowUTC().Add(time.Minute),
			3,
		)
		if err != nil {
			t.Fatalf("begin attempt %d: %v", attempt, err)
		}
		if disposition != telegramBeginExecute {
			t.Fatalf("attempt %d disposition = %q, want execute", attempt, disposition)
		}
		if count != attempt {
			t.Fatalf("attempt count = %d, want %d", count, attempt)
		}
		if failures != attempt-1 {
			t.Fatalf("failure count before attempt %d = %d, want %d", attempt, failures, attempt-1)
		}
		dead, err := store.FailUpdateAttempt(
			ctx,
			10,
			11,
			owner,
			3,
			"deterministic failure",
			time.Time{},
		)
		if err != nil {
			t.Fatalf("fail attempt %d: %v", attempt, err)
		}
		if dead != (attempt == 3) {
			t.Fatalf("attempt %d dead = %v", attempt, dead)
		}
		if err := store.Close(); err != nil {
			t.Fatalf("close after attempt %d: %v", attempt, err)
		}
		store = openStoreAtPath(t, ctx, dbPath)
	}
	defer store.Close()

	next, confirmed, err = store.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load poison checkpoint: %v", err)
	}
	if next != 11 || confirmed != 0 {
		t.Fatalf("poison checkpoint = (%d, %d), want (11, 0)", next, confirmed)
	}
	var poisonStatus, poisonError string
	var poisonAttempts, poisonFailures int
	if err := store.db.QueryRowContext(ctx, `
SELECT status, attempt_count, failure_count, last_error
FROM telegram_update_attempts
WHERE update_id = 10
`).Scan(&poisonStatus, &poisonAttempts, &poisonFailures, &poisonError); err != nil {
		t.Fatalf("read poison attempt: %v", err)
	}
	if poisonStatus != telegramAttemptDead ||
		poisonAttempts != 3 ||
		poisonFailures != 3 ||
		poisonError != "deterministic failure" {
		t.Fatalf(
			"poison row = status=%q attempts=%d failures=%d error=%q",
			poisonStatus,
			poisonAttempts,
			poisonFailures,
			poisonError,
		)
	}
	if err := store.ConfirmUpdateOffset(ctx, 11); err != nil {
		t.Fatalf("confirm poison checkpoint: %v", err)
	}

	owner := "successful-owner"
	disposition, count, failures, err := store.BeginUpdateAttempt(
		ctx,
		12,
		"callback_query",
		"status",
		-100123,
		56,
		43,
		owner,
		store.nowUTC().Add(time.Minute),
		3,
	)
	if err != nil {
		t.Fatalf("begin successful attempt: %v", err)
	}
	if disposition != telegramBeginExecute || count != 1 || failures != 0 {
		t.Fatalf(
			"successful Begin = %q attempts=%d failures=%d",
			disposition,
			count,
			failures,
		)
	}
	if err := store.CompleteUpdateAttempt(ctx, 12, 13, owner); err != nil {
		t.Fatalf("complete successful attempt: %v", err)
	}

	var status, kind, action string
	var attempts int
	if err := store.db.QueryRowContext(ctx, `
SELECT status, attempt_count, update_kind, action
FROM telegram_update_attempts
WHERE update_id = 12
`).Scan(&status, &attempts, &kind, &action); err != nil {
		t.Fatalf("read successful attempt: %v", err)
	}
	if status != telegramAttemptDone || attempts != 1 || kind != "callback_query" || action != "status" {
		t.Fatalf(
			"successful journal row = status=%q attempts=%d kind=%q action=%q",
			status,
			attempts,
			kind,
			action,
		)
	}
	if _, err := store.db.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint SET next_offset = 11 WHERE singleton = 1
`); err != nil {
		t.Fatalf("inject lagging terminal checkpoint: %v", err)
	}
	disposition, attempts, failures, err = store.BeginUpdateAttempt(
		ctx,
		12,
		"callback_query",
		"status",
		-100123,
		56,
		43,
		"repair-owner",
		store.nowUTC().Add(time.Minute),
		3,
	)
	if err != nil {
		t.Fatalf("repair terminal attempt: %v", err)
	}
	if disposition != telegramBeginAlreadyTerminal || attempts != 1 || failures != 0 {
		t.Fatalf(
			"terminal Begin = %q attempts=%d failures=%d",
			disposition,
			attempts,
			failures,
		)
	}
	next, _, err = store.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load repaired terminal checkpoint: %v", err)
	}
	if next != 13 {
		t.Fatalf("repaired terminal next offset = %d, want 13", next)
	}
}

func TestTelegramJournalCrossProcessClaimLeaseAndCrashPoison(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	first := openStoreAtPath(t, ctx, dbPath)
	defer first.Close()
	second := openStoreAtPath(t, ctx, dbPath)
	defer second.Close()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	leaseUntil := now.Add(time.Minute)

	type beginResult struct {
		owner       string
		disposition string
		attempts    int
		failures    int
		err         error
	}
	start := make(chan struct{})
	results := make(chan beginResult, 2)
	var ready sync.WaitGroup
	ready.Add(2)
	begin := func(store *Store, owner string) {
		ready.Done()
		<-start
		disposition, attempts, failures, err := store.BeginUpdateAttempt(
			ctx,
			20,
			"message",
			"queue",
			-100123,
			1,
			42,
			owner,
			leaseUntil,
			3,
		)
		results <- beginResult{
			owner:       owner,
			disposition: disposition,
			attempts:    attempts,
			failures:    failures,
			err:         err,
		}
	}
	go begin(first, "process-a")
	go begin(second, "process-b")
	ready.Wait()
	close(start)

	var execute, busy *beginResult
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatalf("concurrent Begin error: %v", result.err)
		}
		switch result.disposition {
		case telegramBeginExecute:
			copy := result
			execute = &copy
		case telegramBeginBusy:
			copy := result
			busy = &copy
		default:
			t.Fatalf("concurrent Begin disposition = %q", result.disposition)
		}
	}
	if execute == nil || busy == nil {
		t.Fatalf("concurrent claims = execute:%#v busy:%#v", execute, busy)
	}
	if execute.attempts != 1 || execute.failures != 0 ||
		busy.attempts != 1 || busy.failures != 0 {
		t.Fatalf("concurrent claim counters = execute:%#v busy:%#v", execute, busy)
	}
	if err := first.CompleteUpdateAttempt(ctx, 20, 21, execute.owner); err != nil {
		t.Fatalf("complete winning cross-process owner: %v", err)
	}

	disposition, attempts, failures, err := first.BeginUpdateAttempt(
		ctx,
		30,
		"message",
		"queue",
		-100123,
		2,
		42,
		"crash-1",
		now.Add(time.Minute),
		3,
	)
	if err != nil || disposition != telegramBeginExecute || attempts != 1 || failures != 0 {
		t.Fatalf(
			"first crash Begin = %q attempts=%d failures=%d err=%v",
			disposition,
			attempts,
			failures,
			err,
		)
	}
	disposition, _, _, err = second.BeginUpdateAttempt(
		ctx,
		30,
		"message",
		"queue",
		-100123,
		2,
		42,
		"overlap",
		now.Add(time.Minute),
		3,
	)
	if err != nil || disposition != telegramBeginBusy {
		t.Fatalf("recent running Begin = %q err=%v, want busy", disposition, err)
	}

	for generation := 2; generation <= 4; generation++ {
		now = now.Add(2 * time.Minute)
		owner := "crash-" + string(rune('0'+generation))
		store := first
		if generation%2 == 0 {
			store = second
		}
		disposition, attempts, failures, err = store.BeginUpdateAttempt(
			ctx,
			30,
			"message",
			"queue",
			-100123,
			2,
			42,
			owner,
			now.Add(time.Minute),
			3,
		)
		if err != nil {
			t.Fatalf("crash generation %d Begin: %v", generation, err)
		}
		if generation < 4 {
			if disposition != telegramBeginExecute ||
				attempts != generation ||
				failures != generation-1 {
				t.Fatalf(
					"crash generation %d = %q attempts=%d failures=%d",
					generation,
					disposition,
					attempts,
					failures,
				)
			}
		} else if disposition != telegramBeginDead || attempts != 3 || failures != 3 {
			t.Fatalf(
				"fourth Begin = %q attempts=%d failures=%d, want dead 3/3",
				disposition,
				attempts,
				failures,
			)
		}
	}

	next, _, err := first.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load crash-poison checkpoint: %v", err)
	}
	if next != 31 {
		t.Fatalf("crash-poison next offset = %d, want 31", next)
	}
}

func TestTelegramJournalForwardClockJumpReclaimsAndFencesOldJournalOwner(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "queue.db")
	first := openStoreAtPath(t, ctx, dbPath)
	defer first.Close()
	second := openStoreAtPath(t, ctx, dbPath)
	defer second.Close()

	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	disposition, attempts, failures, err := first.BeginUpdateAttempt(
		ctx,
		40,
		"message",
		"queue",
		-100123,
		3,
		42,
		"owner-before-clock-jump",
		now.Add(time.Hour),
		3,
	)
	if err != nil ||
		disposition != telegramBeginExecute ||
		attempts != 1 ||
		failures != 0 {
		t.Fatalf(
			"first Begin = %q attempts=%d failures=%d err=%v",
			disposition,
			attempts,
			failures,
			err,
		)
	}

	// Leases are deliberately wall-clock crash-accounting records. A large
	// forward clock jump can make another process reclaim while the first
	// process's monotonic handler deadline has not elapsed. Owner CAS fences
	// the stale process out of journal transitions, but cannot fence domain
	// side effects; deployment therefore requires a single host and bounded
	// wall-clock movement.
	now = now.Add(2 * time.Hour)
	disposition, attempts, failures, err = second.BeginUpdateAttempt(
		ctx,
		40,
		"message",
		"queue",
		-100123,
		3,
		42,
		"owner-after-clock-jump",
		now.Add(time.Hour),
		3,
	)
	if err != nil ||
		disposition != telegramBeginExecute ||
		attempts != 2 ||
		failures != 1 {
		t.Fatalf(
			"reclaim Begin = %q attempts=%d failures=%d err=%v",
			disposition,
			attempts,
			failures,
			err,
		)
	}
	if err := first.CompleteUpdateAttempt(ctx, 40, 41, "owner-before-clock-jump"); err == nil {
		t.Fatal("stale owner completed after wall-clock reclaim")
	}
	if err := first.AbortUpdateAttempt(ctx, 40, "owner-before-clock-jump"); err == nil {
		t.Fatal("stale owner aborted the reclaimed attempt")
	}
	if _, err := first.FailUpdateAttempt(
		ctx,
		40,
		41,
		"owner-before-clock-jump",
		3,
		"stale process failure",
		time.Time{},
	); err == nil {
		t.Fatal("stale owner failed the reclaimed attempt")
	}
	var status, owner string
	var storedAttempts, storedFailures int
	if err := second.db.QueryRowContext(ctx, `
SELECT status, owner_token, attempt_count, failure_count
FROM telegram_update_attempts
WHERE update_id = 40
`).Scan(&status, &owner, &storedAttempts, &storedFailures); err != nil {
		t.Fatalf("inspect reclaimed attempt: %v", err)
	}
	if status != telegramAttemptRunning ||
		owner != "owner-after-clock-jump" ||
		storedAttempts != 2 ||
		storedFailures != 1 {
		t.Fatalf(
			"reclaimed row changed by stale owner: status=%q owner=%q attempts=%d failures=%d",
			status,
			owner,
			storedAttempts,
			storedFailures,
		)
	}
	next, confirmed, err := second.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load checkpoint after stale transitions: %v", err)
	}
	if next != 0 || confirmed != 0 {
		t.Fatalf("stale transitions advanced checkpoint to (%d, %d)", next, confirmed)
	}
	if err := second.CompleteUpdateAttempt(ctx, 40, 41, "owner-after-clock-jump"); err != nil {
		t.Fatalf("complete reclaimed owner: %v", err)
	}
	assertTelegramJournalState(t, ctx, second, 40, telegramAttemptDone, 41, 0)
}

func TestTelegramJournalAbandonsConfirmedNonterminalRowsInBoundedBatches(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	now := time.Date(2026, 7, 26, 12, 0, 0, 500_000_000, time.UTC)
	store.now = func() time.Time { return now }

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin abandoned fixture transaction: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO telegram_update_attempts (
	update_id, update_kind, action, chat_id, message_id, actor_id,
	attempt_count, failure_count, status, owner_token, lease_until,
	last_error, created_at, updated_at, finished_at
) VALUES (?, 'message', '', 0, 0, 0, 1, 0, ?, ?, ?, '', ?, ?, NULL)
`)
	if err != nil {
		t.Fatalf("prepare abandoned fixture insert: %v", err)
	}
	created := formatTime(now.Add(-time.Hour))
	for id := 1; id <= 300; id++ {
		if _, err := stmt.ExecContext(
			ctx,
			id,
			telegramAttemptPending,
			"",
			nil,
			created,
			created,
		); err != nil {
			t.Fatalf("insert pending row %d: %v", id, err)
		}
	}
	fixtures := []struct {
		id         int
		status     string
		owner      string
		leaseUntil any
	}{
		{id: 301, status: telegramAttemptRunning, owner: "expired-owner", leaseUntil: formatTime(now.Add(-time.Second))},
		{id: 302, status: telegramAttemptRunning, owner: "active-owner", leaseUntil: formatTime(now.Add(time.Hour))},
		{id: 500, status: telegramAttemptRunning, owner: "boundary-owner", leaseUntil: formatTime(now.Add(-time.Second))},
		{id: 501, status: telegramAttemptPending},
	}
	for _, fixture := range fixtures {
		if _, err := stmt.ExecContext(
			ctx,
			fixture.id,
			fixture.status,
			fixture.owner,
			fixture.leaseUntil,
			created,
			created,
		); err != nil {
			t.Fatalf("insert fixture row %d: %v", fixture.id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close abandoned fixture insert: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint
SET next_offset = 600
WHERE singleton = 1
`); err != nil {
		t.Fatalf("set abandoned fixture checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit abandoned fixtures: %v", err)
	}

	if err := store.ConfirmUpdateOffset(ctx, 500); err != nil {
		t.Fatalf("confirm across abandoned rows: %v", err)
	}
	assertTelegramAttemptCount(t, ctx, store, telegramAttemptDead, telegramPruneBatchSize)
	assertTelegramAttemptCount(t, ctx, store, telegramAttemptPending, 45)
	assertTelegramAttemptExists(t, ctx, store, 301, true)

	if _, _, err := store.PruneTelegramUpdateJournal(ctx); err != nil {
		t.Fatalf("maintenance reconciliation: %v", err)
	}
	assertTelegramAttemptCount(t, ctx, store, telegramAttemptDead, 301)
	assertTelegramAttemptCount(t, ctx, store, telegramAttemptPending, 1)

	for _, fixture := range []struct {
		id        int
		status    string
		owner     string
		wantError string
	}{
		{id: 1, status: telegramAttemptDead, wantError: telegramAbandonedUpdateError},
		{id: 301, status: telegramAttemptDead, wantError: telegramAbandonedUpdateError},
		{id: 302, status: telegramAttemptRunning, owner: "active-owner"},
		{id: 500, status: telegramAttemptRunning, owner: "boundary-owner"},
		{id: 501, status: telegramAttemptPending},
	} {
		var status, owner, lastError string
		var lease sql.NullString
		var finished sql.NullString
		if err := store.db.QueryRowContext(ctx, `
SELECT status, owner_token, lease_until, last_error, finished_at
FROM telegram_update_attempts
WHERE update_id = ?
`, fixture.id).Scan(&status, &owner, &lease, &lastError, &finished); err != nil {
			t.Fatalf("inspect update %d: %v", fixture.id, err)
		}
		if status != fixture.status || owner != fixture.owner || lastError != fixture.wantError {
			t.Fatalf(
				"update %d = status=%q owner=%q error=%q, want %q/%q/%q",
				fixture.id,
				status,
				owner,
				lastError,
				fixture.status,
				fixture.owner,
				fixture.wantError,
			)
		}
		if fixture.status == telegramAttemptDead {
			if lease.Valid || !finished.Valid {
				t.Fatalf(
					"abandoned update %d lease=%#v finished=%#v",
					fixture.id,
					lease,
					finished,
				)
			}
		}
	}
}

func TestTelegramJournalCompletionAndConfirmationFailuresDoNotAdvance(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)

	owner := "completion-owner"
	if _, _, _, err := store.BeginUpdateAttempt(
		ctx,
		20,
		"message",
		"queue",
		-100123,
		1,
		42,
		owner,
		store.nowUTC().Add(time.Minute),
		3,
	); err != nil {
		t.Fatalf("begin update attempt: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_telegram_next_offset
BEFORE UPDATE OF next_offset ON telegram_poll_checkpoint
BEGIN
	SELECT RAISE(ABORT, 'injected next-offset failure');
END
`); err != nil {
		t.Fatalf("create next-offset failure trigger: %v", err)
	}

	err := store.CompleteUpdateAttempt(ctx, 20, 21, owner)
	if err == nil || !strings.Contains(err.Error(), "injected next-offset failure") {
		t.Fatalf("complete error = %v, want injected failure", err)
	}
	assertTelegramJournalState(t, ctx, store, 20, telegramAttemptRunning, 0, 0)

	if _, err := store.db.ExecContext(ctx, `DROP TRIGGER fail_telegram_next_offset`); err != nil {
		t.Fatalf("drop next-offset failure trigger: %v", err)
	}
	if err := store.CompleteUpdateAttempt(ctx, 20, 21, owner); err != nil {
		t.Fatalf("complete after trigger removal: %v", err)
	}
	assertTelegramJournalState(t, ctx, store, 20, telegramAttemptDone, 21, 0)

	if _, err := store.db.ExecContext(ctx, `
CREATE TRIGGER fail_telegram_confirmed_offset
BEFORE UPDATE OF confirmed_offset ON telegram_poll_checkpoint
BEGIN
	SELECT RAISE(ABORT, 'injected confirmed-offset failure');
END
`); err != nil {
		t.Fatalf("create confirmation failure trigger: %v", err)
	}
	err = store.ConfirmUpdateOffset(ctx, 21)
	if err == nil || !strings.Contains(err.Error(), "injected confirmed-offset failure") {
		t.Fatalf("confirm error = %v, want injected failure", err)
	}
	assertTelegramJournalState(t, ctx, store, 20, telegramAttemptDone, 21, 0)
}

func TestTelegramJournalRetentionOnlyPrunesSafelyConfirmedTerminalRows(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO telegram_update_attempts (
	update_id, update_kind, action, chat_id, message_id, actor_id,
	attempt_count, status, last_error, created_at, updated_at, finished_at
) VALUES (?, 'message', '', 0, 0, 0, 1, ?, '', ?, ?, ?)
`)
	if err != nil {
		t.Fatalf("prepare terminal insert: %v", err)
	}
	recent := formatTime(now.Add(-time.Hour))
	for id := 1; id <= telegramDoneRetentionLimit+5; id++ {
		if _, err := stmt.ExecContext(ctx, id, telegramAttemptDone, recent, recent, recent); err != nil {
			t.Fatalf("insert done row %d: %v", id, err)
		}
	}
	for id := 20_001; id <= 20_000+telegramDeadRetentionLimit+5; id++ {
		if _, err := stmt.ExecContext(ctx, id, telegramAttemptDead, recent, recent, recent); err != nil {
			t.Fatalf("insert dead row %d: %v", id, err)
		}
	}
	oldDoneID := 15_000
	oldDeadID := 22_000
	unsafeDoneID := 50_000
	unsafeDeadID := 50_001
	old := formatTime(now.Add(-100 * 24 * time.Hour))
	for _, fixture := range []struct {
		id     int
		status string
	}{
		{id: oldDoneID, status: telegramAttemptDone},
		{id: oldDeadID, status: telegramAttemptDead},
		{id: unsafeDoneID, status: telegramAttemptDone},
		{id: unsafeDeadID, status: telegramAttemptDead},
	} {
		if _, err := stmt.ExecContext(ctx, fixture.id, fixture.status, old, old, old); err != nil {
			t.Fatalf("insert old terminal row %d: %v", fixture.id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close terminal insert: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint SET next_offset = 60000 WHERE singleton = 1
`); err != nil {
		t.Fatalf("set retention checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit retention fixtures: %v", err)
	}

	if err := store.ConfirmUpdateOffset(ctx, 50_000); err != nil {
		t.Fatalf("confirm retention checkpoint: %v", err)
	}

	assertTelegramAttemptCount(t, ctx, store, telegramAttemptDone, telegramDoneRetentionLimit+1)
	assertTelegramAttemptCount(t, ctx, store, telegramAttemptDead, telegramDeadRetentionLimit+1)
	assertTelegramAttemptExists(t, ctx, store, oldDoneID, false)
	assertTelegramAttemptExists(t, ctx, store, oldDeadID, false)
	assertTelegramAttemptExists(t, ctx, store, unsafeDoneID, true)
	assertTelegramAttemptExists(t, ctx, store, unsafeDeadID, true)
}

func TestTelegramJournalPruningIsBoundedAndMaintenanceConverges(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)
	now := time.Date(2026, 7, 26, 12, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }

	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin fixture transaction: %v", err)
	}
	stmt, err := tx.PrepareContext(ctx, `
INSERT INTO telegram_update_attempts (
	update_id, update_kind, action, chat_id, message_id, actor_id,
	attempt_count, status, last_error, created_at, updated_at, finished_at
) VALUES (?, 'message', '', 0, 0, 0, 1, ?, '', ?, ?, ?)
`)
	if err != nil {
		t.Fatalf("prepare terminal insert: %v", err)
	}
	recent := formatTime(now.Add(-time.Hour))
	for id := 1; id <= telegramDoneRetentionLimit+telegramPruneBatchSize+5; id++ {
		if _, err := stmt.ExecContext(ctx, id, telegramAttemptDone, recent, recent, recent); err != nil {
			t.Fatalf("insert done row %d: %v", id, err)
		}
	}
	for id := 20_001; id <= 20_000+telegramDeadRetentionLimit+telegramPruneBatchSize+5; id++ {
		if _, err := stmt.ExecContext(ctx, id, telegramAttemptDead, recent, recent, recent); err != nil {
			t.Fatalf("insert dead row %d: %v", id, err)
		}
	}
	const unsafeDoneID = 50_000
	const unsafeDeadID = 50_001
	for _, fixture := range []struct {
		id     int
		status string
	}{
		{id: unsafeDoneID, status: telegramAttemptDone},
		{id: unsafeDeadID, status: telegramAttemptDead},
	} {
		if _, err := stmt.ExecContext(
			ctx,
			fixture.id,
			fixture.status,
			recent,
			recent,
			recent,
		); err != nil {
			t.Fatalf("insert unsafe terminal row %d: %v", fixture.id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close terminal insert: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `
UPDATE telegram_poll_checkpoint SET next_offset = 60000 WHERE singleton = 1
`); err != nil {
		t.Fatalf("set retention checkpoint: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit retention fixtures: %v", err)
	}

	if err := store.ConfirmUpdateOffset(ctx, unsafeDoneID); err != nil {
		t.Fatalf("confirm retention checkpoint: %v", err)
	}
	assertTelegramAttemptCount(
		t,
		ctx,
		store,
		telegramAttemptDone,
		telegramDoneRetentionLimit+6,
	)
	assertTelegramAttemptCount(
		t,
		ctx,
		store,
		telegramAttemptDead,
		telegramDeadRetentionLimit+6,
	)

	// An unchanged confirmation is intentionally a no-op. Periodic
	// maintenance, not every empty poll, performs follow-up bounded batches.
	if err := store.ConfirmUpdateOffset(ctx, unsafeDoneID); err != nil {
		t.Fatalf("repeat unchanged confirmation: %v", err)
	}
	assertTelegramAttemptCount(
		t,
		ctx,
		store,
		telegramAttemptDone,
		telegramDoneRetentionLimit+6,
	)
	assertTelegramAttemptCount(
		t,
		ctx,
		store,
		telegramAttemptDead,
		telegramDeadRetentionLimit+6,
	)

	for pass := 1; ; pass++ {
		done, dead, err := store.PruneTelegramUpdateJournal(ctx)
		if err != nil {
			t.Fatalf("maintenance prune pass %d: %v", pass, err)
		}
		if done > telegramPruneBatchSize || dead > telegramPruneBatchSize {
			t.Fatalf(
				"maintenance prune pass %d exceeded batch: done=%d dead=%d",
				pass,
				done,
				dead,
			)
		}
		if done == 0 && dead == 0 {
			break
		}
		if pass > 10 {
			t.Fatal("maintenance pruning did not converge")
		}
	}

	assertTelegramAttemptCount(
		t,
		ctx,
		store,
		telegramAttemptDone,
		telegramDoneRetentionLimit+1,
	)
	assertTelegramAttemptCount(
		t,
		ctx,
		store,
		telegramAttemptDead,
		telegramDeadRetentionLimit+1,
	)
	assertTelegramAttemptExists(t, ctx, store, unsafeDoneID, true)
	assertTelegramAttemptExists(t, ctx, store, unsafeDeadID, true)
}

func TestTelegramJournalOperationsRespectContextWhileConnectionIsHeld(t *testing.T) {
	tests := []string{"load", "confirm", "begin", "complete"}
	for _, operation := range tests {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			store := openTestStore(t, ctx)
			owner := "held-connection-owner"
			if operation == "complete" {
				if _, _, _, err := store.BeginUpdateAttempt(
					ctx,
					50,
					"message",
					"queue",
					-100123,
					4,
					42,
					owner,
					store.nowUTC().Add(time.Minute),
					3,
				); err != nil {
					t.Fatalf("prepare active attempt: %v", err)
				}
			}
			connection, err := store.db.Conn(ctx)
			if err != nil {
				t.Fatalf("hold sole database connection: %v", err)
			}
			defer connection.Close()

			operationCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
			defer cancel()
			started := time.Now()
			switch operation {
			case "load":
				_, _, err = store.LoadUpdateCheckpoint(operationCtx)
			case "confirm":
				err = store.ConfirmUpdateOffset(operationCtx, 0)
			case "begin":
				_, _, _, err = store.BeginUpdateAttempt(
					operationCtx,
					50,
					"message",
					"queue",
					-100123,
					4,
					42,
					owner,
					store.nowUTC().Add(time.Minute),
					3,
				)
			case "complete":
				err = store.CompleteUpdateAttempt(operationCtx, 50, 51, owner)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("%s error = %v, want deadline", operation, err)
			}
			if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
				t.Fatalf("%s ignored context for %s", operation, elapsed)
			}
		})
	}
}

func openStoreAtPath(t *testing.T, ctx context.Context, path string) *Store {
	t.Helper()
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return store
}

func assertTelegramJournalState(
	t *testing.T,
	ctx context.Context,
	store *Store,
	updateID int,
	wantStatus string,
	wantNext int,
	wantConfirmed int,
) {
	t.Helper()
	var status string
	if err := store.db.QueryRowContext(ctx, `
SELECT status FROM telegram_update_attempts WHERE update_id = ?
`, updateID).Scan(&status); err != nil {
		t.Fatalf("read update %d status: %v", updateID, err)
	}
	if status != wantStatus {
		t.Fatalf("update %d status = %q, want %q", updateID, status, wantStatus)
	}
	next, confirmed, err := store.LoadUpdateCheckpoint(ctx)
	if err != nil {
		t.Fatalf("load checkpoint: %v", err)
	}
	if next != wantNext || confirmed != wantConfirmed {
		t.Fatalf(
			"checkpoint = (%d, %d), want (%d, %d)",
			next,
			confirmed,
			wantNext,
			wantConfirmed,
		)
	}
}

func assertTelegramAttemptCount(t *testing.T, ctx context.Context, store *Store, status string, want int) {
	t.Helper()
	var got int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM telegram_update_attempts WHERE status = ?
`, status).Scan(&got); err != nil {
		t.Fatalf("count %s attempts: %v", status, err)
	}
	if got != want {
		t.Fatalf("%s attempts = %d, want %d", status, got, want)
	}
}

func assertTelegramAttemptExists(t *testing.T, ctx context.Context, store *Store, updateID int, want bool) {
	t.Helper()
	var count int
	if err := store.db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM telegram_update_attempts WHERE update_id = ?
`, updateID).Scan(&count); err != nil {
		t.Fatalf("inspect update %d: %v", updateID, err)
	}
	if got := count == 1; got != want {
		t.Fatalf("update %d exists = %v, want %v", updateID, got, want)
	}
}
