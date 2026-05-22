# Contributing to GenixSearch

## Local setup

* Rust + Cargo (toolchain pinned at `1.95.0` or newer).
* A C/C++ toolchain (`clang`, `g++`) plus `libclang-dev` / `llvm-dev` so
  RocksDB can build.
* Python 3.10+ (only for the perf scripts under `scripts/perf/`).

## Build, run, test

```sh
cargo build               # debug
cargo build --release     # optimised
cargo run -- -c ./genixsearch.cfg
cargo test --lib          # full unit suite
```

For the protocol-level perf rig:

```sh
./scripts/perf/run_perf.sh
```

It rebuilds release, starts `genixsearch`, runs three progressive PUSHI +
QUERY phases against a Spanish product-name CSV, and writes detailed logs
under `data/perf-logs/`. Override sizes via `N1=`, `N2=`, `N3=` env vars.

## Reporting issues

Issues live in the project's tracker. Search for an existing report before
filing a new one; include the failing command, the full server log line,
and the GenixSearch version.

## Submitting patches

* Format Rust code with `rustfmt` before opening a pull request.
* Run `cargo test --lib` and confirm all tests pass.
* Keep the cleanup principle the rest of the codebase follows: small
  imports, explicit error paths at boundaries, no speculative
  abstractions.
* Update `CHANGELOG.md` under the active version for user-visible
  changes.
