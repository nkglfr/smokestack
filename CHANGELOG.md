# Changelog

Versions are published as signed releases; servers with automatic updates
install the newest one directly, whatever versions came in between.

## Unreleased

- **SQLite and the Go toolchain are brought up to date.** `modernc.org/sqlite` goes from 1.34.5 to 1.59.0 — twenty-five minor versions of the engine that writes every measurement — along with its own dependencies and `golang.org/x/sys`. The update requires Go 1.25, so the build, the release workflow and the container image move from 1.22 and 1.23 to 1.25: a version from early 2024 no longer receives the runtime and standard-library fixes that a network-facing service should have. The binary stays static and `CGO_ENABLED=0`, so nothing about how it is deployed changes.

## 0.5.1

Addresses, and who gets to see them. 0.5.0 masked a pinned address and left two other paths open; this closes them and says, on the about page, where the measurements come from — without publishing that address either.

Nothing breaks on upgrade. The masking is on by default, including for instances upgrading, because it shows less rather than more; *Settings → Addresses on public pages* turns it off for an operator who publishes addresses deliberately.

### Fixed

- **A public page shows the network of an address, not the address.** The previous version masked a pinned address; it left two other paths open. A target given as a literal address published it as its host, and a target given by name published the address actually probed under its route — the one value on the page the operator never typed. Both are now masked for anybody who is not logged in, in the ten languages, in share links and in the description indexed by search engines. Host names are untouched: a name is not an address, and it is what says which service a page is about. *Settings → Addresses on public pages* turns the whole thing off for an operator who publishes addresses deliberately, and the per-target *hide the address* setting still removes it from the page altogether rather than masking it. On by default, including for instances upgrading, because the change shows less rather than more.
- **Fixed: a name freed by renaming a target could not be reused** (#46). Renaming changed the title but not the address of the public page, so the old name stayed taken, and creating a target with it failed on a database constraint quoted verbatim at the operator. A rename still leaves the public address alone — a link already in somebody's ticket does not change behind his back — but the address is now a field of its own in the target's form, so freeing the old name is one deliberate edit. Two targets may also legitimately carry the same name: the second now takes `name-2` instead of being refused, accents fold rather than vanish (`Réseau Café` gives `reseau-cafe`), and a genuine collision is reported by naming the target that holds the address.

### New

- **The about page says where the packets leave from.** A measurement with no stated origin is of little use to whoever reads it, so the page now carries the probe's network — reverse name, network, address family, AS number with the operator's name, and links to PeeringDB and RIPEstat so a reader can check the claim rather than take it — and the machine doing the measuring: processor, memory, version, uptime and platform, because a burst is not the same work on two cores and on thirty-two. **The address itself is not published**: the reverse name and the network situate the probe, which is the rule the targets already follow. Behind NAT the page says so instead of showing a local address that would mean nothing. Nothing is fetched while a visitor waits.
- **The link to the other address family is an action rather than a label.** It carries a pair of turning arrows, bold text and a distinct background, because it was a grey chip among grey chips and it is the one thing on that line you are meant to click.

## 0.5.0

One thing a target measured no longer leaks, one piece of work that no longer has to be done twenty times, and a default set that asked nobody's permission.

Nothing breaks on upgrade: no protocol change, no migration, no configuration to revisit. Existing targets keep every value they carry, so the new category parameters change nothing until you ask for it.

### New

- **A category lends its parameters to its targets** (#32). Interval, packets, spacing, timeout, retention, reference traceroute interval and the three thresholds can be set once on a category; every target that leaves the field empty takes it from there. The binding is **dynamic**, as the reporter asked: nothing is copied into a target, so changing the category changes what all of its inheriting targets measure at once. Not inheritable are the host, the protocol, the port and the address family, which are what makes a target a target rather than a copy of its neighbours. Two guardrails come with it — each field shows how many targets currently take their value from it, so an edit states its own reach before it is saved, and a category value that would push a target outside the limits is refused naming that target, rather than producing a burst that cannot fit its interval. Existing targets all carry explicit values, so nothing moves until *Make its targets inherit…* clears the chosen fields across a category, measurements untouched.
- **A fresh instance no longer starts by pinging public DNS resolvers** (#40). It creates four RIPE Atlas anchors instead — Amsterdam, Zurich, Santiago, Tokyo. An anchor exists to be measured, which is consent its operator actually gave; nobody ever asked Cloudflare, Quad9 or Google whether every new installation of a monitoring tool could start pinging them, and "everybody does it" is not an authorisation. A tool that ships pointing at three American platforms also teaches, on its first screen, that those are what the internet is made of, where anchors are run by research networks, universities, exchange points and operators in many countries. They remain a starting point to replace with your own transit, exchanges and services, and `DEPLOY.md` says so. Existing instances are untouched: the set is only created when the database is empty.

### Fixed

- **A pinned address is no longer published in full.** The notice on a target measured at a fixed address named that address, on the public page, in the share links and in the description indexed by search engines. It now names the family and the network it belongs to — *Measured at a fixed IPv4 address (142.251.XXX.XXX)* — which is what the sentence needs to make its point, the point being that the figures describe one machine rather than whatever the name resolves to today. An operator logged in still sees the address he pinned. And a target whose address is set to stay private now says nothing at all: `hide_host` cleared the host and the address count but left the pinned address in the public payload, which defeated the setting entirely.

### Documented

- **Anycast destinations are documented** (#39). One address served from many places steps in latency when the routing moves you between instances, and nothing pins that away — pinning freezes the address, and with anycast the address was never the ambiguity. `DEPLOY.md` and the wiki now say what the tool shows on such a target (a route map with several branches, route changes that are the reading rather than the alarm, and the difference between a latency step with and without one), and what to do about it: raise the thresholds, read the bands rather than the median, and target a unicast address when you want one instance rather than the service.

## 0.4.0

Five of the seven issues opened by a tester in one afternoon, the route of a target made honest in three ways, and two settings that should never have been hardcoded.

Nothing breaks on upgrade: no protocol change, no configuration to revisit. Two migrations run on first start — existing route events are attached to the target they describe, and archived targets move to an `archive` category, so that deleting a category can never take a history with it.

### New

- **A target states which address family it measures** (#36). *Auto* resolved a name to IPv4 when there was one and fell back to IPv6 otherwise, per pass and without saying which it had done — so a name gaining or losing a record changed the destination and the transit inside one series, leaving a graph with two paths in it and no way to tell them apart later. The same defect as a rotating pool measured as one target, fixed the same way: by refusing to mix. Auto is no longer offered for a new target. Targets that have it keep it, since removing it silently would change what they measure, and the back-office now names the situation on each: which family the last 24 hours used, or a warning when they used **both**, with one click to state it. Raised by a tester on #27.
- **IPv4 and IPv6 as a pair of targets** (#27). A name with both an A and an AAAA record is two destinations: different transit, different latency, independent failures. **Also IPv6** next to a target copies it — host, protocol, port, interval, packets, spacing, timeout, thresholds, retention — and sets the copy to the other family, as `<name>-v6`; each public page then links to the other, so the comparison is one click away. A target on automatic is set to **IPv6** in the process and the new half becomes `<name>-v4`: automatic never stated a family, so nothing deliberate is overridden, and the bare name goes to the family that is not the legacy one. A name without an AAAA record is refused rather than paired with a target that can only fail. A target that already states IPv4 keeps its name and gains `<name>-v6`. A single series holding both would average away exactly what the pair exists to show — the same reason a rotating pool is no longer measured as one path. Pairs are recognised by what they measure rather than by a stored link, so one built by hand is recognised as one too.
- **Thresholds are settable, per target and for the instance** (#29). What separates ok, warn and crit was hardcoded at 0.4 % loss, 3 % loss and 1.4× the baseline median. Those are now the shipped defaults, changeable in *Settings → Default thresholds*, and each target can override them: a link known to be poor stops showing red permanently, and a link that matters can be watched more closely than the rest. A zero means "follow the instance", so nothing moves for a target created before this. A rise of less than a millisecond still raises nothing whatever the factor, because that rule exists to stop arithmetic from producing alerts nobody can act on.
- **The `Forwarded` header of RFC 7239 is read**, in preference to the older `X-Forwarded-For` (#28). Both are supported, so an existing reverse proxy keeps working, and the nginx recipe in `DEPLOY.md` now proposes the standard form — which also states the address nginx actually saw rather than appending to what the client sent. In either header only the last element is used, since that is the only one the proxy vouches for.
- **Building from source is documented** (#26), in `DEPLOY.md`: the one build dependency, `make build` and the plain `go build` behind it, cross-compiling, installing what you built with the same layout as a package, running it from a directory without installing anything, and what to run before proposing a change. The commands were executed against this version rather than written from memory.

### The route of a target was misleading in three ways

- **A route event now belongs to its target and to nothing else.** It was recorded at instance level, so every target's page showed every other target's route changes: on an NTP pool you watched marks scroll past that said nothing about that path. A route change is a fact about one path — between the AS announcing this instance's address and the address actually measured — and it now appears on that target's page and nowhere else. `/api/v1/events` takes a `target` parameter: without it, it returns only what concerns the whole instance, which excludes every route event. Events recorded before this are attached to their target during the migration, from the name they carried.
- **The event names both ends of the path.** Instead of a bare `AS path X → Y` it says from where to where: this instance's AS, the AS announcing the measured address, that address, then the path before and after. A route means nothing without its two ends, and those two ends are exactly what you want to read on a target like Netflix.
- **A server that changed is no longer reported as a route that changed.** On a name answering from several machines, two reference traceroutes did not go to the same place: the AS path differs because the destination differs, not because anything was rerouted. That case no longer produces an event, and the route map no longer mixes paths towards different addresses — it keeps the one in use, or the pinned address, and says how many traceroutes it left out. This is what made pool targets unreadable.

### Fixed

- **Fixed: a route with no intermediate AS was drawn as if the two ends touched** (#30). When no hop of the traceroute could be attributed to an autonomous system — silent routers, private addressing, or Team Cymru lookups that had not answered yet — the page showed your network next to the target's network and nothing in between, which reads as "these two are neighbours". That is a claim, and usually a false one. The unknown segment is now marked as such, and the route says why it is empty: no traceroute recorded for this target yet, or a traceroute that revealed no autonomous system. Translated into the ten languages.
- **Fixed: a category could not be deleted once its targets were removed** (#25). Deleting a target archives it so its history survives, and those archived rows were still counted as live members of the category — so a category the operator had emptied refused to go, and the message told him to move targets he could no longer see anywhere. Archived targets are no longer counted; they move to an `archive` category created for the purpose, and that one is refused deletion while it holds anything. The detour matters: `targets.category_id` carries `ON DELETE CASCADE`, so an archived target left in a deleted category would have been deleted with it, measurements included. A test now asserts that foreign keys are enforced, because a suite running without them is more permissive than production and hides exactly that.

## 0.3.0

**Upgrade note — federation.** The format of the signature carried by
inter-instance requests changes: the recipient's AS number is now part of
what is signed, so a request cannot be replayed from one instance to
another. Both sides of a pairing must run 0.3.0 or later. Until a peer is
updated, its requests are refused with `request not addressed to this
instance` and it shows a `last_error` in *Peers and pairing*. Nothing else
in the release requires attention: measurements, targets and history are
untouched.

### Federation hardening

A tester audited the federation code. Eleven findings, all addressed, most of
them by refusing to trust anything a peer says about itself.

- **A pairing request no longer establishes an identity on its own.** It was
  signed with the key it carried, with no check of the announced URL or AS,
  so anyone could present themselves as a well-known network. The instance at
  the announced URL must now publish the same key and the same AS number,
  and **accepting requires the administrator to type the fingerprint** he
  received out of band — the comparison that ties a key to a real operator
  was only ever suggested by a confirmation dialog.
- **A trusted peer's key can no longer be replaced** by a new pairing
  request or a profile re-read: accepting a forged "new request" from a known
  peer used to hand the attacker its place, `trusted` state included. Genuine
  rotation goes through a new **Rotate key** action, which asks for the new
  fingerprint.
- **A signed request is bound to its recipient** (its AS number is part of
  what is signed), so it cannot be replayed from one instance to another. The
  **nonce is recorded only after the signature is verified**, keyed by AS and
  bounded, so an unauthenticated flood can neither fill the cache nor burn a
  peer's nonce in advance. *This changes the signed format: both sides of a
  pairing must run this version or later.*
- **An incident received from a peer is rebuilt locally.** Its identifier,
  the AS it accuses, the target and the free text are validated — the accused
  AS must be a member, the target a public address — and the opening date,
  the notice delay, the severity, the acknowledgement and the closure are
  decided here. A sender could previously pre-acknowledge the incident it
  reported, and set dates in the future that stayed displayed indefinitely.
- **A NOC is only emailed about something this instance measured itself.**
  Three corroborating peers were enough to make somebody else's SMTP server
  send the mail; the fourth condition is now our own measurement.
- **Nothing a peer writes is republished under your name**: the public
  incident feed carries this instance's own wording, and a sentence built
  from the figures for anyone else's. Free text is flattened to one bounded
  printable line everywhere, which also closes header injection in alert
  mail subjects; recipient and sender addresses are validated before SMTP.
- **Announced anchors are checked before being measured**: public unicast
  addresses only, at most eight, and one that the peer's own AS does not
  announce is flagged in the log. A peer could have the whole federation ping
  a third party's address, or an RFC 1918 one.
- **A peer URL is validated at ingest and again before becoming a link**, so
  a `javascript:` URL can neither be stored nor clicked on the public page.
- **All federation traffic refuses non-public addresses** after DNS
  resolution, redirects included, closing the SSRF a peer-controlled URL
  offered.
- **The SMTP password is no longer returned** by the federation identity
  endpoint; the interface is told only whether one is set.
- **An operator can trust only his own release keys**: `exclusive` on the
  first line of `trusted_keys_file` drops the keys embedded in the binary.
- **The federation now has tests** — the audit noted it had none. Identity
  binding, key replacement, audience and replay, nonce ordering, report and
  incident filtering, anchor and URL validation, the notification gate,
  the password masking and mail header safety: 23 new tests.

`DEPLOY.md` gains a section *Federation: what protects what*, including what
is still out of scope — an AS number is declared, not proved, and return
paths stay invisible.

- The changelog check is a **warning** on `main` and a **failure only when
  publishing**: a documentation lag is not a broken build, and a red main
  teaches people to ignore red. A version still cannot be released without
  its section or an Unreleased one.

- The entries of 0.2.12 and 0.2.13 are filed under their own versions: both
  were published while they still sat under *Unreleased*, which the release
  notes on GitHub reflected.

## 0.2.13

- **The global routing view, from RIPE RIS**, under the measured map: which
  prefix covers the target's address, which AS announces it, how many
  collector peers see it, and the networks the collectors see in front of it.
  It is kept in its own block and says so: RIS describes how the rest of the
  internet reaches the target, the map above describes how this probe does.
  Fetched in the background, cached for a week, and used to fill in the
  destination AS when no traceroute exists yet.
- **A map of the route**, in the spirit of a looking-glass bgpmap: one box per
  autonomous system, left to right from your network to the target's, arrows
  for the adjacencies actually observed over the last 25 traceroutes, the
  median latency on entering each network, and the current path solid against
  the earlier ones dashed. A target reached through two transits now shows
  both, which a single chain could not. Targets with one stable path keep the
  chain, and an unmeasured stretch stays an explicit break.
- **Fixed: the route under a graph did not start at your network or end at
  the target's.** It was built only from the autonomous systems seen in the
  traceroute, so a first hop in private space dropped your own AS and silent
  last hops dropped the destination's — exactly the targets where the chain
  mattered. The two ends are now added explicitly: your AS from the instance
  settings, and the AS announcing the address actually probed, resolved from
  that address and cached. An unknown segment in between is shown as such
  instead of letting the ends appear to touch, and the address and the time
  of the traceroute are stated under the chain.

## 0.2.12

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
