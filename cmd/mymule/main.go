// Command mymule is a command-line eD2K (eMule network) client.
package main

import (
	"errors"
	"fmt"
	"os"

	"mymule/internal/api"
	"mymule/internal/config"
)

const version = "0.3.0-dev"

const usage = `mymule - eD2K network client

Usage:
  mymule <command> [flags] [args]

Daemon (recommended):
  start [flags]               Start the engine in the background (flags are remembered)
  stop                        Stop it, saving all progress
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
Data directory: $MYMULE_HOME or the OS config dir (e.g. %AppData%\mymule).
`

var exitCode int

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
	case "start":
		err = cmdStart(args)
	case "stop":
		err = cmdStop(args)
	case "status", "st":
		err = cmdStatus(args)
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
	return nil
}
