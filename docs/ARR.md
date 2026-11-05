# Using MyMule with Radarr, Sonarr, Lidarr, Readarr and Prowlarr

The *arr apps don't know about eD2K. They only talk to **indexers**
(Torznab) and **download clients** such as qBittorrent. MyMule speaks both:

- **Indexer**: `http://<mymule-host>:4711/torznab`, which searches the eD2K
  network.
- **Download client**: MyMule answers like qBittorrent on the same address
  and port, receives what the app chose and reports progress until the app
  imports the file.

Everything runs inside the MyMule daemon and uses the same `api_listen` and
`api_key` as the [HTTP API](API.md).

## 1. Make MyMule reachable from the apps

In `mymule.conf`:

- Apps on **the same computer** (not in Docker): keep the default,
  `api_listen = 127.0.0.1:4711`.
- Apps in **Docker** or on **another computer**: set
  `api_listen = 0.0.0.0:4711`, so it listens on your local network. Only do
  this on a network you trust, and never forward port 4711 in your router.

Restart the daemon (`mymule stop`, `mymule start`) and copy `api_key` from
`mymule.conf`. In the steps below, `<mymule-host>` is:

| Where the app runs | `<mymule-host>` |
|---|---|
| Same computer | `127.0.0.1` |
| Docker on the same computer | `host.docker.internal` (Docker Desktop) or the computer's LAN IP |
| Another computer | the LAN IP of the computer running MyMule, e.g. `192.168.0.50` |

## 2. Add MyMule as an indexer

**In Prowlarr** (it then syncs to all your apps): *Indexers* → *Add
Indexer* → **Generic Torznab**:

- URL: `http://<mymule-host>:4711/torznab`
- API Path: `/api`
- API Key: your `api_key`

**Or directly in Radarr/Sonarr/Lidarr/Readarr**: *Settings* → *Indexers*
→ *+* → **Torznab** (Custom) with the same URL, API Path and API Key.

Click *Test*. The test search returns one item named "MyMule indexer test";
that is expected, because the eD2K network has no "latest releases" feed.
Real results come back when the app searches for a specific title.

## 3. Add MyMule as a download client

In each app: *Settings* → *Download Clients* → *+* → **qBittorrent**:

| Field | Value |
|---|---|
| Host | `<mymule-host>` |
| Port | `4711` |
| Use SSL | off |
| Username | anything, e.g. `mymule` |
| Password | your `api_key` |
| Category | `radarr`, `tv-sonarr`, `lidarr`... (one per app) |

Click *Test*, then *Save*.

Leave *Initial State* on *Start*. MyMule doesn't seed, so ratio and seeding
time limits have no effect.

## 4. Paths: where the app finds the finished file

When a download finishes, MyMule moves it to `download_dir` (by default,
`downloads` next to the executable) and tells the app the full path. The
app must see that folder **at the same path**:

- **Same computer, no Docker**: nothing to do.
- **Docker or another computer**: share or mount MyMule's `download_dir`
  into the app, and add a **Remote Path Mapping** in the app (*Settings* →
  *Download Clients* → *Remote Path Mappings*). Example:
  - Host: `<mymule-host>`
  - Remote Path: `C:\MyMule\downloads\` (as MyMule reports it)
  - Local Path: `/downloads/` (as the app sees it)

## How it works

- A search in the app becomes an eD2K search on **all known servers**,
  which takes about 6 to 10 seconds per search.
- **TV episodes**: Sonarr's season/episode is matched against names like
  `S01E02`, `s01.e02` or `1x02`; season packs against `S01`, `Season 1`,
  `Temporada 1`.
- **"Seeders"** in the app means eD2K sources with the complete file.
  Results that nobody shares (0 sources) are left out.
- **Titles** are tidied up for the apps, which expect torrent-style
  names: server prefixes such as `[kad example.org]` are removed, and
  short language tags (`Spa`, `ESP`, `[spa, eng]`, `Ita`...) get the full
  name appended (`SPANISH`, `ENGLISH`, `ITALIAN`...) so the apps detect the
  language. The downloaded file keeps its real name.
- When the app grabs a result, it downloads a small stand-in `.torrent`
  from MyMule and sends it to the "qBittorrent" client, which is MyMule
  again. The stand-in contains the ed2k link and is useless to any real
  BitTorrent client. You can also send plain `ed2k://` links through the
  qBittorrent API (field `urls`).
- When the app removes a download, MyMule cancels it. If the app asks to
  delete the files, MyMule only deletes files inside `download_dir`.
- Categories are stored in `arr.json` next to the executable.

## Limitations

- Results depend on what people share on eD2K, and names are less
  standardised than in other networks, so the apps may reject releases
  they can't parse. The apps' own release profiles and quality settings
  still apply.
- Downloads can take a while to start: peers serve uploads in queues
  (see `mymule top`). HighID (an open port, see the README) helps a lot.
- One file per download (eD2K links are single files).

Only download content you have the right to.
