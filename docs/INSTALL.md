# Installing MyMule

How to install MyMule on each system, run it in Docker or as a service on
a server or NAS, and check that what you downloaded is genuine.

- [Install](#install)
- [Docker](#docker)
- [First start](#first-start)
- [Running on a server or NAS](#running-on-a-server-or-nas)
- [Checking a download](#checking-a-download)
- [Building from source](#building-from-source)

## Install

| System | How |
|---|---|
| Any, with Docker (also NAS, Raspberry Pi) | see [Docker](#docker) |
| macOS, Linux | `brew install i234dev/tap/mymule` ([Homebrew](https://brew.sh)) |
| Windows | `scoop bucket add mymule https://github.com/i234dev/scoop-bucket` then `scoop install mymule` ([Scoop](https://scoop.sh)) |
| Any | download the archive for your system from [Releases](https://github.com/i234dev/MyMule/releases) and unpack it |

Installed with Homebrew, MyMule keeps its settings and downloads in
`~/MyMule`; with Scoop, in `%USERPROFILE%\scoop\persist\mymule`. Both keep
them across upgrades (`brew upgrade mymule`, `scoop update mymule`).

## Docker

An image for Linux (amd64, arm64 and armv7, so also Raspberry Pi and most
NAS devices) is published with every release. With Docker installed
([Docker Desktop](https://www.docker.com/products/docker-desktop/) on
Windows and macOS), save [docker-compose.yml](../docker-compose.yml) in an
empty folder and run there:

```sh
docker compose up -d
```

Then open <http://localhost:4711> and log in with the `api_key` from
`config/mymule.conf` (created in that folder on the first start). Finished
files go to `downloads/`, partial ones to `downloads/incomplete/`.

Without Compose, the same in one command:

```sh
docker run -d --name mymule --restart unless-stopped   -e PUID=1000 -e PGID=1000 -e TZ=Etc/UTC   -v ./config:/config -v ./downloads:/downloads   -p 4662:4662/tcp -p 4672:4672/udp -p 127.0.0.1:4711:4711/tcp   ghcr.io/i234dev/mymule:latest
```

- **Which user**: MyMule never runs as root. `PUID` and `PGID` are the user
  and group that own the files (on Linux, run `id` to see yours; on
  Windows and macOS the defaults are fine).
- **Settings**: edit `config/mymule.conf`, or set any setting as an
  environment variable `MYMULE_<SETTING>`, e.g. `MYMULE_ARR_SUPPORT=true`
  or `MYMULE_SEARCH_LANGUAGES=es`. Variables win over the file. The image
  turns on the web interface and listens on the container's network
  (`MYMULE_WEB_UI=true`, `MYMULE_API_LISTEN=0.0.0.0:4711`).
- **Commands**: `docker exec mymule mymule status`, `docker logs mymule`
  for the log. Commands run as the `PUID` user even from `docker exec`.
- **HighID**: forward ports 4662 (TCP) and 4672 (UDP) in your router to
  the computer running Docker.
- **Web interface from other devices**: port 4711 (the web interface and
  the API) is published on `127.0.0.1` only. To open it from your phone
  or another computer, publish it on this computer's LAN address instead
  (`<LAN-IP>:4711:4711/tcp`). Never publish it on every address
  (`4711:4711`) of a computer that is on the Internet, such as a VPS:
  Docker's published ports bypass ufw and firewalld, so the API would be
  open to anyone, with only the `api_key` in the way. **Never forward
  port 4711** in your router either.
- **Update**: `docker compose pull && docker compose up -d`. Each release
  is also tagged by version (`:X.Y.Z` and `:X.Y`) if you prefer to
  choose when to update.
- **Radarr, Sonarr...**: set `MYMULE_ARR_SUPPORT=true`; MyMule reports
  downloads as `/downloads/...`, so mount the same folder in the apps as
  `/downloads` too (or add a Remote Path Mapping). See
  [docs/ARR.md](ARR.md).

## First start

**On Windows**, the easiest way: unpack the Windows zip from
[Releases](https://github.com/i234dev/MyMule/releases) into a folder of
your own (in Documents, for example) and double-click `mymule.exe`. MyMule
starts in a window of its own with the web interface on (for this computer
only, unless `api_listen` in `mymule.conf` says otherwise) and opens it in
your browser, already logged in: no commands, no key to type. Keep the
window open while you use MyMule; closing it stops MyMule. Double-clicking
it again while it runs just opens the web interface again. Nothing is
written to `mymule.conf` for this: started from a console, MyMule works
as usual.

Without Docker, MyMule creates `mymule.conf` next to the executable the
first time it runs (see [Configuration](CONFIG.md)). Started any other
way, to use the web interface, turn it on there and start MyMule:

```ini
web_ui = true
```

```sh
mymule start
```

Then open <http://127.0.0.1:4711/> and log in with the `api_key` from
`mymule.conf`. The [web interface guide](WEBUI.md) explains how to open it
from a phone or another computer; the [command line guide](CLI.md) has
every command.

## Running on a server or NAS

`mymule daemon` runs the engine in the foreground, which suits systemd,
OpenRC, Docker or a NAS task scheduler. Run it as its own user, never as
root, and start and stop it through the service manager (not with
`mymule start` as root, which would leave a second copy running as root).

Example systemd unit, with the program and its data in `/srv/mymule`
(owned by the `mymule` user) and downloads in `/data`:

```ini
[Unit]
Description=MyMule eD2K client
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/srv/mymule/mymule daemon
User=mymule
Group=mymule
Restart=on-failure
# Private files by default, and no way to gain privileges.
UMask=0077
NoNewPrivileges=yes
# Read-only system, private /tmp, no access to other users' homes;
# only the data and download folders are writable.
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
ReadWritePaths=/srv/mymule /data
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictSUIDSGID=yes
LockPersonality=yes

[Install]
WantedBy=multi-user.target
```

If a media server (Plex, Jellyfin) has to read the finished files, use
`UMask=0022` instead, or give it a group with access to `/data`.

**Logs.** Under a service manager the log belongs to it (journald, or the
file set in OpenRC's `output_log`); rotate that file with `logrotate`, for
example `/etc/logrotate.d/mymule`:

```
/srv/mymule/mymule.log {
    weekly
    rotate 4
    compress
    missingok
    copytruncate
}
```

`mymule start` manages its own log instead and keeps one previous copy
(`mymule.log.1`) when it passes 20 MB. Either way, chat messages from other
users are logged at most 20 per minute, so nobody can fill the disk with
them.

## Checking a download

**Checking a download.** Every release has a `SHA256SUMS` file and a
signed build provenance attestation, which ties each file to this
repository and the exact commit it was built from. With the GitHub CLI:

```sh
gh attestation verify mymule-v1.3.5-linux-amd64.tar.gz --repo i234dev/MyMule
```

The Windows and macOS binaries are not signed with a commercial
certificate, so Windows SmartScreen or macOS Gatekeeper may warn the
first time you run them; the attestation above is how to check them.
For the same reason, an antivirus program may now and then guess wrong
about a new unsigned program (detections whose name ends in `!ml` are
such guesses); the attestation shows whether the file is the one built
here from the published code.

## Building from source

Requires Go (see `go.mod` for the version).

```sh
go test ./...
go build -o mymule ./cmd/mymule
# cross-compile, e.g. for a Raspberry Pi:
GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build ./cmd/mymule
```

Every push builds and tests on Windows, Linux and macOS, checks the code
and its dependencies for known vulnerabilities (`govulncheck`), and
attaches the binaries to the workflow run. Pushing a tag `vX.Y.Z`
publishes a Release with archives for every platform, a `SHA256SUMS` file
and a signed build provenance attestation (see [Security](CONFIG.md#security)).

To build with your own TMDB key instead of the project's:
`go build -ldflags "-X mymule/internal/meta.ProjectTMDBKey=YOUR_KEY" ./cmd/mymule`.
