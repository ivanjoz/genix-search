#!/usr/bin/env sh

##
#  GenixSearch
#
#  Downloads a prebuilt GenixSearch release from GitHub and installs it.
#
#  The published binaries are fully static musl builds, so there is nothing to
#  compile and no glibc requirement. On aarch64 the script prefers the build
#  tuned for the Ampere Altra (Arm Neoverse N1) — Oracle Cloud's "Ampere A1"
#  shape — and falls back to the portable aarch64 build elsewhere.
#
#  Usage:
#    curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install.sh | sh
#    curl -fsSL .../install.sh | sudo sh -s -- --service --bind 0.0.0.0:1491
#
#    ./install.sh [--version vX.Y.Z] [--flavor auto|generic|ampere]
#                 [--prefix DIR] [--service] [--dry-run] [--keep-download]
#
#  License: Mozilla Public License v2.0 (MPL v2.0)
##

set -eu

REPO="${GENIXSEARCH_REPO:-ivanjoz/genix-search}"
VERSION="latest"
FLAVOR="auto"
PREFIX="${PREFIX:-/usr/local}"
INSTALL_SERVICE=0
DRY_RUN=0
KEEP_DOWNLOAD=0
# Everything after `--service` that we do not understand is forwarded to
# `install_service.py`, so `--bind`, `--auth-password`, `--name`, … all work.
SERVICE_ARGS=""

say() { printf 'genixsearch: %s\n' "$*"; }
die() { printf 'genixsearch: error: %s\n' "$*" >&2; exit 1; }

run() {
  if [ "$DRY_RUN" -eq 1 ]; then
    printf 'genixsearch: would run:'
    printf ' %s' "$@"
    printf '\n'
  else
    "$@"
  fi
}

# Spelled out rather than sed'd out of this file's header, because the usual
# invocation pipes the script into `sh`, where `$0` is not readable.
usage() {
  cat <<'EOF'
Downloads a prebuilt GenixSearch release from GitHub and installs it.

Usage:
  install.sh [options] [install_service.py options]

Options:
  --version VERSION   release tag to install (default: latest)
  --flavor FLAVOR     auto | generic | ampere (default: auto)
                      "ampere" forces the Neoverse N1 build on aarch64
  --prefix DIR        install prefix (default: /usr/local)
  --repo OWNER/NAME   source repository (default: ivanjoz/genix-search)
  --service           install as a systemd unit via install_service.py (root)
  --dry-run           print what would happen, change nothing
  --keep-download     leave the downloaded archive in place
  -h, --help          show this help

Any other argument is forwarded to install_service.py, so with --service you
can also pass --bind, --auth-password, --log-level, --name, --config-dir,
--data-dir, --force, --no-enable, --no-start.
EOF
}

while [ $# -gt 0 ]; do
  case "$1" in
    --version) VERSION="${2:?--version needs a value}"; shift 2 ;;
    --version=*) VERSION="${1#*=}"; shift ;;
    --flavor) FLAVOR="${2:?--flavor needs a value}"; shift 2 ;;
    --flavor=*) FLAVOR="${1#*=}"; shift ;;
    --prefix) PREFIX="${2:?--prefix needs a value}"; shift 2 ;;
    --prefix=*) PREFIX="${1#*=}"; shift ;;
    --repo) REPO="${2:?--repo needs a value}"; shift 2 ;;
    --repo=*) REPO="${1#*=}"; shift ;;
    --service) INSTALL_SERVICE=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --keep-download) KEEP_DOWNLOAD=1; shift ;;
    -h|--help) usage; exit 0 ;;
    *) SERVICE_ARGS="$SERVICE_ARGS $1"; shift ;;
  esac
done

[ "$(uname -s)" = "Linux" ] \
  || die "only Linux builds are published; found $(uname -s). Build from source instead."

case "$FLAVOR" in auto|generic|ampere) ;; *) die "--flavor must be auto, generic or ampere" ;; esac

if [ -n "$SERVICE_ARGS" ] && [ "$INSTALL_SERVICE" -eq 0 ]; then
  die "unrecognised arguments:$SERVICE_ARGS (pass --service to forward them to install_service.py)"
fi

# ── pick the asset ───────────────────────────────────────────────────────────

# MIDR_EL1 part 0xd0c is Arm's Neoverse N1, the core inside the Ampere Altra
# and Altra Max. `lscpu`/`/proc/cpuinfo` expose it as "CPU part".
is_neoverse_n1() {
  grep -qi '^CPU part[[:space:]]*:[[:space:]]*0xd0c$' /proc/cpuinfo 2>/dev/null
}

case "$(uname -m)" in
  x86_64|amd64)
    [ "$FLAVOR" = "auto" ] \
      || say "warning: --flavor $FLAVOR only applies to aarch64; ignoring it on $(uname -m)"
    asset_suffix="x86_64-linux-musl"
    ;;
  aarch64|arm64)
    if [ "$FLAVOR" = "ampere" ] || { [ "$FLAVOR" = "auto" ] && is_neoverse_n1; }; then
      asset_suffix="aarch64-linux-musl-neoverse-n1"
    else
      asset_suffix="aarch64-linux-musl"
    fi
    ;;
  *)
    die "unsupported architecture $(uname -m); only x86_64 and aarch64 builds are published"
    ;;
esac

if command -v curl >/dev/null 2>&1; then
  fetch() { curl -fsSL "$1" -o "$2"; }
  fetch_stdout() { curl -fsSL "$1"; }
elif command -v wget >/dev/null 2>&1; then
  fetch() { wget -qO "$2" "$1"; }
  fetch_stdout() { wget -qO- "$1"; }
else
  die "curl or wget is required"
fi

if [ "$VERSION" = "latest" ]; then
  say "resolving the latest release of $REPO"
  # Plain `sed` rather than `jq`, which is not installed on a minimal host.
  # GitHub pretty-prints the response, so `"tag_name": "…"` sits on its own line.
  VERSION="$(fetch_stdout "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1)"
  [ -n "$VERSION" ] || die "could not resolve the latest release of $REPO"
fi
case "$VERSION" in v*) ;; *) VERSION="v$VERSION" ;; esac

archive="genixsearch-$VERSION-$asset_suffix.tar.gz"
# GENIXSEARCH_BASE_URL exists so this script can be exercised against a local
# `dist/` directory served over HTTP; releases always come from GitHub.
base_url="${GENIXSEARCH_BASE_URL:-https://github.com/$REPO/releases/download/$VERSION}"

say "version:   $VERSION"
say "machine:   $(uname -m)"
say "asset:     $archive"

# ── download and verify ──────────────────────────────────────────────────────

tmp="$(mktemp -d)"
cleanup() { [ "$KEEP_DOWNLOAD" -eq 1 ] || rm -rf "$tmp"; }
trap cleanup EXIT INT TERM

say "downloading $base_url/$archive"
fetch "$base_url/$archive" "$tmp/$archive" \
  || die "could not download $archive — check that $VERSION publishes it"

if fetch "$base_url/SHA256SUMS" "$tmp/SHA256SUMS" 2>/dev/null; then
  expected="$(sed -n "s/^\([0-9a-f]\{64\}\)[[:space:]][[:space:]]*$archive$/\1/p" "$tmp/SHA256SUMS" | head -1)"
  if [ -z "$expected" ]; then
    say "warning: SHA256SUMS has no entry for $archive; skipping verification"
  elif command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$tmp/$archive" | cut -d' ' -f1)"
    [ "$actual" = "$expected" ] || die "checksum mismatch for $archive (expected $expected, got $actual)"
    say "checksum:  ok"
  else
    say "warning: sha256sum not found; skipping verification"
  fi
else
  say "warning: no SHA256SUMS published for $VERSION; skipping verification"
fi

tar -xzf "$tmp/$archive" -C "$tmp"
unpacked="$tmp/genixsearch-$VERSION-$asset_suffix"
[ -x "$unpacked/genixsearch" ] || die "the archive did not contain an executable at genixsearch"

# ── install ──────────────────────────────────────────────────────────────────

if [ "$INSTALL_SERVICE" -eq 1 ]; then
  command -v python3 >/dev/null 2>&1 || die "--service needs python3 to run install_service.py"
  [ "$DRY_RUN" -eq 1 ] || [ "$(id -u)" -eq 0 ] || die "--service must run as root (use sudo)"

  say "installing as a systemd service"
  # `install_service.py` owns the user, unit, config and data directory; give
  # it the binary we just unpacked so it does not try to invoke cargo. It reads
  # the reference config from its own parent directory, which is why the
  # archive keeps the repo-relative `scripts/` layout.
  # shellcheck disable=SC2086
  run python3 "$unpacked/scripts/install_service.py" install \
    --no-build --binary "$unpacked/genixsearch" --prefix "$PREFIX" $SERVICE_ARGS
  exit 0
fi

bindir="$PREFIX/bin"

# `mkdir -p` creates the missing levels, so the permission that matters is on
# the nearest ancestor that already exists — not on $bindir's direct parent,
# which is itself absent for a brand new --prefix.
probe="$bindir"
while [ ! -d "$probe" ] && [ "$probe" != "/" ] && [ "$probe" != "." ]; do
  probe="$(dirname "$probe")"
done
if [ ! -w "$probe" ]; then
  die "$probe is not writable; re-run with sudo or pass --prefix \"\$HOME/.local\""
fi

[ "$DRY_RUN" -eq 1 ] && verb="would install" || verb="installed "

run mkdir -p "$bindir"
run install -m 0755 "$unpacked/genixsearch" "$bindir/genixsearch"
say "$verb $bindir/genixsearch"

# The unpacked archive lives in a temp directory this script deletes on exit,
# so keep the reference config somewhere the "next steps" below can point at.
refconf="$PREFIX/share/genixsearch/genixsearch.cfg"
run install -D -m 0644 "$unpacked/genixsearch.cfg" "$refconf"
say "$verb $refconf"

if [ "$DRY_RUN" -eq 0 ]; then
  say "reports:   $("$bindir/genixsearch" --version 2>&1 | head -1)"
fi

cat <<EOF

Next steps:

  1. Copy the reference config and edit it — see CONFIGURATION.md for
     every key, and set channel.auth_password before binding publicly:
       install -m 0644 $refconf ./genixsearch.cfg

  2. Run it:
       $bindir/genixsearch -c ./genixsearch.cfg

  To run it as a hardened systemd service instead, re-run this script with
  --service (as root), which installs the unit, a system user, and a config
  under /etc/genixsearch/.
EOF

if [ "$KEEP_DOWNLOAD" -eq 1 ]; then
  say "kept the download at $tmp"
fi
