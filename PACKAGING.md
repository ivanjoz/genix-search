# Packaging

GenixSearch ships as a single fully static Linux binary plus its config file.
Every published artefact is a `musl` build, so there is no glibc floor, no
shared-library hunt and nothing to compile on the target host.

## Published artefacts

Each tagged release attaches three tarballs and a `SHA256SUMS` manifest:

| Asset | Target triple | For |
|-------|---------------|-----|
| `genixsearch-<version>-x86_64-linux-musl.tar.gz` | `x86_64-unknown-linux-musl` | Any x86_64 Linux |
| `genixsearch-<version>-aarch64-linux-musl.tar.gz` | `aarch64-unknown-linux-musl` | Any arm64 Linux — AWS Graviton, Raspberry Pi, generic |
| `genixsearch-<version>-aarch64-linux-musl-neoverse-n1.tar.gz` | `aarch64-unknown-linux-musl`, `-C target-cpu=neoverse-n1` | Ampere Altra / Altra Max — Oracle Cloud `VM.Standard.A1.Flex` |

Every tarball unpacks to a `genixsearch-<version>-<suffix>/` directory holding
the binary, `genixsearch.cfg`, `scripts/install_service.py` and the reference
docs. The `scripts/` nesting is deliberate: `install_service.py` reads the
reference config from its own parent directory, so it works straight out of the
archive.

Two aarch64 builds exist because the Ampere one is compiled for Neoverse N1 and
will fault on older arm64 cores; the generic build targets baseline ARMv8-A.

## Installing a release

The one-liner detects the architecture — including Neoverse N1 through
`/proc/cpuinfo` — downloads the matching asset, verifies its checksum and
installs the binary:

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install.sh | sh
```

Install it as a hardened systemd service in one step instead:

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install.sh \
    | sudo sh -s -- --service --bind 0.0.0.0:1491 --auth-password '…'
```

Useful flags: `--version vX.Y.Z` to pin a release, `--flavor generic|ampere` to
override CPU detection, `--prefix "$HOME/.local"` for an unprivileged install,
and `--dry-run` to print every action without touching the system. Anything the
script does not recognise is forwarded to `install_service.py`, so `--name`,
`--config-dir` and `--data-dir` work through it too.

By hand:

```sh
version=v0.1.0
curl -fsSLO "https://github.com/ivanjoz/genix-search/releases/download/$version/genixsearch-$version-x86_64-linux-musl.tar.gz"
curl -fsSLO "https://github.com/ivanjoz/genix-search/releases/download/$version/SHA256SUMS"
sha256sum -c SHA256SUMS --ignore-missing
tar -xzf "genixsearch-$version-x86_64-linux-musl.tar.gz"
sudo install -m 0755 "genixsearch-$version-x86_64-linux-musl/genixsearch" /usr/local/bin/
```

## Cutting a release

Releases are built on a developer machine and uploaded to GitHub. Nothing is
compiled in CI: RocksDB comes from source for all three targets, which is far
too heavy for a hosted runner.

1. Update [`CHANGELOG.md`](./CHANGELOG.md) under the active version with the
   user-facing changes.
2. Bump the version in `Cargo.toml` (`[package] version = ...`) following
   [Semantic Versioning](https://semver.org/), and commit.
3. Build all three targets — expect this to take a while:

   ```sh
   scripts/release_binaries.sh          # or: task release
   ```

4. Check what you are about to publish, then publish it:

   ```sh
   scripts/publish_release.sh --dry-run
   scripts/publish_release.sh --create-tag
   ```

`--dry-run` verifies the artefacts and prints the release notes without
uploading. `--create-tag` tags `HEAD` and pushes it; drop it if you tagged
already. Add `--draft` to stage the release privately first.

`publish_release.sh` refuses to publish a half-built or stale release. It
requires all three tarballs for the version, re-runs `sha256sum -c` over
`SHA256SUMS`, and inspects the binary inside each tarball to confirm its
architecture matches the name — the one mistake a checksum cannot catch, since
it matches the wrong file perfectly. Pass `--allow-missing` to publish a subset
deliberately.

It also needs `gh` authenticated as an account with push access to the
repository. If several accounts are configured, switch first:

```sh
gh auth switch --user <owner>
```

## Building the artefacts locally

[`scripts/release_binaries.sh`](./scripts/release_binaries.sh) produces the
artefacts on any machine with `cargo` and Docker or Podman:

```sh
scripts/release_binaries.sh                          # all three, version from Cargo.toml
scripts/release_binaries.sh --build=linux-arm64-ampere
scripts/release_binaries.sh --version=v0.2.0 --out=./dist --keep-dirs
```

It installs [`cross`](https://github.com/cross-rs/cross) if missing, builds
each target inside the matching `cross` container — which is where the musl
C/C++ toolchain that `librocksdb-sys` needs lives — asserts the result is
statically linked, then writes the tarballs and `SHA256SUMS` under `dist/`.

Building one target at a time accumulates into the same `SHA256SUMS`, so a
release can be assembled across several sessions. What a release consists of
lives in one place, [`scripts/util/release_targets.sh`](./scripts/util/release_targets.sh),
so the builder and the publisher cannot disagree about it.

Three build details matter and are easy to get wrong by hand:

* **`-C target-cpu=neoverse-n1`** goes to the Ampere build only. Both aarch64
  builds share a target triple, so the script gives each its own
  `CARGO_TARGET_DIR` to keep them from invalidating one another.
* **`JEMALLOC_SYS_WITH_LG_PAGE=16`** is set for both aarch64 builds.
  `tikv-jemalloc-sys` freezes the page size at compile time and defaults to the
  build host's — 4 KiB on every x86_64 runner. Oracle Linux and RHEL run
  aarch64 kernels with 64 KiB pages, where a 4 KiB-page binary aborts on its
  first allocation. A 64 KiB build runs correctly on both. It has to be *unset*
  for x86_64, not empty: the crate reads it with `env::var().ok()`, so an empty
  value still reaches configure as a bare `--with-lg-page=`.
* **`-C link-arg=-Wl,-z,stack-size=2097152`** goes to every musl build. musl
  defaults new threads to a 128 KiB stack against glibc's 8 MiB. Rust sizes its
  own `std::thread` stacks, but RocksDB's compaction and flush threads are C++
  threads that take the libc default. musl seeds that default from the ELF
  `PT_GNU_STACK` header, so linking with a 2 MiB stack covers them.

For a plain local build, `cargo build --release --bin genixsearch` still
produces a dynamically linked `./target/release/genixsearch` against the host's
glibc, which is fine for development. Container packaging is covered by the
[`Dockerfile`](./Dockerfile).

## systemd

[`scripts/install_service.py`](./scripts/install_service.py) installs
GenixSearch as a hardened systemd service. It creates the `genixsearch` system
user, installs the binary to `/usr/local/bin`, the config to
`/etc/genixsearch/`, points `store.kv.path` at `/var/lib/genixsearch/`, and
writes the unit to `/etc/systemd/system/genixsearch.service`.

**It compiles nothing by default.** It detects the machine, downloads the
matching prebuilt binary from the latest GitHub release, and verifies it
against the release `SHA256SUMS`. The target host needs only Python 3 and
systemd — no Rust, no C/C++ toolchain, no libclang, and no matching glibc.

The script is self-sufficient: it fetches its own binary *and* its reference
config, so it needs neither a checkout nor the release tarball. One file is
enough:

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install_service.py \
    | sudo python3 - install --bind 0.0.0.0:1491 --auth-password '…'
```

From a checkout or an unpacked tarball it behaves the same way:

```sh
sudo python3 scripts/install_service.py --dry-run             # print every action + file
sudo python3 scripts/install_service.py                       # download, install, enable, start
sudo python3 scripts/install_service.py \
    --bind 0.0.0.0:1491 --auth-password '…' --log-level info
sudo python3 scripts/install_service.py uninstall [--purge]
```

Selecting what gets installed:

| Flag | Effect |
|------|--------|
| _(none)_ | Download the latest release build for this machine |
| `--release-version v0.1.0` | Pin a release instead of taking the latest |
| `--flavor ampere` \| `generic` | Override the aarch64 CPU detection |
| `--binary ./genixsearch` | Install a local binary, download nothing |
| `--from-source` | Compile with cargo on this host (the old behaviour) |
| `--repo OWNER/NAME` | Download from a fork |

On aarch64 the CPU is read from `/proc/cpuinfo`: MIDR part `0xd0c` is Arm
Neoverse N1 — the Ampere Altra and Altra Max, and also AWS Graviton2 — which
selects the `neoverse-n1` build. Anything else gets the portable aarch64 build.

`--from-source` is the only path that still needs a toolchain: RocksDB compiles
from source, so the host needs a C/C++ toolchain and libclang on top of rustup,
or a bare Rust install fails with `linker cc not found`. The script detects and
installs them via `dnf`, `apt-get`, `zypper`, `pacman` or `apk`; pass
`--no-install-build-deps` to fail with instructions instead. That build runs as
`$SUDO_USER`, so `target/` does not become root-owned.

Use `--name`/`--config-dir`/`--data-dir` to run several instances side by side.
Re-running the installer overwrites the unit but keeps an existing config unless
`--force` is passed.
