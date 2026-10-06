# GenixSearch Channel Protocol

GenixSearch listens on a single TCP port (default `[::1]:1491`) and speaks a
plain text, line-oriented protocol called **GenixSearch Channel**. Every
command is terminated by `\n`. The default buffer per command is 20000
bytes — clients should respect the `buffer(...)` value reported in the
`STARTED` banner.

```text
$ telnet ::1 1491
CONNECTED <genixsearch v0.1.0>
START ingest SecretPassword
STARTED ingest protocol(1) buffer(20000)
```

The connection picks one of three exclusive modes via the initial `START`
command. Modes do not switch on a live connection.

---

## 1. Uninitialised

After `CONNECTED <...>`, the only valid command is:

```text
START <mode> <password>
```

`<mode>` ∈ `search | ingest | control`. `<password>` is required when
`channel.auth_password` is set in the config. Any other command before
`STARTED` aborts the connection with `ENDED not_recognized`.

---

## 2. Search mode

```text
START search <password>
```

### Available commands

| Command | Syntax |
|---------|--------|
| `QUERY` | `QUERY <collection> <bucket> "<terms>" [LIMIT(<count>)]? [OFFSET(<count>)]?` |
| `PING`  | `PING` |
| `HELP`  | `HELP [<manual>]?` |
| `QUIT`  | `QUIT` |

### QUERY response

```text
PENDING <event_id>
EVENT QUERY <event_id> <key|score> <key|score> ...
```

* `key` — the signed 32-bit integer the client provided to `PUSHI`.
* `score` — unsigned scoring value. Results are sorted by descending score.
* Tie-break order: `score desc`, `pair_match_count desc`,
  `single_match_count desc`, `key asc`.
* When the result set is empty, the `EVENT QUERY` line has no key tokens.

### Query syntax

```text
QUERY products default "tequila sunrise"
QUERY products default "tequila sunr*"          # trailing-* prefix term
QUERY products default "100l*"                  # numeric prefix
QUERY products default "agua mineral" LIMIT(20) OFFSET(0)
```

A trailing `*` on the last term enables a prefix query that contributes a
prefix-pair match (+15) on top of any single-word/adjacent matches.

### Scoring model

| Match                            | Score |
|----------------------------------|------:|
| Single-word pair-range (short)   | +10   |
| Single-word pair-range (long)    | +12   |
| Adjacent pair signature          | +30   |
| Wrap pair signature              | +6    |
| Prefix pair match                | +15   |

A single document can collect several of these per query.

---

## 3. Ingest mode

```text
START ingest <password>
```

### Available commands

| Command  | Syntax |
|----------|--------|
| `PUSHI`  | `PUSHI <collection> <bucket> <int32_key> "<text>" [<int32_key> "<text>"]...` |
| `POPI`   | `POPI <collection> <bucket> <int32_key>` |
| `FLUSHC` | `FLUSHC <collection>` |
| `FLUSHB` | `FLUSHB <collection> <bucket>` |
| `PING`   | `PING` |
| `HELP`   | `HELP [<manual>]?` |
| `QUIT`   | `QUIT` |

### Write semantics

* `PUSHI` is keyed by the full `(collection, bucket, int32_key)` triple.
  If the key already exists, the previous pair postings are removed before
  the new text is indexed (replace).
* `PUSHI` accepts multiple `<int32_key> "<text>"` pairs in one command.
  The server responds `OK` for a single entry and `RESULT <count>` for
  multiple.
* `POPI` removes every posting linked to the given `int32_key` plus its
  reverse-signature record. Response: `RESULT <count>` where `count` is
  `1` if a record existed and `0` otherwise.
* `FLUSHC` erases the whole collection (closes the open KV handle and
  deletes the on-disk directory). Response: `RESULT 1` if it existed.
* `FLUSHB` clears every pair index + reverse-ref record scoped to the
  given `(collection, bucket)`. Response: `RESULT 1` on success, `RESULT 0`
  when the collection does not exist.

### Examples

```text
PUSHI products default 1 "tequila sunrise"
OK
PUSHI products default 2 "agua mineral 100lts" 3 "agua sin gas"
RESULT 2
POPI products default 2
RESULT 1
FLUSHB products default
RESULT 1
```

---

## 4. Control mode

```text
START control <password>
```

### Available commands

| Command   | Syntax |
|-----------|--------|
| `TRIGGER` | `TRIGGER [<action>]? [<data>]?` |
| `INFO`    | `INFO` |
| `PING`    | `PING` |
| `HELP`    | `HELP [<manual>]?` |
| `QUIT`    | `QUIT` |

### Trigger actions

| Action    | Syntax | Notes |
|-----------|--------|-------|
| `backup`  | `TRIGGER backup <path>`  | Writes a RocksDB backup engine snapshot of every open KV collection under `<path>/kv/`. |
| `restore` | `TRIGGER restore <path>` | Inverse of `backup`. Replaces the current data dir from `<path>/kv/`. |

### INFO response

```text
RESULT uptime(<sec>) clients_connected(<n>) commands_total(<n>) \
       command_latency_best(<ms>) command_latency_worst(<ms>) \
       kv_open_count(<n>)
```

---

## 5. Error responses

Any command may respond with:

```text
ERR <code>
```

Codes:

* `unknown_command`
* `not_found`
* `query_error`
* `internal_error`
* `shutting_down`
* `policy_reject(<reason>)`
* `invalid_format(<expected syntax>)`
* `invalid_meta_key(<key>[<value>])`
* `invalid_meta_value(<key>[<value>])`

---

## 6. Text encoding

* Inputs are UTF-8 between `"`-quotes inside `PUSHI` and `QUERY`.
* `"` and `\` inside text must be escaped as `\"` and `\\`.
* The pair encoder lowercases ASCII, folds Spanish accents (`á→a`,
  `é→e`, …, `ñ→n`), keeps `a-z` and `0-9` inside tokens, and treats
  anything else as a token separator.

---

## 7. Legacy commands

`PUSH`, `POP`, `SUGGEST`, `LIST`, `COUNT`, `FLUSHO`, `BULKFLUSHO`,
`BULKPUSH` no longer exist. Clients written for upstream Sonic must move to
the direct-key (`PUSHI`/`POPI`) commands.
