# ssmon – SpyServer listener monitor

**English** | [Čeština](README.cs.md)

**ssmon** records who listens to a public [Airspy SpyServer](https://airspy.com/), where they
connect from, when, for how long and in which band. It shows everything on a web dashboard with a
map and statistics.

ssmon does not sit in SpyServer's data path. It passively reads the status reports that SpyServer
sends to the public Airspy directory every ~15 s. If ssmon crashes, SpyServer and its listeners keep
running.

- single static Go binary (no CGO, no runtime dependencies, Leaflet embedded),
- data stored in SQLite, statistics computed by SQL queries on the server (fast even with years of
  history),
- offline IP geolocation (DB-IP Lite / GeoLite2 MMDB files), no external APIs,
- a kernel BPF filter passes only report packets to the program, so the IQ stream on the same
  interface costs nothing,
- runs as an unprivileged user with a single capability (`CAP_NET_RAW`), ~10–20 MB RAM,
- bilingual dashboard (English / Czech), switchable with flags.

---

## Contents
1. [How it works](#how-it-works)
2. [Limitations](#limitations)
3. [Requirements](#requirements)
4. [Building](#building)
5. [Manual test](#manual-test)
6. [Production install](#production-install)
7. [The ssmon.service file](#the-ssmonservice-file)
8. [Options](#options)
9. [GeoIP databases and updates](#geoip-databases-and-updates)
10. [Bands (bands.txt)](#bands-bandstxt)
11. [Dashboard](#dashboard)
12. [Database](#database)
13. [Maintenance and upgrades](#maintenance-and-upgrades)
14. [Troubleshooting](#troubleshooting)
15. [Project layout](#project-layout)
16. [Licenses and attribution](#licenses-and-attribution)

---

## How it works

```
listener ──► VPS (public IP) ──WireGuard──► RPi + SpyServer
                   │                              │
                   │◄──── POST /register ─────────┘  (every ~15 s)
                   │      directory.api.airspy.com:8080
                 ssmon (passively reads the reports on wg0)
                   │
                 SQLite ──► dashboard :8090
```

With `list_in_directory = 1`, SpyServer sends a plain HTTP report to the Airspy directory
(`POST /register`, `Content-Type: application/x-sdr-server-status`) in `Key=Value` format:

```
ServerVersion=2.0.1921
MaxClients=1
CurrentClientCount=1
CurrentClients=203.0.113.45
CurrentCenterFrequency=145387999
AntennaLocation=50.0755, 14.4378
...
```

ssmon captures these packets through an `AF_PACKET` socket on the chosen interface (typically the
WireGuard tunnel between the VPS and the RPi) and builds **sessions** from them.

### Running directly on the SpyServer host (no tunnel)

The WireGuard/VPS setup is not required. ssmon only needs to see SpyServer's reports to the directory
(`POST /register` to TCP port 8080) on the interface it watches. If SpyServer has a public IP and ssmon
runs on the same machine, point `-iface` at the machine's network interface. Outgoing packets are
captured too.

```sh
ip -br link                                   # find the interface, e.g. eth0, enp1s0, end0
sudo tcpdump -i eth0 -nn 'tcp port 8080'      # a POST /register must appear within ~15 s
```

What to change:
- `-iface eth0` (your interface name) instead of `-iface wg0`, both in a manual run and in
  `ssmon.service`.
- In `ssmon.service`, `wg-quick@wg0.service` can be removed from `After=` (it is harmless if left in).
- Build for the host's architecture (`uname -m`): `x86_64` → `GOARCH=amd64`, `aarch64` → `GOARCH=arm64`,
  32-bit Raspberry Pi OS (`armv7l`) → `GOARCH=arm GOARM=7`.

Notes:
- Only IPv4 is captured. If the reports leave the host over IPv6, ssmon will not see them (check with
  `tcpdump` as above).
- `-iface any` is not supported; use a specific interface. If SpyServer runs in a network namespace
  or a container, use the interface inside it (or its veth peer on the host).

### Only changes are stored, not every report

| event in the report | database write |
|---|---|
| a new IP appears | **new row** in `sessions` (IP, start, geolocation) and the first row in `tunings` |
| same IP, same frequency | only an **UPDATE** of `last_seen` and the report counter (the file does not grow) |
| same IP, different frequency | **new row** in `tunings` (time and frequency) |
| the IP disappears, or no report for 45 s | the session is closed (`open = 0`) |
| report without a client | the single server-status row is overwritten (online/offline) |

Example: a listener stays connected for 3 hours and retunes four times. That is 720 reports, but
only **1 session row and 5 tuning rows** end up in the database. The session length is
`last_seen − start`, and the time on each frequency comes from the differences between tuning
timestamps.

---

## Limitations

- **The frequency is the receiver's center frequency** (`CurrentCenterFrequency`), not the
  listener's VFO. The listener may be listening anywhere within the displayed bandwidth around the
  center. Frequencies are therefore aggregated and shown **in whole MHz only** (145.000–145.999 =
  "145 MHz"). The band, however, is always derived from the exact value.
- **Initial frequency:** after connecting, SpyServer tunes the client to `initial_frequency`. With
  `-initial-mhz`, the first tuning of a session on that frequency is left out of the statistics. It
  stays visible in the tuning history, marked as the server's initial frequency.
- **Time resolution is ~15 s** (the report interval). Sessions shorter than one interval may not
  show up at all, and durations are approximate.
- **`MaxClients=1` is assumed**, i.e. a single IP in `CurrentClients`.
- The client software version is not included in the reports.
- IPv4 only (reports and clients).

---

## Requirements

**Server (where ssmon runs):**
- Linux with systemd (tested on Ubuntu), amd64 or arm64,
- the interface carrying SpyServer's reports to the directory (e.g. `wg0` on the VPS),
- `curl` and `gunzip` for downloading the GeoIP databases,
- optionally `sqlite3` for your own queries and backups.

**Building:** Go ≥ 1.22 on any machine (Windows, Linux, macOS). Building on a small VPS with 1 GB
RAM is not recommended, because compiling the SQLite driver needs a lot of memory. Build elsewhere
and copy the binary.

---

## Building

On the first build, `go mod tidy` downloads the dependencies and creates `go.sum`. Commit
`go.sum` to the repository.

### Windows (PowerShell)

Install Go with the `.msi` installer from <https://go.dev/dl/> (or `winget install GoLang.Go`),
then **open a new** PowerShell window.

```powershell
cd C:\path\to\ssmon            # folder containing go.mod
go mod tidy
$env:CGO_ENABLED="0"; $env:GOOS="linux"; $env:GOARCH="amd64"
go build -trimpath -ldflags="-s -w" -o ssmon .
```

The result is a Linux binary named `ssmon` (no extension). Copy it to the server with
`scp ssmon user@server:~/` or WinSCP.

### Linux

```sh
# Ubuntu 24.04+: sudo apt install golang-go
# older distros: official tarball from https://go.dev/dl/
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o ssmon .
```

### Target architecture

`uname -m` on the server: `x86_64` → `GOARCH=amd64`, `aarch64` → `GOARCH=arm64`.

### SQLite driver

| driver | build | note |
|---|---|---|
| `modernc.org/sqlite` (default) | `CGO_ENABLED=0 go build …` | pure Go, static binary, easy cross-compiling |
| `github.com/mattn/go-sqlite3` | `CGO_ENABLED=1 go build -tags cgo_sqlite …` | needs gcc, links glibc dynamically |

---

## Manual test

Before installing the service, check that ssmon can see the reports.

```sh
# find the interface toward the SpyServer (WireGuard to the RPi)
sudo wg show | grep interface

chmod +x ssmon update-geoip.sh

# GeoIP databases + band plan (one-time)
sudo apt install -y curl
sudo mkdir -p /opt/ssmon
sudo install -m 755 update-geoip.sh /opt/ssmon/
sudo install -m 644 bands.txt /opt/ssmon/
sudo /opt/ssmon/update-geoip.sh

# allow the dashboard only from your own IP
sudo ufw allow from <your-IP> to any port 8090 proto tcp

# test run (database in the current directory, NOT in /tmp - see Troubleshooting)
sudo ./ssmon -iface wg0 -db ./test.db -listen :8090 \
  -initial-mhz 145 -bands /opt/ssmon/bands.txt \
  -geoip-city /opt/ssmon/geoip/dbip-city-lite.mmdb \
  -geoip-asn /opt/ssmon/geoip/dbip-asn-lite.mmdb
```

Expected output:
```
ssmon: loaded 22 bands from /opt/ssmon/bands.txt
ssmon: geoip: loaded city database /opt/ssmon/geoip/dbip-city-lite.mmdb (DBIP-City-Lite)
ssmon: geoip: loaded ASN database /opt/ssmon/geoip/dbip-asn-lite.mmdb (DBIP-ASN-Lite ...)
ssmon: database ./test.db: 0 sessions
ssmon: dashboard on :8090
ssmon: capturing directory reports on wg0, TCP port 8080
```

Then connect to your SpyServer (SDR++, SDR#…). Within 15 s you should see
`session 1 open: <IP> (<city>, <country>, AS… …)`, and `http://<server>:8090/` shows you as the
current listener. After you disconnect, `session 1 closed` appears within 15–45 s. Stop the test
with Ctrl+C and delete the test database (`rm -f test.db*`).

To run the binary without `sudo`, give it the capability:
`sudo setcap cap_net_raw+ep ./ssmon`.

---

## Production install

```sh
sudo install -m 755 ssmon /usr/local/bin/ssmon
sudo mkdir -p /opt/ssmon
sudo install -m 755 update-geoip.sh /opt/ssmon/
sudo install -m 644 bands.txt /opt/ssmon/
sudo /opt/ssmon/update-geoip.sh               # if not done during the test

sudo cp ssmon.service /etc/systemd/system/     # edit -iface etc. if needed
sudo systemctl daemon-reload
sudo systemctl enable --now ssmon

systemctl status ssmon --no-pager
journalctl -u ssmon -f
```

Monthly GeoIP update (`sudo crontab -e`):
```
17 4 3 * * /opt/ssmon/update-geoip.sh
```

The service starts at boot and restarts within 5 s after a crash. The database lives in
`/var/lib/private/ssmon/ssmon.db` (see [Database](#database)).

---

## The ssmon.service file

```ini
[Unit]
Description=SpyServer listener monitor (passive directory report sniffer + dashboard)
After=network-online.target wg-quick@wg0.service   # start after network and WireGuard
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/ssmon \
    -iface wg0 \                          # interface with SpyServer -> directory traffic
    -db /var/lib/ssmon/ssmon.db \         # SQLite DB (physically in /var/lib/private/ssmon)
    -listen :8090 \                       # dashboard on port 8090, all interfaces
    -initial-mhz 145 \                    # SpyServer initial_frequency (not counted)
    -bands /opt/ssmon/bands.txt \         # band plan
    -geoip-city /opt/ssmon/geoip/dbip-city-lite.mmdb \
    -geoip-asn /opt/ssmon/geoip/dbip-asn-lite.mmdb
ExecReload=/bin/kill -HUP $MAINPID        # systemctl reload = reload GeoIP + backfill
Restart=always                            # restart on crash
RestartSec=5
```
*(The comments above are for explanation only. A comment is not allowed after `\` in `ExecStart`.)*

**Hardening** (the `[Service]` section):

| directive | meaning |
|---|---|
| `DynamicUser=yes` | runs as a throwaway unprivileged user created by systemd |
| `StateDirectory=ssmon` | systemd creates a writable `/var/lib/ssmon` (physically `/var/lib/private/ssmon`) |
| `AmbientCapabilities=CAP_NET_RAW` | the only privilege: capturing packets from an interface |
| `CapabilityBoundingSet=CAP_NET_RAW` | no other capability can be gained |
| `NoNewPrivileges=yes` | no privilege escalation |
| `ProtectSystem=strict` | the whole system is read-only (except `StateDirectory`) |
| `ProtectHome=yes`, `PrivateTmp=yes`, `PrivateDevices=yes` | no access to /home, private /tmp, no devices |
| `ProtectKernelTunables/Modules`, `ProtectControlGroups` | no changes to the kernel or cgroups |
| `RestrictAddressFamilies=AF_PACKET AF_NETLINK AF_INET AF_INET6 AF_UNIX` | allowed socket types: capture (`AF_PACKET`), interface lookup (`AF_NETLINK`), web and DNS |
| `MemoryMax=200M` | safety net against memory leaks |

After any change to the file: `sudo systemctl daemon-reload && sudo systemctl restart ssmon`.

---

## Options

| option | default | meaning |
|---|---|---|
| `-iface` | `wg0` | interface carrying SpyServer's reports (empty = dashboard only, no capture) |
| `-dir-port` | `8080` | TCP port of the Airspy directory |
| `-db` | `/var/lib/ssmon/ssmon.db` | SQLite database file |
| `-listen` | `:8090` | dashboard address (`:9000`, `10.77.77.1:8090`; empty = no dashboard) |
| `-geoip-city` | – | City MMDB database (DB-IP Lite City or GeoLite2-City) |
| `-geoip-asn` | – | ASN MMDB database (DB-IP Lite ASN or GeoLite2-ASN) |
| `-initial-mhz` | `0` | SpyServer's `initial_frequency` in MHz; a session's first tuning there is not counted (0 = off) |
| `-bands` | – | band plan file; without it the built-in table is used |
| `-interval` | `15s` | SpyServer report interval (used for durations and online status) |
| `-gap` | `45s` | close a session when no report mentions it for this long |
| `-anonymize-after` | `0` | after N days, truncate IPs to /24 (IPv4) or /48 (IPv6); 0 = never |
| `-pcap` | – | replay a `tcpdump -w` file into the database and exit (for testing) |

**Signals:** `SIGHUP` (`systemctl reload ssmon`) reloads the GeoIP databases and fills in location
data for sessions that lack it. `SIGTERM`/`SIGINT` shut down cleanly.

---

## GeoIP databases and updates

ssmon reads MMDB files directly (its own reader, no libraries, no online API). The
`update-geoip.sh` script downloads the current **DB-IP Lite City** and **DB-IP Lite ASN** databases
(free, CC BY 4.0), stores them in `/opt/ssmon/geoip/` and runs `systemctl reload ssmon`.

```sh
sudo /opt/ssmon/update-geoip.sh     # manual update
```

DB-IP publishes a new edition at the start of every month, so a cron job on the 3rd of the month
is enough (see above). The script tries both the current and the previous month, so it does not
fail when the new edition is not out yet.

**Backfill:** sessions stored while GeoIP was missing get their location and network filled in
automatically at startup and after every `reload`.

MaxMind GeoLite2 (`GeoLite2-City.mmdb`, `GeoLite2-ASN.mmdb`) works as well. The format is the
same, only the paths change.

---

## Bands (bands.txt)

Format: one line per band, `name; low MHz; high MHz; English name`, `#` starts a comment. The
first name is used in the Czech UI, the optional fourth column in the English UI (without it, the
first name is used in both). **The first match wins**, so narrower bands go above wider ones
(e.g. ISM 433 above 70 cm).

```
# name;               low;    high;   English name
2 m;                  144;    146;    2 m
Letecké (hlas);       118;    137;    Airband (voice)
ISM 433;              433.05; 434.79; ISM 433
70 cm;                430;    440;    70 cm
```

After editing, run `sudo systemctl restart ssmon`. Bands are computed at display time, so changes
apply to all existing data too. Without `-bands`, the built-in table is used (identical to the
included `bands.txt`).

---

## Dashboard

`http://<server>:8090/` (port according to `-listen`). Data refreshes every 15 s.

**Language:** English or Czech, switched with the flags in the top right corner. On the first visit
the language follows the browser (Czech/Slovak → Czech, anything else → English); the choice is
remembered in the browser.

- **server status:** online/offline, time of the last report, current center frequency,
- **listening now:** place, IP, network, frequency and length of the current session,
- **summary numbers:** sessions, unique IPs, countries, total and average listening time,
- **map:** the station and the listeners (marker size by session count, current listener in green),
- **statistics:** countries, bands and frequencies (MHz) by listening time, activity by hour of day
  and by day, networks (ASN) with hosting/cloud networks tagged **DC**,
- **session table:** filter (IP, city, country, network, `AS1234`, MHz, band name), pages of 50,
  the last 6 frequencies in MHz, a **▸ N×** button with the full tuning history, and a tag for IPs
  with 10 or more sessions in the period (typically bots),
- period switch: 24 h / 7 days / 30 days / all.

The dashboard has no login. **Restrict access with a firewall**, e.g.
`ufw allow from <your-IP> to any port 8090 proto tcp`, or bind it to a VPN interface only
(`-listen 10.77.77.1:8090`).

### API

| endpoint | description |
|---|---|
| `GET /api/summary?days=7&tz=3600` | server status and all aggregations (`days=0` = all, `tz` = offset from UTC in seconds) |
| `GET /api/sessions?days=7&q=&limit=50&offset=0` | one page of sessions with a filter |
| `GET /api/session/timeline?id=123` | full tuning history of one session |

All endpoints accept `lang=en` or `lang=cs` (default) for band names.

---

## Database

With `DynamicUser=yes`, the database physically lives in **`/var/lib/private/ssmon/ssmon.db`**
(`/var/lib/ssmon` is only a link to it). Next to it are `ssmon.db-wal` and `ssmon.db-shm` (SQLite
journal); do not delete them while the service is running. The directory is accessible to root only.

| table | contents |
|---|---|
| `sessions` | one row per session: `ip`, `start`, `last_seen` (unix time), `reports`, `open`, `cc`, `country`, `city`, `lat`, `lon`, `asn`, `org` |
| `tunings` | frequency changes in a session: `session_id`, `ts`, `freq` (Hz, exact value from the report) |
| `kv` | the last report (`status`) and the time of the last start |

Example queries (`sudo apt install sqlite3`):
```sh
DB=/var/lib/private/ssmon/ssmon.db
sudo sqlite3 -readonly $DB "SELECT cc, count(*) FROM sessions GROUP BY cc ORDER BY 2 DESC"
sudo sqlite3 -readonly $DB "SELECT ip, org, count(*) n FROM sessions GROUP BY ip ORDER BY n DESC LIMIT 10"
sudo sqlite3 -readonly $DB "SELECT datetime(s.start,'unixepoch','localtime'), s.ip, t.freq/1e6
  FROM sessions s JOIN tunings t ON t.session_id = s.id ORDER BY t.ts DESC LIMIT 20"
```

The data volume is small: tens of rows a day, tens of MB a year even with heavy traffic.

**Online backup:**
```sh
sudo sqlite3 /var/lib/private/ssmon/ssmon.db ".backup /root/ssmon-backup.db"
```

**Privacy:** listener IP addresses are personal data. `-anonymize-after 90` truncates them to /24
after 90 days. Location and network are kept, so the statistics lose nothing important.

---

## Maintenance and upgrades

| task | command |
|---|---|
| new version | `sudo install -m 755 ssmon /usr/local/bin/ssmon && sudo systemctl restart ssmon` |
| change options | edit `/etc/systemd/system/ssmon.service`, then `sudo systemctl daemon-reload && sudo systemctl restart ssmon` |
| change bands | edit `/opt/ssmon/bands.txt`, then `sudo systemctl restart ssmon` |
| update GeoIP | `sudo /opt/ssmon/update-geoip.sh` (normally from cron) |
| log | `journalctl -u ssmon -f` |
| status | `systemctl status ssmon --no-pager` |

The database schema is created at startup if missing. New versions work with existing databases.

---

## Troubleshooting

| symptom | cause and fix |
|---|---|
| `packet socket (needs CAP_NET_RAW): operation not permitted` | started manually without `sudo`. Use `sudo` or `sudo setcap cap_net_raw+ep ./ssmon`. |
| `capture: route ip+net: netlinkrib: address family not supported by protocol` | `AF_NETLINK` is missing from `RestrictAddressFamilies` in `ssmon.service`. |
| `database … is not writable` / `attempt to write a readonly database` | the database belongs to another user. Typically `/tmp/test.db` created without `sudo` (Ubuntu prevents root from writing to other users' files in `/tmp`). Delete it or use a different path. |
| `listen tcp :8090: bind: address already in use` | another process holds the port, often a forgotten manual test. `sudo ss -ltnp \| grep 8090`, then `sudo pkill -f './ssmon'`. |
| `geoip: no database loaded` | the `-geoip-city`/`-geoip-asn` options or the files are missing. Run `update-geoip.sh`. |
| `route ip+net: no such network interface` | wrong interface name in `-iface` (`ip -br link`, `sudo wg show`). |
| dashboard "Offline", no sessions | the reports do not pass through the chosen interface. Check with `sudo tcpdump -i wg0 -nn 'tcp port 8080'` (a `POST /register` must show up within 15 s). Check `list_in_directory = 1` in `spyserver.config`. |
| service "loaded" but the web UI is down | `systemctl status ssmon` shows `activating (auto-restart)`; the cause is in `journalctl -u ssmon -n 30`. |

Testing without live traffic: captured traffic (`sudo tcpdump -i wg0 -w reports.pcap 'tcp port 8080'`)
can be replayed with `./ssmon -pcap reports.pcap -db ./test.db -iface "" -listen ""`.

---

## Project layout

| file | contents |
|---|---|
| `main.go` | options, startup, signals |
| `capture.go` | `AF_PACKET` + BPF filter, pcap replay, TCP reassembly, report parser |
| `monitor.go` | session logic, SQLite schema and writes, anonymization, GeoIP backfill |
| `stats.go` | dashboard queries (aggregations, sessions, tuning history) |
| `web.go` | HTTP server and API, embedded static files |
| `geo.go`, `mmdb.go` | GeoIP: own reader for the MaxMind DB format |
| `bands.go`, `bands.txt` | band plan and its loader |
| `driver_modernc.go`, `driver_cgo.go` | SQLite driver selection (build tag `cgo_sqlite`) |
| `web/index.html` | dashboard (plain HTML/CSS/JS, no framework, EN/CS strings) |
| `web/leaflet/` | Leaflet 1.9.4 (map) |
| `ssmon.service` | systemd unit |
| `update-geoip.sh` | DB-IP Lite download and update |

---

## Licenses and attribution

- **Leaflet** 1.9.4: BSD-2-Clause, see `web/leaflet/LICENSE`.
- **Geolocation:** [IP Geolocation by DB-IP](https://db-ip.com), licensed under
  [CC BY 4.0](https://creativecommons.org/licenses/by/4.0/). Attribution is also shown in the
  dashboard footer.
- **Map tiles:** © [OpenStreetMap](https://www.openstreetmap.org/copyright) contributors. Tiles are
  loaded from `tile.openstreetmap.org`, subject to their
  [tile usage policy](https://operations.osmfoundation.org/policies/tiles/).
- SpyServer and Airspy are products of Airspy.com. ssmon is not affiliated with them.
