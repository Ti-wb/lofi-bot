package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tiwb/tg-obs-bot/internal/app"
	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/singleton"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

const initializationHelperEnabled = "TG_OBS_BOT_INITIALIZATION_HELPER"

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
			name: "worker canceled without parent cancellation",
			err:  errors.Join(context.Canceled, app.ErrRequiredWorkerStopped),
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
