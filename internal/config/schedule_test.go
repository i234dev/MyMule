package config

import (
	"testing"
	"time"
)

func TestLimitsAt(t *testing.T) {
	at := func(h, m int) time.Time { return time.Date(2026, 1, 1, h, m, 0, 0, time.Local) }
	s := Settings{UploadLimit: 100, DownloadLimit: 500, ScheduleFrom: "23:30", ScheduleTo: "07:00", ScheduleUploadLimit: 0, ScheduleDownloadLimit: 0}
	for _, c := range []struct {
		h, m  int
		sched bool
	}{{23, 29, false}, {23, 30, true}, {3, 0, true}, {6, 59, true}, {7, 0, false}, {12, 0, false}} {
		up, down, in := s.LimitsAt(at(c.h, c.m))
		if in != c.sched || (in && (up != 0 || down != 0)) || (!in && (up != 100 || down != 500)) {
			t.Errorf("%02d:%02d: %d %d %v", c.h, c.m, up, down, in)
		}
	}
	day := Settings{UploadLimit: 100, ScheduleFrom: "09:00", ScheduleTo: "18:00", ScheduleUploadLimit: 20}
	if up, _, in := day.LimitsAt(at(10, 0)); !in || up != 20 {
		t.Error("day window")
	}
	if _, _, in := (Settings{}).LimitsAt(at(10, 0)); in {
		t.Error("no schedule")
	}
	var bad Settings
	for _, line := range []string{"schedule_from = 25:00", "schedule_download_limit = 10", "schedule_upload_limit = -1"} {
		if _, err := parseSettings(line, &bad); err == nil {
			t.Errorf("%q accepted", line)
		}
	}
	if checkSettings(Settings{ScheduleFrom: "01:00"}) == nil {
		t.Error("from without to accepted")
	}
}
