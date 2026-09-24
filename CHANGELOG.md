# Changelog

Versions are published as signed releases; servers with automatic updates
install the newest one directly, whatever versions came in between.

## 0.2.3

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
