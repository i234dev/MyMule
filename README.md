# MyMule

A command-line client for the eD2K network (the eMule network), written in Go.
One static binary, no dependencies, runs anywhere: Windows, Linux, macOS,
FreeBSD, and ARM boards or NAS devices.

![mymule top: live view of downloads, uploads and shared files](docs/top-demo.png)

<sub>`mymule top` with demo data (`mymule top -demo`).</sub>

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
- `mymule top`: live full-screen view; search, pick what to download and
  pause, resume or cancel downloads without leaving it
- Optional web interface (downloads, search, sharing) that works on phones
  too; off by default
- HTTP API for scripts, and optional Radarr/Sonarr/Lidarr/Readarr/Prowlarr
  support (Torznab indexer + qBittorrent-compatible client)
- Light: about 15 MB of RAM
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
api_listen = 127.0.0.1:4711  # HTTP API for scripts/apps; empty = off
api_key = (random)      # generated on first run, required by the API
web_ui = false         # true: web interface at http://127.0.0.1:4711/
tmdb_api_key =         # empty: MyMule's TMDB key; your own key; or off
arr_support = false    # true: Radarr/Sonarr/Prowlarr support (see below)
search_languages =     # e.g. es: Radarr/Sonarr also search by Spanish titles
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

## Web interface

An optional web interface, built into the program (no extra files). It is
**off by default**; turn it on in `mymule.conf` and restart the daemon:

```ini
web_ui = true
```

Then open `http://127.0.0.1:4711/` (the `api_listen` address) and log in
with the `api_key` from `mymule.conf`. It has five tabs:

- **Downloads**: progress, speed, sources and queue position; pause,
  resume, cancel, clear finished, and add `ed2k://` links.
- **Movies & series**: search a movie or series by title, in your
  browser's language, and pick it from a list with posters (title, year,
  original title, synopsis). MyMule then searches eD2K by all its titles
  (yours, the original and alternative ones), keeps only the files of
  that work (not remakes, episodes of other shows or other movies with
  similar names) and shows them with their language, resolution, source,
  size and sources, the recommended one first. Files with no sources,
  only found on Kad/torrent, or cam copies are hidden but can be shown.
  For series, choose a season and optionally an episode.
- **Search**: search every known server, with type, size and extension
  filters; sort and filter the results and download one or several.
- **Sharing**: shared files, upload speed and who is downloading from you.
- **Settings**: the movie and series library (see below).

It works on phones, follows the system's light or dark theme and is shown
in English or Spanish depending on the browser's language.

To use it from another device (a phone, another computer), set
`api_listen = 0.0.0.0:4711` and open `http://<computer-ip>:4711/`. Only do
this on a network you trust, and never forward port 4711 in your router.
The session lasts 30 days; changing `api_key` logs everybody out.

When `web_ui = false` the pages don't exist and nothing extra runs; the
interface only uses resources while a browser has it open.

### Movie and series data

Movie and series data and posters come from
[TMDB](https://www.themoviedb.org), with
[Wikidata](https://www.wikidata.org) as a fallback. MyMule includes its
own TMDB key, so there is nothing to set up; you can use your own key
(`tmdb_api_key`) or `tmdb_api_key = off` to use only Wikidata, without
posters. Only search words and ids are sent; posters are fetched by
MyMule (your browser never contacts TMDB) and kept in a small cache
(`cache/posters`, at most 100 MB).

This product uses the TMDB API but is not endorsed or certified by TMDB.

### Movie and series library

Files downloaded from the **Movies & series** tab can be renamed and moved
into their own folders when they finish, ready for Plex, Jellyfin or Kodi.
In **Settings** choose:

- **Movies folder** and **Series folder** on the computer where MyMule
  runs (empty = leave those files in `download_dir`).
- **File names** from ready-made templates or your own, with a live
  example. Templates use tags: `{title}`, `{original_title}`, `{year}`,
  `{quality}`, `{language}`, `{season}`, `{episode}`, `{episode_title}`;
  `/` makes a folder and `{season:2}` adds a leading zero. For example
  `{title} ({year})/{title} ({year})` gives
  `Example Movie (1999)/Example Movie (1999).mkv`, and
  `{title}/Season {season:2}/{title} - S{season:2}E{episode:2}` gives
  `Example Show/Season 01/Example Show - S01E02.mkv`.
- Whether file names use the **title in your language or the original**.
- Whether to **share** those folders (with their subfolders). Moved files
  keep being shared without hashing them again.

Names are made safe for Windows, macOS and Linux, files are never
overwritten (a ` (2)` is added) and nothing is ever written outside the
chosen folders. Settings are kept in `preferences.json`.

## HTTP API for scripts

The daemon serves a JSON API (search, add, pause, resume, cancel, status)
for PHP, Python, shell scripts, cron jobs or home automation. It listens
on `127.0.0.1:4711` by default and every request needs the `api_key` from
`mymule.conf`:

```sh
curl -H "X-Api-Key: YOUR_API_KEY" "http://127.0.0.1:4711/api/v1/search?q=example+distro"
```

See [docs/API.md](docs/API.md) for every endpoint and examples in curl,
PHP and Python.

## Radarr, Sonarr, Lidarr, Readarr and Prowlarr

If you already run the *arr apps, MyMule adds the eD2K network as one more
source. It is most useful for what is hard to find on torrents: dubbed
versions, older films and series, rare releases. The apps don't need any
plugin: MyMule speaks the protocols they already know.

It is **off by default**: set `arr_support = true` in `mymule.conf` to turn
it on. With it off, none of these endpoints exist.

### What is supported

| Feature | Details |
|---|---|
| **Torznab indexer** (`/torznab/api`) | Movie, TV (season/episode), music, audio, book and general searches over all known eD2K servers; Newznab categories (Movies, TV, Audio, Books, PC, Other). Works in Prowlarr ("Generic Torznab") or directly in each app ("Torznab", custom). |
| **qBittorrent-compatible download client** (`/api/v2`) | Login, add (stand-in `.torrent` or plain `ed2k://` links), categories, pause/resume, remove (with or without files), progress and save path until the app imports the file. |
| **Search in your language** | With `search_languages` (e.g. `es`, `it`, `fr`), searches also use the title in those languages, looked up on Wikidata from the IMDb/TMDB/TVDB id. Off by default. |
| **Cleaner results** | Results nobody shares are left out; server prefixes such as `[kad example.org]` are removed; short language tags (`Spa`, `ESP-ING`, `[ita, eng]`...) get the full name added so the apps detect the language. Files keep their real name. |
| **Safe by default** | Same address and `api_key` as the HTTP API, only on this computer unless you change `api_listen`. Removing with files only deletes inside `download_dir`. |

### Quick setup

1. In `mymule.conf`, set `arr_support = true`. Keep
   `api_listen = 127.0.0.1:4711` if the apps run on
   the same computer, or use `0.0.0.0:4711` if they run in Docker or on
   another machine of your network. Copy `api_key`. Optionally set
   `search_languages`. Restart the daemon.
2. **Indexer**: in Prowlarr add *Generic Torznab* (or in Radarr/Sonarr,
   *Settings* → *Indexers* → *Torznab* → *Custom*) with URL
   `http://<mymule-host>:4711/torznab`, API path `/api` and your `api_key`.
3. **Download client**: in each app, *Settings* → *Download Clients* →
   *qBittorrent*, host `<mymule-host>`, port `4711`, any username, your
   `api_key` as password, and a category (`radarr`, `tv-sonarr`...).
4. **Paths**: the app must see MyMule's `download_dir`. On the same
   computer there is nothing to do; otherwise mount that folder in the app
   and, if the path differs, add a *Remote Path Mapping*.

### Limitations

- Results whose only sources are on the Kad network (marked
  `[kad ...]` by some servers) can't be downloaded yet: MyMule doesn't
  speak Kad. Prefer results with several sources.
- eD2K downloads can take a while to start, because peers serve uploads in
  queues. HighID (see above) helps a lot.
- One file per download: eD2K links are single files.
- Never forward port 4711 in your router: the API is meant for your own
  network.

Step-by-step guide, including Docker and Remote Path Mappings:
[docs/ARR.md](docs/ARR.md).

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
