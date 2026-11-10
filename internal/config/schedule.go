package config

import (
	"errors"
	"time"
)

// parseClock reads "HH:MM" as minutes since midnight.
func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, errors.New("not a time")
	}
	return t.Hour()*60 + t.Minute(), nil
}

// LimitsAt returns the speed limits (KB/s) to use at t: the schedule's
// within its window (which may go past midnight), the usual ones else.
func (s Settings) LimitsAt(t time.Time) (upload, download int, scheduled bool) {
	from, err1 := parseClock(s.ScheduleFrom)
	to, err2 := parseClock(s.ScheduleTo)
	if err1 != nil || err2 != nil || from == to {
		return s.UploadLimit, s.DownloadLimit, false
	}
	now := t.Hour()*60 + t.Minute()
	in := from <= now && now < to
	if from > to { // past midnight: 23:00-07:00
		in = now >= from || now < to
	}
	if in {
		return s.ScheduleUploadLimit, s.ScheduleDownloadLimit, true
	}
	return s.UploadLimit, s.DownloadLimit, false
}
