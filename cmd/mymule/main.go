// Command mymule is a command-line eD2K (eMule network) client.
package main

import (
	"fmt"
	"os"
)

const version = "0.1.0-dev"

const usage = `mymule - eD2K network client

Usage:
  mymule <command> [flags] [args]

Commands:
  hash <file>...              Compute the ed2k hash and link of files
  servers update [url]        Download a server.met and merge it into the list
  servers list                Show known servers
  servers add <ip:port>       Add a server manually
  search [flags] <keywords>   Search files on an eD2K server
  sources [flags] <ed2k-link> Ask a server for peers sharing a file
  version                     Print version

Run 'mymule <command> -h' for command flags.
Data directory: $MYMULE_HOME or the OS config dir (e.g. %AppData%\mymule).
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "hash":
		err = cmdHash(args)
	case "servers", "server":
		err = cmdServers(args)
	case "search", "s":
		err = cmdSearch(args)
	case "sources":
		err = cmdSources(args)
	case "version", "-v", "--version":
		fmt.Println("mymule", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
