# Changelog

Versions are published as signed releases; servers with automatic updates
install the newest one directly, whatever versions came in between.

## Unreleased

- CI and release workflows move to `actions/checkout@v5` and
  `actions/setup-go@v6`, which run on Node 24: GitHub was already forcing the
  older ones onto it and is removing the Node 20 runtime. The runner is
  pinned to `ubuntu-24.04` instead of `ubuntu-latest`, so the switch to
  Ubuntu 26 on 19 October cannot land in the middle of a release.

- The notices on a target page (load-balanced name, pinned address, shared
  graph) now span the full width of the page instead of stopping short of the
  graph above them.
- **The route to a target is shown under its graph**: the autonomous systems
  the packets crossed on their way there, as the last traceroute measured
  them, one box per network with its name. It follows the visibility of the
  traceroutes it comes from — never for a private target, a target hiding its
  address, or an instance that does not publish traces — and is available
  through a share link.
- The zoom hint sits under the graph it describes, instead of below the
  figures further down.
- **Events on a graph are readable.** A route change used to write its whole
  AS path across the plot next to a vertical line, which told a visitor
  nothing. The graph now carries a dashed mark with a number, and a list
  underneath spells each one out: date, what happened, and the two AS paths.
  The list also says what the mark actually means: the forward path from this
  probe to this target crossed different networks, nothing about the return
  path or about the instance, and no alert is raised.
- The two strips under the graph say what they are: the 24-hour ribbon
  explains that each block is 30 minutes and what its colour means, and the
  year-long navigator is now labelled *drag to choose a period*, with the
  explanation next to it rather than lost under the figures.

## 0.2.11

- The changelog is sorted per released version, and `scripts/changelog-check.sh`
  keeps it that way: the CI refuses a published version without its own
  section, and checks that sections stay in descending order. Entries for
  0.2.7 to 0.2.10 had piled up under one heading while those four versions
  were already out.
- Fixed: a share link created with 0 days expired after 30 days instead of
  never. The API took an explicit 0 for a missing value and applied the
  default; 30 days now only applies when `days` is left out.

## 0.2.10

- **Reaching the NOC of a network on the path**: each traceroute hop with an
  AS gets a button showing what that network declares in PeeringDB — NOC
  contact first, phone, peering policy, PeeringDB page, looking glass — and a
  mail already written with both AS numbers, the destination, the time and the
  hop involved. One more button creates a read-only share link and puts it in
  the message. Back-office only, so the instance is never an open proxy in
  front of PeeringDB.
- Fixed: the `User-Agent` sent to RIPEstat and PeeringDB still announced
  version 0.1.
- **Share a single target with a read-only link** (idea #15), made for
  troubleshooting across networks: a transit provider's support, another AS's
  NOC or a customer opens your own measurement — percentiles, loss and the
  traceroutes taken when the path degraded — without an account and without
  seeing the rest of your monitoring. The shared page states who measured and
  from where, so the figures mean something to a stranger. Dated, revocable,
  never indexed, limited to that one target even when it is private, and with
  the token stored hashed so a copy of the database hands over nothing.
- Fixed: the host-network card on the public pages could show "undefined"
  while the RIPEstat data was still being fetched in the background.
- **Hops over time** (idea #16): the traceroutes screen charts the hop count
  of each traceroute over 30 days, with the AS path on hover — the visual
  counterpart of the AS-path comparison.
- **Free interval** (idea #14): any value from 10 seconds to a day, bounded
  only by the burst rule. The status window now follows the interval, so a
  target measured every 30 minutes no longer reads as having no data.
- **Retention per target** (idea #17): keep a target's measurements for a
  number of days instead of the instance tiers — raw passes, every rollup
  tier and its traceroutes.
- **The public navigation adapts** (idea #11): Federation and Pairing appear
  only when federation is enabled, Host network only with an AS number. No
  more tabs leading to empty pages.
- **The order of the categories** is settable and drives the public page
  (idea #12).
- **A public target can hide its address** (idea #13): the graph stays
  public, the host, port, pinned address and traceroutes are withheld
  everywhere — page, API and indexed description. For dashboards given to
  customers.

## 0.2.9

- **"Why this loss?"** on each target: the instance reads the shape of the
  loss over 24 hours and tells rate limiting, real outages and path loss
  apart — one or two packets per burst is a limiter, whole passes lost is an
  outage, several packets at once is the path. For a limiter it offers a
  gentler burst, applied in one click; for an outage it says explicitly that
  tuning would only hide it. Nothing is ever applied on its own.

## 0.2.8

- **Topology changes are detected**: a healthy path is compared with the
  previous one, and a change of **AS path** is recorded as an event on the
  graphs and in the log — without alerting, since nothing is broken.
  Addresses are not compared, so parallel links of the same operator raise
  nothing. When a degradation follows such a change, the alert names it
  first. A target can take its reference more often than the instance
  default, in its settings.
- **Deleting a target now archives it.** Its name becomes free again, so a
  target of the same name can be recreated, and its measurements stay
  attached to the archived one. This also fixes a worse problem found while
  testing: SQLite handed the deleted target's identifier to the next one,
  which then inherited its history. Archived targets are listed and can be
  purged for good.
- **A service log screen in the back-office**: the last 500 lines, with a
  filter and optional auto-refresh, for when you have no shell at hand.
- **Failing targets are logged**: one line when a target starts failing, with
  the reason, and one when it answers again. Until now the reason was only in
  the back-office, so `journalctl` and `docker logs` said nothing.
- A **TCP target without a port** now says so instead of repeating Go's
  "missing port in address".
- The guide states plainly that a TCP target reads **no HTTP status code**: a
  service answering 403 is measured like any other.
- The load-balanced notice on a public page no longer lists the addresses,
  only how many there are: they describe the inside of a third-party service
  and there can be many. The list is no longer in the public API either — it
  stays in the back-office, where it serves to pick one to pin.

## 0.2.7

- The back-office invites operators to post their instance URL in the
  project's *Show and tell* discussions, to find networks to pair with.
- The *Unique targets* checkbox no longer crowds the section header: the
  count sits next to the title, and the checkbox became a pill that moves to
  its own line on a narrow screen instead of breaking apart.
- **Fixed: the pairing page showed version 0.1**, a string left hard-coded in
  the first version of the federation profile.
- **Fixed: the host network page could fail with a JSON error.** It fetched
  RIPEstat and PeeringDB while the visitor waited, so a reverse proxy in front
  could answer with its own HTML error page. The server now answers from its
  cache and refreshes in the background, and the page waits instead of
  breaking — whatever a proxy answers.
- **Fixed: a raw translation key could appear on a page** (`detail.rotating`).
  Dictionaries gain keys with every version and were cached for an hour; they
  are now revalidated, and a key a dictionary does not know shows nothing
  rather than its own name.
- The load-balanced notice is restyled: addresses as chips, room above it.
- **Contact: four modes** — form, email address assembled in JavaScript
  against harvesters, links to your own tools, or nothing — plus an optional
  built-in robot check (proof of work, no third party, nothing to read so it
  works in every language).
- Alerting can be switched off **per target** as well as globally: a target
  left out is still measured and its incidents still recorded.
- **Notification channels**: alerts now leave through channels you configure
  — email with your own SMTP settings (server, port, STARTTLS or implicit
  TLS, credentials, sender), the local `sendmail`, a JSON webhook, Slack,
  Microsoft Teams, Telegram, Twilio for SMS and WhatsApp, OVHcloud SMS
  (signed as their API expects) and GatewayAPI. Several at once, each with a
  **Send a test message** button reporting what the provider answered. Chat
  and SMS receive a shortened message, email and webhook the full one.
- **Alerting on your own targets**: an alert when a target stays in incident
  longer than you choose, with the traceroute taken when the incident opened
  and compared with the last healthy path. Silence window per target,
  optional recovery notice, email or webhook. Until now only the federation
  could alert, and only a peer's NOC about their network.
- A wiki page on **sizing and adjusting targets**: how to tell ICMP rate
  limiting, a rotating name and a real path problem apart, with the settings
  to use. Linked from the back-office.

## 0.2.6

- The changelog is checked when a version is published: the release workflow
  reads the section of the version being tagged and uses it as the release
  notes, and refuses to publish without it. Versions 0.2.4 and 0.2.5 had been
  published while the changelog still listed their contents under 0.2.3.

## 0.2.5

- **The public page explains a load-balanced target**: a name answering from
  several addresses carries a notice above its figures — different servers,
  possibly in different places and under different loads — with the addresses
  seen, in the ten languages and in the description read by search engines.
  A pinned target says so instead.
- **Pasted hosts are cleaned**: leading tabs or spaces, a whole URL, brackets
  around an IPv6 address, a trailing dot and invisible characters no longer
  create a target that can never be measured. What cannot be a host is
  refused with the reason.
- The ready-made targets drop `pool.ntp.org` and offer French **university
  time servers** instead (Sorbonne, Lyon 1 in IPv4 and IPv6, Caen, Nice), all
  with a stable address.
- **Rotating names are detected**: a target whose name answered from several
  addresses in 24 hours is flagged in the back-office, with a button to pin
  the address actually measured.
- **Fixed: pages could be served stale from a cache.** The metadata injected
  into the public pages changes with every measurement, but the pages still
  carried the version-wide `ETag`, so a browser or a proxy could be told
  "not modified" and show an outdated description or summary.

## 0.2.4

- **Fixed: replies that do not echo our payload were counted as lost**
  (issue #9). The send time only travelled inside the packet, and a reply
  shorter than 16 bytes, or one whose payload the target rewrote, was thrown
  away — several home routers, CPE and ONT answer exactly like that, which is
  why `ping` saw those targets while smokestack reported 100 % loss. The probe
  now keeps the send time on its side and accepts a bare 8-byte reply.

## 0.2.3

- **A failing target now says why** (issue #9): the back-office shows, under
  its name, whether the name could not be resolved, whether IPv6 is missing on
  the probe, or that nothing replied — with the address actually probed. The
  reason disappears as soon as the target answers again.
- The home page checkbox is now simply **Unique targets**, with a blue marker
  whose tooltip explains what it does (issue #8).
- **Public pages are indexable.** Title, description, canonical address,
  language alternates, social cards and structured data on every page, plus a
  summary readable without JavaScript. Each target gets a readable address
  `/t/<name>`, `/sitemap.xml` lists the public pages and targets, and
  `/robots.txt` keeps crawlers out of the back-office and the API. One switch
  in the publisher page turns it all off. Private targets are never exposed.

## 0.2.2

- **Fixed: editing a target saved only part of it.** The category, interval,
  spacing and timeout were silently ignored (issue #7). Every field is now
  saved, and a test sets and reads back all of them.
- **Fixed: the back-office was unusable on a phone** (issue #6). The menu
  slides over the page and closes when a screen is chosen; the target list
  shows one card per target with its buttons reachable; wide tables scroll
  instead of being cut off.
- The host and the category appear under each target's name.

## 0.2.1

- **Contact form** on the public *About* page: visitors write from the site
  and messages land in the back-office, so the operator's address stays off
  spam lists. Rate-limited, with a bot trap and header-injection checks.
- **Mobile menu** on the public pages: the links were cut off and unreachable
  below 760 px.
- Link from the ready-made targets to the project wiki, which collects
  suggested targets per country.
- Each target is shown once on the home page instead of up to three times; a
  checkbox restores the previous behaviour.
- A catalogue of ready-made targets (public resolvers in IPv4 and IPv6, HTTPS
  endpoints, NTP pool) to enable in one click.
- **Container image** `ghcr.io/nkglfr/smokestack`, for tests, with its
  disclaimer: the host network is mandatory, and a container has no signed
  in-place updates. The service detects containers and says what is degraded.

## 0.2.0

- **Edit a target** from the back-office, and manage categories (rename,
  delete).
- A new target is **measured right away** instead of waiting a whole interval;
  **Check now** repeats that on demand.
- **Separate host and port** for TCP targets, with the usual ports suggested.
- **Install it now** button when a new version is available.
- Working browser Back button, clickable logo, and several back-office fixes.

## 0.1.4

- **Private targets**: measured and visible in the back-office, never on the
  public pages or in the public API.
- The installer asks for the listen IP and port on a first installation
  (`--yes` skips the questions).

## 0.1.3

- Public pages in **ten languages**: English, Danish, Dutch, French, German,
  Italian, Norwegian Bokmål, Portuguese, Spanish and Swedish.
- `smokestack languages` lists them and sets the default; `--lang` at install.

## 0.1.2

- The listen address moves to `/etc/smokestack/smokestack.env`
  (`SMOKESTACK_LISTEN_IP`, `SMOKESTACK_LISTEN_PORT`), with `-ip` and `-port`
  flags. Versions 0.1.1 and earlier do not read that file: after a rollback to
  one of them, the address in `config.json` applies, or the default
  `127.0.0.1:8080`.
- Update checks move to **once a day** by default, configurable from one hour
  to a month.
- English design notes (`docs/DESIGN.md`), screenshots in the README.

## 0.1.1

Same code as 0.1.0: the tag was pushed before the changes. Use 0.1.2 or later.

## 0.1.0

First public release: latency and loss measurement with exact percentiles,
ICMP and TCP over IPv4 and IPv6, traceroute on anomalies compared with the
last healthy path, public status pages, private back-office, federation
between operators, signed in-place updates with automatic rollback.
