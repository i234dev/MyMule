package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"time"

	"mymule/internal/api"
	"mymule/internal/download"
	"mymule/internal/ed2k"
)

func tempDir() (string, error) {
	p := settings.TempDir
	return p, os.MkdirAll(p, 0o700)
}

func logf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "%s "+format+"\n", append([]any{time.Now().Format("15:04:05")}, args...)...)
}

func cmdDownload(args []string) error {
	fs := flag.NewFlagSet("download", flag.ExitOnError)
	ef := addEngineFlags(fs)
	interval := fs.Duration("status", 10*time.Second, "status print interval")
	fs.Parse(args)

	// If the daemon is running, hand the links to it instead.
	if c, err := daemonClient(); err == nil {
		if fs.NArg() == 0 {
			return statusOnce(c)
		}
		r, err := c.Add(api.AddRequest{Links: fs.Args()})
		if err != nil {
			return err
		}
		for _, n := range r.Added {
			fmt.Println("added to the daemon:", n)
		}
		for _, e := range r.Errors {
			fmt.Fprintln(os.Stderr, "error:", e)
		}
		return nil
	}
	if err := lockStandalone(); err != nil {
		return err
	}

	tmp, err := tempDir()
	if err != nil {
		return err
	}
	pfs, bad, err := download.LoadAll(tmp)
	if err != nil {
		return err
	}
	for _, e := range bad {
		logf("skipping a download that cannot be resumed: %v", e)
	}
	known := map[ed2k.Hash]bool{}
	for _, pf := range pfs {
		known[pf.Hash] = true
	}
	for _, arg := range fs.Args() {
		link, err := ed2k.ParseLink(arg)
		if err != nil {
			return fmt.Errorf("%q: %w", arg, err)
		}
		if known[link.Hash] {
			fmt.Fprintf(os.Stderr, "already downloading %s\n", link.Name)
			continue
		}
		pf, err := download.Create(tmp, link)
		if err != nil {
			return err
		}
		known[link.Hash] = true
		pfs = append(pfs, pf)
		fmt.Fprintf(os.Stderr, "added %s (%s)\n", link.Name, humanSize(uint64(link.Size)))
	}
	if len(pfs) == 0 {
		return fmt.Errorf("nothing to download; usage: mymule download [flags] <ed2k-link>...")
	}

	m, err := ef.manager(tmp, false, false)
	if err != nil {
		return err
	}
	for _, pf := range pfs {
		m.Add(pf)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	m.start(ctx)

	show := func() {
		ss, files := m.Status()
		fmt.Print("--- ")
		printStatus(ss, files)
	}
	go func() {
		t := time.NewTicker(*interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				show()
			}
		}
	}()

	err = m.Run(ctx)
	show()
	if err == context.Canceled {
		fmt.Fprintln(os.Stderr, "stopped; progress saved. Run 'mymule download' to resume.")
		return nil
	}
	return err
}

func cmdDownloads(args []string) error {
	tmp, err := tempDir()
	if err != nil {
		return err
	}
	pfs, bad, err := download.LoadAll(tmp)
	if err != nil {
		return err
	}
	for _, e := range bad {
		logf("skipping a download that cannot be resumed: %v", e)
	}
	defer func() {
		for _, pf := range pfs {
			pf.Close()
		}
	}()
	if len(pfs) == 0 {
		fmt.Println("no pending downloads")
		return nil
	}
	fmt.Printf("%-32s %7s %10s  %s\n", "HASH", "DONE", "SIZE", "NAME")
	for _, pf := range pfs {
		done, size := pf.Progress()
		fmt.Printf("%-32s %6.1f%% %10s  %s\n", pf.Hash, 100*float64(done)/float64(size), humanSize(uint64(size)), pf.Name)
	}
	return nil
}

func cmdCancel(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("usage: mymule cancel <hash-prefix or name>")
	}
	if c, err := daemonClient(); err == nil {
		name, err := c.Cancel(args[0])
		if err != nil {
			return err
		}
		fmt.Println("cancelled", name)
		return nil
	}
	if err := lockStandalone(); err != nil {
		return err
	}
	tmp, err := tempDir()
	if err != nil {
		return err
	}
	pfs, bad, err := download.LoadAll(tmp)
	if err != nil {
		return err
	}
	for _, e := range bad {
		logf("skipping a download that cannot be resumed: %v", e)
	}
	q := strings.ToLower(args[0])
	var match []*download.PartFile
	for _, pf := range pfs {
		if strings.HasPrefix(strings.ToLower(pf.Hash.String()), q) || strings.Contains(strings.ToLower(pf.Name), q) {
			match = append(match, pf)
		} else {
			pf.Close()
		}
	}
	switch len(match) {
	case 0:
		return fmt.Errorf("no download matches %q", args[0])
	case 1:
		if err := match[0].Delete(); err != nil {
			return err
		}
		fmt.Println("cancelled", match[0].Name)
		return nil
	}
	for _, pf := range match {
		fmt.Fprintf(os.Stderr, "  %s  %s\n", pf.Hash, pf.Name)
		pf.Close()
	}
	return fmt.Errorf("%d downloads match; be more specific", len(match))
}
