package prefill

import (
	"errors"
	"slices"
	"time"

	"github.com/hustenreizjuengling/picache/internal/prefill/prefillapi"
)

// parseClock parses "HH:MM" into minutes after midnight.
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil || len(s) != 5 {
		return 0, errors.New(`use "HH:MM"`)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// window returns the schedule window that contains now, or else the next
// one: it starts on one of the days at Start and ends at End, the next day
// when End is not after Start. ok is false when the schedule is off.
func window(s prefillapi.Schedule, now time.Time) (start, end time.Time, ok bool) {
	if !s.Enabled || len(s.Days) == 0 {
		return time.Time{}, time.Time{}, false
	}
	from, err1 := parseClock(s.Start)
	to, err2 := parseClock(s.End)
	if err1 != nil || err2 != nil {
		return time.Time{}, time.Time{}, false
	}
	y, m, d := now.Date()
	loc := now.Location()
	// From yesterday (a window over midnight may still run) to a week ahead.
	for i := -1; i <= 7; i++ {
		day := time.Date(y, m, d+i, 0, 0, 0, 0, loc)
		if !slices.Contains(s.Days, int(day.Weekday())) {
			continue
		}
		st := time.Date(y, m, d+i, from/60, from%60, 0, 0, loc)
		en := time.Date(y, m, d+i, to/60, to%60, 0, 0, loc)
		if to <= from {
			en = time.Date(y, m, d+i+1, to/60, to%60, 0, 0, loc)
		}
		if en.After(now) {
			return st, en, true
		}
	}
	return time.Time{}, time.Time{}, false
}
