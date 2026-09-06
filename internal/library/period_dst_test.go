package library

import (
	"testing"
	"time"
)

func TestPeriodInfoAtNewYorkSpringForward(t *testing.T) {
	loc := mustLoadPeriodDSTLocation(t, "America/New_York")
	nightEnd := time.Date(2026, time.March, 8, 6, 0, 0, 0, loc)

	tests := []struct {
		name       string
		at         time.Time
		wantPeriod Period
		wantNext   Period
		wantEnd    time.Time
		wantUntil  time.Duration
	}{
		{
			name:       "night starts before offset change",
			at:         time.Date(2026, time.March, 7, 21, 0, 0, 0, loc),
			wantPeriod: PeriodNight,
			wantNext:   PeriodMorning,
			wantEnd:    nightEnd,
			wantUntil:  8 * time.Hour,
		},
		{
			name:       "last second before skipped hour",
			at:         time.Date(2026, time.March, 8, 1, 59, 59, 0, loc),
			wantPeriod: PeriodNight,
			wantNext:   PeriodMorning,
			wantEnd:    nightEnd,
			wantUntil:  3*time.Hour + time.Second,
		},
		{
			name:       "first representable hour after gap",
			at:         time.Date(2026, time.March, 8, 3, 0, 0, 0, loc),
			wantPeriod: PeriodNight,
			wantNext:   PeriodMorning,
			wantEnd:    nightEnd,
			wantUntil:  3 * time.Hour,
		},
		{
			name:       "last second of shortened night",
			at:         time.Date(2026, time.March, 8, 5, 59, 59, 0, loc),
			wantPeriod: PeriodNight,
			wantNext:   PeriodMorning,
			wantEnd:    nightEnd,
			wantUntil:  time.Second,
		},
		{
			name:       "morning boundary",
			at:         nightEnd,
			wantPeriod: PeriodMorning,
			wantNext:   PeriodDay,
			wantEnd:    time.Date(2026, time.March, 8, 11, 0, 0, 0, loc),
			wantUntil:  5 * time.Hour,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertPeriodInfoAt(t, tt.at, tt.wantPeriod, tt.wantNext, tt.wantEnd, tt.wantUntil)
		})
	}
}

func TestPeriodInfoAtNewYorkFallBack(t *testing.T) {
	loc := mustLoadPeriodDSTLocation(t, "America/New_York")
	nightEnd := time.Date(2026, time.November, 1, 6, 0, 0, 0, loc)
	firstOneThirty := time.Date(2026, time.November, 1, 5, 30, 0, 0, time.UTC).In(loc)
	secondOneThirty := time.Date(2026, time.November, 1, 6, 30, 0, 0, time.UTC).In(loc)

	if firstOneThirty.Hour() != 1 || secondOneThirty.Hour() != 1 {
		t.Fatalf("repeated wall hours = %s and %s, want two 01:30 values", firstOneThirty, secondOneThirty)
	}
	_, firstOffset := firstOneThirty.Zone()
	_, secondOffset := secondOneThirty.Zone()
	if firstOffset != -4*60*60 || secondOffset != -5*60*60 {
		t.Fatalf(
			"repeated wall-hour offsets = %d and %d, want EDT(-14400) then EST(-18000)",
			firstOffset,
			secondOffset,
		)
	}

	tests := []struct {
		name      string
		at        time.Time
		wantUntil time.Duration
	}{
		{
			name:      "night starts before repeated hour",
			at:        time.Date(2026, time.October, 31, 21, 0, 0, 0, loc),
			wantUntil: 10 * time.Hour,
		},
		{
			name:      "first one thirty in daylight time",
			at:        firstOneThirty,
			wantUntil: 5*time.Hour + 30*time.Minute,
		},
		{
			name:      "second one thirty in standard time",
			at:        secondOneThirty,
			wantUntil: 4*time.Hour + 30*time.Minute,
		},
		{
			name:      "last second of extended night",
			at:        time.Date(2026, time.November, 1, 5, 59, 59, 0, loc),
			wantUntil: time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertPeriodInfoAt(
				t,
				tt.at,
				PeriodNight,
				PeriodMorning,
				nightEnd,
				tt.wantUntil,
			)
		})
	}

	assertPeriodInfoAt(
		t,
		nightEnd,
		PeriodMorning,
		PeriodDay,
		time.Date(2026, time.November, 1, 11, 0, 0, 0, loc),
		5*time.Hour,
	)
}

func assertPeriodInfoAt(
	t *testing.T,
	at time.Time,
	wantPeriod Period,
	wantNext Period,
	wantEnd time.Time,
	wantUntil time.Duration,
) {
	t.Helper()

	info := PeriodInfoAt(at)
	if info.Period != wantPeriod {
		t.Fatalf("PeriodInfoAt(%s).Period = %q, want %q", at, info.Period, wantPeriod)
	}
	if info.Next != wantNext {
		t.Fatalf("PeriodInfoAt(%s).Next = %q, want %q", at, info.Next, wantNext)
	}
	if !info.EndsAt.Equal(wantEnd) {
		t.Fatalf("PeriodInfoAt(%s).EndsAt = %s, want %s", at, info.EndsAt, wantEnd)
	}
	if info.EndsAt.Location() != at.Location() {
		t.Fatalf(
			"PeriodInfoAt(%s).EndsAt location = %s, want identical location %s",
			at,
			info.EndsAt.Location(),
			at.Location(),
		)
	}
	if got := info.EndsAt.Sub(at); got != wantUntil {
		t.Fatalf("PeriodInfoAt(%s) remaining duration = %s, want %s", at, got, wantUntil)
	}
}

func mustLoadPeriodDSTLocation(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load IANA location %q: %v", name, err)
	}
	return loc
}
