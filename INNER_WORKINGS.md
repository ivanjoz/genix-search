# GenixSearch Inner Workings

This document walks through what happens inside GenixSearch when a client
issues a command. It assumes you have read [`README.md`](README.md) for the
big picture and [`docs/refactoring.md`](docs/refactoring.md) for the design
the implementation follows.

## 1. The shape of the index

GenixSearch is a key-value engine over RocksDB. Every collection lives in
its own RocksDB directory under `store.kv.path`. Inside a directory there
are three logical index families, distinguished by the first byte of every
key:

| Index | Index id | Key layout | Value |
|-------|---------:|------------|-------|
| Compact pair          | `1` | `idx<1B> | col_hash<u32_be> | bucket_hash<u32_be> | word1_sig<u32_be> | word2_sig<u32_be> | pair_kind<1B>` | `[doc_id u32_be]*` (zigzag-sortable signed `i32`) |
| Extended pair         | `2` | `idx<1B> | col_hash<u32_be> | bucket_hash<u32_be> | word1_slot<9B> | word2_slot<9B> | pair_kind<1B>` | same |
| Doc reverse refs      | `4` | `idx<1B> | col_hash<u32_be> | bucket_hash<u32_be> | sortable_doc_id<u32_be>` | fixed-width ref list |

`pair_kind` is `1 = adjacent`, `2 = wrap`. A `word1_slot` is
`[kind<1B>][sig<u64_be>]` where `kind = 1` is a short signature
left-aligned into the high bytes and `kind = 2` is a full long signature.

Indices `5` and `6` are reserved for optional exact-token / exact-pair
verification hashes (not built in v1).

## 2. Encoding a word

The pair encoder is the part the rest of the engine pivots on. Given a
raw UTF-8 token:

1. Normalise — lowercase ASCII, fold Spanish accents (`á → a`, …, `ñ → n`),
   drop everything that is not `a-z` / `0-9` inside a token, collapse
   repeated consecutive letters (`aaa → a`).
2. Split the normalised text into non-overlapping two-character pairs.
3. Map each pair to a `u8` group code from
   [`docs/bigrams.md`](docs/bigrams.md). Code `255` is reserved.
4. Numeric runs consume two `u8` units: `[NUM_FLAG=255][bucket(value)]`.
   `bucket()` is a fixed quantiser with finer resolution for small numbers
   (steps of 1 up to 80, then 5, then 15, …, capped at code 254).
5. Pack the first 4 units big-endian into a `u32` — the **short signature**
   — or the first 8 into a `u64` — the **long signature**. Words that yield
   ≤ 4 units always live in the short form.

Open-prefix queries (`sunr*`) reuse the same encoder. A trailing single
letter expands into every group code that contains that letter as a second
symbol, producing one prefix candidate per expansion.

A worked example:

```text
"tequila"          -> te qu il a$            -> sig32
"agua mineral"     -> ag ua | mi ne ra l$    -> sig32, sig32
"100lts"           -> [NUM_FLAG][bucket(100)] lt s$
"extraordinario"   -> ex tr ao rd in ar io   -> sig64
```

## 3. Indexing a document — `PUSHI`

The protocol layer parses `PUSHI <collection> <bucket> <int32> "<text>"`
and routes it into `Executor::pushi` (`src/executor/direct.rs`). The flow:

1. `pair::encode_text(text)` produces a `Vec<EncodedWord>`.
2. `pair::pair_entries(&words)` builds the adjacent and wrap pair list:
   * For every consecutive pair `(w_i, w_{i+1})` an `Adjacent` entry.
   * One final `Wrap` entry from the last word back to the first. For a
     single-word document this is `[w][w]`.
3. `StoreKVAction::replace_direct_pair_document(doc_id, entries)`:
   * Reads the reverse-ref record for `doc_id`. If it exists, every
     posting it lists is removed (`remove_pair_doc_id`).
   * Writes the new postings (`insert_pair_doc_id`) sorted by
     `sortable_doc_id` so the resulting `i32` arrays stay
     lexicographically sortable.
   * Replaces the reverse-ref record with one signature ref per entry,
     each fixed-width 20 bytes (`encode_reverse_refs`).

That last step is what makes replacement cheap: deletion is a single
RocksDB read for the reverse list, followed by `O(refs)` posting reads.
There is no full-collection scan.

## 4. Querying — `QUERY`

`Executor::search` (`src/executor/search.rs`) handles every query. The
high-level flow:

1. `pair::encode_prefix_text(terms)` produces:
   * `words: Vec<EncodedWord>` — the complete query tokens.
   * `prefix: Option<EncodedPrefix>` — at most one trailing `*` term,
     expanded into one or more candidate signature byte-streams.
2. Compute `candidate_budget = clamp(limit*10, 500, 5000)` and prepare a
   `HashMap<i32, DirectScore>`.
3. **Single-word range scan.** For every full token, `scan_pair_doc_ids_by_first_word`
   iterates compact-pair and extended-pair keys whose first signature
   equals the token. Every hit is `+10` (short) or `+12` (long).
4. **Prefix expansion.** If a prefix exists, walk every expanded unit
   list and call either `scan_pair_doc_ids_by_second_word_units` (when
   there is a preceding word) or `scan_pair_doc_ids_by_first_word_units`
   (when the prefix is the only term). Hits get `+15`. The score map is
   then filtered to those docs — a prefix term is required, not optional.
5. **Adjacent + wrap pair lookups.** For every adjacent pair the query
   produces (`pair_entries(&words)` filtered to `PairKind::Adjacent`):
   * Look up the same `[X][Y]` as `Adjacent` → `+30`.
   * Look up `[X][Y]` as `Wrap` → `+6`. This is what makes
     `"sunrise tequila"` find the document `"tequila sunrise"` with a
     lower score than the in-order phrase would have scored.
6. Sort by `score desc`, then `pair_match_count desc`, then
   `single_match_count desc`, then `doc_id asc`. Slice with
   `OFFSET`/`LIMIT`. Format as `key|score`.

The single-pass design avoids any join. The scoring map collapses
duplicates from compact/extended/wrap scans without extra bookkeeping.

## 5. Flushing — `FLUSHC`, `FLUSHB`

`FLUSHC <collection>` closes the in-pool RocksDB handle and `rm -rf`s the
collection directory. It is the cheapest reset path.

`FLUSHB <collection> <bucket>` issues three RocksDB `DeleteRange`
operations — one each for the compact pair, extended pair, and doc reverse
prefixes scoped to the `(col_hash, bucket_hash)` pair.

## 6. The tasker

`src/tasker/runtime.rs` runs every 10 seconds. It does two things:

1. **Janitor.** Drops cached RocksDB handles whose `last_used` is older
   than `store.kv.pool.inactive_after`.
2. **Flush.** Writes dirty memtables to SSTs for any handle whose
   `last_flushed` is older than `store.kv.database.flush_after`.

There is no consolidation step (FST is gone) and no in-memory index that
needs draining.

## 7. The connection layer

`src/channel/listen.rs` accepts TCP connections, spawns one thread per
client, hands off to `src/channel/handle.rs`. The handle waits for the
`START` line, then dispatches each subsequent line to one of the three
mode-specific dispatchers in `src/channel/command.rs`.

Each command runs synchronously. `QUERY` is the exception: it emits a
`PENDING <id>` line first and then the `EVENT QUERY <id> <results>` line
once the executor returns. This matches the upstream Sonic eventing
contract; in the GenixSearch implementation today both lines are written
back-to-back on the same thread, but the eventing framing leaves room for
worker pools later.

## 8. Where to look in the code

* `src/pair/` — encoding, signatures, key builders.
* `src/store/kv.rs` — RocksDB pool, acquire/close, prefix scans,
  reverse-ref helpers, replace/delete.
* `src/executor/direct.rs` — `PUSHI` / `POPI`.
* `src/executor/search.rs` — the query path, scoring, ranking.
* `src/channel/command.rs` — protocol dispatcher.
* `src/query/` — the typed `Query` enum + builder + dispatch shim.
* `scripts/perf/` — perf rig that drives the TCP protocol end-to-end.
