package telegram

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

func TestPollRetryHonorsProviderHintWithoutBreakingLocalCap(t *testing.T) {
	svc := newTestService(t, &fakeBotAPI{})
	svc.pollRetryDelay = 10 * time.Second
	svc.pollRetryMaxDelay = 10 * time.Second
	svc.pollProviderHintMax = 30 * time.Second
	svc.retryRandom = func() uint64 { return 0 }

	state := svc.newPollRetryState()
	attempt, _ := state.failure(9 * time.Second)
	if attempt.Delay != 9*time.Second {
		t.Fatalf("first delay = %s, want provider floor 9s", attempt.Delay)
	}
	attempt, _ = state.failure(25 * time.Second)
	if attempt.Delay != 25*time.Second {
		t.Fatalf("second delay = %s, want provider floor 25s", attempt.Delay)
	}
	attempt, _ = state.failure(time.Hour)
	if attempt.Delay != 30*time.Second {
		t.Fatalf("provider-capped delay = %s, want 30s", attempt.Delay)
	}
	if sample := state.recovery(); sample.Consecutive != 3 {
		t.Fatalf("recovery sample = %+v, want three failures", sample)
	}
	attempt, _ = state.failure(0)
	if attempt.Delay != 8*time.Second || attempt.Consecutive != 1 {
		t.Fatalf("first delay after recovery = %+v, want jittered 8s", attempt)
	}
}

func TestTelegramRetryHintHandlesWrappedValueAndPointerAndClampsBeforeConversion(t *testing.T) {
	const max = 15 * time.Minute
	valueErr := tgbotapi.Error{
		Message: "value",
		ResponseParameters: tgbotapi.ResponseParameters{
			RetryAfter: 7,
		},
	}
	pointerErr := &tgbotapi.Error{
		Message: "pointer",
		ResponseParameters: tgbotapi.ResponseParameters{
			RetryAfter: 11,
		},
	}
	hugeErr := tgbotapi.Error{
		Message: "huge",
		ResponseParameters: tgbotapi.ResponseParameters{
			RetryAfter: int(^uint(0) >> 1),
		},
	}

	tests := []struct {
		name string
		err  error
		want time.Duration
	}{
		{name: "value", err: valueErr, want: 7 * time.Second},
		{name: "wrapped value", err: fmt.Errorf("wrapped: %w", valueErr), want: 7 * time.Second},
		{name: "pointer", err: pointerErr, want: 11 * time.Second},
		{name: "wrapped pointer", err: fmt.Errorf("wrapped: %w", pointerErr), want: 11 * time.Second},
		{name: "huge clamps before duration conversion", err: hugeErr, want: max},
		{name: "unrelated", err: errors.New("unrelated"), want: 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := telegramRetryHint(test.err, max); got != test.want {
				t.Fatalf("telegramRetryHint() = %s, want %s", got, test.want)
			}
		})
	}
}

func TestPollFailureLogsAreSampledRedactedAndBounded(t *testing.T) {
	const token = "123456:ABCdefghi_jklmnop"
	svc := newTestService(t, &fakeBotAPI{})
	svc.cfg.Token = token
	var logs bytes.Buffer
	svc.logger = slog.New(slog.NewTextHandler(&logs, nil))
	svc.pollRetryDelay = time.Second
	svc.pollRetryMaxDelay = time.Minute
	svc.retryRandom = func() uint64 { return 0 }
	state := svc.newPollRetryState()
	pollErr := fmt.Errorf("%s %s", token, strings.Repeat("界", 500))

	for range 16 {
		attempt, sample := state.failure(0)
		svc.logPollFailure(pollErr, attempt, sample)
	}
	svc.logPollRecovery(state.recovery())

	got := logs.String()
	if strings.Contains(got, token) {
		t.Fatalf("poll logs leaked token: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("poll logs lack redaction marker: %q", got)
	}
	if count := strings.Count(got, `msg="get telegram updates"`); count != 3 {
		t.Fatalf("failure log count = %d, want first plus failures 8 and 16", count)
	}
	if count := strings.Count(got, `msg="telegram polling recovered"`); count != 1 {
		t.Fatalf("recovery log count = %d, want 1", count)
	}
	for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
		if len(line) > 800 {
			t.Fatalf("log line retained an unbounded recurring error: %d bytes", len(line))
		}
	}
}
