package obs

import (
	"bytes"
	"errors"
	"log/slog"
	"strings"
	"testing"

	retryloop "github.com/tiwb/tg-obs-bot/internal/retry"
)

func TestEventSamplerDoesNotResetOnAlternatingSuccess(t *testing.T) {
	var samplers eventLogSamplers
	if sample := samplers.failure(&samplers.eventDecode); sample.Kind != retryloop.SampleFirst {
		t.Fatalf("first failure sample = %+v", sample)
	}
	for failureNumber := 2; failureNumber <= obsEventFailureLogEvery; failureNumber++ {
		samplers.success(&samplers.eventDecode)
		sample := samplers.failure(&samplers.eventDecode)
		if failureNumber < obsEventFailureLogEvery && sample.Kind != retryloop.SampleNone {
			t.Fatalf("alternating failure %d sample = %+v, want suppressed", failureNumber, sample)
		}
		if failureNumber == obsEventFailureLogEvery && sample.Kind != retryloop.SamplePeriodic {
			t.Fatalf("failure %d sample = %+v, want periodic", failureNumber, sample)
		}
	}

	for range obsEventRecoveryHealthy {
		samplers.success(&samplers.eventDecode)
	}
	if sample := samplers.failure(&samplers.eventDecode); sample.Kind != retryloop.SampleFirst {
		t.Fatalf("failure after sustained healthy events = %+v, want first", sample)
	}
}

func TestOBSDecodeWarningsAreSampledWithoutDelayingEventStream(t *testing.T) {
	var logs bytes.Buffer
	client, err := NewClient(Options{
		URL:             "ws://unused.invalid",
		MediaSourceName: "queue",
		Logger:          slog.New(slog.NewTextHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	for range 16 {
		client.handleEvent([]byte(`{"eventType":`))
	}

	got := logs.String()
	if count := strings.Count(got, `msg="decode OBS event"`); count != 3 {
		t.Fatalf("decode warning count = %d, want first plus failures 8 and 16", count)
	}
	if !strings.Contains(got, "suppressed=6") || !strings.Contains(got, "suppressed=7") {
		t.Fatalf("decode warnings lack suppression summaries: %q", got)
	}
}

func TestOBSRecurringErrorRenderingRedactsThenBounds(t *testing.T) {
	const password = "obs-secret-password"
	client := &Client{opts: Options{Password: password}}
	got := client.boundedLogError(errors.New(password + strings.Repeat("界", 500)))

	if strings.Contains(got, password) {
		t.Fatalf("bounded OBS error leaked password: %q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("bounded OBS error lacks redaction marker: %q", got)
	}
	if len(got) > retryloop.RecurringErrorMaxBytes {
		t.Fatalf("bounded OBS error = %d bytes, want <= %d", len(got), retryloop.RecurringErrorMaxBytes)
	}
}
