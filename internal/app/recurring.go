package app

import (
	"context"
	"log/slog"
	"math"
	"math/rand"
	"time"

	retryloop "github.com/tiwb/tg-obs-bot/internal/retry"
)

const (
	recurringLogEvery        = 8
	recurringJitterPercent   = 20
	obsReconnectInterval     = 5 * time.Second
	obsRetryMaxDelay         = time.Minute
	libraryRetryMaxDelay     = 5 * time.Minute
	watchdogRetryMaxDelay    = 5 * time.Minute
	maintenanceRetryMaxDelay = time.Hour
	eventRecoveryThreshold   = 8
)

type recurringRetry struct {
	backoff *retryloop.Backoff
	sampler *retryloop.Sampler
}

type recurringFailure struct {
	attempt retryloop.Attempt
	sample  retryloop.Sample
}

func newRecurringRetry(initial, max time.Duration, random retryloop.Random) *recurringRetry {
	if random == nil {
		random = rand.Uint64
	}
	return &recurringRetry{
		backoff: retryloop.NewBackoff(retryloop.Policy{
			Initial:         initial,
			Max:             max,
			ProviderHintMax: max,
			JitterPercent:   recurringJitterPercent,
		}, random),
		sampler: retryloop.NewSampler(recurringLogEvery),
	}
}

func (r *recurringRetry) failure() recurringFailure {
	return recurringFailure{
		attempt: r.backoff.Failure(0),
		sample:  r.sampler.Failure(),
	}
}

func (r *recurringRetry) recovery() retryloop.Sample {
	sample := r.sampler.Recovery()
	r.backoff.Reset()
	return sample
}

func logRecurringFailure(logger *slog.Logger, message string, err error, failure recurringFailure) {
	if failure.sample.Kind == retryloop.SampleNone {
		return
	}
	attrs := []any{
		"error", retryloop.BoundedErrorText(err, retryloop.RecurringErrorMaxBytes),
		"retry_in", failure.attempt.Delay,
		"consecutive_failures", failure.sample.Consecutive,
	}
	if failure.sample.Suppressed > 0 {
		attrs = append(attrs, "suppressed", failure.sample.Suppressed)
	}
	logger.Warn(message, attrs...)
}

func logRecurringRecovery(logger *slog.Logger, message string, sample retryloop.Sample) {
	if sample.Kind != retryloop.SampleRecovery {
		return
	}
	logger.Info(
		message,
		"consecutive_failures", sample.Consecutive,
		"suppressed", sample.Suppressed,
	)
}

func logEventFailure(logger *slog.Logger, message string, err error, sample retryloop.Sample) {
	if sample.Kind == retryloop.SampleNone {
		return
	}
	attrs := []any{
		"error", retryloop.BoundedErrorText(err, retryloop.RecurringErrorMaxBytes),
		"consecutive_failures", sample.Consecutive,
	}
	if sample.Suppressed > 0 {
		attrs = append(attrs, "suppressed", sample.Suppressed)
	}
	logger.Warn(message, attrs...)
}

func (s *Service) waitRecurring(ctx context.Context, delay time.Duration) error {
	if s.retrySleep != nil {
		return s.retrySleep(ctx, delay)
	}
	return retryloop.Sleep(ctx, delay)
}

type recurringSchedule struct {
	interval   time.Duration
	now        func() time.Time
	next       time.Time
	retryCycle bool
}

// newRecurringSchedule preserves ticker-style healthy start cadence while
// allowing an actual failure retry to be completion-relative.
func newRecurringSchedule(interval time.Duration, immediate bool, now func() time.Time) *recurringSchedule {
	if now == nil {
		now = time.Now
	}
	schedule := &recurringSchedule{interval: interval, now: now}
	current := now()
	if immediate {
		schedule.next = current
	} else {
		schedule.next = current.Add(interval)
	}
	return schedule
}

func (s *recurringSchedule) delay() time.Duration {
	delay := s.next.Sub(s.now())
	if delay <= 0 {
		return 0
	}
	return delay
}

// beginCycle consumes one ticker slot. Missed ticks collapse to one immediate
// cycle, matching time.Ticker's one-element channel rather than replaying every
// missed interval.
func (s *recurringSchedule) beginCycle() {
	current := s.now()
	if s.retryCycle {
		s.next = current.Add(s.interval)
		s.retryCycle = false
		return
	}
	if s.next.After(current) {
		// An injected waiter may return early. Anchor from the observed start
		// rather than manufacturing a negative operation duration.
		s.next = current.Add(s.interval)
		return
	}
	missed := current.Sub(s.next) / s.interval
	s.next = s.next.Add(missed * s.interval)
	if !s.next.After(current) {
		s.next = s.next.Add(s.interval)
	}
}

func (s *recurringSchedule) failureDelay(delay time.Duration) time.Duration {
	s.retryCycle = true
	return delay
}

func (s *Service) retryClockNow() time.Time {
	if s.retryNow != nil {
		return s.retryNow()
	}
	return time.Now()
}

func maxRetryDelay(base, configuredMax time.Duration) time.Duration {
	if base > configuredMax {
		return base
	}
	return configuredMax
}

// eventErrorSampler samples an event-driven error stream without delaying it.
// Eight consecutive healthy events silently reset the burst, avoiding a
// recovery log (and first-error log) for every alternating good/bad event.
type eventErrorSampler struct {
	sampler         *retryloop.Sampler
	consecutiveGood uint64
}

type playbackNoticeKind uint8

const (
	playbackNoticeInvalidReady playbackNoticeKind = iota
	playbackNoticeInvalidCurrent
	playbackNoticeInvalidRandom
	playbackNoticeRecovered
	playbackNoticeKinds
)

type playbackNotice struct {
	count     uint64
	videoID   int64
	path      string
	errorText string
}

// recordPlaybackNotice stores only a fixed-size, redacted summary while
// playbackMu is held. It intentionally does not log.
func (s *Service) recordPlaybackNotice(kind playbackNoticeKind, videoID int64, path string, err error) {
	if kind >= playbackNoticeKinds {
		return
	}
	path = retryloop.BoundedText(
		s.redactString(path),
		retryloop.RecurringErrorMaxBytes,
	)
	errorText := retryloop.BoundedErrorText(
		s.redactError(err),
		retryloop.RecurringErrorMaxBytes,
	)

	s.mu.Lock()
	notice := &s.playbackNotices[kind]
	if notice.count < math.MaxUint64 {
		notice.count++
	}
	if notice.count == 1 {
		notice.videoID = videoID
		notice.path = path
		notice.errorText = errorText
	}
	s.mu.Unlock()
}

// flushPlaybackNotices moves summaries out from under both playbackMu and mu
// before invoking the logger, which may be slow or backpressured.
func (s *Service) flushPlaybackNotices() {
	s.mu.Lock()
	notices := s.playbackNotices
	s.playbackNotices = [playbackNoticeKinds]playbackNotice{}
	s.mu.Unlock()

	for kind, notice := range notices {
		if notice.count == 0 {
			continue
		}
		attrs := []any{
			"count", notice.count,
			"video_id", notice.videoID,
			"path", notice.path,
		}
		switch playbackNoticeKind(kind) {
		case playbackNoticeInvalidReady:
			attrs = append(attrs, "error", notice.errorText)
			s.logger.Warn("skip invalid ready video path", attrs...)
		case playbackNoticeInvalidCurrent:
			attrs = append(attrs, "error", notice.errorText)
			s.logger.Warn("mark invalid current video failed", attrs...)
		case playbackNoticeInvalidRandom:
			attrs = append(attrs, "error", notice.errorText)
			s.logger.Warn("skip invalid random fallback file", attrs...)
		case playbackNoticeRecovered:
			s.logger.Info("recovered OBS playback", attrs...)
		}
	}
}

func (s *Service) observeLibraryRecoveryScan(err error) {
	s.libraryScanLogMu.Lock()
	if s.libraryScanLog == nil {
		s.libraryScanLog = newEventErrorSampler()
	}
	var sample retryloop.Sample
	if err != nil {
		sample = s.libraryScanLog.failure()
	} else {
		s.libraryScanLog.success()
	}
	s.libraryScanLogMu.Unlock()

	if err != nil {
		logEventFailure(
			s.logger,
			"media library scan found issues during OBS recovery",
			s.redactError(err),
			sample,
		)
	}
}

func newEventErrorSampler() *eventErrorSampler {
	return &eventErrorSampler{sampler: retryloop.NewSampler(recurringLogEvery)}
}

func (s *eventErrorSampler) failure() retryloop.Sample {
	s.consecutiveGood = 0
	return s.sampler.Failure()
}

func (s *eventErrorSampler) success() {
	s.consecutiveGood++
	if s.consecutiveGood < eventRecoveryThreshold {
		return
	}
	s.sampler.Reset()
	s.consecutiveGood = 0
}

type obsRetryOperation uint8

const (
	obsRetryNone obsRetryOperation = iota
	obsRetryConnect
	obsRetryProbe
	obsRetryResume
)

type obsMaintenanceResult struct {
	failure        obsRetryOperation
	err            error
	connected      bool
	recoverConnect bool
	recoverProbe   bool
	recoverResume  bool
}

type obsRetryState struct {
	connect *recurringRetry
	probe   *recurringRetry
	resume  *recurringRetry
}

func newOBSRetryState(random retryloop.Random) obsRetryState {
	return obsRetryState{
		connect: newRecurringRetry(obsReconnectInterval, obsRetryMaxDelay, random),
		probe:   newRecurringRetry(obsReconnectInterval, obsRetryMaxDelay, random),
		resume:  newRecurringRetry(obsReconnectInterval, obsRetryMaxDelay, random),
	}
}

func (s obsRetryState) operation(operation obsRetryOperation) *recurringRetry {
	switch operation {
	case obsRetryConnect:
		return s.connect
	case obsRetryProbe:
		return s.probe
	case obsRetryResume:
		return s.resume
	default:
		return nil
	}
}

func obsFailureMessage(operation obsRetryOperation) string {
	switch operation {
	case obsRetryConnect:
		return "connect OBS failed"
	case obsRetryProbe:
		return "probe OBS failed"
	case obsRetryResume:
		return "resume playback failed"
	default:
		return "OBS maintenance failed"
	}
}

func (s *Service) observeOBSResult(state obsRetryState, result obsMaintenanceResult) time.Duration {
	if result.recoverConnect {
		sample := state.connect.recovery()
		if result.connected {
			if sample.Kind == retryloop.SampleRecovery {
				s.logger.Info(
					"connected to OBS",
					"consecutive_failures", sample.Consecutive,
					"suppressed", sample.Suppressed,
				)
			} else {
				s.logger.Info("connected to OBS")
			}
		}
	}
	if result.recoverProbe {
		logRecurringRecovery(s.logger, "OBS probe recovered", state.probe.recovery())
	}
	if result.recoverResume {
		logRecurringRecovery(s.logger, "OBS playback recovery succeeded", state.resume.recovery())
	}
	if result.failure == obsRetryNone || result.err == nil {
		return obsReconnectInterval
	}
	sequence := state.operation(result.failure)
	if sequence == nil {
		return obsReconnectInterval
	}
	failure := sequence.failure()
	logRecurringFailure(
		s.logger,
		obsFailureMessage(result.failure),
		s.redactError(result.err),
		failure,
	)
	return failure.attempt.Delay
}
