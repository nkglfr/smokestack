# Changelog

Versions are published as signed releases; servers with automatic updates install the newest one directly, whatever versions came in between. The release workflow reads a section straight out of this file and publishes it as the release notes, so how an entry is written matters — see [DEPLOY.md § 6](DEPLOY.md#6-publishing-your-own-releases).

## Unreleased

**An instance can now be measured from several places at once.** Until this batch, one instance meant one probe, running on the instance's own machine: every graph described the path from that one machine to the target, and a loss of packets looked the same whether the target was down or the path from that single vantage point was. Probes can now run on other machines, report to the instance over HTTPS, and measure alongside the local one.

**Nothing changes for an instance that does not want this.** The local probe still reaches the service through its Unix socket, still measures every active target, and is not affected by anything below. There is no migration, and no setting to revisit.

### New

- **An operator can enrol a probe and issue it a token.**

  The ingestion endpoint has always authenticated remote probes — it reads a stored hash, compares it in constant time and refuses an empty one — but nothing could ever issue a token, so the remote mode could not be used at all. The missing half is here, in the back-office under *Instance → Probes* and as `smokestack probe add`, `probe token`, `probe revoke` and `probe list`.

  Only the hash is stored, so a token is shown once when it is issued and cannot be recovered afterwards. Rotating issues a new one and the previous one stops working immediately; revoking withdraws it without issuing a replacement, which is what a probe decommissioned in a hurry needs.

  The local probe is deliberately refused a token. It writes through a socket only a process on the host can open, and an HTTP credential as well would widen the surface for nothing.

  There is no way to delete a probe, only to disable it. Measurements are stored against the probe that made them, in a separate database, so removing the probe would orphan every measurement it ever sent with no way to attribute them again. Disabling stops it at the door and keeps its history readable.

- **The probe protocol is served over the network, not only on the socket.**

  A remote probe speaks exactly the protocol the local one speaks — the same three endpoints, the same payloads — and the two differ only in how the service decides who is calling. On the socket, being able to connect is the authentication, as it always has been. Over the network, the caller is whatever its token resolves to, and nothing otherwise.

  Set `probe.server` to the instance's base URL and `probe.token` to the issued token in the probe's configuration file. The URL must be `https`: a token travels on every request and the target list comes back on the same connection, so the probe refuses to start on plain HTTP rather than warning in a log nobody reads.

  A remote probe does not follow the service's version. It has its own release tree on its own update cadence, so it stays where it is rather than restarting itself to chase a number it cannot reach.

- **A remote probe measures only the targets it was assigned.**

  Enrolling a probe does not hand it the instance's target list. Each probe is given an explicit set of targets, in the back-office beside the probe, and it sees and writes those and nothing else — a measurement or a traceroute naming any other target is refused rather than quietly filed.

  A probe that has just been enrolled is assigned nothing, and therefore measures nothing until someone says what it should measure. That is the deliberate way round: the alternative is a probe that measures everything from the moment it exists, which is a decision nobody made.

  The local probe takes no assignment at all and keeps measuring every active target.

- **The instance records where each probe measures from.**

  The address a probe connects from is recorded automatically, and the AS it falls in is resolved from the same lookup service the Host network page uses. Both appear beside the probe in the back-office.

  Where the observed address would be wrong — a probe behind NAT, or an instance behind a reverse proxy that does not pass the original address on — an address can be stated by hand and takes precedence. Clearing it hands the question back to what the service observes, which keeps being recorded in the meantime, so clearing it lands on something current rather than something stale.

- **A target names the probe that decides its status.**

  With several probes measuring the same target, something has to say which one the target's status follows and which one runs its traceroutes. That is the target's primary probe, chosen among the probes it is assigned to, in the target's own form.

  Leaving it unset means the instance's own local probe, which is what every target has and what every existing instance keeps. A probe cannot be made primary for a target it is not assigned, because that would produce a target whose status nobody is able to report.

  Traceroute and immediate-check requests now go to that probe rather than to whichever probe asks for work first, so an anomaly is traced from the vantage point it was seen from. A request its probe has not collected within two minutes may be run by any probe: a primary that is not answering is itself something worth a traceroute, and running it elsewhere says more than not running it at all.

- **An alert says whether the other probes see the same thing.**

  When a target is in trouble, the probes that also measure it are asked what they see of it, and the answer goes into the alert. Everyone sees it, so it is the target. Only the deciding probe sees it, so it is likely the path from there. Something in between is reported as what it is, rather than resolved into a verdict nobody can check.

  This never changes whether an alert fires, only what it says. Requiring several probes to agree before alerting would turn a probe with a broken path into silence, which is the failure that costs the most and is noticed last.

  A probe with no data, or one whose targets are under maintenance, has no opinion and is not counted either way: counting silence as agreement would manufacture a confirmation out of nothing.

## 0.7.3

Three things an operator ran into on a real instance, which turned out to be three defects rather than three mistakes on his side.

He could not stop the service, so he could not replace the binary; he could not find where to set the address an instance announces to its peers, because there was nowhere to set it but a file; and the installer had said nothing when it left him without one.

**Nothing to reconfigure, and no migration.** An instance installed before this keeps its old probe unit until `install.sh` is re-run — until then, `systemctl stop smokestack smokestack-probe` stops both.

### New

- **The address of this instance is set in the back-office.**

  `federation.base_url` in the configuration file was the only way to give an instance the one thing a pairing request cannot do without, and `install.sh` writes it from `--public-url` — which is optional. An operator who installed without that flag could not pair, and had nowhere in the interface to fix it: 0.7.2 named the setting in the refusal, which is no help if you cannot find it.

  *Publisher → Address of this instance*, beside the AS number, saved without a restart. It is deliberately not the **Website** field next to it: that one is the operator's own site and may be a different domain, and announcing it to peers would send them somewhere they cannot reach. An address that is not plainly `http(s)` is refused when saved rather than at the moment somebody tries to use it.

  The file still wins where both are set, the same order the AS number and the organisation already follow — pin it there to keep it out of the interface's reach.

### Fixed

- **Stopping the service left a `smokestack` process running, and the binary could not be replaced.**

  In the default probe mode there are two systemd units running the same executable. Nothing tied them together, so `systemctl stop smokestack` left `smokestack-probe` running — a `smokestack` process in `ps`, holding the binary open, which reads exactly like a service that restarts itself and makes replacing the file fail with *text file busy*.

  The probe unit is now `PartOf=smokestack.service`: stopping or restarting one does the same to the other, and the probe can still be restarted on its own. An instance installed before this keeps its old unit until `install.sh` is re-run; `systemctl stop smokestack smokestack-probe` stops both in the meantime.

  `DEPLOY.md` says so where the installer is described, along with the thing that makes this moot: releases sit side by side under a `current` symlink switched atomically, so an update never needs the service stopped and never overwrites a running binary.

- **The installer said nothing when it left the instance without an address.**

  `--public-url` is optional, and skipping it silently removed any chance of pairing later. The installer now says so at the end, and names where to set it.

## 0.7.2

One fix, for a wall anybody setting up federation could hit: an instance that had never been given its own public address could not pair, and said so in a way that pointed at the wrong operator.

**Take it if you intend to pair with anyone.** It changes nothing else — same measurements, same pages, no migration — and if your instance already pairs, it had the setting all along and this release only makes the failure legible for the next person.

### Fixed

- **Pairing failed with `HTTP 400: incomplete request` when the fault was a setting at home.**

  A pairing request announces where to reach you. An instance whose `federation.base_url` is empty announces nothing, and the instance being asked refuses the request as incomplete — so the operator read a 400 from the far end and went looking for a problem at the other operator's. `install.sh` writes that setting from `--public-url`, so **any install done without that flag had it empty** and every pairing attempt failed this way.

  Checked before anything is sent now, beside the AS number, with a message that names the setting and says to restart. The instance being asked also names the missing field rather than counting it, so a request from a version without this check still produces something its sender can act on.

## 0.7.1

A private target's measurements could be kept by a cache in front of the instance and handed to a visitor. **An instance behind a CDN or a caching reverse proxy should take this one**; behind nothing it was never exposed, since a browser caches only for the person using it.

Four smaller holes close with it — a target's title could add headers to every alert email, a sendmail recipient could be a command-line option, a federated double-check resolved its name twice, and the live stream endpoint had no ceiling — along with the route map drawing an exchange over a network the current path skips. Nothing to reconfigure, and no migration.

### Fixed

- **The route map wrote an exchange over a network the current path skips, and drew loops through networks crossed once.**

  A link that skipped a column was drawn straight across it, under whatever box stood there, with its exchange written at its middle: on a target reached through two transits, the exchange of the current path covered the AS number and latency of the other one. Such a link now crosses each column through a slot of its own.

  Every unmeasured stretch of the traceroutes was one shared node. A path with silent routers both before and after a transit, common over IPv6, became a loop through that transit, and the link running back left its box sideways along its own row, across every box and link to its right, before turning down at the edge of the map. Each stretch is now a node of its own, named after the network it follows; in `/api/v1/aspath`, its `asn` is `?` followed by that network. A link that genuinely runs back goes round beneath its row through the gutters, never across a box.

  The current path now runs straight along the top row, with the paths seen earlier beneath it, and is drawn over them where they share a run.

- **A private target's measurements were announced as publicly cacheable.**

  `/api/v1/series` set `Cache-Control: public, max-age=15` on every answer, including a target no visitor may read. A shared cache in front of an instance keys on the address, so it could keep an operator's answer and hand it to the next visitor who asked for the same one. Reported by **@Kofysh**.

  Looking at the endpoint reported turned up five more, so the rule is now checked across every read endpoint at once rather than remembered one at a time:

  | | |
  |---|---|
  | `tree` | the operator's answer — every target, private ones included — carried no directive at all, because the authenticated branch returns before the line that sets one |
  | `charts` | the same, in both directions |
  | `availability` | no directive at all, on an endpoint with exactly the series shape |
  | `aspath` | the directive was set at the end of the function, and the two early returns above it skipped it |
  | a 404 | cacheable by default, so a visitor's *target not found* for a private target could be kept and handed back to the operator whose target it is |

  An instance behind a CDN or a caching reverse proxy should upgrade. One behind nothing was never exposed by this: a browser caches only for the person using it.

  The public pages stay plainly cacheable on purpose. A series reads the same for everyone allowed to see it, so the target's visibility decides and no `Vary` is added — `Vary` on a public page means one cache entry per visitor, which is the cache turned off on the pages that need it most during an incident.

- **A target's title could add headers to every alert email.**

  The subject of an alert is built from the title and reached the `Subject:` header unfiltered, on all three mail paths. A title carrying a newline added headers of its own — a `Bcc:`, for instance. Setting a title needs the editor role and configuring a channel needs the admin role, so this let the lesser role write into the greater role's mail.

- **A recipient address could be a command-line option.**

  A sendmail channel hands its sender and recipients to the mail program as arguments, and an argument starting with `-` is an option. Addresses are validated before they get there now, and `--` ends the option list as well. The executable is checked too: absolute, cleaned, and a recognised mail program — in any directory, because that is the part which varies between systems. Reported by **@Kofysh**.

- **A federated double-check resolved the name twice.**

  The guard that refuses a non-public address resolved the name, and the probe resolved it again minutes later and every five minutes after that. A name whose answer changed in between was validated as one address and measured as another. The address the guard accepted is pinned to the check now. Reported by **@Kofysh**.

- **The live stream endpoint had no ceiling.**

  `/api/v1/live` holds a connection, a goroutine and a poll for as long as a client keeps it open, with no limit on how many. It is bounded per address as well as in total — a single global ceiling would let one client take the endpoint from everybody. Nothing in the interface uses this endpoint; it is there for third parties. Reported by **@Kofysh**.

- **Ingestion read the body before the token.**

  `POST /api/v1/ingest` parsed the whole request before checking whether the caller held a probe token, and had no ceiling on the batch. Both roads in — that endpoint and the Unix socket the local probe uses — share one pair of limits now. Reported by **@Kofysh**.

### New

- **There is a security policy, and a private way to use it.**

  The cache leak above arrived as a public pull request, which disclosed the finding, the fix and the affected version at once, before any instance had upgraded. There was nowhere else to send it. [SECURITY.md](SECURITY.md) says where to report, what is in scope, and how a contribution on the security path is handled.

## 0.7.0

Every page of an instance, public and authenticated, now assembles its markup through one engine that escapes by default — and the back-office, which was a single file of two thousand four hundred lines, is twenty.

Around that: the hop where a route crosses a peering is named at last, a visitor chooses light or dark instead of inheriting it, the availability figure can be left unpublished, every back-office screen has its own address, and a page whose first request fails no longer sits blank. The CI gained an audit of the code, the shell, the workflows and the container image, which runs every Monday and not only on the days somebody pushes — and which found a way for a version number to carry a shell command into the release build.

**Two things to read before upgrading.** A TCP target left on the legacy *automatic* address family is now measured over IPv4 when its name has both, as that setting says it should be: the graph of such a target may shift, and setting its family to IPv6 keeps it where it was. Everything else is additive — an instance already publishing its availability figure keeps publishing it, the theme still follows the system until a visitor says otherwise, and there is nothing to reconfigure and no migration to run.

### New

- **The availability figure can be left unpublished.**

  It is the one number on a public target page a reader is likely to read as a commitment, whatever the two warnings beside it say. *Publisher → Availability* now decides whether it is published at all. On by default, and an instance that was already showing it keeps showing it after an upgrade: only an explicit choice takes it away.

  Off does two things, not one. The block leaves every target page, and `/api/v1/availability` stops answering a visitor or a share link — hiding it in the browser alone would leave the endpoint serving the figure to whoever knows the address. The back-office still reads it, since that is where the choice is made.

  Nothing stops being measured or counted. The passes are recorded as before, and switching the setting back on shows the same history, including the period it was hidden.

- **Light or dark is now a choice, not only what the system says.**

  The public pages already followed `prefers-color-scheme`. A visitor whose system is dark but who wants this page light — or the other way round — had no say. A button in the header cycles through three states: *Automatic*, *Light*, *Dark*. The choice is remembered by the browser, on that machine only, like the language.

  *Automatic* stays the default and keeps following the system, including when the system changes while the page is open. From *Automatic* the button goes to the opposite of what is on screen, so the first press always shows — a fixed *Automatic → Light → Dark* order would change nothing on the first press for anyone whose machine is set to light, which is most of them. *Automatic* comes back at the end of the round, the one place where a press that changes only the button is not a surprise: it hands the decision back to the system.

  The dark palette moves from the `prefers-color-scheme` media query to a `data-theme` attribute, which `theme.js` resolves from `<head>` before the first paint — so there is no light flash to swap away, and the palette is written once instead of once per condition. The chart colours read the same attribute, so a graph drawn after a switch is drawn in the right palette.

  The back-office is not covered: it carries its own light-only stylesheet and needs a palette of its own.

- **Every back-office screen has its own address.**

  `/admin/targets`, `/admin/traces`, `/admin/site` — one segment per screen, named after the screen. Refreshing stays where you were instead of dropping you back on the dashboard, the browser's back button works, and an address can be pasted to a colleague who lands on the right page.

  Nothing changed on the server: `GET /admin/` was already registered for its whole subtree, which is why the back-office scripts live under `/backoffice/`.

  Three cases that would otherwise make it worse than no routing at all:

  - **signing in from a deep address** lands you there, not on the dashboard — which is the case that makes a refresh worth having;
  - **an address your role may not open** falls back to the dashboard, and the address follows rather than lying about what is shown;
  - **an address that no longer exists** does the same, without an error.

  The screens keep what they had in memory, so the selected target of the traceroutes page and the target being edited are not in the address. They can be, later, without moving anything that is there now.

- **The hop where the route crosses a peering is named.**

  A peering LAN is deliberately kept out of the global routing table, so the lookup that names every other hop of a traceroute answered nothing for exactly the one an operator looks for: the address where two networks meet. That line stayed blank, and the map of the route drew the peer behind it as unknown.

  PeeringDB publishes both halves of what was missing — which prefixes belong to which exchange, and which member holds each address on them. A hop on a peering LAN now carries the AS of that member and the name of the exchange — a badge in the colour the interface reserves for exchanges, announced by `IX`, identical in the hop table, in the chain and on the map — and the hop that crosses one is marked in the table.

  The table of peering LANs is about 2 600 prefixes: fetched once a week, kept in `config.db` so a restart does not wait on PeeringDB, and consulted in memory. Member addresses are resolved one at a time in the background. No page waits for either, and an instance that cannot reach PeeringDB keeps what it had.

  This table decides what every hop of every traceroute is labelled with, and it comes from a source anyone with a member account can write to. A prefix far wider than a peering LAN can be — a default route above all — is refused and the refusal logged, and where two prefixes overlap the more specific one wins, deterministically.

- **The CI audits the code now, and not only on the days somebody pushes.**

  `make test` proves the tree builds and passes its tests. It says nothing about a published CVE in a dependency, and the formatting rule the project documents as mandatory was enforced nowhere. The CI now runs five jobs in parallel, one per surface so that a red mark says which, and a sixth that reports their verdict — one box to tick in branch protection instead of five:

  - **Go** — `gofmt`, `go vet`, staticcheck and govulncheck, all through `make audit`, so a contributor can run locally exactly what the CI runs. The two analysers and the toolchain they judge are pinned in the Makefile: a new release of either turns the build red on a branch that changed nothing.
  - **Shell** — ShellCheck on `install.sh` and `scripts/`. Seventeen kilobytes of shell that runs as root on a stranger's machine, previously checked only for whether it parses.
  - **Workflows** — actionlint, and zizmor reading them as an attack surface rather than as YAML. It is what found the release hole below.
  - **Image** — Trivy on the image the Dockerfile produces, failing both on a fixable HIGH and on a base whose distribution has stopped answering. It scans the image that job has just built and self-tested, rather than a second build of it.
  - **Tests** — unchanged, and `sh -n install.sh` drops out of it: ShellCheck parses the script too, and then reads it.

  **It also runs every Monday**, which is the part that is not decoration: a vulnerability published the day after a merge concerns a version that is already installed on somebody's machine, and nothing triggered by a commit would ever mention it.

  Turning it on found eight things. Four were functions no caller had, in `sketch.go`, `instance.go`, `maintenance.go` and `doublecheck.go`, and they are removed. The other four are error strings that one style check dislikes, and that check is turned off in `staticcheck.conf` with the reason recorded beside it: ST1005 refuses `"Telegram needs a bot token"` while accepting `"SMTP needs a server"` three lines away in the same switch, so the difference it is pointing at is the case of a brand name, not the style of a message.

### Changed

- **Less rounded, and rounded in one place.**

  Sixty `border-radius` declarations carried thirteen different values for the same handful of roles: 6, 7, 8, 9 and 10 px on controls that are the same kind of control, 12 and 14 px on panels that are the same kind of panel. Nothing distinguished them but the order they were written in.

  They become four variables — `--radius` for panels and cards, `--radius-s` for controls and small surfaces, `--radius-xs` for hairline bars, `--radius-pill` for the badges and chips that stay pills — and the scale is halved: 12 px becomes 6, 8 px becomes 4. True circles (status dots, the step numbers) keep `50%`, because they are circles rather than rounded corners.

  How round the interface looks is now four values rather than sixty. The back-office does not load `app.css`, so it carries the same four in its own `:root` instead of a scale it would never see.

- **The explanations on a target page fold away.**

  Eight grey paragraphs sat under the blocks of a target page, explaining what a band is, what dragging the navigator does, what an event list contains. They are read once and then re-read forever by someone who already knows. Each now sits behind a small `?` next to its section title, closed by default, one open at a time, `Escape` to close.

  Two are gone rather than folded. The route block explained in two sentences what the chain above it already shows, and the traceroute subtitle is shorter: *taken on an anomaly, and once a day for comparison*.

  The one paragraph still shown is the one that says the availability figure is not an SLA. It is there precisely so nobody has to go looking for it.

- **The route to a target takes the width of the page.**

  The AS chain was packed into the left of its block, each hop as wide as its name. It now spreads across the full width with the hops grown to equal size, which is the shape the thing actually is: a path read left to right.

  The *target* label under the last hop is gone with it. The frame is already green where the others are grey, and the origin's is blue — a word under each one said what the colour said first.

- **The load-balancing warning leaves the target pages.**

  A target whose name answered from more than one address carried a paragraph in an amber box explaining that the figures mix machines. It was right, and it was a wall of text above the graph on exactly the targets people look at most — public resolvers and CDNs, which are load-balanced by design.

  The diagnosis is not lost: *Why this loss?* in the back-office still names a rotating name when it is the explanation, which is where an operator looks when a figure puzzles them.

- **HTML is assembled by a template that escapes by default.**

  Pages are built in the browser by concatenating strings, which puts an escaping decision at every interpolation — there are over nine hundred of them. A scan found no defect among them today, which is a credit to whoever wrote them and not a property of the method: getting nine hundred right once is possible, keeping them right as the code changes is a promise nobody keeps by attention alone.

  `html.js` adds a tagged template where the default is safe:

  ```js
  H.html`<td>${t.title}</td>`         // escaped
  H.html`<tr>${rows.map(row)}</tr>`   // nested templates are not
  H.html`<div>${H.raw(svg)}</div>`    // saying so is the only way
  ```

  A reviewer now looks for `H.raw` instead of reading every interpolation to decide whether the value behind it could have come from a person. `null`, `undefined` and `false` render as nothing, so a conditional fragment needs no empty branch, and `H.url` drops anything that is not plainly `http(s)` before it reaches an `href`.

  `federation.html` is migrated as the first case, deliberately: everything that page shows was sent by another instance.

- **The shared header and footer and three more pages are assembled by the template.**

  `app.js`, `about.html`, `network.html` and `pairing.html` join `federation.html`: no `esc()` left in any of them, and no string concatenation building markup. The host network page gains named pieces — `routingCard`, `declaredCard`, `upstreams`, `presence` — in place of one function appending to a string across sixty lines.

  Everything the host network page shows comes from RIPEstat and PeeringDB, and `H.url` now drops a `javascript:` address before it can reach an `href` — which the old code escaped but still rendered.

  `App.escURL` is gone, having lost its last caller. `App.esc` stays until the back-office follows.

- **The overview and the target page are assembled by the template.**

  `index.html` is the last public page to convert: 106 `esc()` calls and twelve `innerHTML` assignments become none of either. The two render functions that ran to sixty and forty lines break into named pieces — `heroSection`, `howBox`, `railGroup`, `netCard`, `detailTags`, `chartCard`, `outcome` — each of which fits on a screen.

  The conversion was checked by comparing what the browser actually built, before and against after, on the overview and on a target page, and again with traceroutes, events and hostile values injected into both: every container came out identical, character for character.

- **The back-office is twenty files instead of one.**

  `admin.html` was 2 392 lines carrying a single 2 245-line `<script>`. Nothing in it could be opened on its own, and `go test` never saw it: the CI extracted the block and ran `node --check` over the whole thing.

  It becomes a 173-line page that loads `/backoffice/*.js`, one file per concern, in the order it already depended on: `core`, `gate`, `shell`, then a file per view. The largest is `targets.js` at 471 lines, the smallest `startup.js` at 23.

  They are plain scripts sharing a global scope rather than ES modules, which is what `app.js`, `i18n.js` and `theme.js` already are: splitting the file was the point, and rewriting a hundred references to shared state to buy module scope was not. Nothing moved between files.

  Checked by driving all twenty-one views in a browser before and after and comparing the markup produced: seventeen identical, and the four that differ do so by a clock — a login line in the audit log, the service log of a different process, two date fields defaulting to *now*, one *last seen*.

  `find web -name "*.js"` in the CI replaces `web/*.js`, which would have stopped checking the back-office the moment it was split, and a page's references to its own scripts and stylesheets are now versioned by pattern rather than from a list of five names that the twenty new files would have silently fallen off.

- **The back-office assembles its markup with the template too.**

  The twenty files of `/backoffice/` lose **238 `esc()` calls and 99 `innerHTML` assignments**, and gain no `H.raw`: not one place in the back-office needs to say a value is already markup. Nothing on any page of this instance, public or authenticated, is built by joining strings any more.

  Two things came out of the conversion, each a defect that escaping by hand had hidden. Markup stored in plain strings — forty-two `'<span class="badge">…</span>'` — was invisible to the eye as markup and had to become templates. Five `.map(esc).join("<br>")` escaped their items and left the separator as live markup, which is the right answer written the wrong way round.

  A third construct was correct and is written differently now. `aria-pressed="${kind === k}"` worked in a plain string, where a boolean stringifies to `true` or `false`, and the tab it highlights was highlighted. Under the template a bare boolean prints nothing, by design, so that a conditional fragment needs no empty branch — which would have turned a working attribute into a silent one. It is spelled out as `"true"` / `"false"`, as every boolean attribute in the codebase now is.

  Checked by driving all twenty-one views before and after and comparing the markup produced, then again with the API client substituted so that every view has rows to draw, each carrying `<img src=x onerror=…>` in every text field: no tag appears escaped anywhere, nothing executes, and the name shows as the text it is. The second pass matters — a view whose table is empty on the instance used for the check compares equal to itself whatever its row path does.

- **There is one way markup reaches the DOM, with no exception left.**

  One line of the back-office still assigned `outerHTML` directly. It worked, and what it wrote was a constant with nothing interpolated into it, so no escaping decision was being skipped — but it was the one place where the engine's output reached the page without going through it, and a rule with one exception is a rule nobody can check.

  `H.replace(el, tpl)` replaces an element the way `H.render(el, tpl)` fills one. It parses through a `<template>`, so a `<tr>` survives the trip where a bare container would have had it dropped by the parser, and it does nothing on an element that is null or already detached.

  `innerHTML`, `outerHTML`, `insertAdjacentHTML` and `document.write` now appear nowhere in `web/` outside `html.js` itself.

- **The footer is the link to the project and nothing else.**

  The copyright line was the last thing standing next to it. It asserted a reserved right over figures the instance publishes openly, next to a page that says those figures come with no warranty and are not an SLA — a claim nobody had asked for and nothing relied on.

  What is left is one line: *Powered by smokestack*, pointing at the project, from the address compiled into the binary.

### Fixed

- **The publisher page said *Saved* and showed the old values back.**

  Filling in the AS number, the title or a contact and saving worked: the value reached the database. The form then re-read `/api/v1/site`, which the server marks `public, max-age=600`, so the browser answered from its own cache — for ten minutes, including across a full page reload. An operator had every reason to conclude the page was broken.

  The back-office now reads with `cache: "no-store"`. Nothing it reads should ever come from a cache: it reads public endpoints, which are cacheable because they are, for a visitor — and the operator who has just saved is not a visitor.

- **A response that depends on who asks no longer says it may be cached for everyone.**

  `/api/v1/site` hands an authenticated caller the NOC phone, the address that receives contact notifications and the operator's own address, and strips all three for a visitor — then marked the answer `public, max-age=600` with no `Vary`. `/api/v1/aspath` is the same shape and matters more: an anonymous caller gets an empty path for a private target, for a target that hides its host, or on an instance that does not publish traceroutes, and masked addresses where the operator masks them.

  A shared cache in front — the reverse proxy [DEPLOY.md](DEPLOY.md) tells you to run — was therefore allowed to keep the operator's answer and hand it to a visitor. No proxy does this unless it is configured to cache, so this is a door left open rather than one anybody walked through, but it is the one thing the stripping exists to prevent.

  Both now send `Vary: Cookie, Authorization`, and `no-store` when the caller is authenticated. The list of traceroutes, which set no header at all on that path, says `no-store` too. A test fails on the old headers.

- **The map of the route drew every network on top of the others.**

  A target reached without a single intermediate AS — your own network, then theirs — produced a map with one box in the corner and a wide empty space, under a line saying no intermediate system was found.

  The server numbers the columns by distance and may skip one: that path gives 0, 2 and 3. Indexing an array by that number leaves a hole at 1, and spreading an array with a hole yields `undefined` where it is missing, so `Math.max(...)` returned `NaN`. Every box was placed at `translate(x, NaN)`, the `viewBox` became `0 0 782 NaN`, and the browser stacked them all in the corner — only the last one drawn was visible.

  Columns that hold nothing are dropped before anything is measured. The map draws the three networks again, each in its place.

- **For five minutes after an update, a browser could pair the new page with the previous stylesheet.**

  Pages are served `no-cache`, so the HTML a visitor sees is always the current one. The stylesheet and the scripts it loads were served `max-age=300`, so for up to five minutes after an upgrade a browser could hold the previous `app.css` against the new markup — and an intermediary cache could hold it for everyone behind it, not just one visitor.

  The pages now name the build in the addresses they load, `/app.css?v=<version>-<build date>`, rewritten on the way out so the files on disk stay plain and openable. A new build is a new address, and no cache can confuse it with the old one.

  Because such an address cannot go stale, the answer is kept for a year instead of five minutes (`max-age=31536000, immutable`), which is also fewer requests than before. An address without a version, or carrying the version of an earlier build, keeps the five-minute cache it has always had: a page cached before this change still works.

- **A translation was downloaded in full on every page view.**

  `/api/v1/i18n/<lang>` was served `no-cache` and carried no validator, so a browser had nothing to revalidate against and fetched the whole dictionary again on each page — about 7 kB compressed, before the first sentence could be shown. The header was deliberate: a dictionary gains keys with every version and can be reloaded while the instance runs, and a stale copy means a page showing `detail.navigator` where a sentence belongs.

  The address now names a revision, a short hash of everything loaded. It changes when a translation changes — including a file dropped into `/var/lib/smokestack/i18n/` and reloaded from the back-office — and does not change when the same content is loaded again, so a restart does not throw away every visitor's cache.

  Under that address a dictionary cannot go stale, so it is kept for a year. The kilobyte that carries the revision is the one thing never cached. Measured in a browser: 7.3 kB on the first page, 0 on the next. An address without a revision, or with an older one, is revalidated exactly as before.

- **An internet exchange never showed its link speed.**

  The host network page read `speed` where the API publishes `speed_mbps`, so the column had printed an em dash followed by `M` for every exchange since it existed.

- **A hop whose lookup timed out stayed blank for good.**

  A resolver that did not answer was written down as "no network announces this address", and nothing ever revisited that verdict. Only an answer is recorded now — a name that does not exist is one, a timeout is not.

  Traceroutes already stored are named from the cache as they are read, so a path recorded with holes fills in on the next view instead of keeping them for its ninety days. Nothing is rewritten: the lookups are kept beside the measurements, not inside them.

- **The route map was unreadable in dark mode.**

  Its text asked for colour variables that only the back-office palette defines, and the public pages do not load it. A property whose value cannot be resolved falls back to the initial one, so the names and the round-trip times came out black whatever the theme, and the box around an unmeasured stretch had no outline at all — in dark mode, black on near-black. The arrow heads were in the same case.

- **A route too long for the column was shrunk until nothing could be read.**

  The whole map was drawn on one line and scaled down to fit, names included. It now folds onto as many rows as the width allows: the boxes keep their size, and a link crossing to the next row runs back along a lane under the one it leaves.

- **A traceroute going silent inside the target's network drew a detour through the unknown.**

  Routers answering nothing between two hops of the same network are internal to it: the packet never left. They were counted as a hole in the AS path, so the map stepped out of the network, drew an unknown box and came back in with an arrow pointing backwards. Those hops now stay inside the network they belong to.

- **A page whose first request failed stayed blank until it was reloaded.**

  The public pages are built in the browser, and the home page drew nothing, header and footer included, until the overview arrived. One failed request was enough: a 502 from the reverse proxy while the service restarts for an update, a 429 from the rate limit, a dropped connection.

  A failed request is now retried after 1, 2 and 4 seconds, following `Retry-After` on a 429. If the overview still cannot be loaded, the page says so and keeps trying, then draws itself as soon as it gets through. A chart whose series cannot be loaded says so in its legend and tries again ten seconds later.

- **A version number could carry a shell command into the release build.**

  The release workflow checked a dispatched version against `[0-9]*.[0-9]*.[0-9]*` before using it. That pattern also accepts `1.2.3; rm -rf /`, because `*` matches any string, and the value went on to be interpolated into the scripts that run `make dist` and `gh release create`. A tag push skipped the check entirely, and a git ref name may contain a semicolon.

  Three changes, each of which alone would have been enough:

  | | |
  |---|---|
  | The version is validated against the shell | rejected unless it is digits, dots and the `-rc1` suffix — and on both doors, not just the dispatched one |
  | Nothing is interpolated into a script | every computed value reaches its `run:` block through the environment |
  | The workflow no longer grants write | `contents: write` sits on the one job that tags and publishes, instead of on every job in the file |

  Reaching this needed permission to start a workflow, which [DEPLOY.md § 6](DEPLOY.md#6-publishing-your-own-releases) already notes is a much lower bar than a shell with push rights. The approval gate stopped a release from being *published*; it did nothing about what the build did on its way there. `inputs.tag` in the image workflow had the same shape and gets the same treatment.

- **The container base had stopped receiving security updates, and the scan said it was clean.**

  The image was built `FROM alpine:3.20`, whose support window closed. A vulnerability scanner reports zero findings for it — not because there are none, but because nobody publishes advisories for that release any more. An unmaintained base is the one case where a clean report is itself the symptom.

  The base moves to `alpine:3.23`. The two releases in between were considered and skipped: 3.21 arrives with four fixable HIGH findings, 3.22 is clean but closer to its own end of life. Instances on the native install are unaffected — the binary is static and carries no distribution with it.

- **A release could have been built with an outdated Go while the audit passed.**

  The workflows asked `setup-go` for "1.26", which resolves to whatever the runner image has cached: four days after 1.26.9, which fixes ten vulnerabilities in the standard library, the runners still handed out 1.26.8. The standard library is linked into the binary, so a release built that week would have carried all ten whatever Go the server has installed — and raising the audit's toolchain alone would have turned the CI green over it. Both workflows now ask for the latest patch.

- **A fork whose owner name has an upper-case letter published no container image.**

  The image job builds `ghcr.io/<owner>/<repo>`, taking the owner as GitHub spells it. A registry refuses an upper-case letter in an image path, so the job failed at the last step of the release — after both packages, `latest.json` and `install.sh` were already published. The native install was unaffected; the image simply did not exist for that tag.

  The owner is lower-cased before the tags are built. This repository's owner is lower-case already, which is why it never showed here, and why a fork meets it only once it publishes its own release. Repairing it burns no version: *Actions → image → Run workflow* with the tag rebuilds and pushes the image for a tag that already exists.

- **A slow name lookup counted as lost packets on TCP targets, on every named target at once.**

  A TCP target was dialled by name, so each connection of a pass looked the name up again, inside its timeout, while ICMP targets resolve once and keep the answer five minutes. When the server's resolver hesitated, connections failed before the handshake, and every TCP target given by name lost them in the same minutes while the ICMP targets stayed at zero.

  TCP targets now resolve once per pass through the same cache and connect to the address. A pinned address, which they ignored, now applies to them, and the address recorded for a pass is the one measured. A name that cannot be resolved fails the pass with that reason.

  A TCP target left on the legacy automatic family is now measured over IPv4 when the name has both, as the setting says. Go used to prefer IPv6 on a dual-stack server, so the graph of such a target may shift; set its family to IPv6 to keep measuring IPv6.

  Because the pass connects to an address rather than to the name, a name with several addresses is measured at one of them — the pinned one, or the first the resolver returns — rather than wherever the dialler happened to succeed. That is the intent: a graph is worth less if it cannot say which address it measured.

## 0.6.2

A bug that made the long views lie by omission, and a public interface with less in it.

The 10-day, 3-month and 1-year views were empty on any instance measuring for more than three days — over data it was holding the whole time. That is the one to upgrade for, and it repairs itself: the tiers are rebuilt backwards from what the instance still has, a chunk at a time, with no command to run.

The rest is the interface saying less. The footer was five columns repeating the header and the About page; it is one line again. The disclaimer that spoke for every operator is gone, and the three open endpoints it carried moved to the About page. The language picker is a flag rather than ten spelled-out names.

Nothing to reconfigure. Three of these came from [TitinDev](https://github.com/TitinDev), with the reasoning in #63, #64 and #65.

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
