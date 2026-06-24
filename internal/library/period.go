package library

import "time"

// Period names the local-time media period.
type Period string

const (
	PeriodMorning Period = "morning"
	PeriodDay     Period = "day"
	PeriodEvening Period = "evening"
	PeriodNight   Period = "night"
)

var orderedPeriods = []Period{
	PeriodMorning,
	PeriodDay,
	PeriodEvening,
	PeriodNight,
}

// PeriodInfo describes the period containing a point in local time. EndsAt is
// the exclusive boundary where Next begins.
type PeriodInfo struct {
	Period Period
	Next   Period
	EndsAt time.Time
}

// Periods returns periods in their daily order.
func Periods() []Period {
	periods := make([]Period, len(orderedPeriods))
	copy(periods, orderedPeriods)
	return periods
}

// ParsePeriod validates a configured or parsed period value.
func ParsePeriod(value string) (Period, error) {
	period := Period(value)
	if period.Valid() {
		return period, nil
	}
	return "", fileError(ErrorInvalidPeriod, KindLoop, "", "period", value, nil)
}

func (p Period) Valid() bool {
	switch p {
	case PeriodMorning, PeriodDay, PeriodEvening, PeriodNight:
		return true
	default:
		return false
	}
}

// Next returns the next period in local-day order, or an empty Period when p is
// invalid.
func (p Period) Next() Period {
	switch p {
	case PeriodMorning:
		return PeriodDay
	case PeriodDay:
		return PeriodEvening
	case PeriodEvening:
		return PeriodNight
	case PeriodNight:
		return PeriodMorning
	default:
		return ""
	}
}

// PeriodAt returns the period for t in t's own location.
func PeriodAt(t time.Time) Period {
	hour := t.Hour()
	switch {
	case hour >= 6 && hour < 11:
		return PeriodMorning
	case hour >= 11 && hour < 17:
		return PeriodDay
	case hour >= 17 && hour < 21:
		return PeriodEvening
	default:
		return PeriodNight
	}
}

// PeriodEnd returns the exclusive end boundary for the period containing t.
func PeriodEnd(t time.Time) time.Time {
	y, m, d := t.Date()
	loc := t.Location()
	switch PeriodAt(t) {
	case PeriodMorning:
		return time.Date(y, m, d, 11, 0, 0, 0, loc)
	case PeriodDay:
		return time.Date(y, m, d, 17, 0, 0, 0, loc)
	case PeriodEvening:
		return time.Date(y, m, d, 21, 0, 0, 0, loc)
	default:
		if t.Hour() >= 21 {
			t = t.AddDate(0, 0, 1)
			y, m, d = t.Date()
		}
		return time.Date(y, m, d, 6, 0, 0, 0, loc)
	}
}

// PeriodInfoAt returns the current period, the next period, and the exclusive
// end boundary for the current period.
func PeriodInfoAt(t time.Time) PeriodInfo {
	period := PeriodAt(t)
	return PeriodInfo{
		Period: period,
		Next:   period.Next(),
		EndsAt: PeriodEnd(t),
	}
}
