package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"mymule/internal/api"
	"mymule/internal/config"
	"mymule/internal/ed2k"
	"mymule/internal/server"
)

// connFlags are shared by commands that need a server connection.
type connFlags struct {
	server  *string
	nick    *string
	timeout *time.Duration
	verbose *bool
}

func addConnFlags(fs *flag.FlagSet) connFlags {
	return connFlags{
		server:  fs.String("server", settings.Server, "server ip:port (empty: try known servers in order)"),
		nick:    fs.String("nick", settings.Nick, "nickname sent to the server"),
		timeout: fs.Duration("timeout", 10*time.Second, "per-server connect/login timeout"),
		verbose: fs.Bool("v", false, "verbose: show server messages"),
	}
}

// connect logs into the requested server, or tries the known servers in
// quality order until one accepts us. Failures are recorded in server.met
// so dead servers sink to the bottom over time.
func (f connFlags) connect(ctx context.Context) (*server.Conn, error) {
	return f.connectWith(ctx, server.Config{})
}

// connectWith is like connect but starts from base (listen port,
// callbacks); identity and logging are filled in here.
func (f connFlags) connectWith(ctx context.Context, cfg server.Config) (*server.Conn, error) {
	return f.connectTo(ctx, cfg, *f.server, false)
}

// connectTo connects to addr, or to the best known servers when addr is
// empty. With tryOthers, if addr doesn't answer the other servers are tried.
func (f connFlags) connectTo(ctx context.Context, cfg server.Config, addr string, tryOthers bool) (*server.Conn, error) {
	hash, err := config.UserHash()
	if err != nil {
		return nil, err
	}
	cfg.UserHash, cfg.Nick = hash, *f.nick
	if *f.verbose {
		cfg.Log = func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, "  "+format+"\n", args...)
		}
	}

	var candidates []server.Info
	list, err := loadServers()
	if err != nil {
		return nil, err
	}
	if addr != "" {
		info, err := parseServerAddr(addr)
		if err != nil {
			return nil, err
		}
		for _, known := range list {
			if known.Addr() == info.Addr() {
				info = known
				break
			}
		}
		candidates = []server.Info{info}
		if tryOthers {
			server.SortByQuality(list)
			for _, s := range list {
				if s.Addr() != info.Addr() {
					candidates = append(candidates, s)
				}
			}
		}
	} else {
		if len(list) == 0 {
			return nil, fmt.Errorf("no servers known; run 'mymule servers update' or pass -server")
		}
		server.SortByQuality(list)
		candidates = list
	}

	// With a listen port configured we expect HighID. Some servers never
	// test the port and always hand out LowID, so if we get LowID we try
	// a few more servers before settling for the first LowID connection.
	const maxHighIDTries = 3
	var fallback *server.Conn
	extraTries := 0
	for _, info := range candidates {
		if fallback != nil {
			if extraTries >= maxHighIDTries {
				break
			}
			extraTries++
		}
		fmt.Fprintf(os.Stderr, "connecting to %s ...\n", info)
		dctx, cancel := context.WithTimeout(ctx, *f.timeout)
		c, err := server.Dial(dctx, info, cfg)
		cancel()
		updateServerStats(list, info, err == nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			if ctx.Err() != nil {
				if fallback != nil {
					fallback.Close()
				}
				return nil, ctx.Err()
			}
			continue
		}
		if c.Status().HighID {
			if fallback != nil {
				fallback.Close()
			}
			fmt.Fprintf(os.Stderr, "connected: %s, HighID\n", info.Addr())
			return c, nil
		}
		if addr != "" && info.Addr() == candidates[0].Addr() {
			// The server the user chose: stay, even with LowID.
			if fallback != nil {
				fallback.Close()
			}
			fmt.Fprintf(os.Stderr, "connected: %s, LowID\n", info.Addr())
			return c, nil
		}
		if cfg.Port == 0 {
			fmt.Fprintf(os.Stderr, "connected: %s, LowID (firewalled)\n", info.Addr())
			return c, nil
		}
		fmt.Fprintf(os.Stderr, "  got LowID although port %d is set; trying other servers\n", cfg.Port)
		if fallback == nil {
			fallback = c
		} else {
			c.Close()
		}
	}
	if fallback != nil {
		fmt.Fprintf(os.Stderr, "connected: %s, LowID (port %d not reachable from outside?)\n", fallback.Info().Addr(), cfg.Port)
		return fallback, nil
	}
	return nil, fmt.Errorf("could not connect to any server")
}

func updateServerStats(list []server.Info, info server.Info, ok bool) {
	for i := range list {
		if list[i].Addr() == info.Addr() {
			if ok {
				list[i].Fails = 0
			} else {
				list[i].Fails++
			}
			saveServers(list)
			return
		}
	}
}

func parseSize(s string) (uint64, error) {
	if s == "" {
		return 0, nil
	}
	s = strings.ToUpper(strings.TrimSpace(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(s, "K"):
		mult, s = 1<<10, s[:len(s)-1]
	case strings.HasSuffix(s, "M"):
		mult, s = 1<<20, s[:len(s)-1]
	case strings.HasSuffix(s, "G"):
		mult, s = 1<<30, s[:len(s)-1]
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid size %q", s)
	}
	return uint64(v * mult), nil
}

func humanSize(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func cmdSearch(args []string) error {
	fs := flag.NewFlagSet("search", flag.ExitOnError)
	cf := addConnFlags(fs)
	ftype := fs.String("type", "", "file type: audio, video, image, program, doc, archive, iso")
	minSize := fs.String("min", "", "minimum size (e.g. 100M, 1.5G)")
	maxSize := fs.String("max", "", "maximum size")
	ext := fs.String("ext", "", "file extension (e.g. mkv)")
	more := fs.Int("more", 0, "fetch up to N extra result pages")
	limit := fs.Int("limit", 50, "max results to print (0 = all)")
	links := fs.Bool("links", false, "print ed2k links instead of a table")
	global := fs.Bool("global", false, "also search every known server over UDP (needs the daemon)")
	fs.Parse(args)

	if fs.NArg() == 0 {
		return fmt.Errorf("usage: mymule search [flags] <keywords>")
	}
	q := server.Query{Keywords: strings.Join(fs.Args(), " "), Extension: *ext}
	var err error
	if q.Type, err = server.ParseFileType(*ftype); err != nil {
		return err
	}
	if q.MinSize, err = parseSize(*minSize); err != nil {
		return err
	}
	if q.MaxSize, err = parseSize(*maxSize); err != nil {
		return err
	}

	// Through the daemon if it is running: it already has a server
	// connection, and a second login with our identity would kick it.
	if dc, err := daemonClient(); err == nil {
		if *global {
			fmt.Fprintln(os.Stderr, "searching all servers (a few seconds)...")
		}
		r, err := dc.Search(api.SearchRequest{
			Query: q.Keywords, Type: *ftype, Min: q.MinSize, Max: q.MaxSize, Ext: q.Extension, More: *more,
			Global: *global,
		})
		if err != nil {
			return err
		}
		res := r.Results
		if *limit > 0 && len(res) > *limit {
			res = res[:*limit]
		}
		printResults(len(res), func(i int) (string, uint32, uint32, uint64, string) {
			x := res[i]
			return x.Name, x.Sources, x.Complete, x.Size, x.Link
		}, *links)
		return nil
	}
	if *global {
		return fmt.Errorf("-global needs the daemon: run 'mymule start' first")
	}
	if err := lockStandalone(); err != nil {
		return err
	}

	ctx := context.Background()
	c, err := cf.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()

	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	results, hasMore, err := c.Search(sctx, q)
	if err != nil {
		return err
	}
	for i := 0; i < *more && hasMore; i++ {
		var page []server.Result
		page, hasMore, err = c.QueryMore(sctx)
		if err != nil {
			break
		}
		results = append(results, page...)
	}

	results = dedupeResults(results)
	sort.SliceStable(results, func(i, j int) bool { return results[i].Sources > results[j].Sources })
	if *limit > 0 && len(results) > *limit {
		results = results[:*limit]
	}
	printResults(len(results), func(i int) (string, uint32, uint32, uint64, string) {
		r := results[i]
		return r.Name, r.Sources, r.CompleteSources, r.Size, r.Link().String()
	}, *links)
	return nil
}

func printResults(n int, get func(i int) (name string, srcs, full uint32, size uint64, link string), links bool) {
	if n == 0 {
		fmt.Println("no results")
		return
	}
	if links {
		for i := 0; i < n; i++ {
			_, _, _, _, l := get(i)
			fmt.Println(l)
		}
		return
	}
	fmt.Printf("%3s %5s %5s %10s  %s\n", "#", "SRCS", "FULL", "SIZE", "NAME")
	for i := 0; i < n; i++ {
		name, srcs, full, size, _ := get(i)
		fmt.Printf("%3d %5d %5d %10s  %s\n", i+1, srcs, full, humanSize(size), name)
	}
	fmt.Fprintf(os.Stderr, "\n%d results. Use -links to get ed2k links.\n", n)
}

func dedupeResults(in []server.Result) []server.Result {
	seen := map[ed2k.Hash]int{}
	out := in[:0]
	for _, r := range in {
		if i, ok := seen[r.Hash]; ok {
			if r.Sources > out[i].Sources {
				out[i].Sources = r.Sources
			}
			continue
		}
		seen[r.Hash] = len(out)
		out = append(out, r)
	}
	return out
}

func cmdSources(args []string) error {
	fs := flag.NewFlagSet("sources", flag.ExitOnError)
	cf := addConnFlags(fs)
	fs.Parse(args)
	if fs.NArg() == 0 {
		return fmt.Errorf("usage: mymule sources [flags] <ed2k-link>...")
	}
	var links []ed2k.Link
	for _, a := range fs.Args() {
		link, err := ed2k.ParseLink(a)
		if err != nil {
			return fmt.Errorf("%q: %w", a, err)
		}
		links = append(links, link)
	}
	ctx := context.Background()
	c, err := cf.connect(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	for i, link := range links {
		if i > 0 {
			time.Sleep(time.Second) // be gentle with the server
		}
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		srcs, err := c.GetSources(sctx, link.Hash, uint64(link.Size))
		cancel()
		if err != nil {
			fmt.Printf("? sources for %s: %v\n", link.Name, err)
			continue
		}
		fmt.Printf("%d sources for %s\n", len(srcs), link.Name)
		for _, s := range srcs {
			fmt.Println(" ", s.Addr())
		}
	}
	return nil
}
