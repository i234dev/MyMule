package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"mymule/internal/api"
	"mymule/internal/ed2k"
)

func TestParseKeys(t *testing.T) {
	in := []byte("a\x1b[A\x1b[B\x1b[5~\x1b[6~\x1bOH\x1b[F\r\n\x7f\x08\t\x1b" + "ñ")
	want := []keyKind{keyRune, keyUp, keyDown, keyPgUp, keyPgDn, keyHome, keyEnd, keyEnter,
		keyBackspace, keyBackspace, keyTab, keyEsc, keyRune}
	got := parseKeys(in)
	if len(got) != len(want) {
		t.Fatalf("got %d keys %+v, want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].kind != want[i] {
			t.Fatalf("key %d: got %v, want %v", i, got[i].kind, want[i])
		}
	}
	if got[0].r != 'a' || got[len(got)-1].r != 'ñ' {
		t.Fatalf("runes: %q %q", got[0].r, got[len(got)-1].r)
	}
	// A lone ESC is the Esc key; Ctrl+C, Ctrl+G and Ctrl+U are recognised.
	if k := parseKeys([]byte{0x1b}); len(k) != 1 || k[0].kind != keyEsc {
		t.Fatalf("lone esc: %+v", k)
	}
	if k := parseKeys([]byte{3, 7, 0x15}); len(k) != 3 || k[0].kind != keyCtrlC || k[1].kind != keyCtrlG || k[2].kind != keyCtrlU {
		t.Fatalf("ctrl keys: %+v", k)
	}
}

func typeText(u *searchUI, s string) { typeTextH(u, s, 30) }

func typeTextH(u *searchUI, s string, h int) {
	for _, k := range parseKeys([]byte(s)) {
		u.handleKey(k, h)
	}
}

func fakeResults(n int) []api.SearchResult {
	var out []api.SearchResult
	for i := 0; i < n; i++ {
		out = append(out, api.SearchResult{Hash: ed2k.Hash{byte(i), 1}, Name: fmt.Sprintf("result %d.iso", i),
			Size: 1 << 30, Sources: uint32(100 - i), Link: fmt.Sprintf("link%d", i)})
	}
	return out
}

func TestSearchFlow(t *testing.T) {
	u := newSearchUI()
	typeText(u, "q")
	if !u.quit {
		t.Fatal("q in the panel should quit")
	}

	u = newSearchUI()
	typeText(u, "/")
	if u.mode != modeInput {
		t.Fatal("/ should open the search prompt")
	}
	typeText(u, "debiam\x7fn netinst\t") // typo fixed with backspace; Tab: type video
	if string(u.input) != "debian netinst" || typeLabel(u.typeIdx) != "video" {
		t.Fatalf("input %q type %s", string(u.input), typeLabel(u.typeIdx))
	}
	typeText(u, "q") // inside the prompt, q is just a letter
	if u.quit || string(u.input) != "debian netinstq" {
		t.Fatal("q must be typed, not quit, while searching")
	}
	typeText(u, "\x7f\r")
	if u.mode != modeResults || !u.searching || u.wantSearch == nil ||
		u.wantSearch.Query != "debian netinst" || u.wantSearch.Type != "video" || !u.wantSearch.Global {
		t.Fatalf("search request: %+v mode %v", u.wantSearch, u.mode)
	}
	id := u.searchID
	u.wantSearch = nil

	// A stale result (older search) is ignored.
	u.searchDone(id-1, fakeResults(3), nil)
	if len(u.results) != 0 || !u.searching {
		t.Fatal("stale search result applied")
	}
	u.searchDone(id, fakeResults(60), nil)
	if u.searching || len(u.results) != 60 {
		t.Fatal("results not applied")
	}

	// Move, select two, scroll a page.
	typeText(u, "\x1b[B ") // down to #2, select it (cursor moves to #3)
	typeText(u, " ")       // select #3
	if len(u.selected) != 2 || u.cursor != 3 {
		t.Fatalf("selected %d cursor %d", len(u.selected), u.cursor)
	}
	typeText(u, "\x1b[6~\x1b[6~")
	if u.cursor < u.offset || u.cursor >= u.offset+pageSize(30) {
		t.Fatalf("cursor %d not visible (offset %d)", u.cursor, u.offset)
	}
	typeText(u, "\r")
	if len(u.wantAdd) != 2 || u.wantAdd[0].Name != "result 1.iso" || u.wantAdd[1].Name != "result 2.iso" {
		t.Fatalf("add request: %+v", u.wantAdd)
	}
	items := u.wantAdd
	u.wantAdd = nil
	u.addDone(items, api.AddResponse{Added: []string{"result 1.iso", "result 2.iso"}}, nil)
	if !u.added[items[0].Hash] || len(u.selected) != 0 || !strings.Contains(u.msg, "2 files") {
		t.Fatalf("after add: added=%v selected=%v msg=%q", u.added, u.selected, u.msg)
	}

	// Enter with nothing selected adds the row under the cursor.
	typeText(u, "\x1b[H\r")
	if len(u.wantAdd) != 1 || u.wantAdd[0].Name != "result 0.iso" {
		t.Fatalf("single add: %+v", u.wantAdd)
	}
	u.wantAdd = nil

	// g toggles all-servers and searches again; Esc goes back to the panel.
	typeText(u, "g")
	if u.global || u.wantSearch == nil || u.wantSearch.Global {
		t.Fatal("g should rerun the search on the current server only")
	}
	typeText(u, "\x1b")
	if u.mode != modeView {
		t.Fatal("Esc should return to the panel")
	}
	typeText(u, "r")
	if u.mode != modeResults {
		t.Fatal("r should reopen the last results")
	}
}

func TestRenderSearchScreens(t *testing.T) {
	now := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	for _, size := range [][2]int{{140, 40}, {80, 24}, {50, 10}} {
		w, h := size[0], size[1]
		u := newSearchUI()
		check := func(stage string) {
			frame := u.render(sampleStatus(3), nil, w, h, now)
			if len(frame) != h {
				t.Fatalf("%s %dx%d: %d lines", stage, w, h, len(frame))
			}
			for i, l := range frame {
				if c := utf8.RuneCountInString(visible(l)); c > w {
					t.Fatalf("%s %dx%d: line %d is %d wide: %q", stage, w, h, i, c, visible(l))
				}
			}
		}
		check("panel")
		typeTextH(u, "/a very long search query that will not fit in a narrow terminal at all", h)
		check("prompt")
		if !strings.Contains(visible(strings.Join(u.render(sampleStatus(3), nil, w, h, now), "\n")), "Search (") {
			t.Fatal("prompt not shown")
		}
		typeTextH(u, "\r", h)
		check("searching")
		u.searchDone(u.searchID, fakeResults(80), nil)
		typeTextH(u, "\x1b[B \x1b[6~", h)
		check("results")
		all := visible(strings.Join(u.render(sampleStatus(3), nil, w, h, now), "\n"))
		if h >= 24 && (!strings.Contains(all, "80 results") || !strings.Contains(all, "▶")) {
			t.Fatalf("results screen:\n%s", all)
		}
	}
}

func TestDemoSearch(t *testing.T) {
	res, err := newDemoBackend().search(api.SearchRequest{Query: "Linux"})
	if err != nil || len(res) < 20 || !strings.Contains(res[0].Name, "Example Linux") {
		t.Fatalf("demo results: %d %v", len(res), err)
	}
	r, _ := newDemoBackend().add(res[:2])
	if len(r.Added) != 2 {
		t.Fatalf("demo add: %+v", r)
	}
}
