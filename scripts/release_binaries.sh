#!/usr/bin/env bash

##
#  GenixSearch
#
#  Builds the release binaries for every published Linux target and packages
#  each one as a tarball, next to a shared SHA256SUMS manifest.
#
#  Every target is a fully static `musl` build, so the artefacts carry no
#  glibc floor and run on any Linux kernel of the matching architecture.
#  Cross-compilation happens inside the `cross` containers, which ship the
#  musl C/C++ toolchain that `librocksdb-sys` needs.
#
#  Usage:
#    scripts/release_binaries.sh                          # every build, version from Cargo.toml
#    scripts/release_binaries.sh --version=v0.2.0         # override the version label
#    scripts/release_binaries.sh --build=linux-arm64-ampere
#    scripts/release_binaries.sh --out=./dist --keep-dirs
#
#  Environment:
#    CONTAINER_ENGINE   docker or podman (auto-detected)
#    CROSS_VERSION      cross version to install when it is missing (0.2.5)
#
#  License: Mozilla Public License v2.0 (MPL v2.0)
##

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

source "$SCRIPT_DIR"/util/die.sh

CROSS_VERSION="${CROSS_VERSION:-0.2.5}"

# Every published build. Keep this list in sync with the `build.matrix` of
# `.github/workflows/release.yml` and with the arch detection in `install.sh`.
ALL_BUILDS=(linux-amd64 linux-arm64 linux-arm64-ampere)

# Extra files shipped inside every tarball, at their repo-relative paths.
#
# The layout is not cosmetic: `install_service.py` resolves the reference
# config as `<script>/../../genixsearch.cfg`, so it has to stay one directory
# below the config to work straight out of the tarball.
EXTRA_FILES=(
  genixsearch.cfg
  README.md
  LICENSE.md
  CONFIGURATION.md
  PROTOCOL.md
  scripts/install_service.py
)

# Resolves a build name into `<target> <target-cpu> <asset-suffix>`.
#
# `neoverse-n1` is the core of the Ampere Altra / Altra Max, which is what
# Oracle Cloud rents out as the "Ampere A1" (VM.Standard.A1.Flex) shape.
build_spec() {
  case "$1" in
    linux-amd64)
      echo "x86_64-unknown-linux-musl - x86_64-linux-musl"
      ;;
    linux-arm64)
      echo "aarch64-unknown-linux-musl - aarch64-linux-musl"
      ;;
    linux-arm64-ampere)
      echo "aarch64-unknown-linux-musl neoverse-n1 aarch64-linux-musl-neoverse-n1"
      ;;
    *)
      die "unknown build '$1' (expected one of: ${ALL_BUILDS[*]})"
      ;;
  esac
}

version=""
out_dir="$ROOT_DIR/dist"
keep_dirs=0
builds=()

for arg in "$@"; do
  case "$arg" in
    --version=*) version="${arg#*=}" ;;
    --build=*)   builds+=("${arg#*=}") ;;
    --out=*)     out_dir="${arg#*=}" ;;
    --keep-dirs) keep_dirs=1 ;;
    -h|--help)   sed -n '3,26p' "${BASH_SOURCE[0]}"; exit 0 ;;
    *)           die "unknown argument '$arg'" ;;
  esac
done

if [ ${#builds[@]} -eq 0 ]; then
  builds=("${ALL_BUILDS[@]}")
fi

# Validate every build name before spending minutes on the first one.
for build in "${builds[@]}"; do
  build_spec "$build" >/dev/null
done

if [ -z "$version" ]; then
  version="v$(sed -n 's/^version = "\(.*\)"$/\1/p' "$ROOT_DIR/Cargo.toml" | head -1)"
  [ "$version" != "v" ] || die "could not read the version from Cargo.toml"
fi
# Tags carry the `v`, `cargo` does not. Normalise on the tag spelling.
case "$version" in v*) ;; *) version="v$version" ;; esac

command -v cargo >/dev/null 2>&1 || die "cargo is required but was not found in PATH"

if [ -z "${CONTAINER_ENGINE-}" ]; then
  if command -v docker >/dev/null 2>&1; then
    CONTAINER_ENGINE=docker
  elif command -v podman >/dev/null 2>&1; then
    CONTAINER_ENGINE=podman
  else
    die "neither docker nor podman was found in PATH; one of them is required by cross"
  fi
fi
command -v "$CONTAINER_ENGINE" >/dev/null 2>&1 \
  || die "$CONTAINER_ENGINE was not found in PATH"

if ! command -v cross >/dev/null 2>&1; then
  log_info "cross was not found; installing cross v$CROSS_VERSION with cargo"
  cargo install cross --version "$CROSS_VERSION" --locked
fi

mkdir -p "$out_dir"
out_dir="$(cd "$out_dir" && pwd)"
sums="$out_dir/SHA256SUMS"

log_info "packaging GenixSearch $version"
log_info "  engine:  $CONTAINER_ENGINE"
log_info "  builds:  ${builds[*]}"
log_info "  output:  $out_dir"

for build in "${builds[@]}"; do
  read -r target target_cpu suffix <<<"$(build_spec "$build")"

  # `cross` compiles with the host's rustc against the container's linker and
  # sysroot, so the target's `std` has to exist on the host side.
  if command -v rustup >/dev/null 2>&1; then
    rustup target list --installed 2>/dev/null | grep -qx "$target" \
      || rustup target add "$target"
  fi

  target_dir="$ROOT_DIR/target/release-$build"
  rustflags="${RUSTFLAGS-}"
  if [ "$target_cpu" != "-" ]; then
    rustflags="$rustflags -C target-cpu=$target_cpu"
  fi

  # musl defaults new threads to a 128 KiB stack, against glibc's 8 MiB. Rust
  # sets its own size for `std::thread`, but RocksDB's compaction and flush
  # threads are C++ threads that take the libc default and can overflow it.
  # musl seeds that default from the ELF PT_GNU_STACK header, so linking with a
  # larger stack size fixes every thread the process spawns.
  case "$target" in
    *-musl) rustflags="$rustflags -C link-arg=-Wl,-z,stack-size=2097152" ;;
  esac

  # `cross` reads CARGO_TARGET_DIR on the host and bind-mounts it into the
  # container, so a per-build directory keeps the two aarch64 builds — which
  # share a target triple but not their RUSTFLAGS — from invalidating each
  # other.
  build_env=(
    "CROSS_CONTAINER_ENGINE=$CONTAINER_ENGINE"
    "CARGO_TARGET_DIR=$target_dir"
    "RUSTFLAGS=$rustflags"
  )

  # `tikv-jemalloc-sys` freezes the page size at compile time and defaults to
  # whatever the build host uses (4 KiB on every x86_64 CI runner). Oracle
  # Linux and RHEL run aarch64 kernels with 64 KiB pages, where such a binary
  # aborts on the first allocation. A 64 KiB build works on both, so pin it.
  #
  # It has to be unset rather than empty for the other targets: the crate reads
  # it with `env::var().ok()`, so an empty value still reaches configure as a
  # bare `--with-lg-page=` and fails the build.
  jemalloc_lg_page=""
  case "$target" in
    aarch64-*)
      jemalloc_lg_page=16
      build_env+=("JEMALLOC_SYS_WITH_LG_PAGE=$jemalloc_lg_page")
      ;;
  esac

  echo
  log_info "building $build"
  log_info "  target:     $target"
  log_info "  target-cpu: $([ "$target_cpu" = "-" ] && echo "<default>" || echo "$target_cpu")"
  log_info "  rustflags:  ${rustflags:-<none>}"
  [ -z "$jemalloc_lg_page" ] || log_info "  page size:  $((2 ** jemalloc_lg_page)) bytes (jemalloc --with-lg-page=$jemalloc_lg_page)"

  env "${build_env[@]}" cross build --locked --release --target "$target" --bin genixsearch

  binary="$target_dir/$target/release/genixsearch"
  [ -f "$binary" ] || die "build finished but no binary at $binary"

  # A dynamically linked artefact would defeat the point of the musl targets,
  # and the failure only shows up on the user's machine. Catch it here.
  # `file` reports these builds as "static-pie linked", since musl targets
  # default to a position-independent static executable.
  if command -v file >/dev/null 2>&1; then
    file_out="$(file -b "$binary")"
    log_info "  file:       $file_out"
    case "$file_out" in
      *"statically linked"* | *"static-pie linked"*) ;;
      *"dynamically linked"*) die "$build produced a dynamically linked binary: $file_out" ;;
      *) log_warn "could not confirm $build is statically linked: $file_out" ;;
    esac
  fi

  stage="$out_dir/genixsearch-$version-$suffix"
  rm -rf "$stage"
  mkdir -p "$stage"
  install -m 0755 "$binary" "$stage/genixsearch"
  for extra in "${EXTRA_FILES[@]}"; do
    install -D -m 0644 "$ROOT_DIR/$extra" "$stage/$extra"
  done
  chmod 0755 "$stage/scripts/install_service.py"

  archive="genixsearch-$version-$suffix.tar.gz"
  tar -czf "$out_dir/$archive" -C "$out_dir" "$(basename "$stage")"
  [ "$keep_dirs" -eq 1 ] || rm -rf "$stage"

  # Drop any previous entry for this exact archive rather than truncating the
  # manifest, so building one `--build` at a time accumulates into one file
  # instead of each run clobbering the last.
  if [ -f "$sums" ]; then
    awk -v archive="$archive" '$2 != archive' "$sums" >"$sums.tmp"
    mv "$sums.tmp" "$sums"
  fi
  ( cd "$out_dir" && sha256sum "$archive" >>"$sums" )
  log_success "packaged $archive ($(du -h "$out_dir/$archive" | cut -f1))"
done

echo
log_success "release artefacts in $out_dir"
cat "$sums"
