# smokestack

**Public latency monitoring for network operators.** A modern take on
SmokePing: one binary, fast graphs, exact percentiles, and a federation
between operators who measure each other.

- 📈 **Latency, jitter and loss** to your transits, exchanges, DNS resolvers and clouds
- 🔍 **Zoom from one year down to 30 seconds**, percentiles stay exact at every level
- 🌍 **Public status page** with a private back-office, in English, French, German and Spanish
- 🤝 **Federation**: pair with other operators, measure each other, get notified of incidents
- 📦 **One static binary**, SQLite inside, optional S3 archive, installs in under a minute

---

## Quick start

On any Linux server with systemd (amd64 or arm64):

```sh
curl -fsSLO https://github.com/nkglfr/smokestack/releases/latest/download/install.sh
sudo sh install.sh --admin-email noc@example.net
```

The installer prints the back-office address and the admin password.
Put a TLS reverse proxy in front of it, for example with Caddy:

```
latency.example.net {
    reverse_proxy 127.0.0.1:8080
}
```

Then open `https://latency.example.net/admin`, set your AS number and add
your targets.

📘 Full guide: **[DEPLOY.md](DEPLOY.md)** (HTTPS, updates, backups, troubleshooting).

---

## Why smokestack

| | SmokePing | smokestack |
|---|---|---|
| Graphs | Rendered on the server at every page view | Drawn in the browser, from pre-aggregated data |
| Percentiles over long periods | Averaged (inaccurate) | Merged histograms (exact) |
| Measurements under server load | Skewed | Kernel timestamps, isolated probe process |
| Storage | RRD files | SQLite + optional S3 archive of raw data |
| Deployment | Perl, fping, rrdtool, web server | One binary |
| Updates | Manual | Signed packages, one click, automatic rollback |

---

## How it works

```
                 Unix socket                    HTTPS
 ┌──────────────┐  targets  ┌──────────────────┐  ┌──────────────────┐
 │ smokestack   │◄──────────│ smokestack       │─►│ reverse proxy    │─► visitors
 │ probe        │──────────►│ web service      │  │ (Caddy, nginx)   │
 │ ICMP / TCP   │ results   │ API · UI · SQLite│  └──────────────────┘
 └──────────────┘           └────────┬─────────┘
  CAP_NET_RAW only                   │ hourly raw archive
  high CPU priority                  ▼
                               S3 (optional)
```

- **The probe runs in its own process.** It is the only one allowed raw
  sockets, it has CPU priority, and replies are timestamped by the kernel.
  Heavy traffic on the website cannot distort a measurement.
- **Percentiles are exact at every zoom level.** Each measurement is stored
  as a small mergeable histogram (DDSketch, 1 % relative error), rolled up
  into 1 min, 5 min, 1 h and 1 day buckets.
- **The right resolution is picked automatically** from the time window you
  look at: raw samples for a few hours, daily buckets for years.
- **Disk usage is bounded.** Raw data is archived hourly to S3 or kept
  locally with a quota; long-term aggregates are never evicted.

Benchmark on one saturated CPU core, loopback targets (true RTT ≈ 0.03 ms):
under web load the probe measures **0.016 ms median, 0.041 ms worst**,
versus 1.7 ms and 50 ms with a naive single-process design.
Details in [DEPLOY.md § 10](DEPLOY.md#10-probe-isolation-and-performance).

---

## Features

**Public site**
- Status overview: faults first, critical targets, all categories
- 24-hour status bar and sparkline per target
- Detail view with drag-to-zoom, 1-year navigator, event annotations, permalinks
- Host network page: your AS from RIPEstat and PeeringDB
- Federation page: paired networks and inter-AS latency matrix
- Light and dark themes, responsive, translated

**Back-office**
- Accounts with four roles (viewer, editor, admin, master) and an audit log
- Targets and categories, ICMP or TCP, every 30 s, 1, 5 or 10 min
- Storage settings (local quota or S3), publisher page, languages
- One-click updates with rollback

**Federation**
- Pairing between instances, approved by a human on both sides
- Ed25519-signed messages, fingerprints compared out of band
- Pairings shown publicly only if both operators agree
- NOC alerts only when several independent observers confirm an incident

---

## Updating

Every release is a signed `.zip`. From the back-office (*Instance → Mise à jour*),
the command line, or automatically:

```sh
sudo smokestack update smokestack-1.3.0-linux-amd64.zip
sudo smokestack rollback
```

The package signature, platform and checksum are verified, the new binary
tests itself, the configuration database is backed up, and the switch is
atomic. If the new version fails to start three times, the previous one is
restored automatically.

---

## Configuration

`/etc/smokestack/config.json`, written by the installer:

```json
{
  "listen": "127.0.0.1:8080",
  "data_dir": "/var/lib/smokestack",
  "probe": { "enabled": true, "mode": "external", "slug": "par-01", "name": "Paris" },
  "update": { "auto_check": true, "auto_apply": false }
}
```

Everything else (targets, storage, S3, languages, federation, users) is set
from the back-office.

---

## API

Public, read-only, JSON:

| Endpoint | |
|---|---|
| `GET /api/v1/overview` | Status of every public target |
| `GET /api/v1/tree` | Categories and targets |
| `GET /api/v1/series?target=ID&from=-30h&points=600` | Percentiles and loss over time |
| `GET /api/v1/events` | Annotated events |
| `GET /api/v1/live` | Server-sent events, latest measurements |
| `GET /api/v1/asn` | Host network (RIPEstat, PeeringDB) |
| `GET /api/v1/fed/peers` | Public federation peers |
| `GET /healthz` | Health check |

Rate limit: 20 requests/s per client IP, bursts of 80.

---

## Building from source

Requires Go 1.22.

```sh
git clone https://github.com/nkglfr/smokestack && cd smokestack
go mod tidy
make test
make build                              # ./dist/smokestack
sudo ./install.sh --package dist/smokestack
```

### Releasing

```sh
make release-key                        # once: signing key pair
git tag v1.3.0 && git push origin v1.3.0
```

GitHub Actions builds linux/amd64 and linux/arm64, signs the packages and
publishes the release. Instances with automatic updates install it within a
few hours. See [DEPLOY.md § 6](DEPLOY.md#6-publishing-your-own-releases).

---

## Security

- The web interface is embedded in the binary: no document root, databases
  and keys are never reachable over HTTP.
- The first admin account requires a one-time setup code.
- The web service has no raw-socket privilege; the probe has nothing else.
- Hardened systemd units: read-only system, private `/tmp`, no new privileges.
- Updates must be signed by a trusted Ed25519 key.

---

## Status

Working and tested, not yet 1.0. Known gaps:

- IPv6 probing
- Traceroute triggered on anomalies
- Back-office still in French only (public pages are translated)
- Remote probes on another machine (same-host isolation is done)

Design notes (in French): [docs/DESIGN.fr.md](docs/DESIGN.fr.md).
