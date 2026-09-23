# Deploying smokestack

smokestack is a single static binary with an embedded web interface and a
local SQLite database. There is no runtime, no external database and no
container required. A small VM (1 vCPU, 1 GB RAM, 10 GB disk) comfortably
probes a few hundred targets.

## Contents

1. [Requirements](#1-requirements)
2. [Install in one command](#2-install-in-one-command)
3. [Put it behind HTTPS](#3-put-it-behind-https)
4. [First steps](#4-first-steps)
5. [Updating](#5-updating)
6. [Publishing your own releases](#6-publishing-your-own-releases)
7. [Backups](#7-backups)
8. [Troubleshooting](#8-troubleshooting)
9. [Uninstalling](#9-uninstalling)
10. [Probe isolation and performance](#10-probe-isolation-and-performance)
11. [IPv6 and traceroutes](#11-ipv6-and-traceroutes)
12. [Container image (tests)](#12-container-image-tests)

---

## 1. Requirements

| | |
|---|---|
| OS | Any Linux with systemd: Debian 11+, Ubuntu 20.04+, RHEL/Alma/Rocky 8+ |
| CPU | amd64 or arm64 |
| Network | Outbound ICMP and TCP towards your targets; one inbound HTTPS port |
| Tools | `curl` (or `wget`) and `unzip` (or `python3`) — present on most images |

The probe sends ICMP from the host itself, so **the host's network is what is
being measured**. Run it inside the AS whose latency you want to publish.

## 2. Install in one command

From a release package:

```sh
curl -fsSLO https://github.com/nkglfr/smokestack/releases/latest/download/install.sh
sudo sh install.sh --admin-email noc@example.net
```

Or with a package you already downloaded:

```sh
sudo sh install.sh --package smokestack-1.2.0-linux-amd64.zip --admin-email noc@example.net
```

Already logged in as root, as on a fresh Debian or Ubuntu server? Run the
same commands without `sudo` (a minimal Debian does not even ship it).

It takes under a minute. At the end, the installer prints the back-office
address and the **master account password — write it down, it is shown once**.

No release published yet, or you prefer to build it yourself? Install from
source (Go 1.22 or later):

```sh
apt install -y git make golang-go
git clone https://github.com/nkglfr/smokestack && cd smokestack
make build
sudo sh install.sh --package dist/smokestack --admin-email noc@example.net
```

### What the installer does

| Step | Detail |
|---|---|
| Verifies the package | Binary checksum against the manifest, architecture, then `smokestack selftest` |
| Creates a system user | `smokestack`, no shell, no home |
| Lays out the files | `/opt/smokestack/releases/<version>/` + `current` symlink |
| Writes the configuration | `/etc/smokestack/config.json`, **only if it does not exist yet** |
| Installs two systemd units | `smokestack` (web) and `smokestack-probe` (probe), hardened, see [section 10](#10-probe-isolation-and-performance) |
| Creates the master account | And prints its password |
| Adds a `smokestack` command | Always runs as the service user, even under `sudo` |

Options:

| Option | Default | |
|---|---|---|
| `--package FILE` | latest release | A `.zip` package or a binary you built |
| `--admin-email ADDR` | `admin@<hostname>` | Master account login |
| `--ip IP` | `127.0.0.1` | Listen IP of the web interface: keep it on localhost behind a reverse proxy; `0.0.0.0` (IPv4) or `::` (IPv4 and IPv6) for all interfaces |
| `--port PORT` | `8080` | Listen port |
| `--listen IP:PORT` | — | Older combined form of `--ip` and `--port` |
| `--public-url URL` | — | Public address, used by federation |
| `--yes` | — | Never ask questions (automation). Questions are also skipped when the script does not run in a terminal |
| `--lang CODE` | `en` | Default language of the public pages: `en`, `da`, `de`, `es`, `fr`, `it`, `nb`, `nl`, `pt`, `sv` |
| `--embedded` | — | Run the probe inside the web service (one process, for very small servers) |
| `--no-service` | — | Skip systemd (containers, custom supervisors) |

On a first installation run from a terminal, the installer asks for the
listen IP and port of the web interface; press Enter to keep the default
(`127.0.0.1` and `8080`). It checks that a specific address really exists on
the server, since the service could not start otherwise. There is no question
when `--ip` or `--port` is given, with `--yes`, on an upgrade, or when the
script does not run in a terminal.

Running the installer again on an installed server **upgrades it in place**
and keeps the configuration and the data.

### File layout

```
/opt/smokestack/releases/1.2.0/smokestack    binaries, one folder per version
/opt/smokestack/current -> releases/1.2.0    active version
/opt/smokestack/previous -> releases/1.1.0   rollback target
/etc/smokestack/config.json                  configuration (root:smokestack 0640)
/etc/smokestack/smokestack.env               listen IP and port of the web interface
/etc/smokestack/release-keys.pub             extra trusted release keys
/var/lib/smokestack/                         databases, archive spool, keys, backups
```

Nothing under `/var/lib/smokestack` is ever served over HTTP: the web
interface is embedded in the binary, there is no document root.

## 3. Put it behind HTTPS

By default smokestack listens on `127.0.0.1:8080` (this machine only) and
expects a TLS reverse proxy in front.

### Changing the listen address

The listen IP and port live in `/etc/smokestack/smokestack.env`:

```sh
SMOKESTACK_LISTEN_IP=127.0.0.1
SMOKESTACK_LISTEN_PORT=8080
```

| `SMOKESTACK_LISTEN_IP` | Meaning |
|---|---|
| `127.0.0.1` | this machine only (default, behind a reverse proxy) |
| `0.0.0.0` or `*` | all IPv4 interfaces |
| `::` | all IPv4 and IPv6 interfaces |
| an address, e.g. `192.0.2.10` or `2001:db8::10` | that address only (no brackets needed for IPv6) |

Edit the file, then `systemctl restart smokestack`; the log confirms the
address in use (`journalctl -u smokestack | grep listening`). Re-running the
installer with `--ip` or `--port` rewrites the file; without them it keeps
it. Ports below 1024 work too, the service is allowed to bind them.

The same settings can be given as flags (`-ip`, `-port`) or in
`config.json` (`listen_ip`, `listen_port`). Flags win over environment
variables, which win over `config.json`. An invalid value stops the service
with a message naming the faulty setting.

Exposing the interface directly (`0.0.0.0`) is fine on a trusted network or
for a test. On the Internet, keep `127.0.0.1` and use a reverse proxy: the
session cookie is only marked `Secure` over HTTPS.

**Caddy** (automatic certificates) — `/etc/caddy/Caddyfile`:

```
latency.example.net {
    reverse_proxy 127.0.0.1:8080
}
```

**nginx**:

```nginx
server {
    listen 443 ssl http2;
    server_name latency.example.net;
    # ssl_certificate ... (certbot --nginx)

    client_max_body_size 210m;          # update packages uploaded from the back-office

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
    }
    location /api/v1/live {             # server-sent events
        proxy_pass http://127.0.0.1:8080;
        proxy_buffering off;
        proxy_read_timeout 1h;
    }
}
```

`X-Forwarded-For` is trusted **only when the proxy runs on the same host**
(connection from loopback). If the proxy is on another machine, login rate
limiting applies to the proxy's address.

## 4. First steps

1. Open `https://latency.example.net/admin` and log in with the printed credentials.
2. **Publisher page**: organisation, **AS number**, NOC contact, default language.
   The AS number enables the *Host network* page (RIPEstat + PeeringDB).
3. **Targets and categories**: add your targets; the star marks *critical
   targets*, always shown at the top of the home page. Each target is **public**
   or **private**: a private target is measured like any other and visible in
   the back-office, but never appears on the public pages nor in the public API
   — useful for a test, an internal address or a customer link. Switch it at any
   time with the *Visibility* button in the target list.
4. **Users**: create accounts for your team (roles: viewer, editor, admin, master).
5. **Language**: public pages exist in English (reference), Danish, Dutch, French, German, Italian, Norwegian (Bokmål), Portuguese, Spanish and Swedish. Visitors get their
   browser's language automatically; the default for everyone else is set in
   the *Publisher page*, at install time (`--lang fr`) or from the command line:

   ```sh
   smokestack languages                 # available languages and the current default
   smokestack languages -default fr
   ```

   To add or fix a language without rebuilding, copy `en.json` from the
   repository (`web/i18n/`) to `/var/lib/smokestack/i18n/xx.json`, translate it,
   then click **Reload** in *Instance → Languages*: missing keys fall back to
   English and are listed there.

If you skipped the installer's account creation, the service prints a one-time
setup code: `journalctl -u smokestack | grep setup` — or run
`sudo smokestack user add -email you@example.net`.

## 5. Updating

Every update is a signed `.zip` package. Whatever the method, smokestack:

1. checks the Ed25519 signature, the platform and the binary checksum;
2. runs the new binary's self-test;
3. backs up `config.db`;
4. switches the `current` link atomically and restarts in place;
5. **rolls back by itself** if the new version fails to start three times.

### From the back-office

*Instance → Updates*: upload the package, review version and signature, click
**Install and restart**. The page reloads when the new version answers.
A **Revert to X** button returns to the previous version at any time.

### From the command line

```sh
sudo smokestack update smokestack-1.3.0-linux-amd64.zip
sudo smokestack rollback
```

### Automatic updates

Two independent settings, in *Instance → Updates*:

| Setting | Default | What it does |
|---|---|---|
| **Check regularly for new versions** | on | Looks for a new release and shows it in the back-office and the log. Never installs anything. |
| **Install new versions automatically** | off | When a newer release is found, downloads, verifies and installs it by itself. |

**When it checks.** Once 2 minutes after each start of the service, then at
the interval chosen in *Instance → Updates*: every 6 hours, **every day (the
default)**, every week or every month. The setting is read again after each
check, so a change takes effect without restarting. The **Check now** button
checks immediately; like the regular check, it never installs by itself.

**Skipping versions is harmless.** A server checking once a week, or switched
off for a month, installs the newest release directly, whatever versions came
in between: releases are complete binaries, not increments, and database
changes are applied on start whichever version they come from. Tested from a
0.1.0 server that jumped straight to 0.1.4, keeping its data.

**Where versions come from.** `update.manifest_url`, by default the
`latest.json` of the project's latest GitHub release. Pre-releases (a version
with a hyphen, such as `0.2.0-rc1`) are never offered: GitHub leaves them out of
the latest release. To follow your own builds or a mirror, point
`manifest_url` to your own `latest.json`.

**What happens when a new version is found**, with automatic installation on:

1. the log says `new version available: 0.1.1 (current 0.1.0)`;
2. the package for this server's platform is downloaded over HTTPS only, and
   its checksum is compared with the one announced in `latest.json`;
3. the package **must be signed by a trusted key**, without exception: the
   `allow_unsigned` setting only ever applies to manual uploads;
4. the new binary runs its self-test, then `config.db` is backed up to
   `/var/lib/smokestack/backups/`;
5. the service switches to the new version and restarts in place, which takes
   a few seconds. The isolated probe keeps measuring meanwhile, then restarts
   itself on the new version: no measurement is lost;
6. after 60 seconds of normal operation the update is confirmed
   (`update to 0.1.1 confirmed` in the log). If the new version fails to start
   three times, the previous one is restored automatically.

Each automatic update appears in the history of *Instance → Updates* with
**auto** as its author.

**Requirements.** An installation made with `install.sh` (the log says
`in-place updates unavailable` otherwise), and outbound HTTPS to `github.com`
and `release-assets.githubusercontent.com` (where GitHub serves release files).

**Following it.**

```sh
journalctl -u smokestack | grep -iE "new version|update"
```

A failed automatic update is logged as `automatic update to X: <reason>` and
retried at the next check; the running version keeps working.

**Which mode to choose.** Automatic installation suits most servers: releases
are signed, tested before switching, and rolled back if they fail. If you
prefer to decide, keep only the regular check: the back-office then shows
*version X available*, and you install it in one click.

### Configuration (`/etc/smokestack/config.json`)

```json
"update": {
  "enabled": true,
  "auto_check": true,
  "auto_apply": false,
  "check_interval_hours": 24,
  "manifest_url": "https://github.com/nkglfr/smokestack/releases/latest/download/latest.json",
  "trusted_keys_file": "/etc/smokestack/release-keys.pub",
  "allow_unsigned": false
}
```

| Field | Meaning |
|---|---|
| `enabled` | `false` forbids any in-place update: binaries then only change when you re-run the installer |
| `auto_check`, `auto_apply` | initial values of the two settings above; once changed in the back-office, the back-office value wins |
| `check_interval_hours` | hours between two checks, 1 to 720 (default 24). The back-office value wins once changed |
| `manifest_url` | where to look for new versions |
| `trusted_keys_file` | extra trusted signing keys, one `ed25519:…` line each, on top of the key built into the binary |
| `allow_unsigned` | accept unsigned packages **uploaded by hand** (tests only); never used by automatic updates |

After editing the file, restart the service: `systemctl restart smokestack`.

### Notes on versions

- **0.1.2** introduces `/etc/smokestack/smokestack.env` for the listen address.
  Versions 0.1.1 and earlier do not read it: after a rollback to one of them,
  the service would listen on the address in `config.json` (`listen`), or on
  the default `127.0.0.1:8080`.
- **0.1.1** contains the same code as 0.1.0 (it was tagged before the changes
  were pushed). Use 0.1.2 or later.

## 6. Publishing your own releases

Releases are built and signed by GitHub Actions when you push a tag.

**Once** (on any machine with Go, a GitHub Codespace works well):

```sh
make release-key                 # creates release.key (secret) and prints the public key
                                 # without make: go run . release keygen -out .
```

- append the printed `ed25519:...` line to `release.pub` and commit it: every
  binary built afterwards trusts that key;
- in the GitHub repository settings, add the **secret**
  `SMOKESTACK_RELEASE_KEY` with the content of `release.key`, and the
  optional **variable** `OFFICIAL_URL` with your website address (the
  footer links to the GitHub repository until you set it);
- store `release.key` offline and delete it from the build machine.
  **Never commit it** (it is in `.gitignore`).

**Each release:**

```sh
git tag v1.3.0 && git push origin v1.3.0
```

A tag with a hyphen (`v0.2.0-rc1`) is published as a **pre-release**: it can
be downloaded and installed by hand, but servers with automatic updates never
install it. Use this to test a version before offering it to everyone.

Pushing the tag is all it takes. Do **not** create the release with the
*Create a new release* button of the GitHub interface: the workflow creates
it itself and would fail if it already exists. Follow the build in the
**Actions** tab; when it is green, the release page lists the two packages,
`install.sh`, `latest.json` and `SHA256SUMS`.

The `release` workflow runs the tests, builds linux amd64 and arm64, signs
the packages and publishes them with `latest.json`, `install.sh` and
`SHA256SUMS`. Instances with automatic updates install it within
`check_interval_hours`.

To build locally instead: `make dist OFFICIAL_URL=... REPO_URL=...`.

**Dependencies** are kept current by Dependabot (weekly pull requests for Go
modules and CI actions). CI runs the tests on each; merge, then tag.

**Rotating the key:** generate a new pair, add the new public key to
`release.pub` next to the old one, release once, then remove the old key in
the following release.

## 7. Backups

| What | Where | How |
|---|---|---|
| Configuration, users, targets | `/var/lib/smokestack/config.db` | `sqlite3 config.db ".backup /backup/config.db"` |
| Recent measurements | `/var/lib/smokestack/metrics.db` | same, or rely on S3 archiving |
| Federation identity | `/var/lib/smokestack/fed.key` | copy it: peers pinned its fingerprint |
| Settings | `/etc/smokestack/` | copy |

`config.db` is also backed up automatically before every update into
`/var/lib/smokestack/backups/` (last five kept).

## 8. Troubleshooting

| Symptom | Check |
|---|---|
| Service does not start | `journalctl -u smokestack -u smokestack-probe -n 50` |
| Probe log says `the probe needs CAP_NET_RAW` | The probe unit was edited: `AmbientCapabilities=CAP_NET_RAW` is required |
| Is the probe healthy? | Back-office dashboard, *Measurement pipeline* card: last measurement age, dropped measurements |
| All targets at 100 % loss | Outbound ICMP filtered? `ping -c3 1.1.1.1` from the host. The log says `ICMP probe unavailable` if the raw socket was refused. |
| "In-place updates unavailable" | The binary must run from `/opt/smokestack/releases/<v>/`: re-run the installer |
| Package rejected: unknown signature | The signing key is not in `release.pub` nor `/etc/smokestack/release-keys.pub` |
| Upload fails behind nginx | `client_max_body_size 210m;` |
| Lost the admin password | `sudo smokestack user add -email other@example.net` creates another master |
| Health check | `curl -s http://127.0.0.1:8080/healthz` → `ok` |
| Interface unreachable from a browser | By default it listens on `127.0.0.1` only: set `SMOKESTACK_LISTEN_IP=0.0.0.0` in `/etc/smokestack/smokestack.env` and restart, or use a reverse proxy. Check the firewall too. |
| `listen address: ... invalid` in the log | Fix the value named in the message in `/etc/smokestack/smokestack.env` |
| `curl: (22) ... 404` when downloading `install.sh` | No release has been published yet: install from source (section 2) or publish one (section 6) |
| `sudo: command not found` | You are root already: run the command without `sudo` |

## 9. Uninstalling

```sh
sudo sh install.sh --uninstall           # keeps /etc/smokestack and /var/lib/smokestack
sudo sh install.sh --uninstall --purge   # removes everything, including measurements
```

## 10. Probe isolation and performance

A latency probe must measure the network, not the load of the server it
runs on. smokestack runs the probe as **a separate process** by default:

| | `smokestack` (web service) | `smokestack-probe` (probe) |
|---|---|---|
| Role | Web interface, API, database, archive | Sends probes, timestamps replies |
| Raw sockets | **no** (`CAP_NET_BIND_SERVICE` only) | `CAP_NET_RAW` only |
| CPU priority | `Nice=5`, `CPUWeight=50` | `Nice=-5`, `CPUWeight=1000` |
| Talks to | the database | the web service, over a Unix socket |

Design points:

- **Kernel timestamps.** ICMP replies are timestamped by the kernel on
  arrival (`SO_TIMESTAMPNS`); TCP handshakes are read from `TCP_INFO`. The
  process's own scheduling delay never adds to a measured round-trip time.
- **The probe never waits.** It reads targets from memory and hands results
  to a non-blocking queue; a single writer stores them in batches on a
  dedicated database connection, separate from the pool serving web reads.
- **Resilient.** If the web service restarts (update, crash), the probe keeps
  measuring and buffers results in memory (up to ~1 h for 1,000 targets).
  After an update, the probe restarts itself on the new version.
- **Fast pages.** The home page overview is computed in the background every
  30 s and served pre-compressed; the target tree and time series are cached;
  responses are gzip-compressed; the API is rate-limited to 20 requests/s per
  client IP (bursts of 80); heavy reads are queued rather than piling up.

### Measured effect

Benchmark on **one CPU core**, 300 targets with history, 8 web clients
saturating the CPU, and 5 loopback targets whose true round-trip time is
about 0.03 ms (any excess comes from the software, not the network):

| Loopback RTT under web load | Before | Embedded probe | Isolated probe (default) |
|---|---|---|---|
| Median | 1.708 ms | 0.017–0.024 ms | **0.016 ms** |
| p95 | 2.467 ms | 0.15–0.17 ms | **0.020 ms** |
| Worst | 50.5 ms | 3–7 ms | **0.041 ms** |
| API p95 | 9–12 ms | 10–15 ms | **9–14 ms** |

The embedded mode (`--embedded`) is still far better than before, but keeps
millisecond-level outliers under heavy load because the send time is taken
in user space. Prefer the default isolated mode for public measurements.

To switch an existing installation, set `"mode": "external"` in the
`probe` section of `/etc/smokestack/config.json` and re-run the installer.

## 11. IPv6 and traceroutes

**IPv6.** Each target has an address family: *auto* (a literal address decides,
a name tries IPv4 then IPv6), *IPv4 only* or *IPv6 only*. To follow a
destination over both families, create two targets with the same host. The
host needs IPv6 connectivity; without it, the probe logs `IPv6 unavailable`
and keeps running on IPv4.

**Traceroute on anomalies.** After each pass, the probe compares loss and
median with the target's own moving baseline. When a target leaves it (loss
above 3 %, or median above 1.4 × baseline and at least 1 ms more), the probe
runs an ICMP traceroute and stores it. Safeguards: at most one anomaly
traceroute every 15 minutes per target, 30 per hour in total, one at a time,
on dedicated sockets so the other measurements are not disturbed. A reference
path is also recorded once a day while each target is healthy, and the
back-office (*Monitoring → Traceroutes*) highlights the routers that changed
or stopped answering compared with it. Hops are enriched with reverse DNS and
origin AS (Team Cymru DNS service).

Traceroutes are **not public by default**, since hops reveal the inside of your
network; enable them in *Instance → Publisher page*.

```json
"probe": {
  "mode": "external",
  "traceroute": { "disabled": false, "max_hops": 30, "reference_hours": 24, "per_hour": 30 }
}
```

Firewall: the probe must receive ICMP *Time Exceeded* and *Destination
Unreachable* messages (a stateful firewall lets them through as related
traffic), and resolve DNS for the hop enrichment.

## 12. Container image (tests)

An image is published with every release:
`ghcr.io/nkglfr/smokestack:latest`.

> **Read this first.** A container is fine to try smokestack out or if you
> already run everything in containers. For **published measurements, prefer
> the native install**: containers add layers that end up in the numbers.
>
> - **Use the host network** (`network_mode: host`). With Docker's default
>   bridge, every probe crosses a virtual bridge and NAT: tens of
>   microseconds of extra delay, jitter under load, a first traceroute hop
>   that is the Docker bridge instead of your router, and no IPv6 (bridge
>   networks have it off by default).
> - **On macOS and Windows**, Docker runs containers inside a virtual
>   machine: you would measure that machine, not your network. Fine to see
>   the interface, useless for real measurements.
> - **No in-place updates**: the image is immutable, so signed updates and
>   the automatic rollback do not apply. You update by pulling a new image.
> - The native install also gives hardened systemd units and a probe process
>   that is isolated and prioritised on the CPU.

```sh
curl -fsSLO https://raw.githubusercontent.com/nkglfr/smokestack/main/docker-compose.yml
docker compose up -d
docker compose logs | grep "setup code"     # to create the first account
```

Or without compose:

```sh
docker run -d --name smokestack \
  --network host --cap-add NET_RAW \
  -e SMOKESTACK_LISTEN_IP=127.0.0.1 -e SMOKESTACK_LISTEN_PORT=8080 \
  -v smokestack-data:/var/lib/smokestack \
  ghcr.io/nkglfr/smokestack:latest
```

| | |
|---|---|
| Configuration and data | in the `smokestack-data` volume, `/var/lib/smokestack` |
| Listen address | `SMOKESTACK_LISTEN_IP` and `SMOKESTACK_LISTEN_PORT` |
| ICMP | needs `--cap-add NET_RAW`; the image carries the capability on the binary and runs as a normal user |
| First account | `docker compose exec smokestack smokestack user add -config /var/lib/smokestack/config.json -email you@example.net` |
| Update | `docker compose pull && docker compose up -d` |
| Command line | `docker compose exec smokestack smokestack languages` |

The image is built and self-tested on every commit by the CI, and published
for amd64 and arm64 with each release.
