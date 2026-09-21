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
curl -fsSLO https://github.com/OWNER/smokestack/releases/latest/download/install.sh
sudo sh install.sh --admin-email noc@example.net
```

Or with a package you already downloaded:

```sh
sudo sh install.sh --package smokestack-1.2.0-linux-amd64.zip --admin-email noc@example.net
```

It takes under a minute. At the end, the installer prints the back-office
address and the **master account password — write it down, it is shown once**.

### What the installer does

| Step | Detail |
|---|---|
| Verifies the package | Binary checksum against the manifest, architecture, then `smokestack selftest` |
| Creates a system user | `smokestack`, no shell, no home |
| Lays out the files | `/opt/smokestack/releases/<version>/` + `current` symlink |
| Writes the configuration | `/etc/smokestack/config.json`, **only if it does not exist yet** |
| Installs a systemd unit | Hardened: read-only system, only `CAP_NET_RAW` granted |
| Creates the master account | And prints its password |
| Adds a `smokestack` command | Always runs as the service user, even under `sudo` |

Options:

| Option | Default | |
|---|---|---|
| `--package FILE` | latest release | A `.zip` package or a binary you built |
| `--admin-email ADDR` | `admin@<hostname>` | Master account login |
| `--listen ADDR:PORT` | `127.0.0.1:8080` | Keep it on localhost behind a reverse proxy |
| `--public-url URL` | — | Public address, used by federation |
| `--no-service` | — | Skip systemd (containers, custom supervisors) |

Running the installer again on an installed server **upgrades it in place**
and keeps the configuration and the data.

### File layout

```
/opt/smokestack/releases/1.2.0/smokestack    binaries, one folder per version
/opt/smokestack/current -> releases/1.2.0    active version
/opt/smokestack/previous -> releases/1.1.0   rollback target
/etc/smokestack/config.json                  configuration (root:smokestack 0640)
/etc/smokestack/release-keys.pub             extra trusted release keys
/var/lib/smokestack/                         databases, archive spool, keys, backups
```

Nothing under `/var/lib/smokestack` is ever served over HTTP: the web
interface is embedded in the binary, there is no document root.

## 3. Put it behind HTTPS

smokestack listens on `127.0.0.1:8080` and expects a TLS reverse proxy in front.

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

> The back-office is in French for now; menu names are given below with their
> French label.

1. Open `https://latency.example.net/admin` and log in with the printed credentials.
2. **Publisher page** (*Page éditeur*): organisation, **AS number**, NOC contact, default language.
   The AS number enables the *Host network* page (RIPEstat + PeeringDB).
3. **Targets and categories** (*Cibles et catégories*): add your targets; the star marks *critical
   targets*, always shown at the top of the home page.
4. **Users** (*Utilisateurs*): create accounts for your team (roles: viewer, editor, admin, master).

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

*Instance → Mise à jour*: upload the package, review version and signature, click
**Installer et redémarrer**. The page reloads when the new version answers.
A **Revenir à X** button returns to the previous version at any time.

### From the command line

```sh
sudo smokestack update smokestack-1.3.0-linux-amd64.zip
sudo smokestack rollback
```

### Automatically

In *Instance → Mise à jour*, tick **Installer automatiquement les nouvelles versions**.
The service checks the release feed every 6 hours (`update.manifest_url`, by
default the project's GitHub releases) and installs new versions. Automatic
updates **always require a trusted signature**, even if unsigned packages
are allowed for manual uploads.

### Configuration (`/etc/smokestack/config.json`)

```json
"update": {
  "enabled": true,
  "auto_check": true,
  "auto_apply": false,
  "check_interval_hours": 6,
  "manifest_url": "https://github.com/OWNER/smokestack/releases/latest/download/latest.json",
  "trusted_keys_file": "/etc/smokestack/release-keys.pub",
  "allow_unsigned": false
}
```

Set `"enabled": false` to forbid any in-place update: binaries then only
change when you re-run the installer.

## 6. Publishing your own releases

Releases are built and signed by GitHub Actions when you push a tag.

**Once:**

```sh
make release-key                 # creates release.key (secret) and prints the public key
```

- append the printed `ed25519:...` line to `release.pub` and commit it: every
  binary built afterwards trusts that key;
- in the GitHub repository settings, add the **secret**
  `SMOKESTACK_RELEASE_KEY` with the content of `release.key`, and the
  **variable** `OFFICIAL_URL` with your website address;
- store `release.key` offline and delete it from the build machine.
  **Never commit it** (it is in `.gitignore`).

**Each release:**

```sh
git tag v1.3.0 && git push origin v1.3.0
```

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
| Service does not start | `journalctl -u smokestack -n 50` |
| All targets at 100 % loss | Outbound ICMP filtered? `ping -c3 1.1.1.1` from the host. The log says `sonde ICMP indisponible` if the raw socket was refused. |
| "In-place updates unavailable" | The binary must run from `/opt/smokestack/releases/<v>/`: re-run the installer |
| Package rejected: unknown signature | The signing key is not in `release.pub` nor `/etc/smokestack/release-keys.pub` |
| Upload fails behind nginx | `client_max_body_size 210m;` |
| Lost the admin password | `sudo smokestack user add -email other@example.net` creates another master |
| Health check | `curl -s http://127.0.0.1:8080/healthz` → `ok` |

## 9. Uninstalling

```sh
sudo sh install.sh --uninstall           # keeps /etc/smokestack and /var/lib/smokestack
sudo sh install.sh --uninstall --purge   # removes everything, including measurements
```
