package main

import (
	"math"
	"time"

	"mymule/internal/api"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/share"
	"mymule/internal/upload"
)

// demoFile describes one fake download for 'top -demo'.
type demoFile struct {
	name     string
	size     int64
	start    float64 // initial fraction done
	speed    float64 // average bytes/s
	sources  int
	dl, q    int
	rank     int
	paused   bool
	finished bool
}

var demoFiles = []demoFile{
	{name: "Example Linux Distro 24.1 amd64 DVD (demo).iso", size: 4_500_000_000, start: 0.42, speed: 1_250_000, sources: 38, dl: 6, q: 11, rank: 3},
	{name: "Demo BSD Release 14.2 amd64 (example).iso", size: 2_800_000_000, start: 0.71, speed: 820_000, sources: 24, dl: 4, q: 7, rank: 9},
	{name: "Example Open Weather Dataset 2025 (CC-BY, demo).tar.gz", size: 610_000_000, start: 0.18, speed: 410_000, sources: 12, dl: 2, q: 5, rank: 21},
	{name: "Demo Handbook - Example Edition.pdf", size: 26_800_000, start: 1, finished: true},
	{name: "Example Podcast Episode 042 (CC-BY, demo).mp3", size: 87_000_000, start: 0.93, speed: 95_000, sources: 3, dl: 1, q: 1, rank: 2},
	{name: "Demo Public Domain Books Collection (example).zip", size: 12_400_000_000, start: 0.07, sources: 9, q: 9, rank: 47},
	{name: "Example Single-Board Computer OS Image (demo).img.xz", size: 1_300_000_000, start: 0.55, sources: 6, paused: true},
}

var demoPeers = []string{"demo-peer-amsterdam", "demo-peer-lisboa", "demo-peer-tokyo", "demo-peer-quebec", "demo-peer-berlin"}

// demoStatus builds a fake but lively status for screenshots; t is the
// time since the demo started.
func demoStatus(t time.Duration) *api.Status {
	sec := t.Seconds()
	wobble := func(phase, amount float64) float64 { return 1 + amount*math.Sin(sec/3+phase) }

	st := &api.Status{
		Server: download.ServerStatus{Connected: true, Name: "Demo Server", Addr: "192.0.2.10:4661", HighID: true},
	}
	for i, d := range demoFiles {
		f := download.FileStatus{
			Hash: ed2k.Hash{0xDE, 0x30, byte(i)}, Name: d.name, Size: d.size,
			Sources: d.sources, Downloading: d.dl, Queued: d.q, BestRank: d.rank,
			Paused: d.paused, Finished: d.finished,
		}
		frac := d.start
		if d.speed > 0 && !d.paused && !d.finished {
			f.Speed = d.speed * wobble(float64(i), 0.25)
			frac += d.speed * sec / float64(d.size)
			if frac >= 1 {
				frac = 1
				f.Speed, f.Finished = 0, true
			}
		}
		f.Done = int64(frac * float64(d.size))
		st.Files = append(st.Files, f)
	}

	// Two or three peers downloading from us, rotating every 20 seconds.
	turn := int(sec / 20)
	up := &upload.Status{MaxSlots: 3, Queue: 14 + turn%5, LimitKB: 100, Total: int64(1_850_000_000 + sec*90_000)}
	for k := 0; k < 2+turn%2; k++ {
		sp := 30_000 * wobble(float64(k)*2, 0.3)
		up.Slots = append(up.Slots, upload.SlotStatus{
			Name:  demoPeers[(turn+k)%len(demoPeers)],
			Addr:  "198.51.100." + itoa3(20+k),
			File:  demoFiles[(turn+k)%4].name,
			Sent:  int64(math.Mod(sec, 20)*sp) + int64(k)*1_200_000,
			Speed: sp,
		})
		up.Speed += sp
	}
	st.Upload = up

	shared := &share.Status{Files: 27, Partial: 6, Bytes: 48_300_000_000}
	if pct := int(math.Mod(sec*4, 100)); sec < 60 {
		shared.Hashing, shared.HashedPct, shared.ToHash = "Example Project Source Code v3.2 (demo).tar.xz", pct, 2
	}
	st.Shared = shared
	return st
}

func itoa3(n int) string {
	if n <= 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}
