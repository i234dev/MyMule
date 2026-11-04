package main

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mymule/internal/api"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/share"
	"mymule/internal/upload"
)

var ansi = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)

func visible(s string) string { return ansi.ReplaceAllString(s, "") }

func sampleStatus(nFiles int) *api.Status {
	st := &api.Status{
		Server: download.ServerStatus{Connected: true, Name: "eMule Sunrise", Addr: "176.123.5.89:4725", HighID: true},
		Upload: &upload.Status{MaxSlots: 3, Queue: 15, Speed: 35000, Total: 1 << 30, LimitKB: 100,
			Slots: []upload.SlotStatus{{Name: "a very long peer nickname indeed", Addr: "1.2.3.4:4662",
				File: "Some shared file with a long long name.iso", Sent: 3 << 20, Speed: 20000}}},
		Shared: &share.Status{Files: 25, Partial: 3, Bytes: 5 << 30, Hashing: "movie.mkv", HashedPct: 45, ToHash: 3},
	}
	for i := 0; i < nFiles; i++ {
		st.Files = append(st.Files, download.FileStatus{
			Hash: ed2k.Hash{byte(i), 0x77},
			Name: fmt.Sprintf("Download number %d with a name that is much too long for narrow terminals.iso", i),
			Size: 4 << 30, Done: int64(i) << 28, Speed: float64(i * 50000), Sources: 12, Downloading: 3, Queued: 5, BestRank: 14,
			Finished: i == 0, Paused: i == 1,
		})
	}
	return st
}

func TestRenderTopFitsTerminal(t *testing.T) {
	now := time.Date(2026, 11, 4, 21, 3, 15, 0, time.UTC)
	for _, size := range [][2]int{{120, 40}, {80, 24}, {60, 12}, {40, 8}} {
		w, h := size[0], size[1]
		for _, n := range []int{0, 3, 50} {
			frame := renderTop(sampleStatus(n), nil, w, h, now)
			if len(frame) != h {
				t.Fatalf("%dx%d, %d files: %d lines", w, h, n, len(frame))
			}
			for i, l := range frame {
				if c := utf8.RuneCountInString(visible(l)); c > w {
					t.Fatalf("%dx%d, %d files: line %d is %d wide: %q", w, h, n, i, c, visible(l))
				}
			}
			all := visible(strings.Join(frame, "\n"))
			if !strings.Contains(all, "DOWNLOADS") || !strings.Contains(all, "quit") {
				t.Fatalf("%dx%d: missing sections:\n%s", w, h, all)
			}
			if n == 50 && !strings.Contains(all, "more") {
				t.Fatalf("%dx%d: overflow not reported:\n%s", w, h, all)
			}
		}
	}
	// Wide terminal shows every section.
	all := visible(strings.Join(renderTop(sampleStatus(3), nil, 140, 40, now), "\n"))
	for _, want := range []string{"HighID", "UPLOADS (1/3 slots, 15 in queue", "SHARED 25 files", "hashing movie.mkv 45%", "paused", "done", "21:03:15"} {
		if !strings.Contains(all, want) {
			t.Errorf("missing %q in:\n%s", want, all)
		}
	}
}

func TestRenderTopDemo(t *testing.T) {
	for _, at := range []time.Duration{0, 30 * time.Second, 10 * time.Minute, 5 * time.Hour} {
		frame := renderTop(demoStatus(at), nil, 120, 30, time.Now())
		all := visible(strings.Join(frame, "\n"))
		for _, want := range []string{"Demo Server", "HighID", "Example Linux Distro", "demo-peer-", "UPLOADS", "SHARED"} {
			if !strings.Contains(all, want) {
				t.Fatalf("at %v: missing %q:\n%s", at, want, all)
			}
		}
		for _, l := range frame {
			if utf8.RuneCountInString(visible(l)) > 120 {
				t.Fatalf("line too wide: %q", visible(l))
			}
		}
	}
}

func TestRenderTopDaemonDown(t *testing.T) {
	frame := renderTop(nil, errors.New("the daemon is not running"), 80, 24, time.Now())
	all := visible(strings.Join(frame, "\n"))
	if !strings.Contains(all, "not running") || !strings.Contains(all, "not connected") || len(frame) != 24 {
		t.Fatalf("frame:\n%s", all)
	}
}
