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
| DELETE | `/api/v1/downloads/{hash}` | Cancel (deletes partial data); for a finished download, only removes it from the list |
| POST | `/api/v1/downloads/clear` | Remove finished downloads from the list |
| GET | `/api/v1/search` | Search the eD2K network |

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
  "link": "ed2k://|file|example-distro-1.0-amd64.iso|734003200|0123456789ABCDEF0123456789ABCDEF|/"
}
```

- `state`: `downloading`, `queued` (waiting in peers' upload queues),
  `stalled` (no source sending data yet), `paused` or `finished`.
- `speed` is in bytes per second; `eta` in seconds (`-1` = unknown).
- `path` is set when the download is finished: where the file is.

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
