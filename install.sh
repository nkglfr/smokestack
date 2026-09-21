#!/bin/sh
# smokestack installer — Debian, Ubuntu, RHEL/Alma/Rocky, any systemd Linux.
#
#   sudo ./install.sh                                   # latest published release
#   sudo ./install.sh --package smokestack-1.2.0-linux-amd64.zip
#   sudo ./install.sh --package ./smokestack           # a binary you built yourself
#   sudo ./install.sh --admin-email noc@example.net --listen 0.0.0.0:8080
#   sudo ./install.sh --uninstall [--purge]
#
# Re-running the script on an installed server upgrades it in place.
set -eu

# Filled in by `make release`; override with SMOKESTACK_MANIFEST_URL.
MANIFEST_URL="${SMOKESTACK_MANIFEST_URL:-https://github.com/CHANGE-ME/smokestack/releases/latest/download/latest.json}"

ROOT=/opt/smokestack
ETC=/etc/smokestack
DATA=/var/lib/smokestack
USER_NAME=smokestack
UNIT=/etc/systemd/system/smokestack.service

PACKAGE=""; ADMIN_EMAIL=""; LISTEN="127.0.0.1:8080"; PUBLIC_URL=""
NO_SERVICE=0; UNINSTALL=0; PURGE=0

say()  { printf '\033[1;34m==>\033[0m %s\n' "$*"; }
warn() { printf '\033[1;33m!!\033[0m  %s\n' "$*" >&2; }
die()  { printf '\033[1;31mxx\033[0m  %s\n' "$*" >&2; exit 1; }

while [ $# -gt 0 ]; do
  case "$1" in
    --package)     PACKAGE="$2"; shift 2 ;;
    --admin-email) ADMIN_EMAIL="$2"; shift 2 ;;
    --listen)      LISTEN="$2"; shift 2 ;;
    --public-url)  PUBLIC_URL="$2"; shift 2 ;;
    --no-service)  NO_SERVICE=1; shift ;;
    --uninstall)   UNINSTALL=1; shift ;;
    --purge)       PURGE=1; shift ;;
    -h|--help)     sed -n '2,12p' "$0"; exit 0 ;;
    *) die "unknown option: $1 (see --help)" ;;
  esac
done

[ "$(id -u)" -eq 0 ] || die "run as root (sudo $0 ...)"
[ "$(uname -s)" = "Linux" ] || die "Linux only"

# ------------------------------------------------------------ uninstall
if [ "$UNINSTALL" -eq 1 ]; then
  say "Stopping and removing the service"
  systemctl disable --now smokestack 2>/dev/null || true
  rm -f "$UNIT" /usr/local/bin/smokestack
  systemctl daemon-reload 2>/dev/null || true
  rm -rf "$ROOT"
  if [ "$PURGE" -eq 1 ]; then
    say "Purging configuration and measurements"
    rm -rf "$ETC" "$DATA"
    userdel "$USER_NAME" 2>/dev/null || true
  else
    say "Kept $ETC and $DATA (use --purge to delete them)"
  fi
  exit 0
fi

case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *) die "unsupported architecture $(uname -m) (amd64 and arm64 are published)" ;;
esac

TMP=$(mktemp -d); trap 'rm -rf "$TMP"' EXIT

fetch() { # url dest
  if command -v curl >/dev/null 2>&1; then curl -fsSL --proto '=https' "$1" -o "$2"
  elif command -v wget >/dev/null 2>&1; then wget -q "$1" -O "$2"
  else die "curl or wget is required"; fi
}

unzip_to() { # zip dir
  if command -v unzip >/dev/null 2>&1; then unzip -q -o "$1" -d "$2"
  elif command -v python3 >/dev/null 2>&1; then python3 -m zipfile -e "$1" "$2"
  elif command -v bsdtar >/dev/null 2>&1; then bsdtar -xf "$1" -C "$2"
  else die "unzip (or python3) is required: apt install unzip"; fi
}

sha256() { sha256sum "$1" | cut -d' ' -f1; }

# ------------------------------------------------------ obtain the binary
if [ -z "$PACKAGE" ]; then
  case "$MANIFEST_URL" in *CHANGE-ME*)
    die "no release source configured: pass --package FILE, or set SMOKESTACK_MANIFEST_URL" ;;
  esac
  say "Looking up the latest release"
  fetch "$MANIFEST_URL" "$TMP/latest.json"
  URL=$(sed -n "/\"linux-$ARCH\"/,/}/s/.*\"url\": *\"\([^\"]*\)\".*/\1/p" "$TMP/latest.json" | head -1)
  SUM=$(sed -n "/\"linux-$ARCH\"/,/}/s/.*\"sha256\": *\"\([^\"]*\)\".*/\1/p" "$TMP/latest.json" | head -1)
  [ -n "$URL" ] || die "no linux-$ARCH package in $MANIFEST_URL"
  say "Downloading $URL"
  fetch "$URL" "$TMP/pkg.zip"
  [ "$(sha256 "$TMP/pkg.zip")" = "$SUM" ] || die "checksum mismatch for the downloaded package"
  PACKAGE="$TMP/pkg.zip"
fi

[ -f "$PACKAGE" ] || die "package not found: $PACKAGE"
mkdir -p "$TMP/pkg"
if head -c 4 "$PACKAGE" | grep -q "PK"; then
  unzip_to "$PACKAGE" "$TMP/pkg"
  BIN=$(find "$TMP/pkg" -maxdepth 2 -type f -name smokestack | head -1)
  MF=$(find "$TMP/pkg" -maxdepth 2 -type f -name manifest.json | head -1)
  [ -n "$BIN" ] && [ -n "$MF" ] || die "invalid package: smokestack and manifest.json are required"
  WANT=$(sed -n 's/.*"sha256": *"\([0-9a-f]*\)".*/\1/p' "$MF")
  [ "$(sha256 "$BIN")" = "$WANT" ] || die "binary checksum does not match the manifest: package altered"
  PKG_ARCH=$(sed -n 's/.*"arch": *"\([a-z0-9]*\)".*/\1/p' "$MF")
  [ "$PKG_ARCH" = "$ARCH" ] || die "package is for $PKG_ARCH, this server is $ARCH"
else
  BIN="$PACKAGE"   # a raw binary built from source
fi
chmod 0755 "$BIN"

# The binary checks itself: embedded assets, English language file, SQLite.
"$BIN" selftest >/dev/null 2>"$TMP/selftest.err" || { cat "$TMP/selftest.err" >&2; die "selftest failed"; }
VERSION=$("$BIN" version | awk '{print $2}')
[ -n "$VERSION" ] || die "cannot read the version of the binary"
say "Installing smokestack $VERSION (linux-$ARCH)"

# ------------------------------------------------------- user and folders
if ! id "$USER_NAME" >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null ||
    useradd -r -d "$DATA" -s /sbin/nologin "$USER_NAME"
fi
install -d -m 0755 -o "$USER_NAME" -g "$USER_NAME" "$ROOT" "$ROOT/releases"
install -d -m 0750 -o "$USER_NAME" -g "$USER_NAME" "$DATA"
install -d -m 0750 -o root -g "$USER_NAME" "$ETC"

# Side-by-side releases; `current` is switched atomically.
install -d -m 0755 -o "$USER_NAME" -g "$USER_NAME" "$ROOT/releases/$VERSION"
install -m 0755 -o "$USER_NAME" -g "$USER_NAME" "$BIN" "$ROOT/releases/$VERSION/smokestack"
OLD=""
if [ -L "$ROOT/current" ]; then OLD=$(basename "$(readlink "$ROOT/current")"); fi
if [ -n "$OLD" ] && [ "$OLD" != "$VERSION" ]; then
  ln -sfn "releases/$OLD" "$ROOT/previous"
  say "Upgrading $OLD -> $VERSION"
fi
ln -sfn "releases/$VERSION" "$ROOT/current.tmp" && mv -Tf "$ROOT/current.tmp" "$ROOT/current"
chown -h "$USER_NAME:$USER_NAME" "$ROOT/current" "$ROOT/previous" 2>/dev/null || true

# --------------------------------------------------------- configuration
FRESH=0
if [ ! -f "$ETC/config.json" ]; then
  FRESH=1
  HOST=$(hostname -s 2>/dev/null || echo probe)
  cat > "$ETC/config.json" <<EOF
{
  "listen": "$LISTEN",
  "data_dir": "$DATA",
  "probe": { "enabled": true, "slug": "$HOST", "name": "$HOST", "location": "" },
  "federation": { "enabled": false, "base_url": "$PUBLIC_URL", "anchors": [] },
  "update": {
    "enabled": true,
    "auto_check": true,
    "auto_apply": false,
    "check_interval_hours": 6,
    "trusted_keys_file": "$ETC/release-keys.pub"
  }
}
EOF
  chown root:"$USER_NAME" "$ETC/config.json"; chmod 0640 "$ETC/config.json"
  say "Configuration written to $ETC/config.json"
fi
[ -f "$ETC/release-keys.pub" ] || {
  printf '# Extra trusted release keys, one per line: ed25519:BASE64\n' > "$ETC/release-keys.pub"
  chmod 0644 "$ETC/release-keys.pub"
}

# Command-line wrapper: always runs as the service user, so that files it
# creates in $DATA stay writable by the service.
cat > /usr/local/bin/smokestack <<EOF
#!/bin/sh
if [ "\$(id -u)" -eq 0 ]; then
  exec runuser -u $USER_NAME -- $ROOT/current/smokestack "\$@"
fi
exec $ROOT/current/smokestack "\$@"
EOF
chmod 0755 /usr/local/bin/smokestack

# --------------------------------------------------------------- systemd
if [ "$NO_SERVICE" -eq 0 ]; then
  command -v systemctl >/dev/null 2>&1 || die "systemd not found (use --no-service and run the binary yourself)"
  cat > "$UNIT" <<EOF
[Unit]
Description=smokestack latency monitoring
Documentation=https://github.com/CHANGE-ME/smokestack
After=network-online.target
Wants=network-online.target
StartLimitIntervalSec=0

[Service]
User=$USER_NAME
Group=$USER_NAME
ExecStart=$ROOT/current/smokestack -config $ETC/config.json
Restart=always
RestartSec=3
# ICMP probes need raw sockets; nothing else is granted.
AmbientCapabilities=CAP_NET_RAW CAP_NET_BIND_SERVICE
CapabilityBoundingSet=CAP_NET_RAW CAP_NET_BIND_SERVICE
NoNewPrivileges=yes
ProtectSystem=strict
ReadWritePaths=$DATA $ROOT
ProtectHome=yes
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX AF_NETLINK
RestrictNamespaces=yes
LockPersonality=yes
SystemCallArchitectures=native
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload
  systemctl enable smokestack >/dev/null 2>&1
  systemctl restart smokestack

  say "Waiting for the service"
  PORT=${LISTEN##*:}; HOSTPART=${LISTEN%:*}
  [ "$HOSTPART" = "0.0.0.0" ] || [ -z "$HOSTPART" ] && HOSTPART=127.0.0.1
  i=0; OK=0
  while [ $i -lt 30 ]; do
    if command -v curl >/dev/null 2>&1 && curl -fs "http://$HOSTPART:$PORT/healthz" >/dev/null 2>&1; then OK=1; break; fi
    if ! command -v curl >/dev/null 2>&1 && systemctl is-active --quiet smokestack; then OK=1; break; fi
    i=$((i+1)); sleep 1
  done
  [ $OK -eq 1 ] || { journalctl -u smokestack -n 30 --no-pager >&2; die "the service did not start"; }
fi

# ------------------------------------------------------ first admin account
CREDS=""
if [ $FRESH -eq 1 ]; then
  EMAIL="${ADMIN_EMAIL:-admin@$(hostname -f 2>/dev/null || hostname)}"
  case "$EMAIL" in *@*.*) ;; *) EMAIL="$EMAIL.local" ;; esac
  if OUT=$(runuser -u "$USER_NAME" -- "$ROOT/current/smokestack" user add \
             -config "$ETC/config.json" -email "$EMAIL" -name Admin -role master 2>/dev/null); then
    CREDS="$OUT"
  else
    warn "could not create the admin account; use the setup code shown by: journalctl -u smokestack | grep setup"
  fi
fi

echo
say "smokestack $VERSION is installed"
echo "    listening on   http://$LISTEN   (put a TLS reverse proxy in front, see DEPLOY.md)"
echo "    back-office    http://$LISTEN/admin"
if [ -n "$CREDS" ]; then
  echo "$CREDS" | sed 's/^/    /'
  echo "    -> write this password down now, it is not stored in clear anywhere"
fi
echo "    logs           journalctl -u smokestack -f"
echo "    update         sudo smokestack update smokestack-X.Y.Z-linux-$ARCH.zip   (or from the back-office)"
