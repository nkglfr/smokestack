# smokestack — design notes

This document explains how smokestack works and why it is built this way.
To install and run it, see [DEPLOY.md](../DEPLOY.md). For an overview, see the
[README](../README.md). A French version is available in
[DESIGN.fr.md](DESIGN.fr.md).

## Contents

1. [Principles](#1-principles)
2. [Processes and background loops](#2-processes-and-background-loops)
3. [Mergeable percentiles](#3-mergeable-percentiles)
4. [Automatic resolution](#4-automatic-resolution)
5. [Storage](#5-storage)
6. [Probe isolation](#6-probe-isolation)
7. [IPv6 and traceroute on anomalies](#7-ipv6-and-traceroute-on-anomalies)
8. [API](#8-api)
9. [Back-office and security](#9-back-office-and-security)
10. [Federation](#10-federation)
11. [Host network (RIPEstat and PeeringDB)](#11-host-network-ripestat-and-peeringdb)
12. [Languages](#12-languages)
13. [Public pages](#13-public-pages)
14. [Updates and releases](#14-updates-and-releases)
15. [Known limits](#15-known-limits)

Proposals under discussion, not implemented, live in [design/](design/): currently [federated double-check](design/federated-double-check.md).

---

## 1. Principles

- **One binary.** The web interface is embedded in the executable. The process
  never serves a file from disk, so the SQLite databases and keys cannot be
  reached through a URL, whatever the reverse proxy configuration.
- **One dependency.** Only `modernc.org/sqlite`, a pure-Go SQLite driver
  (no cgo), which gives a static binary. Everything else uses the standard
  library, including the S3 request signing and the password hashing.
- **Exact statistics.** Percentiles are never averaged; they are recomputed
  from merged histograms at every zoom level.
- **The measurement comes first.** The probe is isolated from the web
  interface so that website traffic can never distort a measurement.

## 2. Processes and background loops

By default two processes run from the same binary:

| Process | systemd unit | Role | Privileges |
|---|---|---|---|
| `smokestack` | `smokestack` | web interface, API, database, archive | no raw socket |
| `smokestack probe` | `smokestack-probe` | sends probes, timestamps replies, runs traceroutes | `CAP_NET_RAW` only, high CPU priority |

They talk over a Unix socket (`probe.sock` in the data directory, mode 0660).
A single-process mode (`"probe": {"mode": "embedded"}`) exists for very small
servers.

**Containers.** The service detects that it runs in a container, and whether
it shares the host's network stack. It reports it in the log and in the
back-office rather than leaving that in the documentation only: a bridged
container measures Docker's NAT as much as the network. The image exists for
tests and container-only setups; the native install stays the reference.

**Listen address.** The web service listens on `127.0.0.1:8080` by default,
behind a reverse proxy. The IP and the port are separate settings, so IPv6
addresses need no brackets: flags `-ip` and `-port`, then the environment
variables `SMOKESTACK_LISTEN_IP` and `SMOKESTACK_LISTEN_PORT` (kept by the
installer in `/etc/smokestack/smokestack.env`), then `config.json`
(`listen_ip`, `listen_port`, or the older combined `listen`). An invalid value
stops the service with a message naming the faulty setting, and the log
states whether the address is reachable from the network.

Background loops in the web service:

| Loop | Period | Role |
|---|---|---|
| writer | 250 ms | stores measurement batches in one transaction |
| rollups | 30 s | cascade samples → 1 min → 5 min → 1 h → 1 day |
| home page overview | 30 s | precomputes the status of every public target |
| archive | 30 s | hourly sealing, S3 upload, quota-based rotation |
| target cache | 10 s, and on change | list of active targets handed to the probe |
| purge | 6 h | applies the retention of each tier |
| update check | 24 h (configurable) | looks for a new signed release (optional) |

In the probe, the scheduler ticks every second. Start offsets are derived
from a hash of the target id, so passes are spread over the interval instead
of all starting on the same second, and stay stable across restarts. Each
pass also starts with a small random delay (up to 950 ms, at most a tenth of
the interval), so that targets landing on the same second do not fire at
once and passes never stay in lockstep with another system's timer.

## 3. Mergeable percentiles

Each pass produces a DDSketch: a histogram with logarithmic buckets
(γ = 1.02, relative error guaranteed within 1 %), serialized in 60 to 150
bytes. Merging two sketches is a bucket-by-bucket addition, which is
associative and exact: the hourly p95 computed from sixty one-minute
sketches equals the p95 of the raw samples.

An average of p95 values, on the other hand, means nothing. It is the classic
mistake of monitoring tools, and the reason long-period graphs often look
smoother than reality. The unit tests check the merge property on 1,200
samples, and that reducing the number of points of a series keeps the
percentiles exact.

## 4. Automatic resolution

The client only sends `from` and `to`; the server picks the table:

| Window | Table | Step |
|---|---|---|
| ≤ 6 h | `samples` | one pass |
| ≤ 4 days | `roll_1m` | 1 min |
| ≤ 45 days | `roll_5m` | 5 min |
| ≤ 500 days | `roll_1h` | 1 h |
| longer | `roll_1d` | 1 day |

Zooming with the mouse simply calls `/api/v1/series` again with the new
window; the resolution follows. The optional `points` parameter merges
consecutive buckets so that a one-year navigator stays light, with exact
percentiles.

Default retention, settable per tier by the operator: raw samples 48 h,
1-minute buckets 1 year, 5-minute buckets 3 years, hourly and daily buckets
forever. A tier may not be kept for less than the window that rolls it up,
nor for less than the tier below it.

**NTP.** A 48-byte client request to port 123, timed the way every other
protocol here is timed, so an NTP target stays comparable with an ICMP one.
Deliberately not NTP's own offset formula: the offset is a different quantity
and does not belong on the same axis as a round trip.

What the round trip cannot show, the reply does. A server answering in two
milliseconds at stratum 16, or with its leap indicator saying its own clock is
adrift, is not a clock, and that is recorded as a failure rather than graphed
as a healthy two milliseconds. Stratum 0 is a refusal — polling too often, or
not welcome — and the four-character reference id says which. The correlation
between a request and its reply is the protocol's own: a server copies the
client's transmit timestamp into the origin field, so a late reply cannot be
timed against the wrong request.

## 5. Storage

Two SQLite databases in the data directory:

- `config.db`: users, sessions, categories, targets, settings, audit log,
  federation state;
- `metrics.db`: samples, rollups, live values, traceroutes.

`metrics.db` is opened twice: a single dedicated connection for writes, and a
pool for the web reads. In WAL mode readers never block the writer, so a heavy
page can never delay the storage of a measurement.

Raw measurements are also archived as hourly gzip-compressed NDJSON chunks.
Three archive modes, changeable at runtime from the back-office:

- `s3`: hourly chunks go to S3 and are then removed locally;
- `local`: everything stays on disk, bounded by a quota and high/low watermarks;
- `hybrid`: local for `keep_local_hours`, then S3, with rotation.

Rotation evicts chunks already on S3 first (they remain retrievable), then the
oldest ones, until usage is back below the low watermark. Hourly and daily
aggregates are kept for ever by default: they weigh a few hundred megabytes
for ten years. An operator with fifty probes can shorten the fine tiers, which
is where the rows actually are.

The S3 upload is signed with SigV4 by hand (about 70 lines) rather than
through an SDK, which keeps the dependency tree to a single module. It works
with Scaleway, OVHcloud, Backblaze, Cloudflare R2, MinIO and Ceph
(`path_style`). The S3 secret never leaves the API: a `PUT` without
`secret_key` keeps the stored one.

## 6. Probe isolation

A latency probe must measure the network, not the load of the server it runs
on. On a naive single-process design, a benchmark on one saturated CPU core
showed loopback targets (true round-trip time ≈ 0.03 ms) measured at 1.7 ms
median with 50 ms peaks. With the current design: 0.016 ms median, 0.041 ms
worst case, under the same load.

What makes the difference:

- **Kernel timestamps.** ICMP replies are timestamped by the kernel on arrival
  (`SO_TIMESTAMPNS`); the send time is written into the packet right before
  the system call. TCP handshake times are read from `TCP_INFO`, measured by
  the TCP stack itself. DNS answers carry the kernel's arrival time too.
- **The probe never waits.** It reads targets from memory and hands results to
  a non-blocking queue. If the queue is full (database stuck for minutes), the
  measurement is counted as dropped and reported, but the probe keeps
  measuring on time.
- **Separate process.** Its own memory, garbage collector and scheduler, the
  only one allowed raw sockets, and favoured by the CPU scheduler
  (`Nice=-5`, `CPUWeight=1000`, against `Nice=5`, `CPUWeight=50` for the web
  service).
- **Resilience.** If the web service restarts, the probe buffers results in
  memory (about one hour for 1,000 targets) and resends them. After an
  update, it notices the new version and restarts itself on it.

On the web side, the home page overview is precomputed and served
pre-compressed, the target tree and the series are cached, responses are
gzip-compressed, the API is rate-limited to 20 requests/s per client IP
(bursts of 80), and heavy reads are queued rather than piling up.

Full benchmark: [DEPLOY.md § 10](../DEPLOY.md#10-probe-isolation-and-performance).

## 7. IPv6 and traceroute on anomalies

**IPv6.** Each target has an address family: *auto* (a literal address
decides; a name tries IPv4, then IPv6), *IPv4 only* or *IPv6 only*. The probe
opens one ICMP and one ICMPv6 socket. For ICMPv6 the kernel computes the
checksum. Names are resolved with a 5-minute cache.

**Anomaly detection.** After each pass, the probe compares loss and median
with a moving baseline of its own for each target (an exponential average of
healthy passes). A target is anomalous when loss exceeds 3 %, or when the
median exceeds 1.4 × the baseline and is at least 1 ms above it. The 1 ms
floor avoids alerts on sub-millisecond jitter.

**Traceroute.** When a target becomes anomalous, the probe runs an ICMP
traceroute on **dedicated sockets**: changing the TTL of the measuring socket
would disturb the pings of every other target meanwhile. Three rounds of
probes are sent over all TTLs, 300 ms apart to ease router ICMP rate limits,
then replies are collected for 2 s. Sequence numbers encode the run, the TTL
and the round, so a late reply can never be mistaken for a current one.

- A router answering *Destination Unreachable* is never taken for the
  destination: the hop is marked `!N`, `!H` or `!A` and the path ends there.
- Hops are enriched with reverse DNS and the origin AS, using the Team Cymru
  DNS service (no extra dependency).
- A hop sitting on an **internet exchange** is named from PeeringDB instead: a
  peering LAN is deliberately kept out of the global routing table, so the
  Cymru lookup returns nothing for the one hop where two networks meet. The
  prefixes of every exchange are fetched weekly and kept in `config.db`; the
  member holding an address is asked for in the background. The hop then
  carries the member's AS and the name of the exchange, which the route and
  its map draw on the link that crossed it.
- Hops are named again when a traceroute is **read**, from those caches: a
  lookup that failed the day the trace ran would otherwise leave a hole in the
  path for as long as the trace is kept.
- Safeguards: at most one anomaly traceroute every 15 minutes per target (one
  per hour while the anomaly lasts), 30 per hour overall, one at a time. An
  incident hitting 200 targets does not trigger 200 traceroutes.
- A **reference path** is recorded once a day while each target is healthy.
  The back-office and the public pages highlight the routers of that path that
  changed or stopped answering during an anomaly.
- Traceroutes can also be requested from the back-office.
- Traceroutes are **not public by default**: hops reveal the inside of the
  operator's network. The publisher can enable them.

## 8. API

Public, read-only, JSON:

```
GET  /api/v1/overview                     status of every public target
GET  /api/v1/tree                         categories and targets
GET  /api/v1/series?target=42&from=-3h    percentiles and loss over time
GET  /api/v1/charts?from=-24h             worst medians and losses
GET  /api/v1/events?from=-7d              annotated events
GET  /api/v1/live                         server-sent events
GET  /api/v1/traceroutes?target=42        if made public by the operator
GET  /api/v1/asn                          host network
GET  /api/v1/fed/peers                    public federation peers
GET  /api/v1/version
GET  /healthz
GET  /badge/<slug>.svg                    status badge, an SVG image (below)
```

`from` and `to` accept a Unix timestamp, an RFC 3339 date or a relative
expression: `now-3h`, or its shorthand `-3h` (units `s`, `m`, `h`, `d`, `w`,
`y`).

Public, read-only, Atom 1.0 (RFC 4287):

```
GET  /feed.atom                           status updates, whole instance
GET  /t/<slug>/feed.atom                  the same for one public target
```

**The feed is a second door, not a wider one.** It is built from the public
overview, so a private target has no feed and appears in none, and a host
reads exactly as the target page shows it — masked, or absent for a target
whose address is private. Maintenance appears once the banner would have
announced it (seven days ahead) and never when it was recorded after the fact;
events are those `/api/v1/events` serves, route changes excepted; no
traceroute, probe or alert detail is ever written into it. The incidents this
instance detects are added only when the publisher page says so
(`public_incidents`, off by default), worded as records rather than
measurements. An incident shorter than the alert delay, opened during a
maintenance window that stops alerts or measurement, on a target with alerts
off, or decided by a probe since disabled stays out. Entries cover the last 30
days, at most 50, in the instance's default language, with `tag:` ids built
from the instance's host, one date stored the first time a feed is built
(`feed_tag_date`) and the row's id, so an incident that closes updates its
entry instead of adding a second one, and no entry reveals when its row was
written. A feed is rebuilt at most every 30 seconds and carries an ETag.

Administration uses a session cookie (back-office) or, for automation,
`Authorization: Bearer <admin_token>` with the token from `config.json`,
which has the master role:

```bash
TOKEN=$(sudo jq -r .admin_token /etc/smokestack/config.json)

curl -s -X POST localhost:8080/api/v1/admin/categories \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"slug":"transit","menu_en":"Transit providers","menu_fr":"Transitaires"}'

curl -s -X POST localhost:8080/api/v1/admin/targets \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"category_id":2,"title":"Example transit","host":"192.0.2.1","family":4,
       "interval_s":30,"packets":10,"spacing_ms":200,"timeout_ms":1500}'

curl -s -X POST localhost:8080/api/v1/admin/targets \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"category_id":2,"title":"Example resolver","host":"192.0.2.53","family":4,
       "proto":"dns","dns_name":".","dns_type":"NS"}'

curl -s -X PUT localhost:8080/api/v1/admin/storage \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"mode":"hybrid",
       "s3":{"endpoint":"s3.fr-par.scw.cloud","bucket":"smoke-prod",
             "prefix":"par-01","region":"fr-par",
             "access_key":"SCW...","secret_key":"...","path_style":false},
       "local":{"quota_bytes":21474836480,"high_watermark":0.85,
                "low_watermark":0.70,"keep_local_hours":48}}'
```

**An API key, so that a script does not hold everything.** The admin token
acts as master and belongs to nobody. An API key belongs to an account, is
made in *Accounts → API keys* or with `smokestack apikey create` on the host,
and is sent the same way, `Authorization: Bearer ssk_…`:

```
GET    /api/v1/admin/apikeys        own keys; an admin sees every account's
POST   /api/v1/admin/apikeys        {"name":"ansible","role":"admin","days":90}
DELETE /api/v1/admin/apikeys/{id}   revoke, effective immediately
```

The target and category routes currently need admin, which is why the
Ansible key above is an admin one.

A key acts as its account, with the lower of two roles: the one it was given —
never above the creator's, never master — and the account's current one.
Demoting the account demotes its keys; disabling or deleting it deletes them.
A key cannot create accounts or keys (the admin token cannot create keys
either, owning no account) nor change its account's password, needs no CSRF
token, and reads private targets on the public endpoints as a session does.
What it can create otherwise — share links, probe tokens, federation peers —
outlives its revocation, and is where to look after a leak. It is shown once
and stored as a SHA-256 hash: a slow hash protects guessable passwords, and
256 random bits are not guessable. The `ssk_` prefix is for secret scanners
and log filters, `ssk_[0-9a-f]{64}` matching a key and nothing else. Its last
use is written at most once a minute, and every audit entry made with it
names the account and the key.

**A share link is a credential.** It is a random token, kept hashed like a
session, that grants read access to exactly one target — series, events and
traceroutes of that target, nothing else, whatever its visibility. It
expires, it is revocable, its uses are counted, and both the page and
`robots.txt` keep it out of search engines: a token that ends up in a search
result is a leak, not a share.

**A badge is the same for everyone.** `GET /badge/<slug>.svg` is a flat
two-part image for an operator's website, a README or a PeeringDB note: the
target's title, then its status as the public pages classify it, in the same
colours. `?metric=uptime&window=30d` (`24h`, `7d`, `30d`, `1y`) shows the
availability figure instead, `?metric=latency` the current median, and
`?label=` replaces the title, cut at 40 characters. Nothing about the caller
changes the answer: a private target answers 404 exactly like a slug that does
not exist, a session reveals nothing more, and **a share token does not unlock
a badge** — a token in an `<img src>` is a credential published to every
reader of the page it is embedded in. That sameness is what lets image proxies
such as GitHub's keep it for a minute. The route is not rate-limited, so it
reads what is already computed: the status and the median from the overview
the background loop builds — a badge never builds it, and answers 503 until
the first build — and the uptime figure from the target page's one-minute
availability cache, which one request fills per target while the others wait
for it, so a busy page costs at most one availability computation a minute.
The figure is written the way the target page writes it, one decimal (two
under 1 %) with the language's decimal mark, so the badge and the page it
links to never disagree; a window the target has not been measured across
reads *since <date>* instead of its name, so a target created last week does
not show a yearly figure. When availability is not published, the uptime badge
says so in grey rather than answering 404, since a broken image on a page the
operator may not control reads as an instance that is down. Words are in the
instance's default language. The back-office gives the Markdown and HTML to
paste, under *Badge* beside each target the public overview lists, built on
the instance's public address (*Address of this instance*).

**Hiding an address is not hiding a target.** A public target can keep its
address private: the series, the status and the title are public, while the
host, the port, the pinned address and the traceroutes are stripped from
every public surface — tree, overview, target page, indexed description and
the traceroute endpoint, since hops would give the address away in one look.
The operator keeps seeing all of it.

**Private targets.** Each target is public or private. A private target is
measured, stored and graphed exactly like the others, but it is left out of
every public endpoint: tree, overview, series, charts, live stream and
traceroutes. The visibility check accepts the API token, an API key or a
back-office session, so an operator sees everything while a visitor sees only
the public targets. The set of public target ids is cached and rebuilt
whenever a target or a category changes, so switching a target takes effect
immediately. A live stream opened by an authenticated caller checks the
credential again every 5 seconds, and falls back to the public targets once
the key is revoked or the session closed.
`/api/v1/overview?all=1` returns private targets to an authenticated caller;
the cached public payload never contains them.

**Contact without an address.** The public pages carry a form rather than the
operator's email address. Messages are stored on the instance and read in the
back-office; the operator replies from their own mail client, so the instance
never sends mail for a visitor and cannot be turned into a relay. A
notification goes to a private address only if SMTP is already configured for
the NOC alerts. The address that receives it, and the general address when the
operator keeps it private, are stripped from the public `/api/v1/site`
response. The form is rate-limited per IP, has a hidden field for bots, and
refuses anything looking like mail-header injection.

**One target, one place.** A faulty target is usually also a critical one
and belongs to a category, so it would appear three times on the home page.
By default it is shown once, in the faults section, and left out of the other
lists; a checkbox switches that off and the choice is kept in the visitor's
browser, not on the server.

**Editing a target.** `PATCH /api/v1/admin/targets/{id}` changes any field,
category included, and only the fields present in the body. Its decoding
struct carries explicit JSON tags: without them, the fields whose name holds
an underscore (`category_id`, `interval_s`, `spacing_ms`, `timeout_ms`) are
silently ignored, and the back-office appears to save without saving. A test
sets every field and reads them back.

**Matching replies.** An echo reply carries our identifier and sequence
number, and normally echoes the payload, where the send time is written. It
is not required to: a reply may come back truncated to its 8-byte header, or
with the payload rewritten, which several home routers and CPE do. The probe
therefore keeps the send time per sequence number on its own side and uses
the echoed one only when it is present and plausible; otherwise those
answered probes were counted as lost.

**No third party in the page.** The robot check on the contact form is a
proof-of-work challenge signed by the instance: the browser looks for a nonce
whose SHA-256 has enough leading zero bits. Nothing to read, so it works in
every language; no external script, so no visitor is exposed to a captcha
provider. The email mode assembles the address in JavaScript rather than
printing it, and the server strips it from the public API in every other
mode.

**Third parties are never on the critical path.** RIPEstat and PeeringDB are
read in the background and served from cache; a handler never waits on them,
because a page that waits a minute is a page a reverse proxy replaces with
its own error.

**Channels.** Delivery is a set of small adapters: a subject and a body go
in, an SMTP session or one provider HTTP call comes out. Adding a provider
means one case in `notify.go`, not touching the alerting logic. SMTP is
spoken directly rather than through `smtp.SendMail`, so implicit TLS,
STARTTLS and no encryption are all reachable, and the local `sendmail` is an
option for hosts that already relay mail. One failing channel never stops the
others, and what the provider answered is reported rather than swallowed.
Chat and SMS receive a shortened message: the traceroute belongs in the
email.

**Alerting, two switches.** A target stays measured whatever the alerting
setting: incidents are recorded either way, only the message is withheld. The
per-target setting is stored as an exception (`alerts_off`) rather than as a
preference, so a request that omits the field leaves alerting on: silencing a
target has to be deliberate.

**Rotating names, said out loud — but not in detail.** The public side
carries the count of addresses a name answered from, never the list: those
belong to somebody else's infrastructure and can be numerous. The list is
served only to an authenticated caller, which is exactly who needs it to
pin one. A visitor reading a median and a loss
figure assumes they describe one machine. When the name answered from
several addresses, the public page says otherwise, above the figures, and so
does the indexed description; the back-office offers to pin one address. The
address actually probed travels with every
measurement and is kept per target for 24 hours. A name that answers from
several addresses is a pool: the back-office says so and offers to pin one
address, which the probe then uses instead of resolving. Without that, a
pool graph mixes different machines and the passes towards a server that
ignores ICMP look exactly like packet loss.

**Deleting is archiving.** A target row is never removed on delete: it is
renamed (`<slug>-histo-<timestamp>`), disabled, taken off the public side and
out of alerting. Two reasons. The name becomes usable again, which is what an
operator expects after deleting something. And the identifier stays taken:
SQLite would otherwise hand it to the next target, which would inherit the
deleted one's samples, rollups and traceroutes — a graph showing another
target's past. Purging is a separate action, and removes the measurements with
the row.

**The log is also in the interface.** The last lines are kept in a bounded
in-memory ring and shown in the back-office, while still going to stdout for
journald or Docker. A log belongs in the system's hands, not in the database;
but the person who needs it does not always have a shell.

**Why there is loss.** The amount of loss says little; its shape says
almost everything. A destination limiting ICMP loses a fixed small number of
packets per burst, never a whole one, however many bursts are affected — the
share of affected bursts is not the signal, which a test caught while the
first rule used it. Whole passes lost are a real outage, and the advice says
so instead of offering to smooth it away. Several packets at once, scattered,
is the path. The analysis runs on demand over the raw passes of the last 24
hours and never changes anything by itself.

**Why a target fails.** Every pass carries the reason it could not measure —
resolution failure, missing IPv6 stack on the probe, send error, or no reply
at all — and the last one is kept per target and shown in the back-office.
Loss alone does not tell an operator whether the packets were filtered, the
name was wrong or the host has no IPv6.

**Pasted hosts.** What people paste carries tabs, spaces, a URL, brackets or
invisible characters. The host is cleaned on save, on creation and on update
alike, and a `host:port` fills the port field. Whitespace is only stripped at
both ends, never inside: removing it there would silently turn
`1.1.1.1 8.8.8.8` into a plausible host name, measured forever against
nothing. Anything that is neither an IP address nor a host name is refused
with the reason.

**TCP targets.** The host and the port are stored separately, so a target can
move between ICMP and TCP without rewriting its address; older targets stored
as `host:port` are migrated on start.

**DNS targets.** A resolver can keep answering ICMP from its kernel while
every query waits out the client's timeout, so a ping says nothing about
whether it resolves; on a production instance, one failing for over an hour
showed only as loss on the targets that depended on it. A `dns` target sends one query per packet over UDP,
recursion desired, for the `dns_name` and `dns_type` it states — `. NS` by
default, the priming query every recursive resolver answers from its cache,
so the figure is the resolver's and no third party's name is looked up. The
type is one of `A`, `AAAA`, `NS`, `SOA`, `MX`, `TXT`; the port defaults to 53.
NOERROR and NXDOMAIN are answers, a truncated one included, since the
resolver is what is timed. SERVFAIL is loss. REFUSED is loss reported as a
configuration to fix, the way a UDP echo reports a port nobody listens on.

The queries of a burst go out at the target's spacing without waiting for each
other, as ICMP echoes do, so a resolver that says nothing holds a pass for the
burst plus the timeout, inside the `packets × spacing_ms + timeout_ms` the
interval is checked against. An answer counts only if it arrives on the socket
connected to the address and port queried, within the timeout of its own
query, and repeats that query's random ID and its question: an answer is never
timed against another query, and one that comes after its timeout is lost
wherever its query sat in the burst. The one reply taken without the question
is a bare error header carrying the right ID, which is how Unbound refuses a
client its access control leaves out; it counts as the loss it names, never as
an answer. Only the header and the question are read, at fixed offsets:
nothing in an answer is followed. The query carries no EDNS record, so the
answer fits 512 bytes. The query is cleared on any other protocol rather than
refused, so that changing only a target's protocol is never turned down over a
field the caller did not send, and a target hiding its address hides its query
name from the public tree as well. A double-check request has no room for a
query, so DNS targets are refused there, and the breakdown stays TCP-only.

**NTP targets.** A 48-byte client request to port 123, timed the way every
other protocol here is timed, so an NTP target sits on the same axis as an
ICMP one. Deliberately not NTP's own offset formula: the offset is a different
quantity and would not be comparable with anything else on the page.

It is a protocol rather than a UDP echo pointed at 123 because an NTP server
does not echo — it answers a client packet with a server packet and refuses
what it does not recognise. And the answer carries what a round trip cannot:
the server's own view of itself. A clock replying in two milliseconds while
reporting stratum 16, or whose leap indicator says its own clock is adrift, is
not a clock, and that is recorded as a failure rather than graphed as a
healthy two milliseconds. Stratum 0 is a refusal rather than a fault, and the
four printable characters of the reference id say which: `RATE` for polling
too often, `DENY` or `RSTR` for a client that is not welcome.

A request and its reply are matched by the protocol's own mechanism, not by a
nonce bolted onto it: a server copies the client's transmit timestamp into the
origin field. A reply arriving late, during the wait for the next packet of
the pass, is therefore dropped rather than timed against the wrong request.
The probe needs no privilege, so unlike ICMP it works in either probe mode.

**Interval and retention.** The interval is free between 10 s and a day: the
list of four values was a restriction, not a measurement constraint, and the
real limit is the burst fitting in the interval. Two things follow from a free
interval and are easy to miss: the "current" status window must follow the
interval rather than sit at 15 minutes, and the aggregation tiers stay as they
are, since rollups are keyed by bucket and not by pass. Retention can also be
set per target, tighter than the instance tiers, which is the only sensible
place for that decision.

**Safeguard on targets.** Creating a target is refused if its burst does not
fit in its interval: `packets × spacing_ms + timeout_ms` must stay below 75 %
of `interval_s`. At 30 seconds this means 10 packets spaced by 200 ms, rather
than SmokePing's 20 packets per second.

**Remote probes.** `POST /api/v1/ingest` accepts measurement batches from a
probe on another machine, with `Authorization: Bearer <probe token>`. A
probe without a token is always rejected, so nobody can inject fake
measurements. Issuing tokens is not available yet (see
[known limits](#15-known-limits)).

**Prometheus metrics.** `GET /metrics` serves the text exposition format
0.0.4 to any authenticated caller, from the viewer role up, private targets
included. It is written by hand: the client library would be a second
module. A scrape reads the live table, the statuses the alerter worked out
on its last round, the small tables of `config.db` and the pipeline's
counters — never the samples or the rollups. A thousand targets seen from
three probes scrape in about 7 ms on a laptop: 30 000 series, 3.5 MiB,
150 KiB gzipped. The endpoint shares the API's rate limit, 20 requests/s
per client IP, which no scrape interval comes near.

No host, port or address is exported, whatever the caller may read
elsewhere: what Prometheus stores is not bound by this instance's visibility
rules. Moments are Unix timestamps rather than ages, as Prometheus
recommends, so a certificate's remaining life is its expiry date and a
probe's silence is `time()` minus its last report.

Below, *target* stands for the labels `target` (slug), `target_id`,
`category` and `proto`.

| Family | Labels | What |
|---|---|---|
| `smokestack_target_rtt_median_seconds`, `smokestack_target_rtt_p95_seconds` | target, `probe` | Last pass; absent when nothing came back or the pass is stale |
| `smokestack_target_loss_ratio` | target, `probe` | Last pass, 0 to 1; absent when the pass is stale |
| `smokestack_target_last_pass_timestamp_seconds` | target, `probe` | |
| `smokestack_target_status` | target, `probe`, `state` | State set: `ok`, `warn`, `crit`, `nodata`, `maint`; refreshed by the alerter every minute |
| `smokestack_target_info` | target, `title`, `public` | Always 1 |
| `smokestack_target_in_maintenance`, `smokestack_target_incident_open` | target | 0 or 1 |
| `smokestack_target_certificate_valid`, `smokestack_target_certificate_expiry_timestamp_seconds` | target | TCP targets the certificate watcher has inspected |
| `smokestack_probe_enabled` | `probe` | |
| `smokestack_probe_last_report_timestamp_seconds` | `probe` | Enabled probes that have reported |
| `smokestack_write_queue_length`, `smokestack_write_queue_capacity`, `smokestack_write_last_batch_duration_seconds` | | The write pipeline |
| `smokestack_measurements_written_total`, `smokestack_measurements_dropped_total`, `smokestack_measurements_write_failed_total` | | Counters since the service started: measurements committed to `metrics.db`, refused by a full write queue, and caught in a batch that failed to commit |
| `smokestack_archive_chunks_pending_upload`, `smokestack_archive_local_bytes` | | The raw archive |
| `smokestack_database_size_bytes` | `database`, `file` | `config` or `metrics`, `main` or `wal` |
| `smokestack_build_info` | `version`, `goversion` | Always 1 |

A pair of target and probe is exported only while that probe is enabled and
still measures that target: the live table keeps a row after either stops,
and exporting it would present a frozen figure as a current one. For the
same reason, a pass is stale once it is older than both three of the
target's intervals and the five minutes after which the alerter calls a
probe silent: its round trip and loss are left out, so a probe that dies during an
outage does not report its last loss forever. Its last pass timestamp stays,
and the silence shows in it.

## 9. Back-office and security

The back-office is at `/admin`, in English.

**First account.** While no account exists, creating the master account
requires a one-time setup code, printed in the service log and written to
`setup-code` in the data directory (mode 0600). Without it, the first visitor
of a freshly exposed instance could take it over. The installer creates the
account itself (`smokestack user add`) and prints the password.

**Roles.**

| Role | Scope |
|---|---|
| `master` | everything, including accounts |
| `admin` | configuration, federation, storage, publisher page — not accounts |
| `editor` | targets and categories |
| `viewer` | read-only |

At least one active master must remain: the last one cannot be deleted,
demoted or disabled.

**Sessions.**

- Passwords hashed with PBKDF2-HMAC-SHA256, 210,000 iterations, 16-byte salt,
  implemented with the standard library.
- Session cookie `HttpOnly`, `SameSite=Lax`, `Secure` as soon as the request
  arrives over HTTPS (directly or through `X-Forwarded-Proto`). 32-byte token
  stored hashed, valid 12 hours.
- CSRF token required on every state-changing request made with the cookie.
- 8 sign-in attempts per IP per 10 minutes, with the same error message
  whether the account exists or not. Checking the current password on a
  password change counts as an attempt. `X-Forwarded-For` is trusted only
  when the connection comes from a local reverse proxy, so it cannot be
  forged to bypass the limit.
- Changing a password or disabling an account closes all its sessions.
  Disabling it also deletes its API keys, so that an account taken over and
  later re-enabled does not get the intruder's keys back. A password change
  leaves them: they are separate credentials, listed with their creation date
  and last use, and revoking them on every routine change would break every
  script that holds one.
- Audit log: sign-ins, failures, creations, pairing decisions, updates, and
  on each entry written for a request made with an API key, the key. Changes
  to targets, categories, events, site settings, storage and federation peers,
  among others, are not written to it yet.

**Before exposing an instance.** Put a TLS reverse proxy in front, with
`proxy_pass` only, never `root` or `alias`. The cookie only becomes `Secure`
over HTTPS.

## 10. Federation

Operators running smokestack can measure each other's *anchors* (addresses
they declare) and share the results. Federation is off by default.

```json
"federation": {
  "enabled": true,
  "asn": "AS64500",
  "org": "Example Telecom",
  "base_url": "https://latency.example.net",
  "anchors": ["192.0.2.1", "2001:db8::1"]
}
```

**Identity.** Each instance has an Ed25519 key pair (`fed.key`, mode 0600)
and a short fingerprint that two operators compare out of band (phone,
exchange mailing list, peering channel) before approving each other.

**Signatures.** Requests between instances are signed at the message level,
not the transport level: headers `X-Fed-ASN`, `X-Fed-Timestamp`,
`X-Fed-Nonce` and `X-Fed-Signature`, over `METHOD \n PATH \n TS \n NONCE \n
sha256(body)`. Clock tolerance of 5 minutes and a nonce cache against replay.
This works behind any reverse proxy, without certificate plumbing.

### Two-step pairing

Each instance publishes a `/pairing` page with its AS, organisation, key
fingerprint and anchors. That is the URL to share.

```
A                                     B
│  adds https://B/pairing
│  GET  B/api/v1/fed/profile          → reads B's key and fingerprint
│  POST B/api/v1/fed/pairing/request  → request signed with A's key
│                                       B records it as pending
│                                       nothing is approved yet
│                                     ┌ B's administrator sees the request,
│                                     │ compares the fingerprint out of band,
│                                     └ accepts → B approves A
│  POST A/api/v1/fed/pairing/response ← answer signed with B's key
│  A checks it against the key read at step 1
│  A approves B                         paired on both sides
```

The request signature proves possession of the announced key, not identity:
trust comes from a human comparing fingerprints, and agreement is required on
**both sides**. The answer must be signed by the key A read at the start,
which prevents a third party from answering in B's place. The pairing token
links request and answer and prevents replay.

A request carries:

| Field | Required | Seen by |
|---|---|---|
| Justification (≥ 20 characters) | yes | the remote administrator |
| Private note | no | the remote administrator |
| Signing contact | no | the remote administrator |
| Agreement to public listing | — | both sides |

The answer can carry a **private reply**, seen only by the requester. None of
these texts ever appear on a public page.

**Public listing needs double consent.** A pairing is listed on `/federation`
only if both administrators agreed. Either can hide it at any time; neither
can show it without the other's agreement (the API refuses `public: true`
without the peer's consent).

Accepting a request approves the peer and creates the measurement targets
towards its anchors in a "Federation" category. Every five minutes each
instance pushes its measurements towards the others' anchors: the inter-AS
matrix builds itself (`GET /api/v1/fed/matrix`).

```bash
# our fingerprint, to share with the other operator
curl -s localhost:8080/api/v1/admin/fed/identity \
  -H "Authorization: Bearer $TOKEN" | jq -r .fingerprint

# request a pairing
curl -s -X POST localhost:8080/api/v1/admin/fed/pairing \
  -H "Authorization: Bearer $TOKEN" \
  -d '{"url":"https://smokestack.other.example/pairing",
       "justification":"We both peer at the same exchanges and share customers.",
       "public_listing":true}'

# list requests, then decide
curl -s localhost:8080/api/v1/admin/fed/pairing -H "Authorization: Bearer $TOKEN" | jq
curl -s -X POST localhost:8080/api/v1/admin/fed/pairing/3/decide \
  -H "Authorization: Bearer $TOKEN" -d '{"accept":true,"reply":"Welcome!"}'
```

### NOC alerting

When a federated anchor exceeds 5 % loss over ten minutes, an incident is
opened and shared with the peers for corroboration. An outgoing notification
(email or webhook) is sent only when **four conditions** are met:

1. at least 3 independent observers confirm it;
2. the 5-minute grace period has passed;
3. the AS concerned has not acknowledged it through `/api/v1/fed/ack`;
4. the AS concerned is an approved peer, who therefore agreed by joining —
   no unsolicited email to a non-member.

A 6-hour suppression window applies per (AS, target) pair. The message
presents a corroborated observation, not a diagnosis, and says so.

### Deliberately not implemented

On-demand measurement towards a third-party prefix is absent on purpose: it
is the amplification and reconnaissance vector a probe network must refuse.
If it ever comes, it will be restricted to prefixes whose origin the
requester proves with a ROA. Locating the faulty hop by intersecting the
traceroutes of several peers is not there either; the matrix has to run in
production first.

## 11. Host network (RIPEstat and PeeringDB)

The `/network` page describes the AS hosting the instance:

- **RIPEstat** (observed routing): holder, country, announced IPv4/IPv6
  prefixes, number of upstream and downstream neighbours, main upstreams;
- **PeeringDB** (declared): network type, peering policy, traffic, ratio, IRR
  as-set, presence at internet exchanges, facilities.

PeeringDB also answers two questions that have nothing to do with this page
and no other source: which prefixes belong to which internet exchange, and
which member holds a given address on one (§ 7).

Answers are cached for 24 h and refreshed in the background. The service only
answers for **our AS and our approved peers**, so the instance is not an open
proxy. An optional PeeringDB key (`"peeringdb_api_key"` in `config.json`)
raises the anonymous rate limits.

## 12. Languages

Public pages are translated by JSON files in `web/i18n/`, embedded in the
binary. **English (`en.json`) is the mandatory reference**: without it,
smokestack refuses to start, and any key missing from another language is
shown in English. Shipped, all complete: English (reference), Danish, Dutch, French, German, Italian, Norwegian (Bokmål), Portuguese, Spanish and Swedish.

Files accept nested objects (`{"home":{"faults":"…"}}` becomes `home.faults`)
and placeholders (`{n}`, `{total}`). Metadata lives under `_meta`: `name` is
the native name, `dir` is `ltr` or `rtl`, and `flag` is the two-letter country
code the header turns into a flag emoji — the pair of regional indicator
symbols — so the language menu has no image to serve and no list of its own
to keep in step. At load time each language is compared
with English: missing keys, unknown keys and **mismatched placeholders** are
reported in the back-office. The `TestI18nFiles` test fails if a shipped
language is incomplete.

`smokestack languages` lists them and `-default CODE` sets the instance
default. Browser codes `no` and `nn` map to Norwegian Bokmål, `pt-BR` to
Portuguese. To add or fix a language without rebuilding, drop `xx.json` into
`/var/lib/smokestack/i18n/` and reload it from the back-office. In the browser,
the language is chosen from `?lang=`, the remembered choice, the browser
languages, the instance default (publisher page), then English.

## 13. Public pages

| Page | Content |
|---|---|
| `/` | status overview, faults, critical targets, categories, detail with zoom |
| `/federation` | public peers, latency in both directions, inter-AS matrix |
| `/network` | host network from RIPEstat and PeeringDB |
| `/pairing` | identity, fingerprint, pairing instructions |
| `/about` | operator, contacts, measurement method |

Each target also has its own address, `/t/<slug>`; the older `#t=<id>` form
still opens the same page. Pages are built in the browser, so a crawler would
otherwise see an empty document: the server injects the title, description,
canonical address, `hreflang` alternates, social cards and JSON-LD, and puts
a plain summary of the public targets inside `<noscript>`. `/sitemap.xml` and
`/robots.txt` are generated from the same data, so a new instance is indexed
without anyone maintaining a list, and private targets never appear. One
setting turns all of it off, and the back-office is always `noindex`.

Every public page also advertises the instance's Atom feed in its head, and a
target page its own beside it, for a reader that finds feeds by the page
address; the footer links to the instance's feed (see the API section).
**Hiding a page is presentation, not policy.** The navigation leaves out a
page the configuration makes empty — Federation and Pairing without
federation, Host network without an AS number — and the operator can hide any
of those three on top of that (`hidden_pages` in `PUT /api/v1/admin/site`). A
hidden page leaves the menu, the links of the other pages and the sitemap, and
its address, `.html` file included, answers 404 rather than redirecting, so
that a search engine drops it. The APIs behind it keep answering: peers call
`/api/v1/fed/*` whatever this instance publishes, and pairing never goes
through the HTML page, so hiding Pairing cannot quietly break it. Federation
is turned off in the configuration file. Home and About cannot be hidden:
About is where the operator is identifiable and reachable. The setting names
what is hidden, so an instance upgraded from before it hides nothing.

All share one stylesheet (automatic dark theme), the translation library and
a common footer. The link to the official website comes from a value compiled
into the binary and cannot be changed from the back-office.

## 14. Updates and releases

A release is a ZIP containing the binary, a `manifest.json` (version,
platform, SHA-256) and an Ed25519 signature of the manifest. Signing the
manifest signs the binary through its checksum. Installed side by side:

```
/opt/smokestack/releases/0.2.1/smokestack
/opt/smokestack/releases/0.2.2/smokestack
/opt/smokestack/current  -> releases/0.2.2
/opt/smokestack/previous -> releases/0.2.1
```

Whether from the back-office, the command line or automatically:

1. signature, platform and checksum are verified; an unsigned package or one
   signed by an unknown key is refused;
2. the new binary runs its self-test (embedded assets, English language file,
   SQLite driver);
3. `config.db` is backed up;
4. the `current` link is switched atomically (a rename) and the process
   re-executes itself in place, keeping its PID; the isolated probe notices
   the new version and follows;
5. if the new version fails to start three times, the previous one is
   restored automatically and the incident is recorded in the history.

Automatic updates: the service checks `latest.json` 2 minutes after each
start, then at the configured interval (once a day by default, 1 hour to 30
days, read again after every check). Checking is on by default and only reports;
installing is a separate, opt-in setting. Automatic installation always
requires a trusted signature, whatever `allow_unsigned` says, and pre-releases
(versions with a hyphen) are never offered, since the release workflow
publishes them as GitHub pre-releases, outside "latest". Releases are built by
GitHub Actions when a `v*` tag is pushed: tests, linux/amd64 and linux/arm64
builds, signing with the `SMOKESTACK_RELEASE_KEY` secret, and publication with
`latest.json`, which instances poll. Dependabot keeps Go modules and CI
actions up to date. Details in [DEPLOY.md § 6](../DEPLOY.md#6-publishing-your-own-releases).

## 15. Known limits

- IPv6 has been verified with unit tests on crafted packets only; it still
  needs to be validated on a dual-stack host.
- Latency-triggered traceroutes are covered by unit tests; loss-triggered ones
  were tested on a multi-hop lab network.
- The raw archive is gzip NDJSON, not yet Parquet.
- The catalogue of ready-made targets is deliberately short and holds only
  services documented as reachable from anywhere; it is not meant to grow
  into a directory.
- Remote probes on another machine: the ingestion endpoint exists and is
  protected, but issuing probe tokens is not available yet. Same-host
  isolation is complete.
