package library

import (
	"testing"
	"time"
)

func TestPeriodBoundariesAndEndTimes(t *testing.T) {
	loc := time.FixedZone("TST", 8*60*60)
	tests := []struct {
		at       time.Time
		period   Period
		next     Period
		endHour  int
		endDay   int
		endMonth time.Month
	}{
		{at: time.Date(2026, 6, 24, 5, 59, 59, 0, loc), period: PeriodNight, next: PeriodMorning, endHour: 6, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 6, 0, 0, 0, loc), period: PeriodMorning, next: PeriodDay, endHour: 11, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 10, 59, 59, 0, loc), period: PeriodMorning, next: PeriodDay, endHour: 11, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 11, 0, 0, 0, loc), period: PeriodDay, next: PeriodEvening, endHour: 17, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 16, 59, 59, 0, loc), period: PeriodDay, next: PeriodEvening, endHour: 17, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 17, 0, 0, 0, loc), period: PeriodEvening, next: PeriodNight, endHour: 21, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 20, 59, 59, 0, loc), period: PeriodEvening, next: PeriodNight, endHour: 21, endDay: 24, endMonth: time.June},
		{at: time.Date(2026, 6, 24, 21, 0, 0, 0, loc), period: PeriodNight, next: PeriodMorning, endHour: 6, endDay: 25, endMonth: time.June},
	}

	for _, tt := range tests {
		info := PeriodInfoAt(tt.at)
		if info.Period != tt.period {
			t.Fatalf("PeriodInfoAt(%s).Period = %s, want %s", tt.at, info.Period, tt.period)
		}
		if info.Next != tt.next {
			t.Fatalf("PeriodInfoAt(%s).Next = %s, want %s", tt.at, info.Next, tt.next)
		}
		if info.EndsAt.Hour() != tt.endHour || info.EndsAt.Day() != tt.endDay || info.EndsAt.Month() != tt.endMonth {
			t.Fatalf("PeriodInfoAt(%s).EndsAt = %s", tt.at, info.EndsAt)
		}
		if info.EndsAt.Location().String() != tt.at.Location().String() {
			t.Fatalf("EndsAt location = %s, want %s", info.EndsAt.Location(), tt.at.Location())
		}
	}
}

func TestParsePeriodAndNext(t *testing.T) {
	for _, period := range Periods() {
		parsed, err := ParsePeriod(string(period))
		if err != nil {
			t.Fatalf("ParsePeriod(%q): %v", period, err)
		}
		if parsed != period {
			t.Fatalf("ParsePeriod(%q) = %q", period, parsed)
		}
	}

	if PeriodMorning.Next() != PeriodDay ||
		PeriodDay.Next() != PeriodEvening ||
		PeriodEvening.Next() != PeriodNight ||
		PeriodNight.Next() != PeriodMorning {
		t.Fatalf("unexpected next period cycle")
	}
	if _, err := ParsePeriod("dawn"); err == nil {
		t.Fatal("expected invalid period error")
	}
}
