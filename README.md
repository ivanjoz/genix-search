# GenixSearch

**Genix Search** is a compact, lossy, ranked search backend optimized for
Spanish product names and short commercial text. It ingests UTF-8 text keyed
by client-supplied `int32` document ids and answers `QUERY` commands with a
ranked `key|score` list over a single TCP connection.

## What it is

* **Pair-first index** — every word is encoded as a deterministic Spanish
  letter-pair signature; numeric runs are quantised into one byte. Two-word
  pair signatures form the main retrieval primitive.
* **Direct `i32` keys** — `PUSHI`/`POPI` take a 32-bit signed integer that
  the client provides. No OID/IID translation layer, no string interning.
* **Ranked output** — `QUERY` returns `key|score key|score …` sorted by
  descending score. Scoring combines single-word matches, adjacent pair
  matches, wrap-pair (cyclic) matches, and prefix matches.
* **Prefix queries** — a trailing `*` on a query term (`tequila sunr*`)
  expands to compatible pair signatures and contributes a prefix-match
  score.
* **Compact storage** — postings are `i32` arrays; reverse refs use a
  fixed-width per-doc record so deletion is one RocksDB read.

## What it is not

* Not a multilingual full-text engine. The normalisation table covers
  Spanish accents + ASCII letters/digits; other scripts are treated as
  separators.
* Not exact. Signatures collide by design; results are candidate sets
  ranked by lossy evidence, never confirmed text equality.
* Not a string-keyed index. There is no `PUSH`, no `POP`, no `SUGGEST`, no
  `LIST`, no FST graph.

## Install

Prebuilt, fully static Linux binaries are attached to every
[release](https://github.com/ivanjoz/genix-search/releases) — no glibc
requirement, nothing to compile. The installer picks the right one for your
machine (x86_64, generic arm64, or arm64 tuned for Ampere Altra / Oracle Cloud
`VM.Standard.A1.Flex`), verifies its checksum and installs it:

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install.sh | sh
```

Or straight into a hardened systemd service — this one file downloads its own
binary and config, so nothing else is needed on the host but Python 3:

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install_service.py \
    | sudo python3 - install --bind 0.0.0.0:1491 --auth-password '…'
```

See [`PACKAGING.md`](PACKAGING.md) for the asset table, manual downloads and
how to reproduce the release builds locally.

## Quick start

```sh
# build (debug)
cargo build

# build (release)
cargo build --release

# run with the default genixsearch.cfg in the working directory
./target/release/genixsearch -c ./genixsearch.cfg
```

Then point a TCP client at `[::1]:1491` and start a session:

```text
CONNECTED <genixsearch v0.1.0>
START ingest SecretPassword
STARTED ingest protocol(1) buffer(20000)
PUSHI products default 1 "tequila sunrise"
OK
PUSHI products default 2 "agua mineral 100lts"
OK
QUIT
ENDED quit
```

```text
START search SecretPassword
STARTED search protocol(1) buffer(20000)
QUERY products default "tequila sunrise" LIMIT(10)
PENDING Bt2m2gYa
EVENT QUERY Bt2m2gYa 1|50
```

See [`PROTOCOL.md`](PROTOCOL.md) for the full command set.

## Perf harness

The repository ships a scripted perf rig that drives the TCP protocol with
50k Spanish product names from a CSV:

```sh
./scripts/perf/run_perf.sh
```

It builds a release binary, boots it against a clean data dir, runs three
progressive PUSHI/QUERY phases (1k → 10k → 50k), and writes per-phase logs
under `data/perf-logs/`. See the comments at the top of the script for
overridable env vars.

## Documentation

* [`PROTOCOL.md`](PROTOCOL.md) — channel commands, syntax, response format.
* [`CONFIGURATION.md`](CONFIGURATION.md) — every `genixsearch.cfg` knob.
* [`INNER_WORKINGS.md`](INNER_WORKINGS.md) — index, ingest, query walkthrough.
* [`docs/refactoring.md`](docs/refactoring.md) — design doc the rewrite
  followed.
* [`docs/bigrams.md`](docs/bigrams.md) — frequency-ordered Spanish bigram
  groups that drive the letter-pair encoder.
* [`CHANGELOG.md`](CHANGELOG.md) — release history.

## License

Mozilla Public License v2.0. See [`LICENSE.md`](LICENSE.md).
