# Configuration, network and security

Everything in `mymule.conf`, how MyMule connects to the eD2K and Kad
networks, and what it does to stay safe among strangers.

- [mymule.conf](#mymuleconf)
- [HighID](#highid)
- [Kad](#kad)
- [How MyMule works with the eMule network](#how-mymule-works-with-the-emule-network)
- [Security](#security)

## mymule.conf

On first run MyMule creates `mymule.conf` next to the executable:

```ini
port = 4662            # TCP port for peers; forward it in your router for HighID
kad = true             # Kad network (no servers needed): true or false
kad_port = 4672        # UDP port for Kad; forward it in your router (UDP) too
upnp = true            # open port and kad_port on the router (UPnP / NAT-PMP); false = by hand
kad_nodes_url = https://upd.emule-security.org/nodes.dat  # first Kad nodes; empty = only from other clients
server =               # ip:port, or empty to choose automatically
nick = MyMule
temp_dir = temp        # partial downloads (relative or absolute path)
download_dir = downloads
max_conns = 40
obfuscation = true     # hide eMule traffic: true, required (only obfuscated) or false
sharing = true         # master switch: false = share and upload nothing
share_downloads = true # share download_dir (partial downloads: verified parts only)
shared_dirs =          # more folders to share, separated by ';'
upload_slots = 20      # peers served at the same time; the rest queue
upload_limit = 1000    # KB/s, 0 = no limit
download_limit = 0     # KB/s, 0 = no limit (else at least 32)
schedule_from =        # speed schedule, e.g. 01:00 (empty = off) ...
schedule_to =          # ... to 08:00, every day: then these limits instead:
schedule_upload_limit = 0
schedule_download_limit = 0
ip_filter_url =        # IP filter list (ipfilter.dat / P2P, plain, gz or zip) refreshed weekly; empty = only your own ipfilter.dat
api_listen = 127.0.0.1:4711  # HTTP API for scripts/apps; empty = off
api_key = (random)      # generated on first run, required by the API
api_hosts =            # extra names the API/web answer to (NAS name, proxy domain)
network_hosts =        # Windows: computers whose \\host\share folders may be library folders
folder_roots =         # where the web interface may put category, download and shared folders (e.g. /data)
web_ui = false         # true: web interface at http://127.0.0.1:4711/
tmdb_api_key =         # empty: MyMule's TMDB key; your own key; or off
arr_support = false    # true: Radarr/Sonarr/Prowlarr support (see ARR.md)
search_languages =     # e.g. es: Radarr/Sonarr also search by Spanish titles
notify_urls =          # notifications: ntfy://topic, tgram://BOT_TOKEN/CHAT_ID, discord://ID/TOKEN, https://webhook
notify_on = completed, wanted, error  # which ones
media_server =         # jellyfin, emby or plex: asked to scan its libraries when files arrive
media_server_url =     # e.g. http://192.168.0.10:8096
media_server_token =   # Jellyfin/Emby API key, or Plex token
```

Settings added in newer versions are appended to an existing file, with
their comments, so you can see them.

Shared files are hashed in the background the first time (this reads the
whole file); the result is cached in `known.json`, so restarts are instant.

Command-line flags (`-port`, `-server`, `-o`...) override the file for one run.
So do environment variables named `MYMULE_` plus the setting in capitals
(`MYMULE_WEB_UI=true`, `MYMULE_UPLOAD_LIMIT=500`), which is how the
[Docker](INSTALL.md#docker) image is configured; they are not written into the file.
Set `MYMULE_HOME` to keep data somewhere other than the executable's folder,
for example when the binary is installed in a read-only location.

Most of these settings can also be changed from the web interface
(**Settings** → **Advanced settings**); see the [web interface guide](WEBUI.md).

## HighID

Peers that cannot reach you only serve you if they can reach you instead,
so forwarding the TCP port (`port` in the config) to your machine gets you
far more sources. `mymule status` shows whether you have HighID or LowID.
If your router allows UPnP or NAT-PMP, MyMule opens the TCP port and the
Kad UDP port on it by itself (and closes them when it stops); the log
says whether it could. Otherwise forward them by hand, or set
`upnp = false` to never try. In Docker this only works with host
networking.
If you have a listen port configured but get LowID, MyMule tries other
servers, since some servers never test the port.

## Kad

Kad is eMule's serverless network. With `kad = true` (the default),
MyMule joins it as a full node, as eMule does:

- global searches (web interface, API, *arr apps) ask Kad too, and every
  download looks for sources there once an hour, so files only found on
  Kad (marked `[kad ...]` by some servers) download as well;
- your shared files are published on Kad, so Kad users find them;
- MyMule keeps what other users publish near its Kad ID and answers
  their searches.

The first time, MyMule gets a list of Kad nodes from `kad_nodes_url`
(the address eMule offers, over HTTPS); later it remembers them in
`nodes.dat`. With `kad_nodes_url` empty it starts from the eMule clients
it meets instead. Nodes from a downloaded list are only used, or passed
on to others, once they answer MyMule with its key.

Forward the Kad UDP port (`kad_port`, 4672 by default) in your router,
as well as the TCP port. Without them Kad still works, behind a firewall:
other nodes reach you through a "buddy" node, or by asking you to connect
to them, and MyMule stores nothing for others (it could not be searched).
`mymule status`, `mymule top` and the web interface show Kad's state.

## How MyMule works with the eMule network

- Search on the connected server, or on every known server at once
  (`search -global`, over UDP); a server that stops answering doesn't
  leave the search empty
- Downloads from eD2K peers, with MD4 verification of every 9.28 MB part:
  corrupt parts are downloaded again, and a peer that keeps sending bad
  data is not used for that file any more. With AICH (an `h=` in the
  link, or a hash enough peers agree on), only the bad 180 KB blocks of
  a corrupt part are downloaded again; MyMule gives other clients the
  AICH data of its shared files too
- Sources from every server in the list (UDP), not only the connected one
- Opens its ports on the router by itself (UPnP or NAT-PMP) when the
  router allows it
- IP filter: eMule's `ipfilter.dat` (or a list downloaded every week from
  `ip_filter_url`) blocks connections from and to the addresses in it
- Wish list: MyMule searches by itself for the movies and episodes you
  want and downloads them when a good file shows up, starting the day they
  come out at home (digital or disc) or air, not while they are only in
  cinemas; or follow a whole series, from the season you choose: every
  episode, already aired or still to come
- Files that cannot exist yet are marked as fakes and never recommended:
  a web, disc or TV copy of a movie before it is out at home, or any file
  of an episode before it airs
- Ratings and comments that eMule users leave on files (from Kad) shown
  next to the files found for a movie or series, so fakes stand out; the
  wish list never downloads a file people call fake; rate your own downloads too,
  and your rating is published in Kad for others, as eMule does
- Notifications when a download finishes, the wish list finds something
  or something fails: ntfy, Telegram, Discord or any webhook; and
  Jellyfin, Emby or Plex told to scan their libraries when files arrive
- Download priorities (high, normal, low), and "first and last parts
  first" to watch a video before it is complete
- Compressed transfers, as eMule's: blocks of files that compress (not
  videos, music or archives) travel compressed, both ways, with clients
  that support it
- eMule collections (`.emulecollection`): `mymule add` or the web
  interface adds every file in them
- Categories: named folders (music, books, software...) for downloads
  that are not movies or episodes; choose one when adding a download and
  the file is moved there when it finishes
- Download and upload speed limits, and a speed schedule (other limits
  at some hours, e.g. none at night); queue places asked over UDP, as eMule
  does, instead of a new connection each time
- Source exchange with other eMule clients: the peers you download from
  tell you where else the file is, and you tell them (only sources that
  answered, with a public address, a limited number and not too often)
- Kad, eMule's network without servers: searches and sources there too,
  your shared files published, and a full Kad node for others (see
  [Kad](#kad))
- Sharing and uploads: shared folders, completed downloads and the verified
  parts of partial downloads; upload queue, slots taken in turns (10 MB
  or 15 minutes each), speed limit
- eMule's credit system: peers that uploaded to you move up in your upload
  queue, and MyMule proves who it is (secure identification) so that the
  eMule and aMule users you upload to do the same for you. Credits only go
  to peers that prove their identity; they are kept in `credits.dat`, and
  your key in `cryptkey.dat` (keep it: with a new one, peers no longer
  recognize you)
- Protocol obfuscation, as eMule's: connections to other users and to
  servers look like random data, which hides eMule traffic from internet
  providers that slow it down (`obfuscation` in `mymule.conf`). It does not
  make you anonymous
- Resume after a stop, a crash or a reboot
- HighID support (listen port) and server callbacks to reach firewalled peers
- Runs as a background daemon, controlled with short commands
  (`status`, `add`, `pause`, `cancel`...) over a local, token-protected API
- `mymule top`: live full-screen view; search, pick what to download and
  pause, resume or cancel downloads without leaving it
- Server list with users and files; switch server or choose one to always
  use (web interface)
- Optional web interface (downloads, search, sharing) that works on phones
  too; off by default
- HTTP API for scripts, and optional Radarr/Sonarr/Lidarr/Readarr/Prowlarr
  support (Torznab indexer + qBittorrent-compatible client)
- Light: about 15 MB of RAM
- Portable: settings and data live next to the executable
- Built to face hostile peers and servers: everything they send is
  checked and bounded (see [Security](#security))

## Security

MyMule talks to strangers: servers and peers can send anything. Everything
they send is checked and bounded: packet and message sizes, tag counts,
decompression, file sizes (256 GiB at most), the number of connections,
sources and queue places per address, and where MyMule will connect to
(only public addresses, never your own network). Data written to a part
that was already verified is refused, and every finished file is checked
against its hash once more before it is handed over. Kad packets are
checked the same way, and what other nodes can make MyMule store, send or
connect to is limited (in bytes, packets and connections, for each
address and in all). Storing what a node publishes needs proof of its
address; the few requests that make MyMule send or connect without it
(firewall checks and callbacks for other nodes, as in eMule) only reach
public addresses, a few at a time. A bug triggered by
what a peer sends ends that connection, not the program. Text from the
network (file names, nicks, messages) is cleaned before it reaches the
terminal or the log.

What is up to you:

- **Keep the API private.** Leave `api_listen` on `127.0.0.1` unless you
  need the web interface or the *arr apps from another device, and never
  forward port 4711 in your router. The `api_key` gives full control of
  MyMule: treat it like a password and keep the generated random one.
- **Keep MyMule's folder private.** Its files (`mymule.conf` with the
  `api_key`, the log, the list of shared files) are created readable only
  by you, and MyMule never shares them. On Windows, keep the MyMule folder
  inside your user profile (for example in Documents), not directly under
  `C:\`, where other users of the computer could read the key or replace
  `mymule.exe`; MyMule warns you if it is elsewhere.
- **Movie and series folders** can't be a drive root, your user folder,
  a system folder or MyMule's own folder, and only video, audio and
  subtitle files are moved into them or shared from them.
- **Folders that get any file**: category folders, and the download,
  temporary and shared folders when changed from the web interface,
  follow the same rules and must also be inside MyMule's folder,
  `download_dir` or a folder listed in `folder_roots`, which only
  `mymule.conf` can change. Files from the network keep the names peers
  gave them, so in the wrong folder (a web server's, one the system runs
  files from at login...) they could be run. Hidden files (`.something`)
  are never moved into a category's folder.
- **Notifications leave your computer.** With `notify_urls` set, the names
  of finished downloads (and what the wish list found) are sent to the
  services you list there (ntfy, Telegram, Discord, your webhook). Use
  your own ntfy server or a topic nobody can guess.
- **Remember eD2K is public**: other users see your IP address and the
  files you share. Finished downloads are shared by default, as in eMule
  (`share_downloads = false` to stop).

To report a security problem, please open a GitHub issue asking for a
private contact rather than describing it publicly.
