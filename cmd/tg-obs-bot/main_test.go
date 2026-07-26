package main

import (
	"context"
	"errors"
	"testing"

	"github.com/tiwb/tg-obs-bot/internal/app"
	"github.com/tiwb/tg-obs-bot/internal/telegram"
)

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
