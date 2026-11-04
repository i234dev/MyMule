package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mymule/internal/api"
	"mymule/internal/config"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/server"
	"mymule/internal/share"
	"mymule/internal/upload"
)

// daemonClient returns a client for the running daemon, or ErrNotRunning.
func daemonClient() (*api.Client, error) {
	dir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	return api.Connect(dir)
}

// mustDaemon is like daemonClient but with a helpful error.
func mustDaemon() (*api.Client, error) {
	c, err := daemonClient()
	if errors.Is(err, api.ErrNotRunning) {
		return nil, errors.New("the daemon is not running; start it with 'mymule start'")
	}
	return c, err
}

// engineFlags are the options shared by 'download' and 'daemon'.
type engineFlags struct {
	conn     connFlags
	port     *uint
	out      *string
	maxConns *int
}

func addEngineFlags(fs *flag.FlagSet) engineFlags {
	return engineFlags{
		conn:     addConnFlags(fs),
		port:     fs.Uint("port", uint(settings.Port), "TCP port for incoming peer connections (0 = disabled); forward it in your router to get HighID"),
		out:      fs.String("o", settings.DownloadDir, "directory for completed files"),
		maxConns: fs.Int("max-conns", settings.MaxConns, "maximum simultaneous peer connections"),
	}
}

// engine bundles the download manager with the sharing components it
// uses, which must be started alongside it.
type engine struct {
	*download.Manager
	lib *share.Library
	up  *upload.Manager
}

// start runs the shared-folder scanner and the upload slot scheduler.
func (e *engine) start(ctx context.Context) {
	if e.lib != nil {
		go e.lib.Run(ctx, 10*time.Minute)
		go e.up.Run(ctx)
	}
}

func (e engineFlags) manager(tmp string, stayAlive bool) (*engine, error) {
	uh, err := config.UserHash()
	if err != nil {
		return nil, err
	}
	debug := func(string, ...any) {}
	if *e.conn.verbose {
		debug = logf
	}

	var (
		m   *download.Manager
		lib *share.Library
		up  *upload.Manager
	)
	if settings.Sharing {
		var dirs []string
		if settings.ShareDownloads {
			os.MkdirAll(*e.out, 0o755)
			dirs = append(dirs, *e.out)
		}
		dirs = append(dirs, settings.SharedDirs...)
		known, err := config.Path("known.json")
		if err != nil {
			return nil, err
		}
		lib = share.NewLibrary(dirs, known, logf)
		up = upload.New(upload.Config{
			Slots:     settings.UploadSlots,
			LimitKBps: settings.UploadLimit,
			Library:   lib,
			Dial:      func(ip net.IP, port uint16) { m.DialForUpload(ip, port) },
			Log:       logf,
			Debug:     debug,
		})
	}
	m = download.NewManager(download.Config{
		UserHash:    uh,
		Nick:        *e.conn.nick,
		Port:        uint16(*e.port),
		TempDir:     tmp,
		IncomingDir: *e.out,
		MaxConns:    *e.maxConns,
		StayAlive:   stayAlive,
		Connect:     e.conn.connectWith,
		Servers: func() []server.Info {
			list, _ := loadServers()
			server.SortByQuality(list)
			return list
		},
		Log:   logf,
		Debug: debug,
		Trace: os.Getenv("MYMULE_TRACE") != "",
		OnComplete: func(name, path string) {
			logf("COMPLETED %s -> %s", name, path)
		},
		Library: lib,
		Upload:  up,
	})
	return &engine{Manager: m, lib: lib, up: up}, nil
}

// cmdDaemon runs the engine in the foreground with the control API.
// Use it directly under systemd/a NAS task scheduler, or via 'start'.
func cmdDaemon(args []string) error {
	fs := flag.NewFlagSet("daemon", flag.ExitOnError)
	ef := addEngineFlags(fs)
	fs.Parse(args)

	tmp, err := tempDir()
	if err != nil {
		return err
	}
	pfs, err := download.LoadAll(tmp)
	if err != nil {
		return err
	}
	m, err := ef.manager(tmp, true)
	if err != nil {
		return err
	}
	for _, pf := range pfs {
		m.Add(pf)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir, _ := config.Dir()
	srv, err := api.Listen(m.Manager, dir, stop)
	if err != nil {
		return fmt.Errorf("control API: %w", err)
	}
	defer srv.Close()
	m.start(ctx)
	logf("daemon started (pid %d), %d downloads; port %d, temp %s, downloads %s",
		os.Getpid(), len(pfs), *ef.port, tmp, *ef.out)
	logf("sharing: %s; upload: %d slots, limit %s", sharedDescription(*ef.out), settings.UploadSlots, limitDescription())

	err = m.Run(ctx)
	logf("daemon stopped; progress saved")
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

// ---- start / stop ----

// cmdStart launches 'mymule daemon' in the background. Settings come from
// mymule.conf; flags given here override them for this run only.
func cmdStart(args []string) error {
	if c, err := daemonClient(); err == nil {
		fmt.Printf("daemon already running (pid %d)\n", c.PID())
		return nil
	}
	// Validate flags now, so mistakes are reported here and not in the log.
	fs := flag.NewFlagSet("start", flag.ExitOnError)
	ef := addEngineFlags(fs)
	fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		return err
	}
	logPath, err := config.Path("mymule.log")
	if err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(exe, append([]string{"daemon"}, args...)...)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.Env = os.Environ()
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	pid := cmd.Process.Pid
	cmd.Process.Release()

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		if _, err := daemonClient(); err == nil {
			conf, _ := config.SettingsPath()
			fmt.Printf("daemon started (pid %d)\n  config:    %s\n  temp:      %s\n  downloads: %s\n  log:       %s\n",
				pid, conf, settings.TempDir, *ef.out, logPath)
			return nil
		}
	}
	return fmt.Errorf("daemon did not come up; see %s", logPath)
}

func cmdStop(args []string) error {
	c, err := mustDaemon()
	if err != nil {
		return err
	}
	if err := c.Stop(); err != nil {
		return err
	}
	for i := 0; i < 50; i++ {
		time.Sleep(200 * time.Millisecond)
		if _, err := daemonClient(); err != nil {
			fmt.Println("daemon stopped")
			return nil
		}
	}
	return errors.New("daemon is still shutting down")
}

// ---- status ----

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	watch := fs.Duration("w", 0, "refresh every interval (e.g. -w 5s) until Ctrl+C")
	fs.Parse(args)
	c, err := mustDaemon()
	if err != nil {
		return err
	}
	for {
		st, err := c.Status()
		if err != nil {
			return err
		}
		if *watch > 0 {
			fmt.Print("\033[H\033[2J") // clear screen
		}
		printStatus(st.Server, st.Files)
		printSharing(st)
		if *watch <= 0 {
			return nil
		}
		time.Sleep(*watch)
	}
}

func printStatus(ss download.ServerStatus, files []download.FileStatus) {
	srv := "not connected"
	if ss.Connected {
		id := "LowID"
		if ss.HighID {
			id = "HighID"
		}
		srv = fmt.Sprintf("%s (%s, %s)", ss.Name, ss.Addr, id)
	}
	var total float64
	for _, f := range files {
		total += f.Speed
	}
	fmt.Printf("%s | server: %s | %s/s\n", time.Now().Format("15:04:05"), srv, humanSize(uint64(total)))
	if len(files) == 0 {
		fmt.Println("no downloads; add one with 'mymule add <ed2k-link>'")
		return
	}
	for _, f := range files {
		pct := 100 * float64(f.Done) / float64(f.Size)
		state := fmt.Sprintf("src %d dl %d q %d", f.Sources, f.Downloading, f.Queued)
		if f.BestRank > 0 {
			state += fmt.Sprintf(" (#%d)", f.BestRank)
		}
		switch {
		case f.Finished:
			state = "done"
		case f.Paused:
			state = "paused"
		}
		eta := ""
		if f.Speed > 1024 && !f.Finished && !f.Paused {
			eta = " eta " + (time.Duration(float64(f.Size-f.Done)/f.Speed) * time.Second).Round(time.Second).String()
		}
		fmt.Printf("%.8s [%5.1f%%] %10s/s  %-22s %s%s\n", f.Hash, pct, humanSize(uint64(f.Speed)), state, f.Name, eta)
	}
}

// ---- add / pause / resume / cancel / clear ----

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

func cmdAdd(args []string) error {
	fs := flag.NewFlagSet("add", flag.ExitOnError)
	var sources multiFlag
	fs.Var(&sources, "source", "extra source ip:port for the links (repeatable), e.g. a NAS on your LAN")
	fs.Parse(args)
	if fs.NArg() == 0 {
		return errors.New("usage: mymule add [-source ip:port] <ed2k-link>...")
	}
	for _, a := range fs.Args() {
		if _, err := ed2k.ParseLink(a); err != nil {
			return fmt.Errorf("%q: %w", a, err)
		}
	}
	c, err := mustDaemon()
	if err != nil {
		return err
	}
	r, err := c.Add(api.AddRequest{Links: fs.Args(), Sources: sources})
	if err != nil {
		return err
	}
	for _, n := range r.Added {
		fmt.Println("added", n)
	}
	for _, e := range r.Errors {
		fmt.Fprintln(os.Stderr, "error:", e)
	}
	if len(r.Errors) > 0 {
		return errors.New("some links were not added")
	}
	return nil
}

func cmdPauseResume(pause bool) func([]string) error {
	return func(args []string) error {
		if len(args) != 1 {
			return errors.New("usage: mymule pause|resume <hash-prefix or name>")
		}
		c, err := mustDaemon()
		if err != nil {
			return err
		}
		var name string
		if pause {
			name, err = c.Pause(args[0])
		} else {
			name, err = c.Resume(args[0])
		}
		if err != nil {
			return err
		}
		if pause {
			fmt.Println("paused", name)
		} else {
			fmt.Println("resumed", name)
		}
		return nil
	}
}

func cmdClear(args []string) error {
	c, err := mustDaemon()
	if err != nil {
		return err
	}
	n, err := c.Clear()
	if err != nil {
		return err
	}
	fmt.Printf("removed %d finished downloads from the list\n", n)
	return nil
}

// ---- log ----

func cmdLog(args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	n := fs.Int("n", 30, "number of lines")
	fs.Parse(args)
	p, err := config.Path("mymule.log")
	if err != nil {
		return err
	}
	f, err := os.Open(p)
	if err != nil {
		return fmt.Errorf("no log yet (%s)", p)
	}
	defer f.Close()
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > *n {
			lines = lines[1:]
		}
	}
	for _, l := range lines {
		fmt.Println(l)
	}
	return sc.Err()
}

func sharedDescription(downloads string) string {
	if !settings.Sharing {
		return "disabled (sharing = false)"
	}
	var dirs []string
	if settings.ShareDownloads {
		dirs = append(dirs, downloads)
	}
	dirs = append(dirs, settings.SharedDirs...)
	if len(dirs) == 0 {
		return "only verified parts of partial downloads"
	}
	return strings.Join(dirs, ", ") + " + partial downloads"
}

func limitDescription() string {
	if settings.UploadLimit == 0 {
		return "none"
	}
	return fmt.Sprintf("%d KB/s", settings.UploadLimit)
}
