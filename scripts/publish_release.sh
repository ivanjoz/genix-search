#!/usr/bin/env bash

##
#  GenixSearch
#
#  Uploads locally built release artefacts to a GitHub release.
#
#  Nothing is compiled here. `release_binaries.sh` does the building on your
#  machine — RocksDB from source, three targets — and this script publishes
#  whatever it left in dist/, after checking the artefacts are complete,
#  checksum-clean and built for the architecture their name claims.
#
#  Usage:
#    scripts/release_binaries.sh                          # build all three first
#    scripts/publish_release.sh --dry-run                 # check everything, upload nothing
#    scripts/publish_release.sh                           # publish, version from Cargo.toml
#    scripts/publish_release.sh --version=v0.2.0 --draft
#    scripts/publish_release.sh --create-tag              # tag HEAD and push it first
#
#  Requires the GitHub CLI (`gh`) authenticated as an account with push
#  access to the repository.
#
#  License: Mozilla Public License v2.0 (MPL v2.0)
##

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

source "$SCRIPT_DIR"/util/die.sh
source "$SCRIPT_DIR"/util/release_targets.sh

version=""
dist_dir="$ROOT_DIR/dist"
repo=""
draft=0
dry_run=0
create_tag=0
allow_missing=0

for arg in "$@"; do
  case "$arg" in
    --version=*)     version="${arg#*=}" ;;
    --dist=*)        dist_dir="${arg#*=}" ;;
    --repo=*)        repo="${arg#*=}" ;;
    --draft)         draft=1 ;;
    --dry-run)       dry_run=1 ;;
    --create-tag)    create_tag=1 ;;
    --allow-missing) allow_missing=1 ;;
    -h|--help)       sed -n '3,22p' "${BASH_SOURCE[0]}" | sed 's/^#  \{0,1\}//;s/^#$//'; exit 0 ;;
    *)               die "unknown argument '$arg'" ;;
  esac
done

command -v gh >/dev/null 2>&1 \
  || die "the GitHub CLI (gh) is required: https://cli.github.com"

if [ -z "$version" ]; then
  version="v$(sed -n 's/^version = "\(.*\)"$/\1/p' "$ROOT_DIR/server/Cargo.toml" | head -1)"
  [ "$version" != "v" ] || die "could not read the version from Cargo.toml"
fi
case "$version" in v*) ;; *) version="v$version" ;; esac

if [ -z "$repo" ]; then
  # Derive the repository from the push remote rather than `gh repo view`, so
  # it does not depend on which account `gh` currently has active.
  origin="$(git -C "$ROOT_DIR" remote get-url origin 2>/dev/null || true)"
  [ -n "$origin" ] || die "no git remote 'origin'; pass --repo=OWNER/NAME"
  repo="$(printf '%s\n' "$origin" \
    | sed -e 's#^git@[^:]*:##' -e 's#^https\{0,1\}://[^/]*/##' -e 's#\.git$##')"
  case "$repo" in
    */*) ;;
    *) die "could not parse OWNER/NAME out of '$origin'; pass --repo=OWNER/NAME" ;;
  esac
fi

# ── check the artefacts ──────────────────────────────────────────────────────

[ -d "$dist_dir" ] || die "no artefact directory at $dist_dir — run scripts/release_binaries.sh first"
dist_dir="$(cd "$dist_dir" && pwd)"
sums="$dist_dir/SHA256SUMS"

assets=()
missing=()

for build in "${RELEASE_BUILDS[@]}"; do
  read -r target _target_cpu suffix <<<"$(release_build_spec "$build")"
  archive="genixsearch-$version-$suffix.tar.gz"

  if [ ! -f "$dist_dir/$archive" ]; then
    missing+=("$archive")
    continue
  fi

  # An artefact built for the wrong target is the one mistake a checksum
  # cannot catch, because the checksum matches the wrong file perfectly.
  if command -v file >/dev/null 2>&1; then
    expected_arch="$(release_expected_arch "$target")"
    actual_arch="$(tar -xzOf "$dist_dir/$archive" "genixsearch-$version-$suffix/genixsearch" 2>/dev/null | file -b - || true)"
    case "$actual_arch" in
      *"$expected_arch"*) ;;
      "") die "$archive does not contain genixsearch-$version-$suffix/genixsearch" ;;
      *) die "$archive holds a $actual_arch binary, expected $expected_arch — rebuild it" ;;
    esac
  fi

  assets+=("$dist_dir/$archive")
done

if [ ${#missing[@]} -gt 0 ]; then
  if [ "$allow_missing" -eq 0 ]; then
    log_error "missing artefacts for $version:"
    for archive in "${missing[@]}"; do log_error "  $archive"; done
    die "build them with scripts/release_binaries.sh --version=$version (or pass --allow-missing)"
  fi
  log_warn "publishing without ${#missing[@]} of ${#RELEASE_BUILDS[@]} builds: ${missing[*]}"
fi

[ ${#assets[@]} -gt 0 ] || die "no artefacts found for $version in $dist_dir"

[ -f "$sums" ] || die "no SHA256SUMS in $dist_dir — rebuild with scripts/release_binaries.sh"

# Re-verify rather than trust: dist/ survives across runs, so a stale tarball
# from an interrupted build can sit next to a current manifest.
log_info "verifying checksums"
( cd "$dist_dir" && sha256sum -c --ignore-missing "$sums" ) \
  || die "checksum verification failed in $dist_dir"

for asset in "${assets[@]}"; do
  archive="$(basename "$asset")"
  grep -q "  $archive\$" "$sums" \
    || die "SHA256SUMS has no entry for $archive — rebuild it so it is covered"
done

assets+=("$sums")

# ── check the tag ────────────────────────────────────────────────────────────

if [ -n "$(git -C "$ROOT_DIR" status --porcelain 2>/dev/null)" ]; then
  log_warn "the working tree has uncommitted changes; the artefacts may not match $version"
fi

tag_exists_remote=0
git -C "$ROOT_DIR" ls-remote --exit-code --tags origin "refs/tags/$version" >/dev/null 2>&1 \
  && tag_exists_remote=1
tag_exists_local=0
git -C "$ROOT_DIR" rev-parse -q --verify "refs/tags/$version" >/dev/null 2>&1 \
  && tag_exists_local=1

if [ "$tag_exists_remote" -eq 0 ]; then
  if [ "$tag_exists_local" -eq 1 ]; then
    log_info "pushing existing local tag $version"
    [ "$dry_run" -eq 1 ] || git -C "$ROOT_DIR" push origin "refs/tags/$version"
  elif [ "$create_tag" -eq 1 ]; then
    log_info "tagging HEAD as $version and pushing"
    if [ "$dry_run" -eq 0 ]; then
      git -C "$ROOT_DIR" tag -a "$version" -m "GenixSearch $version"
      git -C "$ROOT_DIR" push origin "refs/tags/$version"
    fi
  elif [ "$dry_run" -eq 1 ]; then
    log_warn "tag $version does not exist; a real run would need --create-tag"
  else
    die "tag $version does not exist locally or on origin; pass --create-tag to tag HEAD"
  fi
fi

# ── check we can actually publish ────────────────────────────────────────────

account="$(gh api user --jq .login 2>/dev/null || true)"
can_push="$(gh api "repos/$repo" --jq '.permissions.push' 2>/dev/null || echo "unknown")"
if [ "$can_push" != "true" ]; then
  log_warn "the active gh account${account:+ ($account)} cannot push to $repo"
  log_warn "switch to an account with write access first:"
  log_warn "  gh auth switch --user ${repo%%/*}"
  [ "$dry_run" -eq 1 ] || die "refusing to attempt a release that would fail"
fi

# ── publish ──────────────────────────────────────────────────────────────────

notes="$(mktemp)"
trap 'rm -f "$notes"' EXIT INT TERM

{
  echo "Fully static Linux binaries — no glibc requirement, nothing to compile."
  echo
  echo "| Asset | For |"
  echo "|-------|-----|"
  echo "| \`genixsearch-$version-x86_64-linux-musl.tar.gz\` | Any x86_64 Linux |"
  echo "| \`genixsearch-$version-aarch64-linux-musl.tar.gz\` | Any arm64 Linux (Graviton, Raspberry Pi, generic) |"
  echo "| \`genixsearch-$version-aarch64-linux-musl-neoverse-n1.tar.gz\` | Ampere Altra / Altra Max — Oracle Cloud \`VM.Standard.A1.Flex\`, built with \`-C target-cpu=neoverse-n1\` |"
  echo
  echo "Install the right one automatically:"
  echo
  echo '```sh'
  echo "curl -fsSL https://raw.githubusercontent.com/$repo/main/scripts/install.sh | sh"
  echo '```'
  echo
  echo "Or as a hardened systemd service:"
  echo
  echo '```sh'
  echo "curl -fsSL https://raw.githubusercontent.com/$repo/main/scripts/install.sh \\"
  echo "    | sudo sh -s -- --service --bind 0.0.0.0:1491"
  echo '```'
  echo
  echo "Verify a download against \`SHA256SUMS\`:"
  echo
  echo '```sh'
  echo "sha256sum -c SHA256SUMS --ignore-missing"
  echo '```'
  echo
  echo "See [PACKAGING.md](https://github.com/$repo/blob/main/server/PACKAGING.md) for the"
  echo "manual route and [CONFIGURATION.md](https://github.com/$repo/blob/main/server/CONFIGURATION.md)"
  echo "for every config knob."
} >"$notes"

log_info "publishing GenixSearch $version"
log_info "  repo:    $repo"
log_info "  account: ${account:-unknown}"
log_info "  assets:  ${#assets[@]}"
for asset in "${assets[@]}"; do
  log_info "    $(basename "$asset") ($(du -h "$asset" | cut -f1))"
done

if [ "$dry_run" -eq 1 ]; then
  echo
  log_info "release notes that would be published:"
  sed 's/^/    /' "$notes"
  echo
  log_success "dry run: nothing was uploaded"
  exit 0
fi

create_args=(--repo "$repo" --title "GenixSearch $version" --notes-file "$notes")
[ "$draft" -eq 0 ] || create_args+=(--draft)

if gh release view "$version" --repo "$repo" >/dev/null 2>&1; then
  # Re-publishing the same version: replace the assets rather than fail on a
  # name that already exists.
  log_info "release $version already exists; replacing its assets"
  gh release upload "$version" "${assets[@]}" --repo "$repo" --clobber
else
  gh release create "$version" "${assets[@]}" "${create_args[@]}"
fi

log_success "published $(gh release view "$version" --repo "$repo" --json url --jq .url)"
