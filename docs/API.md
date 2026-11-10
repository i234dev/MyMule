# MyMule HTTP API

MyMule's daemon (`mymule start` or `mymule daemon`) serves a small JSON API
so scripts and other programs can search, add and manage downloads:
PHP, Python, shell scripts, cron jobs, home automation, and so on.

## Setup

The API is configured in `mymule.conf`, next to the executable:

```ini
api_listen = 127.0.0.1:4711
api_key = (a random key, generated on the first run)
```

- **`api_listen`**: where the API listens.
  - `127.0.0.1:4711` (the default): only programs on the same computer.
  - `0.0.0.0:4711`: any device on your network, e.g. a web server or a NAS.
    Use it only on a network you trust, and **never forward this port in
    your router**: the API must not be reachable from the internet.
  - Empty: the API is disabled.
- **`api_key`**: generated randomly on first run, different on every
  installation. Every request must send it. Treat it like a password. You
  can replace it with any random text of 12 characters or more.

Restart the daemon (`mymule stop`, `mymule start`) after changing either.

## Authentication

Send the key in one of these ways (the first one is preferred):

| Method | Example |
|---|---|
| `X-Api-Key` header | `X-Api-Key: YOUR_API_KEY` |
| Bearer token | `Authorization: Bearer YOUR_API_KEY` |
| Query parameter | `?apikey=YOUR_API_KEY` (for tools that only accept a URL) |

Requests without the right key get `401`. All responses are JSON. Errors
look like `{"error": "description"}` with a 4xx or 5xx status.

Two protections to keep in mind when you call the API from a script:

- **Wrong keys block the address.** After 10 wrong keys within 10 minutes
  from one address (any of the ways above, the web login, the
  qBittorrent login), keys from that address are refused for 15 minutes,
  even the right one if it is shorter than 24 characters. A longer key,
  like the generated one, cannot be guessed and keeps working, so nobody
  can lock you out. Requests without any key don't count.
- **Browsers on other sites.** A key sent by a browser on behalf of
  another web site (an image or form on a page you visit, detected by
  the `Sec-Fetch-Site`, `Origin` or `Referer` headers) is counted apart,
  so a page cannot block your address. After 10 wrong keys sent that
  way, only further keys sent that way are refused for 15 minutes, and
  only when the key is shorter than 24 characters. The right long key
  always works. Scripts and programs don't send these headers.
- **Host names.** The API answers to IP addresses, `localhost` and the
  computer's own name. If you call it by another name (a NAS name, a
  domain behind a reverse proxy), add that name to `api_hosts` in
  `mymule.conf`, or requests get `421`. This stops web pages from
  reaching the API through DNS tricks.

## Endpoints

Base URL: `http://127.0.0.1:4711` (or the address you set in `api_listen`).

| Method | Path | What it does |
|---|---|---|
| GET | `/api/v1/version` | Name and version |
| GET | `/api/v1/status` | Server, Kad, speeds, downloads, uploads, shared files |
| GET | `/api/v1/downloads` | All downloads |
| GET | `/api/v1/downloads/{hash}` | One download |
| POST | `/api/v1/downloads` | Add ed2k links |
| POST | `/api/v1/downloads/{hash}/pause` | Pause |
| POST | `/api/v1/downloads/{hash}/resume` | Resume |
| POST | `/api/v1/downloads/{hash}/priority` | `{"priority": "high"}`: `low`, `normal` or `high`; higher priorities get sources first |
| POST | `/api/v1/downloads/{hash}/preview` | `{"preview": true}`: download the first and last parts first |
| DELETE | `/api/v1/downloads/{hash}` | Cancel (deletes partial data); for a finished download, only removes it from the list |
| POST | `/api/v1/downloads/clear` | Remove finished downloads from the list |
| POST | `/api/v1/downloads/collection` | Add every file of an eMule collection: the body is the `.emulecollection` file (or text with ed2k links) |
| GET | `/api/v1/search` | Search the eD2K network |
| GET | `/api/v1/settings` | Every setting of `mymule.conf` (with the web interface on) |
| PUT | `/api/v1/settings` | Change settings in `mymule.conf` (with the web interface on) |
| GET | `/api/v1/wanted` | The wish list (with the web interface on) |
| POST | `/api/v1/wanted` | Add a movie, episode or season to the wish list |
| POST | `/api/v1/wanted/{id}/search` | Search for a wish list entry now |
| POST | `/api/v1/wanted/{id}/date` | `{"date": "2026-12-24"}`: start searching for it that day; `{"date": ""}`: when it comes out |
| DELETE | `/api/v1/wanted/{id}` | Remove a wish list entry |
| GET | `/api/v1/files/{hash}/notes?size=N` | What users say about a file: ratings and comments, from Kad |
| POST | `/api/v1/notify/test` | Send a test notification and ask the media server for a scan |

`{hash}` is the 32-character ed2k hash of the file, as returned in `hash`.

### Download object

```json
{
  "hash": "0123456789ABCDEF0123456789ABCDEF",
  "name": "example-distro-1.0-amd64.iso",
  "size": 734003200,
  "done": 367001600,
  "progress": 0.5,
  "speed": 120000,
  "eta": 3058,
  "sources": 12,
  "downloading_from": 3,
  "queued_at": 5,
  "best_queue_rank": 14,
  "state": "downloading",
  "path": "",
  "link": "ed2k://|file|example-distro-1.0-amd64.iso|734003200|0123456789ABCDEF0123456789ABCDEF|/",
  "priority": "normal",
  "preview": false
}
```

- `state`: `downloading`, `queued` (waiting in peers' upload queues),
  `stalled` (no source sending data yet), `paused` or `finished`.
- `speed` is in bytes per second; `eta` in seconds (`-1` = unknown).
- `path` is set when the download is finished: where the file is.
- `priority`: `low`, `normal` or `high`. When there are more sources than
  connections, those of higher-priority downloads are asked first.
- `preview`: the first and last parts are downloaded first, so a video
  can be played before it is complete.

### Add downloads

JSON body:

```json
{"links": ["ed2k://|file|example.iso|734003200|0123456789ABCDEF0123456789ABCDEF|/"],
 "sources": ["192.0.2.20:4662"]}
```

or a form with one or more `link` fields (and optional `source` fields).
`sources` are extra peers to try, e.g. another computer on your network
that has the file. The reply lists what was added and what failed:

```json
{"added": ["example.iso"], "errors": []}
```

### Search

`GET /api/v1/search?q=example+distro&type=iso&limit=20`

| Parameter | Meaning |
|---|---|
| `q` | Keywords (required) |
| `type` | `video`, `audio`, `image`, `doc`, `archive`, `iso`, `program` |
| `min`, `max` | Size limits in bytes |
| `ext` | File extension, e.g. `pdf` |
| `global` | `true` (default): all known servers, takes a few seconds; `false`: only the connected server |
| `limit` | Maximum number of results |

Results are sorted by number of sources:

```json
{"results": [{"hash": "0123456789ABCDEF0123456789ABCDEF", "name": "example.iso",
  "size": 734003200, "sources": 42, "complete": 40,
  "link": "ed2k://|file|example.iso|734003200|0123456789ABCDEF0123456789ABCDEF|/"}]}
```

Pass `link` to `POST /api/v1/downloads` to download a result.

### All-time totals in the status

`GET /api/v1/status` includes `totals`: what MyMule has sent and
received since it started counting them (they are kept in `stats.json`,
next to `mymule.conf`, across restarts):

| Field | Meaning |
|---|---|
| `uploaded` | Bytes sent to other users, all time |
| `downloaded` | Bytes received from them, all time |
| `since` | When counting started (RFC 3339 time) |

`upload.total` is only since MyMule last started.

### Kad in the status

`GET /api/v1/status` includes `kad` when Kad is on (`kad = true` in
`mymule.conf`):

| Field | Meaning |
|---|---|
| `error` | Only when Kad is on but cannot run (for example its UDP port is in use); the other fields are then empty |
| `connected` | Kad nodes answered in the last 20 minutes |
| `contacts` | Kad nodes known |
| `firewalled` | Other clients cannot connect to your TCP port |
| `udp_firewalled` | Other Kad nodes cannot reach your Kad UDP port |
| `udp_tested` | The UDP test has given a result (until then `udp_firewalled` is a guess) |
| `buddy` | Firewalled, reached through another Kad node that relays callbacks |
| `serving_buddy` | Relaying callbacks for a firewalled node |
| `searches` | Kad lookups running |
| `stored_keywords`, `stored_sources`, `stored_notes` | What other nodes published to you (only when your Kad port is reachable) |

### Router in the status

`GET /api/v1/status` includes `router` when `upnp = true`: which ports
MyMule opened on the router with UPnP or NAT-PMP.

| Field | Meaning |
|---|---|
| `method` | `UPnP` or `NAT-PMP` when the ports are open |
| `ports` | The ports open, such as `TCP 4662` and `UDP 4672` |
| `error` | Why none could be opened (forward them by hand) |

No `method` and no `error`: still trying.

### Settings

`GET /api/v1/settings` returns `file` (the path of `mymule.conf`) and
`settings`, one entry per setting: `key`, `value` (as written in the
file), `default`, `comment` (its explanation), `kind` (`bool`, `int` or
`text`), `locked` and `env`.

`PUT /api/v1/settings` with `{"values": {"upload_limit": "300"}}` writes
those values into `mymule.conf`, keeping the rest of the file and its
comments. If any value is wrong, nothing is written (`400`). Changes take
effect when MyMule restarts.

Settings with `locked` (`api_listen`, `api_key`, `api_hosts`,
`network_hosts`, `web_ui`) decide who can reach MyMule, so they are only
changed in `mymule.conf` itself (`403`); `api_key` is never sent. With
`env`, a `MYMULE_` environment variable overrides the file, so a change
there does not apply while it is set. Settings with `secret`
(`notify_urls`, `media_server_token`) hold tokens: their `value` is never
sent, `set` says whether one is there, and a new value replaces it.

`POST /api/v1/notify/test` (with the web interface on) sends a test
notification and asks the media server for a scan, with the settings
MyMule started with, and answers how each went: `notify` and
`media_server` (`sent` / `scanning`, `off`, or the error).

### Wish list

Movies, episodes and seasons that MyMule searches for by itself and
downloads, as if picked in the web interface, once a file good enough for
the search preferences shows up (never a cinema copy). One search every
10 minutes at most, and each entry every 6 hours, so the servers are not
flooded.

`POST /api/v1/wanted` with `{"work_id": "tmdb-movie-123", "lang": "es-ES"}`
(`work_id` as in `/api/v1/media/search`; for a series also `season` and
`episode`, 0 for a whole season) adds an entry. Each entry has `id`,
`title`, `year`, `kind`, `season`, `episode`, `status` (`wanted`,
`downloading` or `done`), `searches`, `last_search`, and `hash` and
`file` once found.

An entry is not searched for before it comes out at home: `not_before`,
with `not_before_is` saying why (`digital` or `disc` release, `aired` for
an episode, or `estimated` from the cinema release). That date is checked
again every day. `POST /api/v1/wanted/{id}/search` searches for it now
anyway. You can also choose the day yourself: `not_before` (`YYYY-MM-DD`)
when adding it, or `POST /api/v1/wanted/{id}/date` with `{"date":
"2026-12-24"}` later; `not_before_is` is then `manual`, and that date is
not checked again. `{"date": ""}` goes back to the date it comes out. If its download is cancelled, the entry is searched for again and
that file is never taken again.

### File ratings and comments

`GET /api/v1/files/{hash}/notes?size=N` (`N`: the file size in bytes)
asks Kad what eMule users said about the file: `votes` (ratings given),
`fake` (of them, "fake or corrupt"), `average` (1 to 5), `suspect` (at
least half of the ratings say fake) and some `comments` (`rating`,
`comment`). It takes up to about 12 seconds; the answer is kept for an
hour. With Kad off, `kad` is false and there is nothing. The wish list
does not download a file that is `suspect`.

## Examples

### curl

```sh
KEY=YOUR_API_KEY
API=http://127.0.0.1:4711/api/v1

curl -H "X-Api-Key: $KEY" $API/status
curl -H "X-Api-Key: $KEY" "$API/search?q=example+distro&type=iso&limit=5"
curl -H "X-Api-Key: $KEY" -d 'link=ed2k://|file|example.iso|734003200|0123456789ABCDEF0123456789ABCDEF|/' $API/downloads
curl -H "X-Api-Key: $KEY" -X POST $API/downloads/0123456789ABCDEF0123456789ABCDEF/pause
curl -H "X-Api-Key: $KEY" -X DELETE $API/downloads/0123456789ABCDEF0123456789ABCDEF
```

On Windows PowerShell, use `curl.exe` instead of `curl`.

### PHP

```php
<?php
const API = 'http://127.0.0.1:4711/api/v1';
const KEY = 'YOUR_API_KEY';

function mymule(string $method, string $path, ?array $form = null): array {
    $ch = curl_init(API . $path);
    curl_setopt_array($ch, [
        CURLOPT_CUSTOMREQUEST  => $method,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_HTTPHEADER     => ['X-Api-Key: ' . KEY],
        CURLOPT_TIMEOUT        => 60,
    ]);
    if ($form !== null) {
        curl_setopt($ch, CURLOPT_POSTFIELDS, http_build_query($form));
    }
    $body = curl_exec($ch);
    $code = curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    curl_close($ch);
    $data = json_decode($body ?: '{}', true) ?? [];
    if ($code >= 400) {
        throw new RuntimeException($data['error'] ?? "HTTP $code");
    }
    return $data;
}

// Search, then download the result with most sources.
$found = mymule('GET', '/search?' . http_build_query(['q' => 'example distro', 'type' => 'iso', 'limit' => 5]));
if ($found['results']) {
    $best = $found['results'][0];
    mymule('POST', '/downloads', ['link' => $best['link']]);
    echo "Downloading {$best['name']}\n";
}

// Progress of every download.
foreach (mymule('GET', '/downloads')['downloads'] as $d) {
    printf("%-11s %5.1f%%  %s\n", $d['state'], $d['progress'] * 100, $d['name']);
}
```

### Python

```python
import requests

API, KEY = "http://127.0.0.1:4711/api/v1", "YOUR_API_KEY"
s = requests.Session()
s.headers["X-Api-Key"] = KEY

results = s.get(f"{API}/search", params={"q": "example distro", "limit": 5}).json()["results"]
if results:
    s.post(f"{API}/downloads", json={"links": [results[0]["link"]]}).raise_for_status()
for d in s.get(f"{API}/downloads").json()["downloads"]:
    print(f'{d["state"]:<11} {d["progress"]*100:5.1f}%  {d["name"]}')
```

## Notes

- The API only accepts plain HTTP. To reach it from outside your home
  network, use a VPN rather than exposing the port.
- Searches with `global=true` ask every server in the list and take
  about 6 to 10 seconds.
- The CLI (`mymule status`, `mymule add`...) uses a separate, private
  connection to the daemon; it keeps working even when the HTTP API is
  disabled.
