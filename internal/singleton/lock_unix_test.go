//go:build darwin || linux

package singleton

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const (
	lockHelperEnabled = "TG_OBS_BOT_LOCK_HELPER"
	lockHelperPath    = "TG_OBS_BOT_LOCK_HELPER_PATH"
	lockHelperToken   = "TG_OBS_BOT_LOCK_HELPER_TOKEN"
	lockHelperMode    = "TG_OBS_BOT_LOCK_HELPER_MODE"
	lockHelperReady   = "TG_OBS_BOT_LOCK_HELPER_READY"
	testBotToken      = "123456789:test-secret-token"
)

var helperHeldLock *Lock

func TestAcquireContendsAndCleanCloseReleases(t *testing.T) {
	setRedirectedUserEnvironment(t)
	databasePath := filepath.Join(t.TempDir(), "runtime", "queue.db")
	first, err := Acquire(databasePath, testBotToken)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	t.Cleanup(func() {
		_ = first.Close()
	})

	info, err := os.Stat(first.Path())
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %04o, want 0600", got)
	}
	botInfo, err := os.Stat(first.BotLockPath())
	if err != nil {
		t.Fatalf("stat bot lock file: %v", err)
	}
	if got := botInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("bot lock mode = %04o, want 0600", got)
	}
	lockDirectoryInfo, err := os.Stat(filepath.Dir(first.BotLockPath()))
	if err != nil {
		t.Fatalf("stat per-user lock directory: %v", err)
	}
	if got := lockDirectoryInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("per-user lock directory mode = %04o, want 0700", got)
	}
	userRoot := filepath.Dir(filepath.Dir(first.BotLockPath()))
	userRootInfo, err := os.Stat(userRoot)
	if err != nil {
		t.Fatalf("stat per-user lock root: %v", err)
	}
	if got := userRootInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("per-user lock root mode = %04o, want 0700", got)
	}
	var rootStat unix.Stat_t
	if err := unix.Stat(userRoot, &rootStat); err != nil {
		t.Fatalf("inspect per-user lock root ownership: %v", err)
	}
	if got, want := int(rootStat.Uid), unix.Geteuid(); got != want {
		t.Fatalf("per-user lock root uid = %d, want %d", got, want)
	}
	if strings.Contains(first.BotLockPath(), testBotToken) ||
		strings.Contains(first.BotLockPath(), "test-secret-token") {
		t.Fatalf("bot lock path leaked token: %q", first.BotLockPath())
	}
	digest := sha256.Sum256([]byte(testBotToken))
	wantBotLockName := botLockFilePrefix + hex.EncodeToString(digest[:]) + botLockFileSuffix
	if got := filepath.Base(first.BotLockPath()); got != wantBotLockName {
		t.Fatalf("bot lock name = %q, want SHA-256 identity %q", got, wantBotLockName)
	}
	if _, err := Acquire(databasePath, testBotToken); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("second acquire error = %v, want contention", err)
	}

	if err := first.Close(); err != nil {
		t.Fatalf("close first lock: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("second Close is not idempotent: %v", err)
	}
	replacement, err := Acquire(databasePath, testBotToken)
	if err != nil {
		t.Fatalf("acquire after clean Close: %v", err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatalf("close replacement lock: %v", err)
	}
}

func TestAcquireCanonicalizesRelativeAndSymlinkAliases(t *testing.T) {
	setRedirectedUserEnvironment(t)
	root := t.TempDir()
	realRuntime := filepath.Join(root, "real-runtime")
	if err := os.MkdirAll(realRuntime, 0o755); err != nil {
		t.Fatalf("create real runtime: %v", err)
	}
	aliasRuntime := filepath.Join(root, "runtime-alias")
	if err := os.Symlink(realRuntime, aliasRuntime); err != nil {
		t.Fatalf("create runtime symlink: %v", err)
	}
	databasePath := filepath.Join(realRuntime, "queue.db")
	first, err := Acquire(databasePath, testBotToken)
	if err != nil {
		t.Fatalf("acquire real path: %v", err)
	}
	defer first.Close()

	for index, alias := range []string{
		filepath.Join(aliasRuntime, "queue.db"),
		mustRelativePath(t, databasePath),
		filepath.Join(realRuntime, "nested", "..", "queue.db"),
	} {
		assertDatabaseContention(
			t,
			alias,
			fmt.Sprintf("98765432%d:alias-token", index),
		)
	}

	canonicalRuntime, err := filepath.EvalSymlinks(realRuntime)
	if err != nil {
		t.Fatalf("resolve expected runtime: %v", err)
	}
	if want := filepath.Join(canonicalRuntime, "queue.db"); first.DatabasePath() != want {
		t.Fatalf("canonical database = %q, want %q", first.DatabasePath(), want)
	}
}

func TestAcquireCanonicalizesDatabaseFileSymlink(t *testing.T) {
	setRedirectedUserEnvironment(t)
	root := t.TempDir()
	target := filepath.Join(root, "canonical.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatalf("create target database: %v", err)
	}
	alias := filepath.Join(root, "alias.db")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatalf("create database symlink: %v", err)
	}

	first, err := Acquire(target, testBotToken)
	if err != nil {
		t.Fatalf("acquire target: %v", err)
	}
	defer first.Close()
	assertDatabaseContention(t, alias, "987654321:file-alias-token")
}

func TestAcquireFencesDatabaseAndTelegramBotIdentities(t *testing.T) {
	setRedirectedUserEnvironment(t)
	firstDatabase := filepath.Join(t.TempDir(), "queue.db")
	secondDatabase := filepath.Join(t.TempDir(), "queue.db")
	first, err := Acquire(firstDatabase, testBotToken)
	if err != nil {
		t.Fatalf("acquire first resource pair: %v", err)
	}
	defer first.Close()

	_, err = Acquire(secondDatabase, testBotToken)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("same token with different database error = %v, want contention", err)
	}
	var botContention *ContentionError
	if !errors.As(err, &botContention) || botContention.Resource != "telegram_bot" {
		t.Fatalf("same-token contention = %#v, want Telegram resource", botContention)
	}
	if strings.Contains(err.Error(), testBotToken) ||
		strings.Contains(err.Error(), "test-secret-token") {
		t.Fatalf("bot contention error leaked token: %v", err)
	}

	_, err = Acquire(firstDatabase, "987654321:different-token")
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("different token with same database error = %v, want contention", err)
	}
	var databaseContention *ContentionError
	if !errors.As(err, &databaseContention) || databaseContention.Resource != "database" {
		t.Fatalf("same-database contention = %#v, want database resource", databaseContention)
	}

	independent, err := Acquire(secondDatabase, "987654321:different-token")
	if err != nil {
		t.Fatalf("different token and database should be independent: %v", err)
	}
	defer independent.Close()
	if first.DatabaseLockPath() == independent.DatabaseLockPath() ||
		first.BotLockPath() == independent.BotLockPath() {
		t.Fatalf(
			"independent resources share locks: first=%q/%q second=%q/%q",
			first.DatabaseLockPath(),
			first.BotLockPath(),
			independent.DatabaseLockPath(),
			independent.BotLockPath(),
		)
	}
}

func TestKernelLockReleasesAfterSubprocessForcedExit(t *testing.T) {
	tests := []string{"panic", "os_exit", "kill"}
	for _, mode := range tests {
		t.Run(mode, func(t *testing.T) {
			setRedirectedUserEnvironment(t)
			databasePath := filepath.Join(t.TempDir(), "queue.db")
			cmd := exec.Command(os.Args[0], "-test.run=^TestSingletonLockHelperProcess$")
			cmd.Env = append(
				os.Environ(),
				lockHelperEnabled+"=1",
				lockHelperPath+"="+databasePath,
				lockHelperToken+"="+testBotToken,
				lockHelperMode+"="+mode,
			)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatalf("helper stdout: %v", err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatalf("start helper: %v", err)
			}
			scanner := bufio.NewScanner(stdout)
			ready := false
			for scanner.Scan() {
				if strings.HasPrefix(strings.TrimSpace(scanner.Text()), lockHelperReady+" ") {
					ready = true
					break
				}
			}
			if !ready {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf(
					"helper did not acquire lock: scan=%v stderr=%s",
					scanner.Err(),
					stderr.String(),
				)
			}

			switch mode {
			case "panic":
				if err := cmd.Wait(); err == nil {
					t.Fatal("panicking helper unexpectedly succeeded")
				}
			case "os_exit":
				if err := cmd.Wait(); err == nil {
					t.Fatal("os.Exit helper unexpectedly succeeded")
				}
			case "kill":
				if _, err := Acquire(databasePath, "987654321:other-process-token"); !errors.Is(err, ErrAlreadyRunning) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("live subprocess database contention error = %v", err)
				}
				otherDatabasePath := filepath.Join(t.TempDir(), "queue.db")
				if _, err := Acquire(otherDatabasePath, testBotToken); !errors.Is(err, ErrAlreadyRunning) {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("live subprocess bot contention error = %v", err)
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatalf("kill helper: %v", err)
				}
				if err := cmd.Wait(); err == nil {
					t.Fatal("killed helper unexpectedly succeeded")
				}
			}

			replacement, err := acquireEventually(databasePath, testBotToken, time.Second)
			if err != nil {
				t.Fatalf(
					"acquire after %s: %v (helper stderr: %s)",
					mode,
					err,
					stderr.String(),
				)
			}
			if err := replacement.Close(); err != nil {
				t.Fatalf("close replacement: %v", err)
			}
		})
	}
}

func TestTelegramBotFenceIgnoresRedirectedProcessDirectories(t *testing.T) {
	root := t.TempDir()
	firstEnvironment := filepath.Join(root, "first-environment")
	secondEnvironment := filepath.Join(root, "second-environment")
	for _, path := range []string{firstEnvironment, secondEnvironment} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create redirected environment directory: %v", err)
		}
	}

	token := fmt.Sprintf(
		"123456789:env-redirect-secret-%d-%s",
		os.Getpid(),
		filepath.Base(root),
	)
	firstDatabase := filepath.Join(root, "first-runtime", "queue.db")
	first := exec.Command(os.Args[0], "-test.run=^TestSingletonLockHelperProcess$")
	first.Env = environmentWithOverrides(map[string]string{
		lockHelperEnabled: "1",
		lockHelperPath:    firstDatabase,
		lockHelperToken:   token,
		lockHelperMode:    "kill",
		"HOME":            filepath.Join(firstEnvironment, "home"),
		"XDG_CONFIG_HOME": filepath.Join(firstEnvironment, "config"),
		"TMPDIR":          filepath.Join(firstEnvironment, "tmp"),
	})
	firstStdout, err := first.StdoutPipe()
	if err != nil {
		t.Fatalf("first helper stdout: %v", err)
	}
	var firstStderr bytes.Buffer
	first.Stderr = &firstStderr
	if err := first.Start(); err != nil {
		t.Fatalf("start first helper: %v", err)
	}
	firstWaited := false
	t.Cleanup(func() {
		if firstWaited {
			return
		}
		_ = first.Process.Kill()
		_ = first.Wait()
	})
	firstScanner := bufio.NewScanner(firstStdout)
	if !firstScanner.Scan() || !strings.HasPrefix(strings.TrimSpace(firstScanner.Text()), lockHelperReady+" ") {
		t.Fatalf(
			"first helper did not acquire stable bot lock: scan=%v stderr=%s",
			firstScanner.Err(),
			firstStderr.String(),
		)
	}
	firstReadyLine := strings.TrimSpace(firstScanner.Text())
	if err := first.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("first helper exited after reporting ready: %v stderr=%s", err, firstStderr.String())
	}
	if _, err := Acquire(filepath.Join(root, "parent-runtime", "queue.db"), token); !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf(
			"parent did not observe first helper bot lock (%s): %v",
			firstReadyLine,
			err,
		)
	}

	secondDatabase := filepath.Join(root, "second-runtime", "queue.db")
	second := exec.Command(os.Args[0], "-test.run=^TestSingletonLockHelperProcess$")
	second.Env = environmentWithOverrides(map[string]string{
		lockHelperEnabled: "1",
		lockHelperPath:    secondDatabase,
		lockHelperToken:   token,
		lockHelperMode:    "acquire_once",
		"HOME":            filepath.Join(secondEnvironment, "home"),
		"XDG_CONFIG_HOME": filepath.Join(secondEnvironment, "config"),
		"TMPDIR":          filepath.Join(secondEnvironment, "tmp"),
	})
	secondOutput, err := second.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf(
			"second helper error = %v output=%s, want contention exit; first=%s",
			err,
			secondOutput,
			firstReadyLine,
		)
	}
	if got := exitErr.ExitCode(); got != 2 {
		t.Fatalf("second helper exit = %d, want 2; output=%s", got, secondOutput)
	}
	if !strings.Contains(string(secondOutput), ErrAlreadyRunning.Error()) {
		t.Fatalf("second helper did not report bot contention: %s", secondOutput)
	}
	if strings.Contains(string(secondOutput), token) ||
		strings.Contains(string(secondOutput), "env-redirect-secret") {
		t.Fatalf("redirected-environment contention leaked token: %s", secondOutput)
	}
	if strings.Contains(string(secondOutput), lockHelperReady) {
		t.Fatalf("second same-token helper acquired lock: %s", secondOutput)
	}

	independent := exec.Command(os.Args[0], "-test.run=^TestSingletonLockHelperProcess$")
	independent.Env = environmentWithOverrides(map[string]string{
		lockHelperEnabled: "1",
		lockHelperPath:    filepath.Join(root, "third-runtime", "queue.db"),
		lockHelperToken:   token + "-independent",
		lockHelperMode:    "acquire_once",
		"HOME":            filepath.Join(secondEnvironment, "another-home"),
		"XDG_CONFIG_HOME": filepath.Join(secondEnvironment, "another-config"),
		"TMPDIR":          filepath.Join(secondEnvironment, "another-tmp"),
	})
	independentOutput, err := independent.CombinedOutput()
	if err != nil {
		t.Fatalf("different-token helper failed: %v output=%s", err, independentOutput)
	}
	if !strings.Contains(string(independentOutput), lockHelperReady) {
		t.Fatalf("different-token helper did not acquire independent lock: %s", independentOutput)
	}

	if err := first.Process.Kill(); err != nil {
		t.Fatalf("kill first helper: %v", err)
	}
	if err := first.Wait(); err == nil {
		t.Fatal("killed first helper unexpectedly succeeded")
	}
	firstWaited = true
}

func TestSingletonLockHelperProcess(t *testing.T) {
	if os.Getenv(lockHelperEnabled) != "1" {
		return
	}
	lock, err := Acquire(os.Getenv(lockHelperPath), os.Getenv(lockHelperToken))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// Keep the descriptors strongly reachable for helper modes that remain
	// alive indefinitely; os.File finalizers must not release the test lock.
	helperHeldLock = lock
	// Intentionally do not defer Close: the forced-exit modes verify that kernel
	// descriptor teardown, not Go cleanup or PID-file deletion, releases it.
	fmt.Println(lockHelperReady, lock.BotLockPath(), "mode="+os.Getenv(lockHelperMode))
	switch os.Getenv(lockHelperMode) {
	case "acquire_once":
		if err := lock.Close(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		helperHeldLock = nil
		return
	case "panic":
		panic("simulated backend crash")
	case "os_exit":
		os.Exit(23)
	case "kill":
		time.Sleep(24 * time.Hour)
	default:
		os.Exit(3)
	}
}

func mustRelativePath(t *testing.T, path string) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	relative, err := filepath.Rel(workingDirectory, path)
	if err != nil {
		t.Fatalf("make relative path: %v", err)
	}
	return relative
}

func setRedirectedUserEnvironment(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	t.Setenv("TMPDIR", filepath.Join(home, "tmp"))
}

func environmentWithOverrides(overrides map[string]string) []string {
	environment := make([]string, 0, len(os.Environ())+len(overrides))
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			environment = append(environment, entry)
		}
	}
	for key, value := range overrides {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func acquireEventually(databasePath string, token string, timeout time.Duration) (*Lock, error) {
	deadline := time.Now().Add(timeout)
	for {
		lock, err := Acquire(databasePath, token)
		if err == nil {
			return lock, nil
		}
		if !errors.Is(err, ErrAlreadyRunning) || time.Now().After(deadline) {
			return nil, err
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertDatabaseContention(t *testing.T, databasePath string, token string) {
	t.Helper()
	_, err := Acquire(databasePath, token)
	if !errors.Is(err, ErrAlreadyRunning) {
		t.Fatalf("database alias %q error = %v, want contention", databasePath, err)
	}
	var contention *ContentionError
	if !errors.As(err, &contention) || contention.Resource != "database" {
		t.Fatalf(
			"database alias %q contention = %#v, want database resource",
			databasePath,
			contention,
		)
	}
}
