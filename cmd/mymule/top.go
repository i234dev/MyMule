package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"mymule/internal/api"
)

// ANSI styles.
const (
	sReset   = "\x1b[0m"
	sBold    = "\x1b[1m"
	sDim     = "\x1b[2m"
	sReverse = "\x1b[7m"
	sGreen   = "\x1b[32m"
	sYellow  = "\x1b[33m"
	sCyan    = "\x1b[36m"
	sRed     = "\x1b[31m"
)

// seg is a piece of text with a style. Lines are built from segments so
// they can be cut to the terminal width without breaking escape codes.
type seg struct{ text, style string }

// fit renders segments, cutting the visible text at width runes and
// padding with spaces when pad is set.
func fit(segs []seg, width int, pad bool) string {
	var b strings.Builder
	left := width
	for _, s := range segs {
		if left <= 0 {
			break
		}
		t := s.text
		if n := utf8.RuneCountInString(t); n > left {
			t = truncRunes(t, left)
		}
		left -= utf8.RuneCountInString(t)
		if s.style != "" {
			b.WriteString(s.style + t + sReset)
		} else {
			b.WriteString(t)
		}
	}
	if pad && left > 0 {
		last := ""
		if len(segs) > 0 {
			last = segs[len(segs)-1].style
		}
		b.WriteString(last + strings.Repeat(" ", left) + sReset)
	}
	return b.String()
}

func truncRunes(s string, n int) string {
	if n <= 0 {
		return ""
	}
	i := 0
	for pos := range s {
		if i == n {
			return s[:pos]
		}
		i++
	}
	return s
}

// padRight / padLeft pad or cut s to exactly n runes.
func padRight(s string, n int) string {
	c := utf8.RuneCountInString(s)
	if c > n {
		if n <= 1 {
			return truncRunes(s, n)
		}
		return truncRunes(s, n-1) + "…"
	}
	return s + strings.Repeat(" ", n-c)
}

func padLeft(s string, n int) string {
	c := utf8.RuneCountInString(s)
	if c >= n {
		return truncRunes(s, n)
	}
	return strings.Repeat(" ", n-c) + s
}

func bar(frac float64, width int, style string) []seg {
	frac = max(0, min(1, frac))
	full := int(frac*float64(width) + 0.5)
	return []seg{
		{strings.Repeat("█", full), style},
		{strings.Repeat("░", width-full), sDim},
	}
}

func fmtETA(d time.Duration) string {
	switch {
	case d <= 0:
		return ""
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	case d < 100*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func speed(bps float64) string { return humanSize(uint64(bps)) + "/s" }

// renderTop builds the screen for one frame. It returns at most h lines,
// none wider than w visible characters.
func renderTop(st *api.Status, fetchErr error, w, h int, now time.Time) []string {
	w = max(w, 20)
	h = max(h, 5)
	var lines []string
	add := func(segs ...seg) { lines = append(lines, fit(segs, w, false)) }

	// Header.
	var down, upSpeed float64
	srv := "not connected"
	if st != nil {
		for _, f := range st.Files {
			down += f.Speed
		}
		if st.Upload != nil {
			upSpeed = st.Upload.Speed
		}
		if st.Server.Connected {
			id := "LowID"
			if st.Server.HighID {
				id = "HighID"
			}
			srv = fmt.Sprintf("%s (%s) %s", st.Server.Name, st.Server.Addr, id)
		}
	}
	header := fmt.Sprintf(" MyMule %s │ %s │ ↓ %s  ↑ %s ", version, srv, speed(down), speed(upSpeed))
	clock := now.Format("15:04:05") + " "
	gap := w - utf8.RuneCountInString(header) - utf8.RuneCountInString(clock)
	if gap > 0 {
		header += strings.Repeat(" ", gap) + clock
	}
	lines = append(lines, fit([]seg{{header, sReverse + sBold}}, w, true))

	if fetchErr != nil {
		add()
		add(seg{"  " + fetchErr.Error(), sRed})
		add(seg{"  Start the daemon with 'mymule start'. Retrying every second...", sDim})
		return finishFrame(lines, w, h)
	}

	// Column widths depend on the terminal width.
	wide := w >= 100
	barW := 20
	if w < 80 {
		barW = 10
	}

	// Downloads.
	var dlLines []string
	active := 0
	for _, f := range st.Files {
		if !f.Finished {
			active++
		}
	}
	add()
	add(seg{fmt.Sprintf(" DOWNLOADS (%d active, %d total)", active, len(st.Files)), sBold + sCyan})
	if len(st.Files) == 0 {
		dlLines = append(dlLines, fit([]seg{{`  No downloads. Add one with: mymule add "ed2k://|file|...|/"`, sDim}}, w, false))
	}
	for _, f := range st.Files {
		frac := float64(f.Done) / float64(max(f.Size, 1))
		style := sGreen
		state := fmt.Sprintf("src %d ↓%d q%d", f.Sources, f.Downloading, f.Queued)
		if f.BestRank > 0 {
			state += fmt.Sprintf(" #%d", f.BestRank)
		}
		eta := ""
		switch {
		case f.Finished:
			state = "done"
		case f.Paused:
			state, style = "paused", sYellow
		case f.Speed > 512:
			eta = fmtETA(time.Duration(float64(f.Size-f.Done)/f.Speed) * time.Second)
		}
		segs := []seg{{"  ", ""}}
		segs = append(segs, bar(frac, barW, style)...)
		segs = append(segs, seg{padLeft(fmt.Sprintf("%.1f%%", 100*frac), 7) + " ", sBold})
		if wide {
			segs = append(segs, seg{padLeft(humanSize(uint64(f.Size)), 10) + " ", sDim})
		}
		spStyle := ""
		if f.Speed > 0 {
			spStyle = sGreen
		}
		segs = append(segs, seg{padLeft(speed(f.Speed), 12) + "  ", spStyle})
		segs = append(segs, seg{padRight(state, 18) + " ", ""})
		if wide {
			segs = append(segs, seg{padLeft(eta, 7) + "  ", sDim})
		}
		used := 0
		for _, s := range segs {
			used += utf8.RuneCountInString(s.text)
		}
		segs = append(segs, seg{padRight(f.Name, max(w-used, 1)), ""})
		dlLines = append(dlLines, fit(segs, w, false))
	}

	// Uploads and shared files, built first so downloads get what's left.
	var tail []string
	tadd := func(segs ...seg) { tail = append(tail, fit(segs, w, false)) }
	if st.Upload != nil {
		u := st.Upload
		limit := "no limit"
		if u.LimitKB > 0 {
			limit = fmt.Sprintf("limit %d KB/s", u.LimitKB)
		}
		tadd()
		tadd(seg{fmt.Sprintf(" UPLOADS (%d/%d slots, %d in queue, %s, %s sent)",
			len(u.Slots), u.MaxSlots, u.Queue, limit, humanSize(uint64(u.Total))), sBold + sCyan})
		if len(u.Slots) == 0 {
			tadd(seg{"  Nobody is downloading from you right now.", sDim})
		}
		for _, s := range u.Slots {
			who := s.Name
			if who == "" {
				who = s.Addr
			}
			segs := []seg{
				{"  ↑ ", sGreen},
				{padLeft(speed(s.Speed), 12) + "  ", sGreen},
				{padRight(who, 22) + " ", ""},
				{padLeft(humanSize(uint64(s.Sent)), 10) + "  ", sDim},
			}
			used := 0
			for _, x := range segs {
				used += utf8.RuneCountInString(x.text)
			}
			segs = append(segs, seg{padRight(s.File, max(w-used, 1)), ""})
			tail = append(tail, fit(segs, w, false))
		}
	}
	if st.Shared != nil {
		s := st.Shared
		text := fmt.Sprintf(" SHARED %d files (%s)", s.Files, humanSize(uint64(s.Bytes)))
		if s.Partial > 0 {
			text += fmt.Sprintf(", %d partial", s.Partial)
		}
		tadd()
		segs := []seg{{text, sBold + sCyan}}
		if s.Hashing != "" {
			segs = append(segs, seg{fmt.Sprintf("  hashing %s %d%% (%d left)", s.Hashing, s.HashedPct, s.ToHash), sYellow})
		}
		tadd(segs...)
	}

	// Fit: header + downloads + tail + footer must be <= h.
	room := h - len(lines) - len(tail) - 2 // footer and its spacer
	if room < len(dlLines) {
		if room < 1 {
			room = 1
		}
		hidden := len(dlLines) - room + 1
		dlLines = append(dlLines[:room-1:room-1], fit([]seg{{fmt.Sprintf("  … and %d more (enlarge the window)", hidden), sDim}}, w, false))
		if room == 1 {
			dlLines = dlLines[len(dlLines)-1:]
		}
	}
	lines = append(lines, dlLines...)
	lines = append(lines, tail...)
	return finishFrame(lines, w, h)
}

// finishFrame pads the frame to h lines with the footer at the bottom.
func finishFrame(lines []string, w, h int) []string {
	footer := fit([]seg{{" q", sBold}, {" quit   ", sDim}, {"Ctrl+C", sBold}, {" quit   ", sDim},
		{"mymule add/pause/cancel", sBold}, {" work from another terminal", sDim}}, w, false)
	if len(lines) > h-1 {
		lines = lines[:h-1]
	}
	for len(lines) < h-1 {
		lines = append(lines, "")
	}
	return append(lines, footer)
}

// cmdTop shows a live, full-screen view of the daemon.
func cmdTop(args []string) error {
	fs := flag.NewFlagSet("top", flag.ExitOnError)
	interval := fs.Duration("i", time.Second, "refresh interval")
	once := fs.Bool("once", false, "print a single frame and exit")
	fs.Parse(args)

	if *once {
		w, h := termSize()
		c, err := daemonClient()
		var st *api.Status
		if err == nil {
			var s api.Status
			if s, err = c.Status(); err == nil {
				st = &s
			}
		}
		fmt.Println(strings.Join(renderTop(st, err, w, h, time.Now()), "\n"))
		return nil
	}

	restore, ok := termSetup()
	if !ok {
		// Not a terminal (redirected output): print once.
		c, err := mustDaemon()
		if err != nil {
			return err
		}
		return statusOnce(c)
	}
	defer restore()

	out := os.Stdout
	out.WriteString("\x1b[?1049h\x1b[?25l") // alternate screen, hide cursor
	defer out.WriteString("\x1b[?25h\x1b[?1049l")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		b := make([]byte, 1)
		for {
			if n, err := os.Stdin.Read(b); err != nil || n == 0 {
				return
			}
			if b[0] == 'q' || b[0] == 'Q' || b[0] == 3 {
				stop()
				return
			}
		}
	}()

	var (
		client  *api.Client
		st      *api.Status
		err     error
		lastW   int
		lastH   int
		fetched time.Time
	)
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		w, h := termSize()
		resized := w != lastW || h != lastH
		if time.Since(fetched) >= *interval {
			fetched = time.Now()
			if client == nil {
				client, err = daemonClient()
			}
			if client != nil {
				var s api.Status
				if s, err = client.Status(); err == nil {
					st = &s
				} else {
					client = nil // daemon went away; reconnect next time
				}
			}
			if err != nil {
				st = nil
			}
			resized = true
		}
		if resized {
			lastW, lastH = w, h
			frame := renderTop(st, err, w, h, time.Now())
			var b strings.Builder
			b.WriteString("\x1b[H")
			for i, l := range frame {
				b.WriteString(l)
				b.WriteString("\x1b[K")
				if i < len(frame)-1 {
					b.WriteString("\r\n")
				}
			}
			b.WriteString("\x1b[J")
			out.WriteString(b.String()) // one write per frame: no flicker
		}
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
		}
	}
}
