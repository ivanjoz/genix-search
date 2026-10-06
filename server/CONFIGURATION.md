# GenixSearch Configuration

## Sources

GenixSearch reads its configuration as [TOML]. The default path is
`./genixsearch.cfg` (the reference copy is `server/genixsearch.cfg`). Pass `--config <path>` (or `-c`) to override.

Every key can also be supplied through `GENIXSEARCH_` environment
variables, which take precedence over the static file. The TOML path
separator is `__`, so `foo.bar.baz` becomes `GENIXSEARCH_FOO__BAR__BAZ`.

## Reference

### `[server]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `log_level` | string (`debug`, `info`, `warn`, `error`) | `"error"` | Verbosity of logging. Set to `"error"` in production. |

### `[channel]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `inet` | string (IPv4 / IPv6 + port) | `"[::1]:1491"` | TCP listener address. |
| `tcp_timeout` | integer (seconds) | `300` | Idle/dead client timeout. |
| `auth_password` | string | _none_ | Required password for `START`. Optional but recommended. |

### `[channel.search]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `query_limit_default` | integer | `10` | Default `LIMIT` if `QUERY` does not provide one. |
| `query_limit_maximum` | integer | `100` | Hard cap on `LIMIT` for `QUERY`. |

### `[store.kv]`

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| `path` | string (UNIX path) | `"./data/store/kv/"` | Root path for per-collection RocksDB directories. |
| `pool.inactive_after` | integer (seconds) | `1800` | After this long with no use a collection handle is dropped from the in-memory pool. |
| `database.flush_after` | integer (seconds) | `900` | Background tasker flushes a handle that has been dirty for this long. Must be < `pool.inactive_after`. |
| `database.compress` | bool | `true` | Use Zstd compression for SSTs. |
| `database.parallelism` | integer | `2` | RocksDB compaction + flush threads. |
| `database.max_files` | integer | _unlimited_ | Cap on simultaneously open files per database. |
| `database.max_compactions` | integer | `1` | Concurrent compaction jobs. |
| `database.max_flushes` | integer | `1` | Concurrent flush jobs. |
| `database.write_buffer` | integer (KB) | `16384` | Write-buffer size before flush. Must be a multiple of `1024`. |
| `database.write_ahead_log` | bool | `true` | Enable WAL. Disabling sacrifices crash safety for write throughput. |

## Environment variable interpolation

A handful of keys also accept `${env.NAME}` placeholders that expand from
the process environment at startup: `server.log_level`, `channel.inet`,
`channel.auth_password`, `store.kv.path`.

[TOML]: https://toml.io/
