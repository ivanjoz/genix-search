# Packaging

GenixSearch is built as a single static-ish Linux binary plus its config
file. The release flow is:

1. Update [`CHANGELOG.md`](./CHANGELOG.md) under the active version with
   the user-facing changes.
2. Bump the version in `Cargo.toml` (`[package] version = ...`) following
   [Semantic Versioning](https://semver.org/).
3. Build the release binary:

   ```sh
   cargo build --release --bin genixsearch
   ```

   The artefact lives at `./target/release/genixsearch`.

4. Smoke-test it locally:

   ```sh
   ./target/release/genixsearch -c ./genixsearch.cfg
   ./scripts/perf/run_perf.sh        # optional: drives the TCP protocol
   ```

5. Ship `target/release/genixsearch` and `genixsearch.cfg` to the target
   host. Container packaging is covered by the [`Dockerfile`](./Dockerfile).

## systemd

[`scripts/install_service.py`](./scripts/install_service.py) builds the
binary and installs it as a systemd service. It creates the `genixsearch`
system user, installs the binary to `/usr/local/bin`, the config to
`/etc/genixsearch/`, points `store.kv.path` at `/var/lib/genixsearch/`, and
writes a hardened unit to `/etc/systemd/system/genixsearch.service`.

```sh
sudo python3 scripts/install_service.py --dry-run             # print every action + file
sudo python3 scripts/install_service.py                       # build, install, enable, start
sudo python3 scripts/install_service.py \
    --bind 0.0.0.0:1491 --auth-password '…' --log-level info
sudo python3 scripts/install_service.py uninstall [--purge]
```

Because `librocksdb-sys` compiles RocksDB from source, the host needs a
C/C++ toolchain and libclang on top of rustup — a bare Rust install fails
with `linker cc not found`. The script detects and installs them via `dnf`,
`apt-get`, `zypper`, `pacman` or `apk`; pass `--no-install-build-deps` to
fail with instructions instead.

The build runs as `$SUDO_USER`, so `target/` does not become root-owned.
Use `--no-build --binary <path>` to install a cross-compiled artefact (see
[`scripts/cross_compile_ampere_neoverse_n1.sh`](./scripts/cross_compile_ampere_neoverse_n1.sh)),
and `--name`/`--config-dir`/`--data-dir` to run several instances side by
side. Re-running the installer overwrites the unit but keeps an existing
config unless `--force` is passed.
