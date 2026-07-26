// Package retry provides bounded retry timing and O(1) repeated-failure
// sampling for long-running service loops.
package retry

import (
	"context"
	"math"
	"math/bits"
	"math/rand"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	defaultInitial     = time.Second
	defaultSampleEvery = uint64(8)
	// RecurringErrorMaxBytes bounds one rendered recurring error before it is
	// retained by asynchronous logging.
	RecurringErrorMaxBytes = 512
)

// Policy defines one exponential retry sequence. Max caps the locally
// generated delay. ProviderHintMax is a separate safety cap for a provider's
// stronger retry hint.
type Policy struct {
	Initial         time.Duration
	Max             time.Duration
	ProviderHintMax time.Duration
	JitterPercent   uint64
}

// Random returns one uniformly distributed uint64. Tests may inject a
// deterministic implementation.
type Random func() uint64

// Backoff is intended to be owned by one recurring loop.
type Backoff struct {
	policy   Policy
	random   Random
	failures uint64
}

// Attempt describes the next retry after a failure.
type Attempt struct {
	Delay       time.Duration
	Consecutive uint64
}

// NewBackoff constructs a normalized retry sequence.
func NewBackoff(policy Policy, random Random) *Backoff {
	if policy.Initial <= 0 {
		policy.Initial = defaultInitial
	}
	if policy.Max < policy.Initial {
		policy.Max = policy.Initial
	}
	if policy.ProviderHintMax < policy.Max {
		policy.ProviderHintMax = policy.Max
	}
	if policy.JitterPercent > 100 {
		policy.JitterPercent = 100
	}
	if random == nil {
		random = rand.Uint64
	}
	return &Backoff{policy: policy, random: random}
}

// Failure advances the exponential sequence and returns its bounded delay.
// A positive provider hint wins when it is stronger, up to ProviderHintMax.
func (b *Backoff) Failure(providerHint time.Duration) Attempt {
	base := b.policy.Initial
	for step := uint64(0); step < b.failures && base < b.policy.Max; step++ {
		if base > b.policy.Max/2 {
			base = b.policy.Max
			break
		}
		base *= 2
	}
	if base > b.policy.Max {
		base = b.policy.Max
	}

	delay := jitter(base, b.policy.Max, b.policy.JitterPercent, b.random)
	if providerHint > b.policy.ProviderHintMax {
		providerHint = b.policy.ProviderHintMax
	}
	if providerHint > delay {
		delay = providerHint
	}
	b.failures = saturatingIncrement(b.failures)
	return Attempt{Delay: delay, Consecutive: b.failures}
}

// Reset starts the sequence over and returns the prior failure count.
func (b *Backoff) Reset() uint64 {
	failures := b.failures
	b.failures = 0
	return failures
}

// Failures returns the current consecutive failure count.
func (b *Backoff) Failures() uint64 {
	return b.failures
}

// SampleKind identifies whether a failure/recovery should be emitted.
type SampleKind uint8

const (
	SampleNone SampleKind = iota
	SampleFirst
	SamplePeriodic
	SampleRecovery
)

// Sample is an O(1) decision for repeated-failure logging.
type Sample struct {
	Kind        SampleKind
	Consecutive uint64
	Suppressed  uint64
}

// Sampler emits the first failure, every Nth failure with the number suppressed
// since the previous emission, and one recovery. It stores no error strings or
// unbounded key set and is intended to be serialized by its owning loop.
type Sampler struct {
	every       uint64
	consecutive uint64
	suppressed  uint64
}

// NewSampler constructs a sampler. Values below two use the default period.
func NewSampler(every uint64) *Sampler {
	if every < 2 {
		every = defaultSampleEvery
	}
	return &Sampler{every: every}
}

// Failure records one failure and returns an emission decision.
func (s *Sampler) Failure() Sample {
	s.consecutive = saturatingIncrement(s.consecutive)
	if s.consecutive == 1 {
		return Sample{Kind: SampleFirst, Consecutive: s.consecutive}
	}
	if s.consecutive%s.every == 0 {
		sample := Sample{
			Kind:        SamplePeriodic,
			Consecutive: s.consecutive,
			Suppressed:  s.suppressed,
		}
		s.suppressed = 0
		return sample
	}
	s.suppressed = saturatingIncrement(s.suppressed)
	return Sample{Kind: SampleNone, Consecutive: s.consecutive}
}

// Recovery records a genuine recovery and returns at most one emission.
func (s *Sampler) Recovery() Sample {
	if s.consecutive == 0 {
		return Sample{}
	}
	sample := Sample{
		Kind:        SampleRecovery,
		Consecutive: s.consecutive,
		Suppressed:  s.suppressed,
	}
	s.consecutive = 0
	s.suppressed = 0
	return sample
}

// Reset clears a sampler without emitting a recovery event.
func (s *Sampler) Reset() {
	s.consecutive = 0
	s.suppressed = 0
}

// Sleep waits without making cancellation wait for the retry delay.
func Sleep(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// BoundedErrorText converts an already-redacted error to valid UTF-8 and caps
// it by bytes. It never retains or keys state by the returned text.
func BoundedErrorText(err error, maxBytes int) string {
	if err == nil {
		return ""
	}
	return BoundedText(err.Error(), maxBytes)
}

// BoundedText converts text to valid UTF-8 and caps it by bytes.
func BoundedText(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	inputTruncated := len(value) > maxBytes
	if inputTruncated {
		// The caller needs only a bounded rendered prefix. Avoid validating and
		// copying an arbitrarily large, already-redacted error string.
		value = value[:maxBytes]
	}
	text := strings.ToValidUTF8(value, "\uFFFD")
	if !inputTruncated && len(text) <= maxBytes {
		return text
	}
	const suffix = "…"
	if maxBytes < len(suffix) {
		return ""
	}
	end := len(text)
	if limit := maxBytes - len(suffix); end > limit {
		end = limit
	}
	for end > 0 && end < len(text) && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + suffix
}

func jitter(base, max time.Duration, percent uint64, random Random) time.Duration {
	if percent == 0 || base <= 1 {
		return base
	}
	span := scaleDuration(base, percent, 100)
	if span <= 0 {
		return base
	}
	lower := base - span
	width := uint64(span) * 2
	var offset uint64
	if width == math.MaxUint64 {
		offset = random()
	} else {
		offset = random() % (width + 1)
	}
	maxOffset := uint64(max - lower)
	if offset >= maxOffset {
		return max
	}
	delay := lower + time.Duration(offset)
	return delay
}

func scaleDuration(value time.Duration, numerator, denominator uint64) time.Duration {
	if value <= 0 || numerator == 0 || denominator == 0 {
		return 0
	}
	high, low := bits.Mul64(uint64(value), numerator)
	if high >= denominator {
		return time.Duration(math.MaxInt64)
	}
	scaled, _ := bits.Div64(high, low, denominator)
	if scaled > math.MaxInt64 {
		return time.Duration(math.MaxInt64)
	}
	return time.Duration(scaled)
}

func saturatingIncrement(value uint64) uint64 {
	if value == math.MaxUint64 {
		return value
	}
	return value + 1
}
