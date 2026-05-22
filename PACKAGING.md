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
   host. Service file, systemd unit, or container packaging is the
   operator's responsibility — there is no preinstall scaffolding shipped
   with the source tree.
