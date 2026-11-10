package stats

import (
	"path/filepath"
	"testing"
	"time"
)

func TestHistory(t *testing.T) {
	p := filepath.Join(t.TempDir(), "history.json")
	h := openHistory(p)
	start := time.Date(2026, 11, 10, 22, 58, 0, 0, time.Local)
	// A minute at 1 MB/s down and 100 KB/s up, four times: two minutes in
	// one hour, two in the next (and the next day at midnight).
	for i := 1; i <= 4; i++ {
		at := start.Add(time.Duration(i) * time.Minute)
		if i > 2 {
			at = at.Add(time.Hour) // 00:01, 00:02 the next day
		}
		h.add(at, 60_000_000, 6_000_000, 60)
	}
	minutes, hours, days := h.Snapshot()
	if len(minutes) != 4 || minutes[0].Down != 1_000_000 || minutes[0].Up != 100_000 {
		t.Fatalf("minutes %+v", minutes)
	}
	if len(hours) != 2 || hours[0].Down != 1_000_000 || !hours[1].T.Equal(time.Date(2026, 11, 11, 0, 0, 0, 0, time.Local)) {
		t.Fatalf("hours %+v", hours)
	}
	if len(days) != 2 || days[0].Down != 120_000_000 || days[1].Day != "2026-11-11" {
		t.Fatalf("days %+v", days)
	}
	if err := h.save(); err != nil {
		t.Fatal(err)
	}
	re := openHistory(p)
	m2, h2, d2 := re.Snapshot()
	if len(m2) != 4 || len(h2) != 1 || len(d2) != 2 { // the open hour is not saved
		t.Fatalf("reopened: %d %d %d", len(m2), len(h2), len(d2))
	}
	h.add(start, -1, 0, 60) // counters going back: ignored
	if m, _, _ := h.Snapshot(); len(m) != 4 {
		t.Fatal("bad sample kept")
	}
}
