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
		server:  fs.String("server", "", "server ip:port (default: try known servers in order)"),
		nick:    fs.String("nick", "MyMule", "nickname sent to the server"),
		timeout: fs.Duration("timeout", 10*time.Second, "per-server connect/login timeout"),
		verbose: fs.Bool("v", false, "verbose: show server messages"),
	}
}

// connect logs into the requested server, or tries the known servers in
// quality order until one accepts us. Failures are recorded in server.met
// so dead servers sink to the bottom over time.
func (f connFlags) connect(ctx context.Context) (*server.Conn, error) {
	hash, err := config.UserHash()
	if err != nil {
		return nil, err
	}
	cfg := server.Config{UserHash: hash, Nick: *f.nick}
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
	if *f.server != "" {
		info, err := parseServerAddr(*f.server)
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
	} else {
		if len(list) == 0 {
			return nil, fmt.Errorf("no servers known; run 'mymule servers update' or pass -server")
		}
		server.SortByQuality(list)
		candidates = list
	}

	for _, info := range candidates {
		fmt.Fprintf(os.Stderr, "connecting to %s ...\n", info)
		dctx, cancel := context.WithTimeout(ctx, *f.timeout)
		c, err := server.Dial(dctx, info, cfg)
		cancel()
		updateServerStats(list, info, err == nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  failed: %v\n", err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		st := c.Status()
		kind := "LowID (firewalled)"
		if st.HighID {
			kind = "HighID"
		}
		fmt.Fprintf(os.Stderr, "connected: %s, %s\n", info.Addr(), kind)
		return c, nil
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
	if len(results) == 0 {
		fmt.Println("no results")
		return nil
	}

	if *links {
		for _, r := range results {
			fmt.Println(r.Link())
		}
		return nil
	}
	fmt.Printf("%3s %5s %5s %10s  %s\n", "#", "SRCS", "FULL", "SIZE", "NAME")
	for i, r := range results {
		fmt.Printf("%3d %5d %5d %10s  %s\n", i+1, r.Sources, r.CompleteSources, humanSize(r.Size), r.Name)
	}
	fmt.Fprintf(os.Stderr, "\n%d results. Use -links to get ed2k links.\n", len(results))
	return nil
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
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: mymule sources [flags] <ed2k-link>")
	}
	link, err := ed2k.ParseLink(fs.Arg(0))
	if err != nil {
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
	srcs, err := c.GetSources(sctx, link.Hash, uint64(link.Size))
	if err != nil {
		return err
	}
	fmt.Printf("%d sources for %s\n", len(srcs), link.Name)
	for _, s := range srcs {
		fmt.Println(" ", s.Addr())
	}
	return nil
}
