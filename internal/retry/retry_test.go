package retry

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestBackoffGrowsCapsHonorsProviderHintAndResets(t *testing.T) {
	backoff := NewBackoff(Policy{
		Initial:         2 * time.Second,
		Max:             10 * time.Second,
		ProviderHintMax: 30 * time.Second,
		JitterPercent:   0,
	}, func() uint64 { return 0 })

	for index, want := range []time.Duration{
		2 * time.Second,
		4 * time.Second,
		8 * time.Second,
		10 * time.Second,
		10 * time.Second,
	} {
		got := backoff.Failure(0)
		if got.Delay != want || got.Consecutive != uint64(index+1) {
			t.Fatalf("failure %d = %+v, want delay=%s consecutive=%d", index+1, got, want, index+1)
		}
	}
	if got := backoff.Failure(25 * time.Second); got.Delay != 25*time.Second {
		t.Fatalf("strong provider hint delay = %s, want 25s", got.Delay)
	}
	if got := backoff.Failure(time.Hour); got.Delay != 30*time.Second {
		t.Fatalf("provider hint safety cap delay = %s, want 30s", got.Delay)
	}
	if failures := backoff.Reset(); failures != 7 {
		t.Fatalf("reset failures = %d, want 7", failures)
	}
	if got := backoff.Failure(0); got.Delay != 2*time.Second || got.Consecutive != 1 {
		t.Fatalf("first failure after reset = %+v", got)
	}
}

func TestBackoffJitterIsBoundedAndLocalCapIsStrict(t *testing.T) {
	policy := Policy{
		Initial:         10 * time.Second,
		Max:             10 * time.Second,
		ProviderHintMax: 10 * time.Second,
		JitterPercent:   20,
	}
	low := NewBackoff(policy, func() uint64 { return 0 }).Failure(0).Delay
	high := NewBackoff(policy, func() uint64 { return math.MaxUint64 }).Failure(0).Delay

	if low != 8*time.Second {
		t.Fatalf("low jitter = %s, want 8s", low)
	}
	if high < 8*time.Second || high > 10*time.Second {
		t.Fatalf("high jitter = %s, want within [8s,10s]", high)
	}
}

func TestBackoffMaxDurationAndCountersSaturateWithoutOverflow(t *testing.T) {
	initial := time.Duration(math.MaxInt64 / 2)
	backoff := NewBackoff(Policy{
		Initial:         initial,
		Max:             time.Duration(math.MaxInt64),
		ProviderHintMax: time.Duration(math.MaxInt64),
		JitterPercent:   20,
	}, func() uint64 { return 0 })
	backoff.failures = math.MaxUint64

	got := backoff.Failure(0)
	minimum := time.Duration(
		uint64(math.MaxInt64) - uint64(math.MaxInt64)/5,
	)
	if got.Delay != minimum {
		t.Fatalf("max-duration low jitter = %s, want %s", got.Delay, minimum)
	}
	if got.Consecutive != math.MaxUint64 || backoff.Failures() != math.MaxUint64 {
		t.Fatalf("failure counter wrapped: attempt=%d state=%d", got.Consecutive, backoff.Failures())
	}

	sampler := NewSampler(8)
	sampler.consecutive = math.MaxUint64
	sampler.suppressed = math.MaxUint64
	sample := sampler.Failure()
	if sample.Consecutive != math.MaxUint64 || sampler.suppressed != math.MaxUint64 {
		t.Fatalf("sampler counters wrapped: sample=%+v suppressed=%d", sample, sampler.suppressed)
	}
}

func TestBackoffMaxInt64TwentyPercentJitterStaysWithinPositiveBounds(t *testing.T) {
	maxDelay := time.Duration(math.MaxInt64)
	minDelay := maxDelay - maxDelay/5
	policy := Policy{
		Initial:         maxDelay,
		Max:             maxDelay,
		ProviderHintMax: maxDelay,
		JitterPercent:   20,
	}

	low := NewBackoff(policy, func() uint64 { return 0 }).Failure(0).Delay
	highOffset := uint64(maxDelay/5) * 2
	high := NewBackoff(policy, func() uint64 { return highOffset }).Failure(0).Delay
	if low != minDelay || low <= 0 {
		t.Fatalf("low jitter = %s, want positive lower bound %s", low, minDelay)
	}
	if high != maxDelay {
		t.Fatalf("high jitter = %s, want strict cap %s", high, maxDelay)
	}
}

func TestSleepCancellationIsImmediate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	err := Sleep(ctx, time.Hour)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Sleep error = %v, want canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Fatalf("canceled Sleep took %s", elapsed)
	}
}

func TestSamplerSuppressesPeriodicallyAndEmitsOneRecovery(t *testing.T) {
	sampler := NewSampler(4)
	want := []Sample{
		{Kind: SampleFirst, Consecutive: 1},
		{Kind: SampleNone, Consecutive: 2},
		{Kind: SampleNone, Consecutive: 3},
		{Kind: SamplePeriodic, Consecutive: 4, Suppressed: 2},
		{Kind: SampleNone, Consecutive: 5},
	}
	for index, expected := range want {
		if got := sampler.Failure(); got != expected {
			t.Fatalf("failure %d sample = %+v, want %+v", index+1, got, expected)
		}
	}
	recovery := sampler.Recovery()
	if recovery != (Sample{Kind: SampleRecovery, Consecutive: 5, Suppressed: 1}) {
		t.Fatalf("recovery = %+v", recovery)
	}
	if got := sampler.Recovery(); got.Kind != SampleNone {
		t.Fatalf("second recovery = %+v, want none", got)
	}
	if got := sampler.Failure(); got.Kind != SampleFirst || got.Consecutive != 1 {
		t.Fatalf("failure after recovery = %+v, want first", got)
	}
}

func TestBoundedErrorTextIsUTF8SafeAndByteBounded(t *testing.T) {
	err := fmt.Errorf("prefix %s secret-tail", strings.Repeat("界", 300))
	got := BoundedErrorText(err, RecurringErrorMaxBytes)
	if len(got) > RecurringErrorMaxBytes {
		t.Fatalf("bounded error length = %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("bounded error is invalid UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded error lacks truncation marker: %q", got)
	}
}

func TestBoundedTextRepairsInvalidUTF8BeforeApplyingByteCap(t *testing.T) {
	got := BoundedText(strings.Repeat("\xff", 300), RecurringErrorMaxBytes)
	if len(got) > RecurringErrorMaxBytes {
		t.Fatalf("bounded text length = %d", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatalf("bounded text is invalid UTF-8: %q", got)
	}
	if got := BoundedText("not retained", 0); got != "" {
		t.Fatalf("zero-byte bound retained %q", got)
	}
	hugeInvalidRun := BoundedText(strings.Repeat("\x80", 1<<20), RecurringErrorMaxBytes)
	if len(hugeInvalidRun) > RecurringErrorMaxBytes || !utf8.ValidString(hugeInvalidRun) {
		t.Fatalf("huge invalid input produced unsafe output: length=%d value=%q", len(hugeInvalidRun), hugeInvalidRun)
	}
	if !strings.HasSuffix(hugeInvalidRun, "…") {
		t.Fatalf("huge invalid input lacks truncation marker: %q", hugeInvalidRun)
	}
}
