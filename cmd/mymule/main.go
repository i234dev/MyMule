// Command mymule is a command-line eD2K (eMule network) client.
package main

import (
	"errors"
	"fmt"
	"os"

	"mymule/internal/api"
	"mymule/internal/config"
)

// version is overridden at build time by the release pipeline:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/mymule
var version = "0.9.1-dev"

const usage = `mymule - eD2K network client

Usage:
  mymule <command> [flags] [args]

Daemon (recommended):
  start [flags]               Start the engine in the background
  stop                        Stop it, saving all progress
  top                         Live full-screen view of everything (q to quit)
  status [-w 5s]              Server, speed and progress of every download
  add [-source ip:port] <link>...  Add ed2k links
  search [flags] <keywords>   Search (through the daemon if it is running)
  pause|resume <hash|name>    Pause or resume a download
  cancel <hash|name>          Cancel a download and delete its data
  clear                       Remove finished downloads from the list
  log [-n 50]                 Show the daemon log
  daemon [flags]              Run the engine in the foreground (systemd, NAS...)

Standalone:
  download [flags] [link]...  Download in the foreground (no args: resume pending)
  downloads                   List pending downloads
  sources [flags] <link>...   Ask a server for peers sharing files
  hash <file>...              Compute the ed2k hash and link of files
  servers update|list|add     Manage the server list
  version                     Print version

Run 'mymule <command> -h' for command flags.

Everything lives next to the mymule executable (or in $MYMULE_HOME):
  mymule.conf   settings (port, server, nick, folders...); flags override it
  temp/         partial downloads      downloads/   completed files
`

var exitCode int

// settings is loaded from mymule.conf before any command runs; flag
// defaults come from it.
var settings = config.Defaults()

func main() {
	run()
	os.Exit(exitCode)
}

// releaseLock is set by lockStandalone and released when run returns.
var releaseLock = func() {}

// lockStandalone takes the data directory lock for commands that log in
// or touch download files themselves (i.e. not through the daemon).
func lockStandalone() error {
	release, err := config.Lock()
	if err != nil {
		if _, derr := daemonClient(); derr == nil {
			return errors.New("the daemon is running; use 'mymule status', 'add', 'search', 'cancel'... or 'mymule stop' first")
		}
		return err
	}
	releaseLock = release
	return nil
}

func run() {
	defer func() { releaseLock() }()
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		exitCode = 2
		return
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "version", "-v", "--version", "help", "-h", "--help":
	default:
		if settings, err = config.LoadSettings(); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			exitCode = 1
			return
		}
		// Partial downloads are private; finished ones may be read by
		// other programs (a media server), so that folder stays open.
		for dir, perm := range map[string]os.FileMode{settings.TempDir: 0o700, settings.DownloadDir: 0o755} {
			if err := os.MkdirAll(dir, perm); err != nil {
				fmt.Fprintln(os.Stderr, "warning: cannot create", dir+":", err)
			}
		}
		for _, note := range config.ImportLegacy(settings.TempDir) {
			fmt.Fprintln(os.Stderr, note)
		}
	}
	switch cmd {
	case "start":
		err = cmdStart(args)
	case "stop":
		err = cmdStop(args)
	case "status", "st":
		err = cmdStatus(args)
	case "top", "monitor", "ui":
		err = cmdTop(args)
	case "add":
		err = cmdAdd(args)
	case "pause":
		err = cmdPauseResume(true)(args)
	case "resume":
		err = cmdPauseResume(false)(args)
	case "clear":
		err = cmdClear(args)
	case "log":
		err = cmdLog(args)
	case "daemon":
		if err = lockStandalone(); err == nil {
			err = cmdDaemon(args)
		}
	case "search", "s":
		err = cmdSearch(args)
	case "cancel":
		err = cmdCancel(args)
	case "download", "dl", "get":
		err = cmdDownload(args)
	case "downloads", "list":
		if c, derr := daemonClient(); derr == nil {
			err = statusOnce(c)
		} else if err = lockStandalone(); err == nil {
			err = cmdDownloads(args)
		}
	case "sources":
		if err = lockStandalone(); err == nil {
			err = cmdSources(args)
		}
	case "hash":
		err = cmdHash(args)
	case "servers", "server":
		err = cmdServers(args)
	case "version", "-v", "--version":
		fmt.Println("mymule", version)
		fmt.Println("Copyright (C) 2026 i234dev. License GPLv3+: GNU GPL version 3 or later.")
		fmt.Println("This is free software: you are free to change and redistribute it.")
		fmt.Println("There is NO WARRANTY, to the extent permitted by law.")
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		exitCode = 2
		return
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		exitCode = 1
	}
}

func statusOnce(c *api.Client) error {
	st, err := c.Status()
	if err != nil {
		return err
	}
	printStatus(st.Server, st.Files)
	printSharing(st)
	return nil
}

// printSharing prints the upload and shared-files summary lines.
func printSharing(st api.Status) {
	if u := st.Upload; u != nil {
		fmt.Printf("upload: %s/s, %d/%d slots, %d in queue, %s sent\n",
			humanSize(uint64(u.Speed)), len(u.Slots), u.MaxSlots, u.Queue, humanSize(uint64(u.Total)))
	}
	if s := st.Shared; s != nil {
		line := fmt.Sprintf("shared: %d files (%s), %d partial", s.Files, humanSize(uint64(s.Bytes)), s.Partial)
		if s.Hashing != "" {
			line += fmt.Sprintf("; hashing %s %d%% (%d left)", s.Hashing, s.HashedPct, s.ToHash)
		}
		fmt.Println(line)
	}
}
