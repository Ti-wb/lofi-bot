package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/app"
	"github.com/tiwb/tg-obs-bot/internal/asynclog"
	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/secret"
	"github.com/tiwb/tg-obs-bot/internal/singleton"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

const (
	initializationHelperEnabled = "TG_OBS_BOT_INITIALIZATION_HELPER"
	asyncLogHelperEnabled       = "TG_OBS_BOT_ASYNC_LOG_HELPER"
	asyncLogHelperExitCode      = "TG_OBS_BOT_ASYNC_LOG_EXIT_CODE"
)

func TestInitializationExitCodeDistinguishesInstanceContention(t *testing.T) {
	if got := initializationExitCode(fmt.Errorf("wrapped: %w", singleton.ErrAlreadyRunning)); got != instanceContentionExitCode {
		t.Fatalf("contention exit code = %d, want %d", got, instanceContentionExitCode)
	}
	if got := initializationExitCode(errors.New("ordinary startup failure")); got != genericInitializationFailure {
		t.Fatalf("generic initialization exit code = %d, want %d", got, genericInitializationFailure)
	}
	if instanceContentionExitCode == 0 || instanceContentionExitCode == genericInitializationFailure {
		t.Fatalf("instance contention exit code %d is not distinct and nonzero", instanceContentionExitCode)
	}
}

func TestInstanceContentionProducesDistinctProcessExit(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "config"))
	databasePath := filepath.Join(t.TempDir(), "queue.db")
	const token = "123456789:process-exit-secret"
	lock, err := singleton.Acquire(databasePath, token)
	if err != nil {
		t.Fatalf("hold resource locks: %v", err)
	}
	defer lock.Close()

	cmd := exec.Command(os.Args[0], "-test.run=^TestInitializationContentionHelperProcess$")
	cmd.Env = append(
		os.Environ(),
		initializationHelperEnabled+"=1",
		"TG_OBS_BOT_INITIALIZATION_DB="+databasePath,
		"TG_OBS_BOT_INITIALIZATION_TOKEN="+token,
	)
	output, err := cmd.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Fatalf("helper error = %v output=%s, want process exit", err, output)
	}
	if got := exitErr.ExitCode(); got != instanceContentionExitCode {
		t.Fatalf("helper exit = %d, want %d; output=%s", got, instanceContentionExitCode, output)
	}
	if strings.Contains(string(output), token) ||
		strings.Contains(string(output), "process-exit-secret") {
		t.Fatalf("contention process output leaked token: %s", output)
	}
}

func TestInitializationContentionHelperProcess(t *testing.T) {
	if os.Getenv(initializationHelperEnabled) != "1" {
		return
	}
	service, err := app.New(config.Config{
		TelegramBotToken: os.Getenv("TG_OBS_BOT_INITIALIZATION_TOKEN"),
		DataDir:          filepath.Join(t.TempDir(), "data"),
		DatabasePath:     os.Getenv("TG_OBS_BOT_INITIALIZATION_DB"),
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if service != nil {
		service.Close()
		os.Exit(0)
	}
	os.Exit(initializationExitCode(err))
}

func TestServiceExitCode(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		parentErr error
		want      int
	}{
		{name: "success", want: 0},
		{name: "normal cancellation", err: context.Canceled, parentErr: context.Canceled, want: 0},
		{
			name:      "stuck update during cancellation",
			err:       errors.Join(context.Canceled, telegram.ErrUpdateHandlerStuck),
			parentErr: context.Canceled,
			want:      internalStallExitCode,
		},
		{
			name: "stuck worker shutdown",
			err:  errors.Join(errors.New("worker failed"), app.ErrWorkerShutdownStuck),
			want: internalStallExitCode,
		},
		{
			name: "stuck infrastructure shutdown",
			err:  errors.Join(errors.New("reporter failed"), app.ErrInfrastructureShutdownStuck),
			want: internalStallExitCode,
		},
		{
			name: "worker canceled without parent cancellation",
			err:  errors.Join(context.Canceled, app.ErrRequiredWorkerStopped),
			want: 1,
		},
		{
			name: "required infrastructure failure",
			err:  errors.Join(errors.New("pipe broke"), app.ErrRequiredInfrastructureStopped),
			want: 1,
		},
		{
			name:      "required worker failure wins cancellation race",
			err:       errors.Join(context.Canceled, app.ErrRequiredWorkerStopped),
			parentErr: context.Canceled,
			want:      1,
		},
		{name: "ordinary failure", err: errors.New("boom"), want: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := serviceExitCode(tt.err, tt.parentErr); got != tt.want {
				t.Fatalf("serviceExitCode(%v, %v) = %d, want %d", tt.err, tt.parentErr, got, tt.want)
			}
		})
	}
}

func TestFatalExitCodesRemainBoundedWhenStdoutIsUnread(t *testing.T) {
	for _, exitCode := range []int{
		genericInitializationFailure,
		internalStallExitCode,
		instanceContentionExitCode,
	} {
		t.Run(strconv.Itoa(exitCode), func(t *testing.T) {
			elapsed, err, stderr := runBlockedStdoutHelper(t, exitCode)
			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("helper error = %v, want exit %d; stderr=%s", err, exitCode, stderr)
			}
			if got := exitErr.ExitCode(); got != exitCode {
				t.Fatalf("helper exit = %d, want %d; stderr=%s", got, exitCode, stderr)
			}
			if elapsed > 3*time.Second {
				t.Fatalf("blocked fatal exit %d took %v", exitCode, elapsed)
			}
		})
	}
}

func TestNormalLogFlushDeadlineRemainsBoundedWhenStdoutIsUnread(t *testing.T) {
	elapsed, err, stderr := runBlockedStdoutHelper(t, 0)
	if err != nil {
		t.Fatalf("normal helper error = %v; stderr=%s", err, stderr)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("normal blocked flush took %v", elapsed)
	}
}

func TestAsyncLogExitHelperProcess(t *testing.T) {
	if os.Getenv(asyncLogHelperEnabled) != "1" {
		return
	}

	exitCode, err := strconv.Atoi(os.Getenv(asyncLogHelperExitCode))
	if err != nil {
		os.Exit(125)
	}
	logger, _, handler := newProcessLogger(os.Stdout)
	payload := strings.Repeat("x", 16*1024)
	for i := 0; i < asynclog.QueueCapacity*16; i++ {
		logger.Error("fill stdout pipe", "sequence", i, "payload", payload)
	}
	logger.Error("fatal sentinel", "exit_code", exitCode)
	if exitCode == 0 {
		flushLogs(handler)
		os.Exit(0)
	}
	exitWithLogFlush(handler, exitCode)
}

func TestProcessLoggerKeepsStartupSecretsRedacted(t *testing.T) {
	const (
		token    = "123456789:main-logger-token"
		password = "main-logger-password"
	)
	var output bytes.Buffer
	logger, _, handler := newProcessLogger(&output)
	logger.Error(
		"initialize service",
		"error",
		secret.RedactError(
			fmt.Errorf("telegram %s OBS %s", token, password),
			token,
			password,
		),
	)
	flushLogs(handler)

	logged := output.String()
	if strings.Contains(logged, token) ||
		strings.Contains(logged, "main-logger-token") ||
		strings.Contains(logged, password) {
		t.Fatalf("process logger leaked startup secret: %q", logged)
	}
	if !strings.Contains(strings.ToLower(logged), "redacted") {
		t.Fatalf("process logger output lacks redaction marker: %q", logged)
	}
}

func runBlockedStdoutHelper(t *testing.T, exitCode int) (time.Duration, error, string) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestAsyncLogExitHelperProcess$")
	cmd.Env = append(
		os.Environ(),
		asyncLogHelperEnabled+"=1",
		asyncLogHelperExitCode+"="+strconv.Itoa(exitCode),
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("helper stdout pipe: %v", err)
	}
	defer stdout.Close()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	start := time.Now()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start helper: %v", err)
	}
	wait := make(chan error, 1)
	go func() {
		wait <- cmd.Wait()
	}()

	select {
	case err := <-wait:
		return time.Since(start), err, stderr.String()
	case <-time.After(4 * time.Second):
		_ = cmd.Process.Kill()
		<-wait
		t.Fatalf("helper exit %d blocked beyond deadline; stderr=%s", exitCode, stderr.String())
		return 0, nil, ""
	}
}
