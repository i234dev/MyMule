package main

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mymule/internal/ed2k"
)

func TestDownloadsMode(t *testing.T) {
	st := sampleStatus(5) // file 0 finished, file 1 paused, others active
	u := newSearchUI()
	u.setFiles(st.Files)

	typeText(u, "d")
	if u.mode != modeDownloads || u.dlSel != st.Files[0].Hash {
		t.Fatalf("d should open the downloads mode on the first file (mode %v)", u.mode)
	}

	// p on a finished download does nothing but explain.
	typeText(u, "p")
	if u.wantAction != nil || !strings.Contains(u.msg, "already finished") {
		t.Fatalf("pause finished: %+v %q", u.wantAction, u.msg)
	}

	// Down to the paused one: p resumes it.
	typeText(u, "\x1b[Bp")
	if a := u.wantAction; a == nil || a.kind != "resume" || a.name != st.Files[1].Name {
		t.Fatalf("resume: %+v", a)
	}
	u.actionDone(u.wantAction, 0, nil)
	u.wantAction = nil

	// Down to an active one: p pauses it.
	typeText(u, "\x1b[Bp")
	if a := u.wantAction; a == nil || a.kind != "pause" || a.hash != st.Files[2].Hash.String() {
		t.Fatalf("pause: %+v", a)
	}
	u.wantAction = nil

	// c asks first; anything but y keeps the download.
	typeText(u, "c")
	if u.confirm == nil || u.confirm.kind != "cancel" || u.wantAction != nil {
		t.Fatalf("cancel must ask first: %+v %+v", u.confirm, u.wantAction)
	}
	typeText(u, "n")
	if u.confirm != nil || u.wantAction != nil {
		t.Fatal("n must not cancel")
	}
	typeText(u, "\x1b[3~y") // Del, then confirm
	if a := u.wantAction; a == nil || a.kind != "cancel" || a.hash != st.Files[2].Hash.String() {
		t.Fatalf("cancel after y: %+v", a)
	}
	u.wantAction = nil

	// A finished download is only removed from the list.
	typeText(u, "\x1b[Hc")
	if u.confirm == nil || u.confirm.kind != "remove" {
		t.Fatalf("finished download should be 'remove': %+v", u.confirm)
	}
	typeText(u, "y")
	u.wantAction = nil

	typeText(u, "x")
	if a := u.wantAction; a == nil || a.kind != "clear" {
		t.Fatalf("clear: %+v", a)
	}
	u.wantAction = nil

	// The selection follows the file, not the row, across refreshes.
	typeText(u, "\x1b[B\x1b[B\x1b[B")
	sel := u.dlSel
	u.setFiles(append(st.Files[1:2:2], st.Files[2:]...)) // first file disappeared
	if u.dlSel != sel {
		t.Fatal("selection jumped to another file after a refresh")
	}

	typeText(u, "\x1b")
	if u.mode != modeView {
		t.Fatal("Esc should go back to the panel")
	}
}

func TestRenderDownloadsMode(t *testing.T) {
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	for _, size := range [][2]int{{140, 40}, {80, 24}, {50, 10}} {
		w, h := size[0], size[1]
		u := newSearchUI()
		st := sampleStatus(50)
		u.setFiles(st.Files)
		typeTextH(u, "d\x1b[6~\x1b[6~c", h)
		frame := u.render(st, nil, w, h, now)
		if len(frame) != h {
			t.Fatalf("%dx%d: %d lines", w, h, len(frame))
		}
		for i, l := range frame {
			if c := utf8.RuneCountInString(visible(l)); c > w {
				t.Fatalf("%dx%d: line %d is %d wide: %q", w, h, i, c, visible(l))
			}
		}
		all := visible(strings.Join(frame, "\n"))
		if !strings.Contains(all, "▶") {
			t.Fatalf("%dx%d: selected row not visible:\n%s", w, h, all)
		}
		if w >= 80 && !strings.Contains(all, "DELETE") {
			t.Fatalf("%dx%d: confirmation not shown:\n%s", w, h, all)
		}
	}
}

func TestDemoActions(t *testing.T) {
	d := newDemoBackend()
	st := demoStatus(30 * time.Second)
	target := st.Files[0].Hash
	d.action(dlAction{kind: "pause", hash: target.String()})
	got := d.apply(demoStatus(30 * time.Second))
	if !got.Files[0].Paused || got.Files[0].Speed != 0 {
		t.Fatal("demo pause not applied")
	}
	d.action(dlAction{kind: "cancel", hash: target.String()})
	for _, f := range d.apply(demoStatus(30 * time.Second)).Files {
		if f.Hash == target {
			t.Fatal("demo cancel not applied")
		}
	}
	n, _ := d.action(dlAction{kind: "clear"})
	if n == 0 {
		t.Fatal("demo clear removed nothing")
	}
	for _, f := range d.apply(demoStatus(30 * time.Second)).Files {
		if f.Finished {
			t.Fatal("finished demo download still listed after clear")
		}
	}
	_ = ed2k.Hash{}
}
