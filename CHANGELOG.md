# Changelog

Versions are published as signed releases; servers with automatic updates install the newest one directly, whatever versions came in between. The release workflow reads a section straight out of this file and publishes it as the release notes, so how an entry is written matters — see [DEPLOY.md § 6](DEPLOY.md#6-publishing-your-own-releases).

## Unreleased

### Changed

- **SQLite and the Go toolchain move together again.**

  `modernc.org/sqlite` goes from 1.59.0 to 1.60.1, with `modernc.org/libc` and `golang.org/x/sys`. It raises the `go` directive to 1.26, so the build, the release workflow and the container image move from 1.25 to 1.26 in the same change.

  Dependabot proposed the dependency alone, which could not build: the module would have been refused before the tests ran. This is the second time that has happened, and the reason is structural rather than accidental — a bot that bumps one dependency cannot know the toolchain it now requires is pinned in three other files.

  The binary stays static and `CGO_ENABLED=0`; nothing about deployment changes.

- **The footer no longer disclaims the measurements it publishes.**
  The bottom line of every public page read `© 2026 <operator> — All measurements are published as-is, without warranty.`. The second half was shipped as a translated string, so it spoke for operators who had never written it: instances whose terms live on their own website, and instances that publish figures they stand behind.
  What remains is the copyright line and the link to the project. A disclaimer belongs on the About page, which is the operator's own text and can say exactly what that instance means. The `footer.rights` key leaves the ten language files with it.

- **The footer is one line again: the copyright and the link to the project.**
  It had grown into five columns repeated at the bottom of every public page, and nothing in them was published only there:
  | Column | Where it already is |
  |---|---|
  | Brand, organisation, location | the header, and `/about` |
  | Pages | the header navigation |
  | Operator | `/about` — organisation, ASN, owner, location, PeeringDB, NOC, website |
  | Open data | `/api/v1/overview`, `/healthz`, `/api/v1/version` |
  | Software | the project link, which stays in the bottom line |
  The cost is honest and small: no public page links the three open-data endpoints any more. They are documented, unauthenticated and unchanged, and `/about` is where a pointer to them would belong if one is wanted.
  Nineteen keys leave the ten language files, and `.fgrid`, `.fbrand` and `.fcol` leave the stylesheet. No endpoint, stored value or published surface changes.

- **The About page carries the three open endpoints.** Removing the footer took away the only link from any public page to `/api/v1/overview`, `/api/v1/version` and `/healthz`, leaving them documented in the README and nowhere a visitor would look. They now sit on the About page, next to what the operator already publishes about himself, which is where somebody wondering whether the figures are machine-readable actually goes.

- **The language is chosen from a flag, not from a list of ten names.**
  The header carried a `<select>` spelling out every shipped language — *English, Dansk, Deutsch, Español, Français, Italiano, Nederlands, Norsk bokmål, Português, Svenska*. It took a quarter of the bar on a laptop and a third of the width on a phone, for a control a visitor uses once.
  It is now the flag of the current language, and a menu where each flag keeps its native name: a flag alone names no language, and several of them stand for more than one. It behaves as a menu button — `aria-haspopup`, `aria-checked` on the current language, arrow keys, Escape — and the listeners it needs on the document are removed when it closes rather than piling up one set per language change.
  The country comes from the language file itself: `_meta.flag` (`"FR"`, `"SE"`) sits next to `name` and `dir`, is published by `/api/v1/i18n`, and is required of every shipped language by `TestI18nFiles`. A language dropped into `/var/lib/smokestack/i18n/` therefore arrives with its flag, and the interface has no table of countries to keep in step. A value that is not a two-letter country code is ignored, and that language falls back to its code.
  English flies the British flag and Norwegian Bokmål the Norwegian one — both are one edit away in the files, with no rebuild. Where a platform has no flag glyphs, the pair of regional indicators renders as the two letters of the country, which is why the button shows the flag alone.

### Fixed

- **The 10-day, 3-month and 1-year views were empty, and the year navigator with them** (#68).
  The coarse aggregation tiers never held more than the window the rollup rebuilds, so an instance measuring for two weeks drew three days at best and nothing at all on the longest views — over data it was holding the whole time.
  `RollupRange` clears whole destination buckets but read its source from the raw instant it was given. The bucket containing that instant was therefore rebuilt from only the part of its source lying after it, and when no source row fell in that part, the bucket was cleared and not rebuilt at all. Since the cleared range advances one whole bucket at a time while the read start sweeps continuously, the trailing bucket was reliably destroyed in the moment before the window moved past it. Every coarse bucket met that fate as the window left it, which is why a tier could never accumulate.
  The read is now aligned to the destination bucket, as the delete always was. That also fixes a quieter error in the same place: the edge bucket was being published as a complete hour while holding a fraction of one — three five-minute rows out of twelve, in the case this was found with.
  **An upgrade does not lose anything, and does not repair the past by itself**, so a backfill comes with it: a bounded chunk every twenty seconds, newest missing span first, until each tier covers everything its source still holds. An instance with months of fine-grained measurement recovers its long views without the start-up doing anything but measuring, and it says in the log when the tiers are complete. It never reaches further back than a tier is allowed to keep, so it cannot fight the next purge.

## 0.6.1

Two things 0.6.0 got wrong or got late: the availability figure it shipped never worked, and the federated double-check landed a few minutes after the tag went out.

An instance on automatic updates installs this by itself. Nothing to reconfigure.

### Fixed

- **The availability figure never worked in 0.6.0.**

  The two counters it is computed from were written on a code path the probe does not use. `Store.Record` has no caller outside the tests; the probe writes through `Store.RecordBatch`, a second copy of the same `INSERT` that did not carry the columns.

  On a real instance both stayed at zero, so every window read *nothing measured over this period*. The test suite passed throughout, because the tests were the only thing exercising the path that worked.

  The fix is not to add the columns to the second copy. `Record` now delegates to `RecordBatch`, so there is one write path and the tests exercise the one production runs. Two tests come with it, both of which fail without the change: one drives availability through the batch path on purpose, the other writes the same measurement through each entry point and compares every column of the resulting rows.

  **Availability therefore counts from this version, not from 0.6.0.** Upgrading now, the 24-hour window fills within a day and the thirty-day one within a month. The interface says so rather than inferring a past it cannot reconstruct.

### New

- **A peer in another AS can double-check one of your targets.**

  One vantage point cannot tell a target that is down from a path that is broken. A traceroute helps and often does not settle it: a path breaking four hops away is consistent with both readings. A paired peer in a different network settles it in one pass.

  Three answers, and the third is kept rather than forced into one of the first two:

  | What the peer sees | What it means |
  |---|---|
  | reaches it, you do not | the fault is between you and the target |
  | neither of you reaches it | the target is down |
  | reaches it only sometimes | reported as such |

  The request goes out by itself when an incident opens — the answer is wanted at three in the morning, not when a second operator wakes up — and comes back within a pass or two rather than at the end of the window.

  The difficulty of this feature is consent, not measurement. Unguarded, it is a way to make somebody else's machine probe a third party and launder the origin behind their AS number. Three guards, and each has a test that fails when that guard alone is removed:

  - **A grant, in advance, per peer.** One-directional and revocable, and revoking stops what is already running — a revocation that left running checks in place would be a revocation in name only.
  - **Only an address the requester already publishes**, verified by reading its own public API rather than taken on trust. This is the only guard that constrains *what* can be probed rather than how much. Accepted cost: a private target cannot be double-checked.
  - **Caps** on duration, concurrency, daily count and interval, with a ceiling a grant can lower but never raise. An operator who grants too much is refused by his own instance rather than protected by his peer's manners.

  What the measuring side does with the passes matters as much. The temporary target is private and absent from the tree; its measurements are diverted before the write path reaches the cascade, so they never touch that instance's availability, fault counts or history; the target and every pass are deleted when the window closes. It is listed in its back-office throughout, with the AS that asked, because consent given in advance must not mean invisible.

  The corroboration you get back stays in your back-office by default. Publishing it on the target's public page is a switch, off when shipped: it publishes a fact about a third party's reachability, measured by somebody who never agreed to have it published.

  The reasoning, including what was deliberately not done, is in `docs/design/federated-double-check.md`.

## 0.6.0

Five answers to the same question: what does a graph fail to tell an operator looking at it during an incident.

A certificate that will expire is not visible at all until it does. A target answering four packets out of five looks like a target losing twenty per cent, which is not what an operator means by unavailable. A restart you planned looks exactly like a target that died. And eighty milliseconds of latency says nothing about which of the resolver, the network or the handshake spent them.

A minor release: no protocol change and nothing to reconfigure.

One thing to know about the availability figure: it is counted from two new columns written from this version on, so it covers the period since the upgrade, and within a month the thirty-day window is complete. Everything else applies to existing instances as they stand — the certificate watch starts on its own, the maintenance calendar is empty until a window is declared, and the response-time breakdown measures nothing until asked.

The design note that preceded the double-check is kept in `docs/design/`, as written, with the two questions it left open marked as settled: the reasoning is the useful part, and a specification that hides its own trade-offs is worse than one that shows them.

### New

- **TLS certificates are watched, and their expiry announced in advance.**

  A certificate that expires without anyone noticing is one of the few outages that is entirely predictable. Every TCP target with a port is now inspected twice a day: one full TLS handshake, the certificate read, and nothing written to the latency series — a handshake is far slower than a bare connection and would distort the measurement it sits next to.

  One message per threshold crossed, at 30, 14, 7 and 1 day before expiry and on the day itself, rather than one per check: a certificate left alone for a month produces a handful of messages instead of sixty. A renewal rearms the whole sequence.

  Verification is the one a browser does, chain and name included, because a monitoring tool that accepts a certificate a client would refuse warns about nothing. A wrong name, an unverifiable chain, an expired certificate and a handshake that never completes are each reported in a sentence that names the cause, rather than quoting `x509: certificate signed by unknown authority` at an operator.

  The first threshold, the extra recipients and the global switch live on the back-office *TLS certificates* page, alerts leave through the notification channels already configured, and the same page carries a per-target switch and an *Inspect now* button.

- **Availability, counted in measurement passes rather than packets.**

  The detail page carries the availability over 24 hours, 7 days, 30 days and one year, defined as the proportion of measurement passes where the target answered at least one packet.

  That definition is deliberate and is printed next to the figure. A target losing one packet in five permanently is available, not unavailable a fifth of the time, and a loss rate answers a different question.

  The caveat sits next to the number as well, rather than in a footnote: this is not an SLA, it is what one probe saw from one point of the Internet, on ICMP or TCP, with no contractual exclusion. The misreading worth preventing is exactly that one.

  A window with no measurement shows a dash instead of zero — unknown and nought are not the same thing — and a window whose measurements start well after its beginning says so, so a target created last week does not advertise a yearly figure.

  Two counters are added to each measurement table and carried through the aggregation cascade, which is what makes a one-year figure a handful of rows to read rather than a scan of the whole history.

- **A maintenance calendar, with a banner on the public page.**

  A target you restart on purpose is not a target that is down, and left undeclared it costs twice: it wakes the on-call for nothing, and it leaves a hole in the graph that nobody can explain six months later.

  A window is declared per target, with a title and a note written to be read by a visitor, and carries two switches rather than one because both needs exist:

  - **silence the alerting, keep measuring** — when you want to watch without being woken. The incident is still recorded; only the message is held back;
  - **stop the measurement as well** — when the work would read as a hundred per cent loss and pollute the statistics.

  The public pages say so. A banner on the target's page explains the stop, in the ten languages, and states whether the measurement itself is paused: a gap in the graph and a silenced alert are not the same thing to a reader. The home page lists what is in progress and what is announced for the coming week, because a reader who knows a stop is planned does not open a ticket when it happens. A target whose measurement is stopped shows as *maintenance* rather than as a fault, and is counted out of the fault list instead of heading it.

  The passes inside a window that stopped the measurement are excluded from the availability figure, which is the whole point: an announced restart must not degrade the number the way an outage does. A period entirely under maintenance reads as unknown rather than as nought.

  Overlapping windows keep the strictest of the two, so a permissive window cannot cancel one that deliberately stops the probe. A window is capped at thirty days: a target stopped indefinitely is a target to disable, not to put under maintenance.

  The filtering happens at the single point both probe modes read their targets from, so it applies to the embedded probe and the isolated one alike, and the target comes back by itself when the window closes.

- **A response-time breakdown, on demand, in the back-office.**

  A target that answers in eighty milliseconds does not say where those eighty milliseconds go, and a slow resolver and a slow network produce the same figure with two different fixes.

  A TCP target can now be split into its steps — a fresh name resolution, the TCP connection, and the TLS handshake where the port expects one — each timed separately, with a bar showing which step dominates before the numbers are read, and the address actually reached named beside it.

  Deliberately on demand rather than on every pass, and deliberately not historised. The measurement costs a resolution and a full handshake, which is expensive to repeat every minute; the latency series must keep its definition, so this is a separate measurement beside the graph rather than a change to what the graph means; and it is a diagnostic you look at when something is wrong, not a metric to follow. One row per target, overwritten, and nothing on the public pages.

  The connection time comes from the kernel where the kernel knows it, so process load does not enter the figure. The handshake is timed without verifying the certificate on purpose: its validity is the job of the certificate watch, which checks it properly. The page states that the total is not the number on the public graph, which counts the connection alone.

### Changed

- **SQLite and the Go toolchain are brought up to date.**

  `modernc.org/sqlite` goes from 1.34.5 to 1.59.0 — twenty-five minor versions of the engine that writes every measurement — along with its own dependencies and `golang.org/x/sys`.

  The update requires Go 1.25, so the build, the release workflow and the container image move from 1.22 and 1.23 to 1.25: a version from early 2024 no longer receives the runtime and standard-library fixes a network-facing service should have.

  The binary stays static and `CGO_ENABLED=0`, so nothing about how it is deployed changes.

- **The fourth tile of the detail page reads *Loss* instead of *Uptime*.** It always showed the packet loss over the displayed window, subtracted from a hundred; the name invited precisely the reading the availability card now answers properly.

## 0.5.1

Addresses, and who gets to see them.

0.5.0 masked a pinned address and left two other paths open. This closes them, and says on the about page where the measurements come from — without publishing that address either.

Nothing breaks on upgrade. The masking is on by default, including for instances upgrading, because it shows less rather than more; *Settings → Addresses on public pages* turns it off for an operator who publishes addresses deliberately.

### Fixed

- **A public page shows the network of an address, not the address.**

  The previous version masked a pinned address and left two other paths open. A target given as a literal address published it as its host, and a target given by name published the address actually probed under its route — the one value on the page the operator never typed.

  Both are now masked for anybody who is not logged in: in the ten languages, in share links, and in the description indexed by search engines. Host names are untouched — a name is not an address, and it is what says which service a page is about.

  *Settings → Addresses on public pages* turns the whole thing off, and the per-target *hide the address* setting still removes it from the page altogether rather than masking it.

- **A name freed by renaming a target could not be reused** (#46).

  Renaming changed the title but not the address of the public page, so the old name stayed taken, and creating a target with it failed on a database constraint quoted verbatim at the operator.

  A rename still leaves the public address alone — a link already in somebody's ticket does not change behind his back — but the address is now a field of its own in the target's form, so freeing the old name is one deliberate edit.

  Two targets may also legitimately carry the same name. The second now takes `name-2` instead of being refused, accents fold rather than vanish (`Réseau Café` gives `reseau-cafe`), and a genuine collision is reported by naming the target that holds the address.

### New

- **The about page says where the packets leave from.**

  A measurement with no stated origin is of little use to whoever reads it. The page now carries the probe's network — reverse name, network, address family, AS number with the operator's name, and links to PeeringDB and RIPEstat, so a reader can check the claim rather than take it.

  It also carries the machine doing the measuring: processor, memory, version, uptime and platform, because a burst is not the same work on two cores and on thirty-two.

  **The address itself is not published.** The reverse name and the network situate the probe, which is the rule the targets already follow. Behind NAT the page says so instead of showing a local address that would mean nothing. Nothing is fetched while a visitor waits.

- **The link to the other address family is an action rather than a label.** It carries a pair of turning arrows, bold text and a distinct background, because it was a grey chip among grey chips and it is the one thing on that line you are meant to click.

## 0.5.0

One thing a target measured no longer leaks, one piece of work that no longer has to be done twenty times, and a default set that asked nobody's permission.

Nothing breaks on upgrade: no protocol change, no migration, no configuration to revisit. Existing targets keep every value they carry, so the new category parameters change nothing until you ask for it.

### New

- **A category lends its parameters to its targets** (#32).

  Interval, packets, spacing, timeout, retention, reference traceroute interval and the three thresholds can be set once on a category; every target that leaves the field empty takes it from there.

  The binding is **dynamic**, as the reporter asked: nothing is copied into a target, so changing the category changes what all of its inheriting targets measure at once. Not inheritable are the host, the protocol, the port and the address family, which are what makes a target a target rather than a copy of its neighbours.

  Two guardrails come with it. Each field shows how many targets currently take their value from it, so an edit states its own reach before it is saved; and a category value that would push a target outside the limits is refused naming that target, rather than producing a burst that cannot fit its interval.

  Existing targets all carry explicit values, so nothing moves until *Make its targets inherit…* clears the chosen fields across a category, measurements untouched.

- **A fresh instance no longer starts by pinging public DNS resolvers** (#40).

  It created targets on Google, Cloudflare and Quad9 without asking, which measures somebody else's infrastructure by default and makes every instance look alike.

  A new instance now starts with four RIPE Atlas anchors, which exist to be measured and are spread across four continents, and every one of them was resolved before being written down.

### Fixed

- **A pinned address is no longer published in full.**

  The notice on a target measured at a fixed address quoted that address in full on a public page, which is the operator's business to publish and not the tool's to decide.

  It now names the family and the network — *Measured at a fixed IPv4 address (142.251.XXX.XXX)* — which says what the notice is for without handing the address to everybody who opens the page.

### Documented

- **Anycast destinations are documented** (#39).

  One address served from many places steps outside what a latency graph can say: two passes a minute apart may have reached two different continents, and the median of the two describes nothing.

  `DESIGN.md` now says so, and says what to do instead — measure a named instance of the service where one exists, and read an anycast graph as a lower bound rather than a measurement of one machine.

## 0.4.0

Five of the seven issues opened by a tester in one afternoon, the route of a target made honest in three ways, and two settings that should never have been hardcoded.

Nothing breaks on upgrade: no protocol change, no configuration to revisit. Two migrations run on first start — existing route events are attached to the target they describe, and archived targets move to an `archive` category, so that deleting a category can never take a history with it.

### New

- **A target states which address family it measures** (#36).

  *Auto* resolved a name to IPv4 when there was one and fell back to IPv6 otherwise, so a target could change family behind the operator's back and the graph would mix two networks without saying so.

  A target now declares IPv4, IPv6, or the legacy automatic behaviour, and the family it measures is shown on its page. Existing targets keep the automatic behaviour, which is what they were already doing; the option is named for what it is rather than presented as a sensible default.

- **IPv4 and IPv6 as a pair of targets** (#27).

  A name with both an A and an AAAA record is two destinations: different transit, different latency, different failures. Measuring one of the two and calling it the service is a measurement that hides its own subject.

  A target can now have a twin measuring the same name in the other family, created in one click, and each page carries a link to the other so the comparison is one click away rather than a search.

  The suffix goes on the IPv4 one (`name-v4`), following Bortzmeyer's argument that IPv6 is the address family and IPv4 the legacy one, and it is only applied where nothing deliberate is being overridden.

- **Thresholds are settable, per target and for the instance** (#29).

  What separates ok, warn and crit was hardcoded at 0.4 % loss, 3 % loss and a latency factor of two. Those numbers suit a transit link and are wrong for a satellite link, a home connection or an intercontinental path.

  They are now settings, at instance level and per target, the per-target value winning where it is set. The shipped values are unchanged, so nothing moves until somebody decides it should.

- **The `Forwarded` header of RFC 7239 is read**, in preference to the older `X-Forwarded-For` (#28). Both are supported, so an existing reverse proxy keeps working, and the nginx recipe in `DEPLOY.md` now proposes the standard form — which also states the address nginx actually saw rather than appending to what the client sent. In either header only the last element is used, since that is the only one the proxy vouches for.
- **Building from source is documented** (#26), in `DEPLOY.md`: the one build dependency, `make build` and the plain `go build` behind it, cross-compiling, installing what you built with the same layout as a package, running it from a directory without installing anything, and what to run before proposing a change. The commands were executed against this version rather than written from memory.

### The route of a target was misleading in three ways

- **A route event now belongs to its target and to nothing else.**

  It was recorded at instance level, so a route change seen on one target appeared on the pages of every other, and a reader could not tell which target had actually moved.

  Events are now scoped per target, and a migration attaches the existing ones to the target they describe rather than discarding them.

- **The event names both ends of the path.** Instead of a bare `AS path X → Y` it says from where to where: this instance's AS, the AS announcing the measured address, that address, then the path before and after. A route means nothing without its two ends, and those two ends are exactly what you want to read on a target like Netflix.
- **A server that changed is no longer reported as a route that changed.** On a name answering from several machines, two reference traceroutes did not go to the same place: the AS path differs because the destination differs, not because anything was rerouted. That case no longer produces an event, and the route map no longer mixes paths towards different addresses — it keeps the one in use, or the pinned address, and says how many traceroutes it left out. This is what made pool targets unreadable.

### Fixed

- **Fixed: a route with no intermediate AS was drawn as if the two ends touched** (#30).

  When no hop in between announced an AS number — a common case inside one network, or where hops do not answer — the chain showed the origin next to the destination, which reads as a direct adjacency that does not exist.

  The gap is now shown as a gap, with the reason it is empty, so an absence of data is no longer displayed as a fact.

- **Fixed: a category could not be deleted once its targets were removed** (#25).

  Deleting a target archived it rather than removing it, and an archived target still counted as belonging to its category, so the category refused to go with no visible reason.

  Archived targets now move to an `archive` category of their own, which is itself undeletable while it holds anything. A category with nothing left in it can be deleted, and no history can be taken away with it.

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
