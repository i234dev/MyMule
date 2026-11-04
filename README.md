# MyMule

A command-line client for the eD2K network (the eMule network), written in Go.
One static binary, no dependencies, runs anywhere: Windows, Linux, macOS,
FreeBSD, and ARM boards or NAS devices.

## Features

- Search on the connected server, or on every known server at once
  (`search -global`, over UDP)
- Downloads from eD2K peers, with MD4 verification of every 9.28 MB part:
  corrupt parts are thrown away and downloaded again
- Sources from every server in the list (UDP), not only the connected one
- Sharing and uploads: shared folders, completed downloads and the verified
  parts of partial downloads; upload queue, slots taken in turns, speed limit
- Resume after a stop, a crash or a reboot
- HighID support (listen port) and server callbacks to reach firewalled peers
- Runs as a background daemon, controlled with short commands
  (`status`, `add`, `pause`, `cancel`...) over a local, token-protected API
- `mymule top`: live full-screen view of downloads, uploads and sharing
- Portable: settings and data live next to the executable

Not implemented yet: Kad, protocol obfuscation, credits.

## Quick start

Download the archive for your system from
[Releases](../../releases), unpack it and run:

```sh
mymule servers update        # fetch a server list
mymule start                 # start the engine in the background
mymule search -links ubuntu  # search; -links prints ed2k:// links
mymule add "ed2k://|file|...|/"
mymule top                   # live view (q to quit)
mymule stop
```

Quote ed2k links: they contain `|`, which shells treat as a pipe.

Run `mymule help` for every command and `mymule <command> -h` for its flags.

## Configuration

On first run MyMule creates `mymule.conf` next to the executable:

```ini
port = 4662            # TCP port for peers; forward it in your router for HighID
server =               # ip:port, or empty to choose automatically
nick = MyMule
temp_dir = temp        # partial downloads (relative or absolute path)
download_dir = downloads
max_conns = 40
sharing = true         # master switch: false = share and upload nothing
share_downloads = true # share download_dir (partial downloads: verified parts only)
shared_dirs =          # more folders to share, separated by ';'
upload_slots = 3       # peers served at the same time; the rest queue
upload_limit = 100     # KB/s, 0 = no limit
```

Settings added in newer versions are appended to an existing file, with
their comments, so you can see them.

Shared files are hashed in the background the first time (this reads the
whole file); the result is cached in `known.json`, so restarts are instant.

Command-line flags (`-port`, `-server`, `-o`...) override the file for one run.
Set `MYMULE_HOME` to keep data somewhere other than the executable's folder,
for example when the binary is installed in a read-only location.

### HighID

Peers that cannot reach you only serve you if they can reach you instead,
so forwarding the TCP port (`port` in the config) to your machine gets you
far more sources. `mymule status` shows whether you have HighID or LowID.
If you have a listen port configured but get LowID, MyMule tries other
servers, since some servers never test the port.

## Running on a server or NAS

`mymule daemon` runs the engine in the foreground, which suits systemd,
Docker or a NAS task scheduler. Example systemd unit:

```ini
[Unit]
Description=MyMule eD2K client
After=network-online.target

[Service]
ExecStart=/opt/mymule/mymule daemon
User=mymule
Restart=on-failure

[Install]
WantedBy=multi-user.target
```

## Building

Requires Go (see `go.mod` for the version).

```sh
go test ./...
go build -o mymule ./cmd/mymule
# cross-compile, e.g. for a Raspberry Pi:
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build ./cmd/mymule
```

Every push builds and tests on Windows, Linux and macOS, and the binaries
are attached to the workflow run. Pushing a tag `vX.Y.Z` publishes a
Release with archives for every platform and a `SHA256SUMS` file.

## Legal

MyMule is a protocol client. Only share and download content you have the
right to.

## License

Copyright (C) 2026 i234dev

This program is free software: you can redistribute it and/or modify it
under the terms of the GNU General Public License as published by the Free
Software Foundation, either version 3 of the License, or (at your option)
any later version. See [LICENSE](LICENSE).
