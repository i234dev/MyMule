package stats

import (
	"encoding/json"
	"os"
	"sync"
	"time"

	"mymule/internal/fsutil"
)

// The history of the transfer speeds, for the charts of the web
// interface: a point a minute for the last day, an hour for the last 30
// days, and the bytes of each day for the last year.

// Point is the average speed (bytes per second) over a minute or an hour
// starting at T.
type Point struct {
	T    time.Time `json:"t"`
	Down float64   `json:"down"`
	Up   float64   `json:"up"`
}

// Day is what was transferred on a day (local time).
type Day struct {
	Day  string `json:"day"` // 2006-01-02
	Down int64  `json:"down"`
	Up   int64  `json:"up"`
}

// Limits of the history.
const (
	keepMinutes = 24 * 60
	keepHours   = 30 * 24
	keepDays    = 366
)

// History keeps the speeds.
type History struct {
	path string
	mu   sync.Mutex
	h    struct {
		Minutes []Point `json:"minutes"`
		Hours   []Point `json:"hours"`
		Days    []Day   `json:"days"`
	}
	// The hour being summed up.
	hourStart        time.Time
	hourDown, hourUp int64
	hourSecs         float64
}

func openHistory(path string) *History {
	h := &History{path: path}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &h.h)
	}
	return h
}

// add records down and up bytes transferred over the secs before at.
func (h *History) add(at time.Time, down, up int64, secs float64) {
	if secs <= 0 || down < 0 || up < 0 {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	start := at.Add(-time.Duration(secs * float64(time.Second))).Truncate(time.Minute)
	h.h.Minutes = append(h.h.Minutes, Point{T: start, Down: float64(down) / secs, Up: float64(up) / secs})
	if len(h.h.Minutes) > keepMinutes {
		h.h.Minutes = h.h.Minutes[len(h.h.Minutes)-keepMinutes:]
	}

	hour := start.Truncate(time.Hour)
	if !hour.Equal(h.hourStart) {
		h.closeHourLocked()
		h.hourStart = hour
	}
	h.hourDown += down
	h.hourUp += up
	h.hourSecs += secs

	day := at.Local().Format(time.DateOnly)
	if n := len(h.h.Days); n == 0 || h.h.Days[n-1].Day != day {
		h.h.Days = append(h.h.Days, Day{Day: day})
		if len(h.h.Days) > keepDays {
			h.h.Days = h.h.Days[len(h.h.Days)-keepDays:]
		}
	}
	d := &h.h.Days[len(h.h.Days)-1]
	d.Down += down
	d.Up += up
}

// closeHourLocked stores the hour being summed up.
func (h *History) closeHourLocked() {
	if h.hourStart.IsZero() || h.hourSecs <= 0 {
		return
	}
	p := Point{T: h.hourStart, Down: float64(h.hourDown) / h.hourSecs, Up: float64(h.hourUp) / h.hourSecs}
	if n := len(h.h.Hours); n > 0 && h.h.Hours[n-1].T.Equal(p.T) {
		h.h.Hours[n-1] = p // the same hour again (after a restart)
	} else {
		h.h.Hours = append(h.h.Hours, p)
	}
	if len(h.h.Hours) > keepHours {
		h.h.Hours = h.h.Hours[len(h.h.Hours)-keepHours:]
	}
	h.hourDown, h.hourUp, h.hourSecs = 0, 0, 0
}

// Snapshot returns the history: minutes, hours (with the current one) and
// days.
func (h *History) Snapshot() (minutes, hours []Point, days []Day) {
	h.mu.Lock()
	defer h.mu.Unlock()
	minutes = append([]Point{}, h.h.Minutes...)
	hours = append([]Point{}, h.h.Hours...)
	if h.hourSecs > 0 {
		hours = append(hours, Point{T: h.hourStart, Down: float64(h.hourDown) / h.hourSecs, Up: float64(h.hourUp) / h.hourSecs})
	}
	days = append([]Day{}, h.h.Days...)
	return
}

func (h *History) save() error {
	h.mu.Lock()
	b, err := json.Marshal(h.h)
	h.mu.Unlock()
	if err != nil || h.path == "" {
		return err
	}
	return fsutil.WriteFile(h.path, b, 0o600)
}
