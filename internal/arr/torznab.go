package arr

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mymule/internal/api"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/server"
)

// Newznab/Torznab categories (top level; apps also ask for subcategories
// such as 2040, which match by thousands).
const (
	catMovies = 2000
	catAudio  = 3000
	catPC     = 4000
	catTV     = 5000
	catBooks  = 7000
	catOther  = 8000
)

var (
	episodeRe = regexp.MustCompile(`(?i)(?:\bs(\d{1,2})[ ._-]?e(\d{1,3})\b|\b(\d{1,2})x(\d{2,3})\b)`)
	seasonRe  = regexp.MustCompile(`(?i)\bs(\d{1,2})\b|\bseason[ ._-]?(\d{1,2})\b|\btemporada[ ._-]?(\d{1,2})\b`)
)

// category guesses a Newznab category from a file name and server type.
func category(name, ftype string) int {
	switch {
	case ftype == server.TypeVideo || server.FileTypeFor(name) == server.TypeVideo:
		if episodeRe.MatchString(name) || seasonRe.MatchString(name) {
			return catTV
		}
		return catMovies
	case ftype == server.TypeAudio || server.FileTypeFor(name) == server.TypeAudio:
		return catAudio
	case server.FileTypeFor(name) == server.TypeDocument:
		return catBooks
	case server.FileTypeFor(name) == server.TypeProgram || server.FileTypeFor(name) == server.TypeCDImage:
		return catPC
	}
	return catOther
}

// matchesEpisode keeps results for the requested season/episode, written
// as S01E02, s01.e02 or 1x02. Season-only requests match S01 / Season 1.
func matchesEpisode(name string, season, ep int) bool {
	if season <= 0 {
		return true
	}
	for _, m := range episodeRe.FindAllStringSubmatch(name, -1) {
		s, e := atoi(m[1]+m[3]), atoi(m[2]+m[4])
		if s == season && (ep <= 0 || e == ep) {
			return true
		}
	}
	if ep <= 0 {
		for _, m := range seasonRe.FindAllStringSubmatch(name, -1) {
			if atoi(m[1]+m[2]+m[3]) == season {
				return true
			}
		}
	}
	return false
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func xmlEscape(s string) string {
	var b bytes.Buffer
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

func torznabError(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?>`+"\n"+`<error code="%d" description="%s"/>`+"\n", code, xmlEscape(msg))
}

// caps describes what the indexer supports. The ids are only offered
// when titles in other languages are looked up from them.
func (a *Adapter) caps() string {
	movieParams, tvParams := "q", "q,season,ep"
	if a.lookup != nil {
		movieParams, tvParams = "q,imdbid,tmdbid", "q,season,ep,imdbid,tvdbid"
	}
	return `<?xml version="1.0" encoding="UTF-8"?>
<caps>
  <server version="1.0" title="MyMule" strapline="eD2K network search" url="https://github.com/i234dev/MyMule"/>
  <limits max="100" default="50"/>
  <searching>
    <search available="yes" supportedParams="q"/>
    <tv-search available="yes" supportedParams="` + tvParams + `"/>
    <movie-search available="yes" supportedParams="` + movieParams + `"/>
    <music-search available="yes" supportedParams="q"/>
    <audio-search available="yes" supportedParams="q"/>
    <book-search available="yes" supportedParams="q"/>
  </searching>
  <categories>
    <category id="2000" name="Movies"/>
    <category id="3000" name="Audio"/>
    <category id="4000" name="PC"/>
    <category id="5000" name="TV"/>
    <category id="7000" name="Books"/>
    <category id="8000" name="Other"/>
  </categories>
</caps>
`
}

// torznab serves /torznab/api: t=caps and the search functions.
func (a *Adapter) torznab(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.REST.Authorized(r) {
		torznabError(w, http.StatusUnauthorized, 100, "Incorrect API key")
		return
	}
	q := r.URL.Query()
	switch t := q.Get("t"); t {
	case "caps":
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		fmt.Fprint(w, a.caps())
	case "search", "tvsearch", "movie", "music", "audio", "book":
		a.tzSearch(w, r, t)
	default:
		torznabError(w, http.StatusBadRequest, 202, "No such function: "+t)
	}
}

type tzItem struct {
	link     ed2k.Link
	sources  uint32
	complete uint32
	cat      int
}

func (a *Adapter) tzSearch(w http.ResponseWriter, r *http.Request, t string) {
	q := r.URL.Query()
	text := strings.TrimSpace(q.Get("q"))
	season, ep := atoi(q.Get("season")), atoi(q.Get("ep"))
	limit := atoi(q.Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	offset := max(atoi(q.Get("offset")), 0)
	wantCats := map[int]bool{}
	for _, c := range strings.Split(q.Get("cat"), ",") {
		if n := atoi(strings.TrimSpace(c)); n > 0 {
			wantCats[n/1000*1000] = true
		}
	}

	ids := idsFromQuery(q, t == "tvsearch")
	var queries []string
	if t == "movie" || t == "tvsearch" {
		queries = a.searchTitles(r.Context(), text, ids)
	} else if s := searchText(text); s != "" {
		queries = []string{s}
	}

	var items []tzItem
	if len(queries) == 0 && text == "" && ids.empty() {
		// Apps run an empty search to test the indexer and for RSS
		// syncs; eD2K has no "latest" feed, so answer with a single,
		// clearly labelled item that never matches a real release.
		items = []tzItem{{link: ed2k.Link{Name: "MyMule indexer test - search with a query to get real results",
			Size: 1024, Hash: ed2k.Hash{0x4D, 0x79, 0x4D, 0x75, 0x6C, 0x65}}, sources: 1, cat: catOther}}
	} else {
		var ftype string
		switch t {
		case "movie", "tvsearch":
			ftype = server.TypeVideo
		case "music", "audio":
			ftype = server.TypeAudio
		case "book":
			ftype = server.TypeDocument
		}
		// One eD2K search per title, one after another: global (UDP)
		// answers don't say which search they belong to.
		var results []api.SearchResult
		seen := map[ed2k.Hash]bool{}
		for i, text := range queries {
			res, err := a.search(r.Context(), server.Query{Keywords: text, Type: ftype}, true)
			if err != nil {
				if i == 0 {
					torznabError(w, http.StatusServiceUnavailable, 900, "search failed: "+err.Error())
					return
				}
				break
			}
			for _, x := range res {
				if !seen[x.Hash] {
					seen[x.Hash] = true
					results = append(results, x)
				}
			}
		}
		for _, res := range results {
			// Results nobody shares can never complete; some servers
			// also list such entries as filler. Leave them out.
			if res.Sources == 0 {
				continue
			}
			if !matchesEpisode(res.Name, season, ep) {
				continue
			}
			cat := category(res.Name, res.Type)
			if t == "tvsearch" && cat == catMovies {
				cat = catTV
			}
			if len(wantCats) > 0 && !wantCats[cat] {
				continue
			}
			items = append(items, tzItem{link: ed2k.Link{Name: res.Name, Size: int64(res.Size), Hash: res.Hash},
				sources: res.Sources, complete: res.Complete, cat: cat})
		}
	}
	if offset < len(items) {
		items = items[offset:]
	} else {
		items = nil
	}
	if len(items) > limit {
		items = items[:limit]
	}
	a.writeFeed(w, r, items)
}

func (a *Adapter) baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (a *Adapter) writeFeed(w http.ResponseWriter, r *http.Request, items []tzItem) {
	base := a.baseURL(r)
	now := time.Now().UTC().Format(time.RFC1123Z)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<rss version="2.0" xmlns:atom="http://www.w3.org/2005/Atom" xmlns:torznab="http://torznab.com/schemas/2015/feed">` + "\n")
	b.WriteString("<channel>\n<title>MyMule</title>\n<description>eD2K network search</description>\n")
	for _, it := range items {
		dl := fmt.Sprintf("%s/torznab/download/%s?name=%s&size=%d&apikey=%s", base, it.link.Hash,
			url.QueryEscape(it.link.Name), it.link.Size, url.QueryEscape(a.cfg.Key))
		peers := it.sources
		seeders := max(it.complete, min(it.sources, 1))
		fmt.Fprintf(&b, "<item>\n<title>%s</title>\n<guid isPermaLink=\"false\">%s</guid>\n", xmlEscape(releaseTitle(it.link.Name)), it.link.Hash)
		fmt.Fprintf(&b, "<link>%s</link>\n<comments>%s</comments>\n<pubDate>%s</pubDate>\n", xmlEscape(dl), xmlEscape(it.link.String()), now)
		fmt.Fprintf(&b, "<size>%d</size>\n<category>%d</category>\n", it.link.Size, it.cat)
		fmt.Fprintf(&b, "<enclosure url=\"%s\" length=\"%d\" type=\"application/x-bittorrent\"/>\n", xmlEscape(dl), it.link.Size)
		for _, at := range [][2]string{
			{"category", strconv.Itoa(it.cat)},
			{"size", strconv.FormatInt(it.link.Size, 10)},
			{"seeders", strconv.Itoa(int(seeders))},
			{"peers", strconv.Itoa(int(peers))},
			{"infohash", InfoHash(it.link)},
			{"downloadvolumefactor", "1"},
			{"uploadvolumefactor", "1"},
		} {
			fmt.Fprintf(&b, "<torznab:attr name=\"%s\" value=\"%s\"/>\n", at[0], xmlEscape(at[1]))
		}
		b.WriteString("</item>\n")
	}
	b.WriteString("</channel>\n</rss>\n")
	w.Header().Set("Content-Type", "application/rss+xml; charset=utf-8")
	fmt.Fprint(w, b.String())
}

// torrentFile serves the stand-in .torrent that the apps "download" and
// then hand to the qBittorrent-compatible API.
func (a *Adapter) torrentFile(w http.ResponseWriter, r *http.Request) {
	if !a.cfg.REST.Authorized(r) {
		http.Error(w, "missing or wrong API key", http.StatusUnauthorized)
		return
	}
	link, err := linkFromDownloadURL(r.URL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/x-bittorrent")
	w.Header().Set("Content-Disposition", `attachment; filename="`+strings.ReplaceAll(download.SanitizeName(link.Name), `"`, "")+`.torrent"`)
	w.Write(Torrent(link))
}

// linkFromDownloadURL rebuilds the ed2k link from a /torznab/download URL.
func linkFromDownloadURL(u *url.URL) (ed2k.Link, error) {
	parts := strings.Split(strings.TrimSuffix(u.Path, "/"), "/")
	h, err := ed2k.ParseHash(parts[len(parts)-1])
	if err != nil {
		return ed2k.Link{}, err
	}
	size, err := strconv.ParseInt(u.Query().Get("size"), 10, 64)
	name := u.Query().Get("name")
	if err != nil || size <= 0 || name == "" {
		return ed2k.Link{}, fmt.Errorf("download URL needs name and size")
	}
	return ed2k.Link{Name: name, Size: size, Hash: h}, nil
}
