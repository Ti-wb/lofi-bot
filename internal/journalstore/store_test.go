package journalstore

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestOpenCreatesParentAndPrivateDatabaseFiles(t *testing.T) {
	ctx := context.Background()
	parent := filepath.Join(t.TempDir(), "nested", "journal")
	databasePath := filepath.Join(parent, "updates.db")

	store, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer store.Close()

	info, err := os.Stat(parent)
	if err != nil {
		t.Fatalf("stat created parent: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("created parent %q is not a directory", parent)
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		assertMode(t, path, 0o600)
	}
}

func TestPreparedDatabaseKeepsSQLiteSidecarsPrivateWithOpenUmask(t *testing.T) {
	ctx := context.Background()
	directory := filepath.Join(t.TempDir(), "external-database")
	if err := os.Mkdir(directory, 0o777); err != nil {
		t.Fatalf("create external database directory: %v", err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatalf("make external database directory permissive: %v", err)
	}

	oldUmask := syscall.Umask(0)
	t.Cleanup(func() {
		syscall.Umask(oldUmask)
	})

	databasePath := filepath.Join(directory, "journal.db")
	if err := prepareDatabaseFile(databasePath); err != nil {
		t.Fatalf("prepare database: %v", err)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open prepared database: %v", err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	store := &Store{db: db}
	if err := store.migrate(ctx); err != nil {
		t.Fatalf("migrate prepared database: %v", err)
	}

	assertMode(t, directory, 0o777)
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		assertMode(t, path, 0o600)
	}
}

func TestOpenSecuresExternalDatabaseFilesWithoutChangingParent(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "external-database")
	if err := os.Mkdir(directory, 0o777); err != nil {
		t.Fatalf("create external database directory: %v", err)
	}
	if err := os.Chmod(directory, 0o777); err != nil {
		t.Fatalf("make external database directory permissive: %v", err)
	}

	oldUmask := syscall.Umask(0)
	t.Cleanup(func() {
		syscall.Umask(oldUmask)
	})

	databasePath := filepath.Join(directory, "journal.db")
	store, err := Open(context.Background(), databasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	assertMode(t, directory, 0o777)
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		assertMode(t, path, 0o600)
	}
}

func TestSecureDatabaseFilesNormalizesPreexistingSidecars(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "journal.db")
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		if err := os.WriteFile(path, []byte("existing"), 0o644); err != nil {
			t.Fatalf("create permissive SQLite file %s: %v", path, err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatalf("make SQLite file permissive %s: %v", path, err)
		}
	}

	if err := secureDatabaseFiles(databasePath); err != nil {
		t.Fatalf("secure existing SQLite files: %v", err)
	}
	for _, path := range []string{databasePath, databasePath + "-wal", databasePath + "-shm"} {
		assertMode(t, path, 0o600)
	}
}

func TestOpenConfiguresJournalPragmasAndDoesNotCreateVideosTable(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, ctx)

	if got := store.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("max open connections = %d, want 1", got)
	}

	var busyTimeout int
	if err := store.db.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("read busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}

	var foreignKeys int
	if err := store.db.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("read foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	assertTableExists(t, ctx, store.db, "telegram_poll_checkpoint", true)
	assertTableExists(t, ctx, store.db, "telegram_update_attempts", true)
	assertTableExists(t, ctx, store.db, "videos", false)
}

func TestOpenLeavesExistingVideosQueueTableUnchanged(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "shared.db")
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatalf("open seed database: %v", err)
	}
	const videosSchema = `CREATE TABLE videos (
		id INTEGER PRIMARY KEY,
		sentinel TEXT NOT NULL,
		queue_position INTEGER NOT NULL
	)`
	if _, err := db.ExecContext(ctx, videosSchema); err != nil {
		t.Fatalf("create sentinel videos table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
INSERT INTO videos (id, sentinel, queue_position) VALUES (7, 'preserve-me', 19)
`); err != nil {
		t.Fatalf("insert sentinel video: %v", err)
	}
	var schemaBefore string
	if err := db.QueryRowContext(ctx, `
SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'videos'
`).Scan(&schemaBefore); err != nil {
		t.Fatalf("read videos schema before Open: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close seed database: %v", err)
	}

	store, err := Open(ctx, databasePath)
	if err != nil {
		t.Fatalf("Open shared database: %v", err)
	}
	defer store.Close()

	var schemaAfter string
	if err := store.db.QueryRowContext(ctx, `
SELECT sql FROM sqlite_master WHERE type = 'table' AND name = 'videos'
`).Scan(&schemaAfter); err != nil {
		t.Fatalf("read videos schema after Open: %v", err)
	}
	if schemaAfter != schemaBefore {
		t.Fatalf("videos schema changed:\nbefore: %s\nafter:  %s", schemaBefore, schemaAfter)
	}
	var sentinel string
	var queuePosition int
	if err := store.db.QueryRowContext(ctx, `
SELECT sentinel, queue_position FROM videos WHERE id = 7
`).Scan(&sentinel, &queuePosition); err != nil {
		t.Fatalf("read sentinel video after Open: %v", err)
	}
	if sentinel != "preserve-me" || queuePosition != 19 {
		t.Fatalf(
			"sentinel video changed: sentinel=%q queue_position=%d",
			sentinel,
			queuePosition,
		)
	}
}

func TestCloseIsNilSafe(t *testing.T) {
	var store *Store
	if err := store.Close(); err != nil {
		t.Fatalf("nil Store.Close: %v", err)
	}
	store = &Store{}
	if err := store.Close(); err != nil {
		t.Fatalf("zero Store.Close: %v", err)
	}
}

func assertMode(t *testing.T, path string, want os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := info.Mode().Perm(); got != want {
		t.Fatalf("%s mode = %04o, want %04o", path, got, want)
	}
}

func assertTableExists(t *testing.T, ctx context.Context, db *sql.DB, name string, want bool) {
	t.Helper()
	var count int
	if err := db.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?
`, name).Scan(&count); err != nil {
		t.Fatalf("inspect table %s: %v", name, err)
	}
	if got := count == 1; got != want {
		t.Fatalf("table %s exists = %v, want %v", name, got, want)
	}
}
