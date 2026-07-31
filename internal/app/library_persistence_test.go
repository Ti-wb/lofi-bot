package app

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestLastMusicPersistenceFailureIsReturnedAndRetried(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "library.db")
	svc, fakeOBS := newLibraryTestServiceAtDBPath(t, dbPath)
	writeLibraryFile(t, svc.cfg.LoopMediaDir, "loop_day_cafe_001.mp4")
	writeLibraryFile(t, svc.cfg.MusicMediaDir, "music_alpha.mp3")
	svc.now = fixedNow("2026-06-24T12:00:00+08:00")
	if err := svc.ScanLibrary(ctx); err != nil {
		t.Fatalf("scan library: %v", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open failure-injection database: %v", err)
	}
	defer db.Close()
	if _, err := db.ExecContext(ctx, `
CREATE TRIGGER fail_last_music_insert
BEFORE INSERT ON library_kv
WHEN NEW.key = 'last_music_id'
BEGIN
	SELECT RAISE(FAIL, 'injected last-music persistence failure');
END;
`); err != nil {
		t.Fatalf("create failure trigger: %v", err)
	}

	err = svc.ensureLibraryPlayback(ctx, false)
	if err == nil {
		t.Fatal("playback swallowed last-music persistence failure")
	}
	if svc.activeMusicID == "" || svc.pendingLastMusicID != svc.activeMusicID {
		t.Fatalf(
			"pending persistence = %q active music = %q, want the played track queued for retry",
			svc.pendingLastMusicID,
			svc.activeMusicID,
		)
	}
	musicCalls := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]

	if _, err := db.ExecContext(ctx, `DROP TRIGGER fail_last_music_insert`); err != nil {
		t.Fatalf("remove failure trigger: %v", err)
	}
	if err := svc.ensureLibraryPlayback(ctx, false); err != nil {
		t.Fatalf("retry pending last-music persistence: %v", err)
	}
	if svc.pendingLastMusicID != "" {
		t.Fatalf("pending last music = %q, want cleared after retry", svc.pendingLastMusicID)
	}
	stored, err := svc.libDB.LastMusicID(ctx)
	if err != nil {
		t.Fatalf("read persisted last music: %v", err)
	}
	if stored != svc.activeMusicID {
		t.Fatalf("persisted last music = %q, want active %q", stored, svc.activeMusicID)
	}
	if got := fakeOBS.sourcePlayCalls[svc.cfg.OBSMusicSourceName]; got != musicCalls {
		t.Fatalf("music replayed during persistence-only retry: calls=%d want=%d", got, musicCalls)
	}
}
