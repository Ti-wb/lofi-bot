package obs

import (
	"log/slog"
	"sync"

	retryloop "github.com/tiwb/tg-obs-bot/internal/retry"
	"github.com/tiwb/tg-obs-bot/internal/secret"
)

const (
	obsEventFailureLogEvery = 8
	obsEventRecoveryHealthy = 8
)

type eventErrorSampler struct {
	sampler         *retryloop.Sampler
	consecutiveGood uint64
}

type eventLogSamplers struct {
	mu sync.Mutex

	eventDecode    eventErrorSampler
	responseDecode eventErrorSampler
	eventOverflow  eventErrorSampler
	centerSource   eventErrorSampler
}

func (s *eventLogSamplers) failure(target *eventErrorSampler) retryloop.Sample {
	s.mu.Lock()
	defer s.mu.Unlock()
	if target.sampler == nil {
		target.sampler = retryloop.NewSampler(obsEventFailureLogEvery)
	}
	target.consecutiveGood = 0
	return target.sampler.Failure()
}

func (s *eventLogSamplers) success(target *eventErrorSampler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if target.sampler == nil {
		return
	}
	target.consecutiveGood++
	if target.consecutiveGood < obsEventRecoveryHealthy {
		return
	}
	target.sampler.Reset()
	target.consecutiveGood = 0
}

func logSampledEventWarning(logger *slog.Logger, message string, sample retryloop.Sample, attrs ...any) {
	if sample.Kind == retryloop.SampleNone {
		return
	}
	attrs = append(attrs, "consecutive_failures", sample.Consecutive)
	if sample.Suppressed > 0 {
		attrs = append(attrs, "suppressed", sample.Suppressed)
	}
	logger.Warn(message, attrs...)
}

func (c *Client) boundedLogError(err error) string {
	return retryloop.BoundedErrorText(
		secret.RedactError(err, c.opts.Password),
		retryloop.RecurringErrorMaxBytes,
	)
}
