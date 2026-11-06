package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"mymule/internal/api"
	"mymule/internal/arr"
	"mymule/internal/authlimit"
	"mymule/internal/config"
	"mymule/internal/download"
	"mymule/internal/ed2k"
	"mymule/internal/library"
	"mymule/internal/meta"
	"mymule/internal/prefs"
	"mymule/internal/server"
	"mymule/internal/share"
	"mymule/internal/upload"
	"mymule/internal/web"
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
	lib   *share.Library
	up    *upload.Manager
	prefs *prefs.Store   // preferences edited in the web interface
	items *library.Store // downloads that are movies or episodes

	nextMu sync.Mutex
	next   string // server to connect to next, chosen in the web interface
}

// serverTarget is the server to connect to now: one just chosen in the
// web interface, else the preferred one, else the -server flag/setting
// ("" = automatic).
func (e *engine) serverTarget(flag string) string {
	e.nextMu.Lock()
	next := e.next
	e.next = ""
	e.nextMu.Unlock()
	if next != "" {
		return next
	}
	if e.prefs != nil {
		if p := e.prefs.Get().Server; p != "" {
			return p
		}
	}
	return flag
}

// ConnectTo switches to another server now.
func (e *engine) ConnectTo(addr string) {
	e.nextMu.Lock()
	e.next = addr
	e.nextMu.Unlock()
	e.Reconnect()
}

// Servers returns the known servers.
func (e *engine) Servers() ([]server.Info, error) { return loadServers() }

// UpdateServers downloads the server list again.
func (e *engine) UpdateServers() (total, added int, err error) {
	return updateServerList(defaultServerMetURLs, logf)
}

// organize renames and moves a finished movie or episode into its folder
// (when the user chose one), keeping it shared without hashing it again.
func (e *engine) organize(h ed2k.Hash, name, path string) {
	it, ok := e.items.Take(h.String())
	if !ok {
		return
	}
	if !library.IsMedia(path) {
		logf("%s: not a video, audio or subtitle file; left in %s", name, filepath.Dir(path))
		return
	}
	dst := library.Destination(e.prefs.Library(), it, path)
	if dst == "" {
		return
	}
	final, err := library.Move(path, dst)
	if err != nil {
		logf("%s: cannot move to %s: %v", name, dst, err)
		return
	}
	e.SetFinishedPath(h, final)
	if e.lib != nil {
		e.lib.Moved(path, final)
	}
	logf("ORGANIZED %s -> %s", name, final)
}

// updateShared shares the movie and series folders (with subfolders) when
// the preferences say so.
func (e *engine) updateShared() {
	if e.lib == nil {
		return
	}
	st := e.prefs.Library()
	var trees []string
	if st.Organize && st.Share { // only while the library is in use
		for _, d := range []string{st.MoviesDir, st.SeriesDir} {
			if d != "" {
				trees = append(trees, d)
			}
		}
	}
	e.lib.SetTrees(trees)
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
		eng = &engine{}
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
		lib.Protect([]string{filepath.Dir(known)}, []string{tmp})
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
		Connect: func(ctx context.Context, cfg server.Config) (*server.Conn, error) {
			// The daemon follows the server chosen in the web interface;
			// if it doesn't answer, the others are tried.
			return e.conn.connectTo(ctx, cfg, eng.serverTarget(*e.conn.server), true)
		},
		Servers: func() []server.Info {
			list, _ := loadServers()
			server.SortByQuality(list)
			return list
		},
		Log:   logf,
		Debug: debug,
		Trace: os.Getenv("MYMULE_TRACE") != "",
		OnComplete: func(h ed2k.Hash, name, path string) {
			logf("COMPLETED %s -> %s", name, path)
			go eng.organize(h, name, path) // moving to another disk can take a while
		},
		Library: lib,
		Upload:  up,
	})
	dataDir, err := config.Dir()
	if err != nil {
		return nil, err
	}
	*eng = engine{Manager: m, lib: lib, up: up,
		prefs: prefs.Open(filepath.Join(dataDir, "preferences.json"), dataDir),
		items: library.OpenStore(filepath.Join(dataDir, "library.json"))}
	eng.prefs.Protect(tmp)
	eng.updateShared()
	return eng, nil
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
	pfs, bad, err := download.LoadAll(tmp)
	if err != nil {
		return err
	}
	for _, e := range bad {
		logf("skipping a download that cannot be resumed: %v", e)
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
	if dir, err := config.Dir(); err == nil {
		if w := config.LocationWarning(dir); w != "" {
			logf("WARNING: %s", w)
		}
	}
	if stopAPI := startPublicAPI(m, *ef.out); stopAPI != nil {
		defer stopAPI()
	}

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
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
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

// publicRoutes builds the handler served on api_listen: the REST API for
// scripts, plus the Torznab indexer and qBittorrent-compatible API used
// by Radarr, Sonarr, Lidarr, Readarr and Prowlarr.
func publicRoutes(e *engine, downloadDir string) http.Handler {
	m := e.Manager
	mux := http.NewServeMux()
	rest := api.NewREST(m, settings.APIKey, version)
	mux.Handle("/api/v1/", rest)
	if settings.WebUI {
		ui := web.New(settings.APIKey, version)
		if p, err := config.Path("web_sessions.json"); err == nil {
			ui.SetStateFile(p)
		}
		rest.AlsoAllow(ui.Session)
		rest.EnableMedia(mediaSource())
		rest.EnableLibrary(e.prefs, e.items, e.updateShared)
		rest.EnableServers(e)
		if settings.TMDBKey != "off" {
			dir, _ := config.Dir()
			posters := meta.NewPosters(filepath.Join(dir, "cache", "posters"), 100<<20)
			mux.Handle("GET /web/poster/{size}/{file}", ui.RequireLogin(posters))
		}
		ui.Routes(mux)
	}
	if settings.ArrSupport {
		store, _ := config.Path("arr.json")
		arr.New(arr.Config{Manager: m, REST: rest, Key: settings.APIKey, DownloadDir: downloadDir,
			StorePath: store, Log: logf, Languages: settings.SearchLanguages,
			UserAgent: userAgent()}).Routes(mux)
	}
	return authlimit.HostCheck(settings.APIHosts, mux)
}

// startPublicAPI serves the HTTP API on api_listen, if configured. It
// returns a function that shuts it down, or nil.
func startPublicAPI(m *engine, downloadDir string) func() {
	if settings.APIListen == "" {
		logf("HTTP API disabled (api_listen is empty)")
		if settings.WebUI || settings.ArrSupport {
			logf("warning: web_ui and arr_support need api_listen; they are off too")
		}
		return nil
	}
	ln, err := net.Listen("tcp", settings.APIListen)
	if err != nil {
		logf("HTTP API disabled: cannot listen on %s: %v", settings.APIListen, err)
		return nil
	}
	hs := newHTTPServer(publicRoutes(m, downloadDir))
	go hs.Serve(ln)
	// The key is never logged: logs get pasted into bug reports.
	logf("HTTP API on http://%s (the key is api_key in mymule.conf; see docs/API.md and docs/ARR.md)", ln.Addr())
	if len(settings.APIKey) < 20 {
		logf("WARNING: api_key is short (%d characters); a long random one (like the generated 32) is much harder to guess", len(settings.APIKey))
	}
	if settings.WebUI {
		logf("web interface on %s (log in with api_key)", webURL(ln.Addr()))
	}
	if settings.ArrSupport {
		logf("Radarr/Sonarr support on: Torznab at /torznab, qBittorrent API at /api/v2")
		if len(settings.SearchLanguages) > 0 {
			logf("*arr searches also use titles in: %s (from Wikidata)", strings.Join(settings.SearchLanguages, ", "))
		}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		hs.Shutdown(ctx)
	}
}

// newHTTPServer is the public API's server. Without the time limits a
// client could keep connections open for ever by sending its request, or
// reading the answer, very slowly. The write limit leaves room for a
// global search (up to 45 s).
func newHTTPServer(h http.Handler) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout: 60 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 120 * time.Second}
}

// webURL is the address to open in a browser for a listener address:
// a wildcard address is shown as localhost.
func webURL(a net.Addr) string {
	host, port, err := net.SplitHostPort(a.String())
	if err != nil {
		return "http://" + a.String() + "/"
	}
	if ip := net.ParseIP(host); ip == nil || ip.IsUnspecified() {
		return "http://127.0.0.1:" + port + "/ (from other devices: http://<this computer's IP>:" + port + "/)"
	}
	return "http://" + net.JoinHostPort(host, port) + "/"
}

// userAgent identifies MyMule to the movie databases, as they ask.
func userAgent() string { return "MyMule/" + version + " (https://github.com/i234dev/MyMule)" }

// mediaSource is where the web interface looks movies and series up: TMDB
// (MyMule's key unless the user set one) with Wikidata as a fallback, or
// only Wikidata with tmdb_api_key = off.
func mediaSource() meta.Source {
	wd := meta.NewWikidata(userAgent())
	if settings.TMDBKey == "off" {
		return wd
	}
	key := settings.TMDBKey
	if key == "" {
		key = meta.ProjectTMDBKey
	}
	return meta.WithFallback(meta.NewTMDB(key), wd)
}
