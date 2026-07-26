package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/tiwb/tg-obs-bot/internal/app"
	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/secret"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

const internalStallExitCode = 70

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("load config", "error", secret.RedactError(err))
		os.Exit(1)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	}))
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	service, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("initialize service", "error", secret.RedactError(err, cfg.SensitiveValues()...))
		os.Exit(1)
	}
	defer service.Close()

	err = service.Run(ctx)
	exitCode := serviceExitCode(err, ctx.Err())
	if exitCode == 0 {
		return
	}
	logger.Error("service stopped", "error", secret.RedactError(err, cfg.SensitiveValues()...))
	// os.Exit intentionally bypasses deferred Close on fatal failures. A stuck
	// handler or worker may still own a dependency lock; the process supervisor
	// is the hard isolation boundary for that state.
	os.Exit(exitCode)
}

func serviceExitCode(err error, parentErr error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, telegram.ErrUpdateHandlerStuck),
		errors.Is(err, app.ErrWorkerShutdownStuck):
		return internalStallExitCode
	case errors.Is(err, app.ErrRequiredWorkerStopped):
		return 1
	case parentErr != nil && errors.Is(err, parentErr):
		return 0
	default:
		return 1
	}
}
