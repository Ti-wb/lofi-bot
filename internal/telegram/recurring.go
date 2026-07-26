package telegram

import (
	"context"
	"errors"
	"math/rand"
	"time"

	retryloop "github.com/tiwb/tg-obs-bot/internal/retry"

	tgbotapi "github.com/go-telegram-bot-api/telegram-bot-api/v5"
)

const (
	defaultPollRetryMaxDelay   = time.Minute
	defaultPollProviderHintMax = 15 * time.Minute
	pollRetryJitterPercent     = 20
	pollFailureLogEvery        = 8
)

type pollRetryState struct {
	backoff *retryloop.Backoff
	sampler *retryloop.Sampler
}

func (s *Service) newPollRetryState() *pollRetryState {
	initial := s.pollRetryDelay
	if initial <= 0 {
		initial = defaultPollRetryDelay
	}
	maxDelay := s.pollRetryMaxDelay
	if maxDelay < initial {
		maxDelay = defaultPollRetryMaxDelay
	}
	if maxDelay < initial {
		maxDelay = initial
	}
	providerMax := s.pollProviderHintMax
	if providerMax < maxDelay {
		providerMax = defaultPollProviderHintMax
	}
	if providerMax < maxDelay {
		providerMax = maxDelay
	}
	random := s.retryRandom
	if random == nil {
		random = rand.Uint64
	}
	return &pollRetryState{
		backoff: retryloop.NewBackoff(retryloop.Policy{
			Initial:         initial,
			Max:             maxDelay,
			ProviderHintMax: providerMax,
			JitterPercent:   pollRetryJitterPercent,
		}, random),
		sampler: retryloop.NewSampler(pollFailureLogEvery),
	}
}

func (s *pollRetryState) failure(providerHint time.Duration) (retryloop.Attempt, retryloop.Sample) {
	return s.backoff.Failure(providerHint), s.sampler.Failure()
}

func (s *pollRetryState) recovery() retryloop.Sample {
	s.backoff.Reset()
	return s.sampler.Recovery()
}

func (s *Service) waitPollRetry(ctx context.Context, delay time.Duration) error {
	if s.pollSleep != nil {
		return s.pollSleep(ctx, delay)
	}
	return retryloop.Sleep(ctx, delay)
}

func (s *Service) logPollFailure(err error, attempt retryloop.Attempt, sample retryloop.Sample) {
	if sample.Kind == retryloop.SampleNone {
		return
	}
	attrs := []any{
		"error", retryloop.BoundedErrorText(s.redactError(err), retryloop.RecurringErrorMaxBytes),
		"retry_in", attempt.Delay,
		"consecutive_failures", sample.Consecutive,
	}
	if sample.Suppressed > 0 {
		attrs = append(attrs, "suppressed", sample.Suppressed)
	}
	s.logger.Warn("get telegram updates", attrs...)
}

func (s *Service) logPollRecovery(sample retryloop.Sample) {
	if sample.Kind != retryloop.SampleRecovery {
		return
	}
	s.logger.Info(
		"telegram polling recovered",
		"consecutive_failures", sample.Consecutive,
		"suppressed", sample.Suppressed,
	)
}

func telegramRetryHint(err error, max time.Duration) time.Duration {
	if err == nil || max <= 0 {
		return 0
	}
	retryAfter := 0
	var pointerError *tgbotapi.Error
	if errors.As(err, &pointerError) && pointerError != nil {
		retryAfter = pointerError.RetryAfter
	} else {
		var valueError tgbotapi.Error
		if errors.As(err, &valueError) {
			retryAfter = valueError.RetryAfter
		}
	}
	if retryAfter <= 0 {
		return 0
	}
	maxSeconds := int64(max / time.Second)
	if maxSeconds <= 0 {
		return 0
	}
	seconds := int64(retryAfter)
	if seconds > maxSeconds {
		seconds = maxSeconds
	}
	return time.Duration(seconds) * time.Second
}
