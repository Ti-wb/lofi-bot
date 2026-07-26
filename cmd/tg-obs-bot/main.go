package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/tiwb/tg-obs-bot/internal/app"
	"github.com/tiwb/tg-obs-bot/internal/asynclog"
	"github.com/tiwb/tg-obs-bot/internal/config"
	"github.com/tiwb/tg-obs-bot/internal/secret"
	"github.com/tiwb/tg-obs-bot/internal/singleton"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

const (
	internalStallExitCode        = 70
	instanceContentionExitCode   = 73
	genericInitializationFailure = 1
	logFlushTimeout              = 500 * time.Millisecond
)

func main() {
	logger, logLevel, logHandler := newProcessLogger(os.Stdout)
	slog.SetDefault(logger)

	cfg, err := config.Load()
	if err != nil {
		logger.Error("load config", "error", secret.RedactError(err))
		exitWithLogFlush(logHandler, genericInitializationFailure)
		return
	}
	logLevel.Set(cfg.LogLevel)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	service, err := app.New(cfg, logger)
	if err != nil {
		logger.Error("initialize service", "error", secret.RedactError(err, cfg.SensitiveValues()...))
		exitWithLogFlush(logHandler, initializationExitCode(err))
		return
	}
	serviceClosed := false
	defer func() {
		if !serviceClosed {
			service.Close()
		}
	}()

	err = service.Run(ctx)
	exitCode := serviceExitCode(err, ctx.Err())
	if exitCode == 0 {
		serviceClosed = true
		service.Close()
		flushLogs(logHandler)
		return
	}
	logger.Error("service stopped", "error", secret.RedactError(err, cfg.SensitiveValues()...))
	// os.Exit intentionally bypasses deferred Close on fatal failures. A stuck
	// handler or worker may still own a dependency lock; the process supervisor
	// is the hard isolation boundary for that state. Kernel teardown still
	// closes both singleton descriptors before a replacement can acquire them.
	exitWithLogFlush(logHandler, exitCode)
}

func newProcessLogger(writer io.Writer) (*slog.Logger, *slog.LevelVar, *asynclog.Handler) {
	level := new(slog.LevelVar)
	level.Set(slog.LevelInfo)
	handler := asynclog.New(slog.NewTextHandler(writer, nil), level)
	return slog.New(handler), level, handler
}

func flushLogs(handler *asynclog.Handler) {
	ctx, cancel := context.WithTimeout(context.Background(), logFlushTimeout)
	defer cancel()
	if err := handler.Shutdown(ctx); err != nil {
		handler.Abort()
	}
}

func exitWithLogFlush(handler *asynclog.Handler, exitCode int) {
	flushLogs(handler)
	os.Exit(exitCode)
}

func initializationExitCode(err error) int {
	if errors.Is(err, singleton.ErrAlreadyRunning) {
		return instanceContentionExitCode
	}
	return genericInitializationFailure
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
