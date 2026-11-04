package main

import (
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"mymule/internal/api"
	"mymule/internal/ed2k"
)

// Interactive search inside 'mymule top'.
//
//	view    --/ or s-->  input  --Enter-->  results  --Enter--> add downloads
//	view   <----Esc----  input  <---Esc---  results
//
// Key handling only changes state and records requests (wantSearch,
// wantAdd); the top loop runs them in the background and reports back
// with searchDone/addDone, so the screen never freezes.

type uiMode int

const (
	modeView uiMode = iota
	modeInput
	modeResults
)

var searchTypes = []string{"", "video", "audio", "iso", "doc", "archive", "program", "image"}

func typeLabel(i int) string {
	if searchTypes[i] == "" {
		return "any"
	}
	return searchTypes[i]
}

const maxShownResults = 300

type searchUI struct {
	mode    uiMode
	input   []rune
	typeIdx int
	global  bool

	query     string
	results   []api.SearchResult
	cursor    int
	offset    int
	selected  map[ed2k.Hash]bool
	added     map[ed2k.Hash]bool
	searching bool
	searchID  int

	msg      string
	msgStyle string
	quit     bool

	wantSearch *api.SearchRequest
	wantAdd    []api.SearchResult
}

func newSearchUI() *searchUI {
	return &searchUI{global: true, selected: map[ed2k.Hash]bool{}, added: map[ed2k.Hash]bool{}}
}

// pageSize is how many result rows fit on a screen of height h.
func pageSize(h int) int { return max(h-6, 1) }

func (u *searchUI) handleKey(k key, h int) {
	if k.kind == keyCtrlC {
		u.quit = true
		return
	}
	switch u.mode {
	case modeView:
		if k.kind != keyRune {
			return
		}
		switch k.r {
		case 'q', 'Q':
			u.quit = true
		case '/', 's', 'S':
			u.mode, u.input = modeInput, []rune(u.query)
		case 'r', 'R':
			if u.query != "" {
				u.mode = modeResults
			}
		}

	case modeInput:
		switch k.kind {
		case keyEsc:
			if len(u.results) > 0 {
				u.mode = modeResults
			} else {
				u.mode = modeView
			}
		case keyEnter:
			if q := strings.TrimSpace(string(u.input)); q != "" {
				u.startSearch(q)
			}
		case keyBackspace:
			if len(u.input) > 0 {
				u.input = u.input[:len(u.input)-1]
			}
		case keyCtrlU:
			u.input = nil
		case keyTab:
			u.typeIdx = (u.typeIdx + 1) % len(searchTypes)
		case keyCtrlG:
			u.global = !u.global
		case keyRune:
			if len(u.input) < 120 {
				u.input = append(u.input, k.r)
			}
		}

	case modeResults:
		n, page := len(u.results), pageSize(h)
		switch k.kind {
		case keyEsc:
			u.mode = modeView
		case keyUp:
			u.cursor--
		case keyDown:
			u.cursor++
		case keyPgUp:
			u.cursor -= page
		case keyPgDn:
			u.cursor += page
		case keyHome:
			u.cursor = 0
		case keyEnd:
			u.cursor = n - 1
		case keyTab:
			u.typeIdx = (u.typeIdx + 1) % len(searchTypes)
			u.rerun()
		case keyEnter:
			u.requestAdd()
		case keyRune:
			switch k.r {
			case ' ':
				if n > 0 && !u.added[u.results[u.cursor].Hash] {
					h := u.results[u.cursor].Hash
					u.selected[h] = !u.selected[h]
					if !u.selected[h] {
						delete(u.selected, h)
					}
					u.cursor++
				}
			case '/', 's', 'S':
				u.mode, u.input = modeInput, []rune(u.query)
			case 'g', 'G':
				u.global = !u.global
				u.rerun()
			case 't', 'T':
				u.typeIdx = (u.typeIdx + 1) % len(searchTypes)
				u.rerun()
			case 'q', 'Q':
				u.mode = modeView
			}
		}
		u.cursor = max(0, min(u.cursor, n-1))
		if u.cursor < u.offset {
			u.offset = u.cursor
		}
		if u.cursor >= u.offset+page {
			u.offset = u.cursor - page + 1
		}
	}
}

func (u *searchUI) startSearch(q string) {
	u.query = q
	u.searching = true
	u.searchID++
	u.results, u.cursor, u.offset = nil, 0, 0
	u.selected = map[ed2k.Hash]bool{}
	u.mode = modeResults
	u.msg = ""
	u.wantSearch = &api.SearchRequest{Query: q, Type: searchTypes[u.typeIdx], Global: u.global}
}

func (u *searchUI) rerun() {
	if u.query != "" {
		u.startSearch(u.query)
	}
}

// requestAdd queues the selected results, or the one under the cursor.
func (u *searchUI) requestAdd() {
	var pick []api.SearchResult
	for _, r := range u.results {
		if u.selected[r.Hash] && !u.added[r.Hash] {
			pick = append(pick, r)
		}
	}
	if len(pick) == 0 && len(u.results) > 0 {
		r := u.results[u.cursor]
		if u.added[r.Hash] {
			u.msg, u.msgStyle = "Already added: "+r.Name, sYellow
			return
		}
		pick = []api.SearchResult{r}
	}
	if len(pick) > 0 {
		u.wantAdd = pick
		u.msg, u.msgStyle = fmt.Sprintf("Adding %d download(s)...", len(pick)), sYellow
	}
}

func (u *searchUI) searchDone(id int, res []api.SearchResult, err error) {
	if id != u.searchID {
		return // an older search finished after a newer one started
	}
	u.searching = false
	if err != nil {
		u.msg, u.msgStyle = "Search failed: "+err.Error(), sRed
		return
	}
	if len(res) > maxShownResults {
		res = res[:maxShownResults]
	}
	u.results = res
	if len(res) == 0 {
		u.msg, u.msgStyle = "No results. Press / to search again, Tab to change the type.", sYellow
	}
}

func (u *searchUI) addDone(items []api.SearchResult, resp api.AddResponse, err error) {
	if err != nil {
		u.msg, u.msgStyle = "Could not add: "+err.Error(), sRed
		return
	}
	ok := map[string]bool{}
	for _, n := range resp.Added {
		ok[n] = true
	}
	for _, it := range items {
		if ok[it.Name] {
			u.added[it.Hash] = true
			delete(u.selected, it.Hash)
		}
	}
	switch {
	case len(resp.Errors) > 0:
		u.msg, u.msgStyle = "Not added: "+strings.Join(resp.Errors, "; "), sRed
	case len(resp.Added) == 1:
		u.msg, u.msgStyle = "✓ Downloading "+resp.Added[0], sGreen
	default:
		u.msg, u.msgStyle = fmt.Sprintf("✓ Downloading %d files", len(resp.Added)), sGreen
	}
}

// ---- rendering ----

func (u *searchUI) render(st *api.Status, fetchErr error, w, h int, now time.Time) []string {
	w, h = max(w, 20), max(h, 5)
	var frame []string
	if u.mode == modeResults || (u.mode == modeInput && (len(u.results) > 0 || u.searching)) {
		frame = u.renderResults(st, w, h, now)
	} else {
		frame = renderTop(st, fetchErr, w, h, now)
		frame[len(frame)-1] = fit([]seg{{" /", sBold}, {" search   ", sDim}, {"r", sBold}, {" last results   ", sDim},
			{"q", sBold}, {" quit   ", sDim}, {"Ctrl+C", sBold}, {" quit", sDim}}, w, false)
	}
	if u.mode == modeInput {
		flags := "type: " + typeLabel(u.typeIdx)
		if u.global {
			flags += ", all servers"
		} else {
			flags += ", current server"
		}
		frame[len(frame)-2] = fit([]seg{{" Enter", sBold}, {" search   ", sDim}, {"Tab", sBold}, {" type   ", sDim},
			{"Ctrl+G", sBold}, {" all servers on/off   ", sDim}, {"Ctrl+U", sBold}, {" clear   ", sDim},
			{"Esc", sBold}, {" cancel", sDim}}, w, false)
		prompt := fmt.Sprintf(" Search (%s): ", flags)
		text := string(u.input)
		// Keep the end of long input visible.
		if room := w - utf8.RuneCountInString(prompt) - 2; utf8.RuneCountInString(text) > room && room > 0 {
			r := []rune(text)
			text = "…" + string(r[len(r)-room+1:])
		}
		frame[len(frame)-1] = fit([]seg{{prompt, sBold + sCyan}, {text, ""}, {"█", sCyan}}, w, false)
	}
	return frame
}

func (u *searchUI) renderResults(st *api.Status, w, h int, now time.Time) []string {
	lines := []string{headerLine(st, w, now), ""}
	add := func(segs ...seg) { lines = append(lines, fit(segs, w, false)) }

	scope := "current server"
	if u.global {
		scope = "all servers"
	}
	info, infoStyle := fmt.Sprintf("   %d results", len(u.results)), sDim
	switch {
	case u.searching && u.global:
		info, infoStyle = "   searching all servers…", sYellow
	case u.searching:
		info, infoStyle = "   searching…", sYellow
	case len(u.selected) > 0:
		info += fmt.Sprintf(", %d selected", len(u.selected))
	}
	// The result count matters more than the full query: shorten the
	// query to whatever room is left.
	rest := fmt.Sprintf("\"  ·  type: %s  ·  %s", typeLabel(u.typeIdx), scope)
	room := w - utf8.RuneCountInString(" SEARCH \""+rest+info)
	q := padRight(u.query, max(min(utf8.RuneCountInString(u.query), room), 6))
	add(seg{" SEARCH \"" + q + rest, sBold + sCyan}, seg{info, infoStyle})
	add(seg{"         #  SRCS  FULL        SIZE  NAME", sDim})

	page := pageSize(h)
	end := min(u.offset+page, len(u.results))
	for i := u.offset; i < end; i++ {
		r := u.results[i]
		mark, markStyle := "[ ]", sDim
		switch {
		case u.added[r.Hash]:
			mark, markStyle = " ✓ ", sGreen
		case u.selected[r.Hash]:
			mark, markStyle = "[x]", sYellow
		}
		pointer, rowStyle := "  ", ""
		if i == u.cursor {
			pointer, rowStyle = "▶ ", sReverse
		}
		nameStyle := ""
		if u.added[r.Hash] {
			nameStyle = sGreen
		}
		segs := []seg{
			{pointer, rowStyle + sCyan},
			{mark + " ", rowStyle + markStyle},
			{fmt.Sprintf("%3d %5d %5d  %10s  ", i+1, r.Sources, r.Complete, humanSize(r.Size)), rowStyle},
			{r.Name, rowStyle + nameStyle},
		}
		lines = append(lines, fit(segs, w, i == u.cursor))
	}
	if !u.searching && len(u.results) > page {
		add(seg{fmt.Sprintf("  showing %d-%d of %d  (↑↓ PgUp PgDn)", u.offset+1, end, len(u.results)), sDim})
	}

	for len(lines) < h-2 {
		lines = append(lines, "")
	}
	lines = lines[:h-2]
	lines = append(lines, fit([]seg{{" " + u.msg, u.msgStyle}}, w, false))
	lines = append(lines, fit([]seg{{" ↑↓", sBold}, {" move  ", sDim}, {"Space", sBold}, {" select  ", sDim},
		{"Enter", sBold}, {" download  ", sDim}, {"/", sBold}, {" new search  ", sDim}, {"Tab", sBold}, {" type  ", sDim},
		{"g", sBold}, {" all servers  ", sDim}, {"Esc", sBold}, {" back", sDim}}, w, false))
	return lines
}

// ---- backends ----

// searchBackend runs searches and adds downloads for the UI.
type searchBackend interface {
	search(req api.SearchRequest) ([]api.SearchResult, error)
	add(items []api.SearchResult) (api.AddResponse, error)
}

type daemonBackend struct{}

func (daemonBackend) search(req api.SearchRequest) ([]api.SearchResult, error) {
	c, err := mustDaemon()
	if err != nil {
		return nil, err
	}
	r, err := c.Search(req)
	return r.Results, err
}

func (daemonBackend) add(items []api.SearchResult) (api.AddResponse, error) {
	c, err := mustDaemon()
	if err != nil {
		return api.AddResponse{}, err
	}
	links := make([]string, len(items))
	for i, it := range items {
		links[i] = it.Link
	}
	return c.Add(api.AddRequest{Links: links})
}

// demoBackend invents results for screenshots; nothing touches the network.
type demoBackend struct{}

func (demoBackend) search(req api.SearchRequest) ([]api.SearchResult, error) {
	time.Sleep(700 * time.Millisecond)
	topic := strings.TrimSpace(req.Query)
	if topic == "" {
		topic = "Sample"
	}
	kinds := []struct{ label, ext string }{
		{"amd64 DVD (demo)", "iso"}, {"Handbook (example)", "pdf"}, {"Source Code (demo)", "tar.xz"},
		{"Open Dataset (CC-BY, demo)", "zip"}, {"Podcast (CC-BY, demo)", "mp3"}, {"arm64 Image (demo)", "img.xz"},
		{"Documentation (example)", "epub"}, {"Installer (demo)", "msi"},
	}
	var out []api.SearchResult
	for i := 0; i < 42; i++ {
		k := kinds[i%len(kinds)]
		if req.Type == "iso" && k.ext != "iso" && k.ext != "img.xz" {
			continue
		}
		name := fmt.Sprintf("Example %s %d.%d %s.%s", topic, 1+i/len(kinds), i%len(kinds), k.label, k.ext)
		srcs := uint32(max(140-i*3, 1))
		out = append(out, api.SearchResult{
			Hash: ed2k.Hash{0xDE, 0x40, byte(i)}, Name: name, Size: uint64(30_000_000 + i*97_000_000),
			Sources: srcs, Complete: srcs * 9 / 10, Link: "ed2k://|file|demo|1|00000000000000000000000000000000|/",
		})
	}
	return out, nil
}

func (demoBackend) add(items []api.SearchResult) (api.AddResponse, error) {
	time.Sleep(200 * time.Millisecond)
	var r api.AddResponse
	for _, it := range items {
		r.Added = append(r.Added, it.Name)
	}
	return r, nil
}
