# GenixSearch Inner Workings

This document walks through what happens inside GenixSearch when a client
issues a command. It assumes you have read [`README.md`](README.md) for the
big picture.

## 1. The shape of the index

GenixSearch is a key-value engine over RocksDB. Every collection lives in
its own RocksDB directory under `store.kv.path`. Inside a directory there
are three logical index families, distinguished by the first byte of every
key:

| Index | Index id | Key layout | Value |
|-------|---------:|------------|-------|
| Compact pair          | `1` | `idx<1B> | col_hash<u32_be> | bucket_hash<u32_be> | word1_sig<u32_be> | word2_sig<u32_be>` | `[doc_id<u32_be> | meta<u8>]*` |
| Extended pair         | `2` | `idx<1B> | col_hash<u32_be> | bucket_hash<u32_be> | word1_slot<9B> | word2_slot<9B>` | same |
| Doc reverse refs      | `4` | `idx<1B> | col_hash<u32_be> | bucket_hash<u32_be> | sortable_doc_id<u32_be>` | fixed-width ref list |

A `word1_slot` is `[kind<1B>][sig<u64_be>]` where `kind = 1` is a short
signature left-aligned into the high bytes and `kind = 2` is a full long
signature.

Each posting value is a packed sequence of 5-byte entries: a 4-byte
`sortable_doc_id` followed by a 1-byte `meta`. The meta byte carries the
pair kind and the doc-side positions of the two words:

```text
bit 7    : kind        (0 = Adjacent, 1 = Wrap)
bits 6-5 : word1_pos   (0-2 = exact, 3 = saturated "≥3")
bits 4-3 : word2_pos   (0-2 = exact, 3 = saturated "≥3")
bits 2-0 : reserved
```

Both Adjacent and Wrap postings for the same `(word1, word2)` pair live
under a single RocksDB key; the kind bit distinguishes them at read
time.

Indices `5` and `6` are reserved for optional exact-token / exact-pair
verification hashes (not built in v1).

## 2. Encoding a word

The pair encoder is the part the rest of the engine pivots on. Given a
raw UTF-8 token:

1. Normalise — lowercase ASCII, fold Spanish accents (`á → a`, …, `ñ → n`),
   drop everything that is not `a-z` / `0-9` inside a token, collapse
   repeated consecutive letters (`aaa → a`).
2. Split the normalised text into non-overlapping two-character pairs.
3. Map each pair to a `u8` group code from the frequency-ordered table in
   `server/src/pair/spanish_bigrams.rs`. Code `255` is reserved.
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
and routes it into `Executor::pushi` (`server/src/executor/direct.rs`). The flow:

1. `pair::encode_text(text)` produces a `Vec<EncodedWord>`.
2. `pair::pair_entries(&words)` builds the adjacent and wrap pair list,
   each paired with a `PostingMeta { kind, w1_pos, w2_pos }`:
   * For every consecutive pair `(w_i, w_{i+1})` an `Adjacent` entry
     with positions `(i, i+1)` clamped at 3.
   * One final `Wrap` entry from the last word back to the first with
     positions `(N-1, 0)` clamped at 3. For a single-word document this
     is `[w][w]` with positions `(0, 0)`.
3. `StoreKVAction::replace_direct_pair_document(doc_id, entries)`:
   * Reads the reverse-ref record for `doc_id`. If it exists, every
     posting it lists is removed (`remove_pair_postings_for_doc`).
   * Writes the new postings (`insert_pair_posting`) sorted by
     `sortable_doc_id` so the 5-byte `(doc_id, meta)` entries stay
     lexicographically sortable on `doc_id`.
   * Replaces the reverse-ref record with one signature ref per entry,
     each fixed-width 20 bytes (`encode_reverse_refs`).

That last step is what makes replacement cheap: deletion is a single
RocksDB read for the reverse list, followed by `O(refs)` posting reads.
There is no full-collection scan.

## 4. Querying — `QUERY`

`Executor::search` (`server/src/executor/search.rs`) handles every query. All
scoring constants referenced below live in `server/src/executor/scoring.rs` —
that file is the single tuning surface. The high-level flow:

1. `pair::encode_prefix_text(terms)` produces:
   * `words: Vec<EncodedWord>` — the complete query tokens.
   * `prefix: Option<EncodedPrefix>` — at most one trailing `*` term,
     expanded into one or more candidate signature byte-streams.
2. Compute `candidate_budget = clamp(limit*10, 500, 5000)` and prepare a
   `HashMap<i32, DirectScore>`.
3. **Single-word range scan.** For every full token,
   `scan_pair_postings_by_first_word` iterates compact-pair and
   extended-pair keys whose first signature equals the token. The scan
   aggregates per doc the **minimum** `w1_pos` across all postings —
   a doc that has the token at multiple positions only counts once,
   credited at its earliest occurrence. Each hit then earns:
   * A length-based base score `word_base_score(token) =
     (WORD_BASE_SCORE_BASE + token_len).clamp(WORD_BASE_SCORE_FLOOR,
     WORD_BASE_SCORE_CEIL)`. With the current constants `6 / 8 / 12`
     this means a 2-letter token scores `8`, a 4-letter token scores
     `10`, and any token of 6+ letters caps at `12`.
   * A **query-side position bonus**: `+QUERY_FIRST_WORD_BONUS` when the
     token is at query position 0, `+QUERY_SECOND_WORD_BONUS` when at
     position 1, else 0. This makes the user's primary intent outweigh
     accidental matches on trailing tokens (e.g. `pimienta roja de
     cocina` ranks "Pimienta negra molida" above "Bateria de Cocina").
   * A **doc-side position bonus**: `+DOC_FIRST_WORD_BONUS` when the
     matched token sits at doc position 0, `+DOC_SECOND_WORD_BONUS` at
     position 1, else 0. This lifts docs that lead with the matched
     token (e.g. "Pimienta negra molida" above "Sal y pimienta de
     cocina" when the query is `pimienta`).

   Spanish stopwords (`de`, `la`, `con`, `para`, …) are stripped before
   encoding by `pair::is_spanish_stopword`, both at index time and at
   query time, so they neither bloat the pair index nor contribute to
   scoring.
4. **Prefix expansion.** Two kinds of prefixes feed this step:
   * **Explicit** — the user typed `foo*`. Anchored to the preceding
     word when one exists (`scan_pair_postings_by_second_word_units`),
     otherwise unanchored (`scan_pair_postings_by_first_word_units`).
     The score map is then *filtered* to prefix-matching docs — an
     explicit prefix is required, not optional.
   * **Auto** — when the user typed at least two whitespace terms with
     no `*`, the last complete (non-stopword) word is also encoded as a
     non-explicit prefix. Always unanchored. Does *not* filter the score
     map, so docs that match the preceding words but lack the partial
     last-word match still appear — they just don't get the prefix
     bonus. This is what lets `pimienta moli` rank "Pimienta blanca
     molida" above "Pimienta de cerdo" without dropping the latter.
     Auto-prefix also skips any doc that already received the full
     single-word score for the same token (the prefix would otherwise
     double-count `leche` against itself in a query like `pimienta de
     leche`).

   Either kind earns `+PREFIX_SCORE` per hit, plus the
   `+QUERY_FIRST_WORD_BONUS` first-word bonus when the prefix is itself
   the leading term (only possible for explicit prefixes since
   auto-prefix requires a preceding complete word). The doc-side
   position bonus is also applied — using the posting's `w2_pos` when
   the prefix is anchored to a preceding word, otherwise `w1_pos`.
5. **Adjacent pair lookups.** For every adjacent pair the query produces
   (`words.windows(2)`), look up the single key `[X][Y]`. Each posting
   under that key carries a `kind` bit:
   * `kind = Adjacent` → `+ADJACENT_PAIR_SCORE`.
   * `kind = Wrap` → `+WRAP_PAIR_SCORE`. This is what makes
     `"sunrise tequila"` find the document `"tequila sunrise"` with a
     lower score than the in-order phrase would have scored.

   Both contributions come from the same RocksDB key in a single read —
   the kind bit was moved out of the key into the meta byte.
6. Sort by `score desc`, then `pair_match_count desc`, then
   `single_match_count desc`, then `doc_id asc`. Slice with
   `OFFSET`/`LIMIT`. Format as `key|score`.

The single-pass design avoids any join. The scoring map collapses
duplicates from compact/extended scans without extra bookkeeping; the
per-doc min-position aggregation makes the doc-side bonus stable when a
token appears in multiple pairs of the same doc.

## 5. Flushing — `FLUSHC`, `FLUSHB`

`FLUSHC <collection>` closes the in-pool RocksDB handle and `rm -rf`s the
collection directory. It is the cheapest reset path.

`FLUSHB <collection> <bucket>` issues three RocksDB `DeleteRange`
operations — one each for the compact pair, extended pair, and doc reverse
prefixes scoped to the `(col_hash, bucket_hash)` pair.

## 6. The tasker

`server/src/tasker/runtime.rs` runs every 10 seconds. It does two things:

1. **Janitor.** Drops cached RocksDB handles whose `last_used` is older
   than `store.kv.pool.inactive_after`.
2. **Flush.** Writes dirty memtables to SSTs for any handle whose
   `last_flushed` is older than `store.kv.database.flush_after`.

There is no consolidation step (FST is gone) and no in-memory index that
needs draining.

## 7. The connection layer

`server/src/channel/listen.rs` accepts TCP connections, spawns one thread per
client, hands off to `server/src/channel/handle.rs`. The handle waits for the
`START` line, then dispatches each subsequent line to one of the three
mode-specific dispatchers in `server/src/channel/command.rs`.

Each command runs synchronously. `QUERY` is the exception: it emits a
`PENDING <id>` line first and then the `EVENT QUERY <id> <results>` line
once the executor returns. This matches the upstream Sonic eventing
contract; in the GenixSearch implementation today both lines are written
back-to-back on the same thread, but the eventing framing leaves room for
worker pools later.

## 8. Where to look in the code

* `server/src/pair/` — encoding, signatures, key builders, `PostingMeta`
  pack/unpack, posting codec.
* `server/src/store/kv.rs` — RocksDB pool, acquire/close, prefix scans,
  reverse-ref helpers, replace/delete.
* `server/src/executor/direct.rs` — `PUSHI` / `POPI`.
* `server/src/executor/search.rs` — the query path, ranking glue.
* `server/src/executor/scoring.rs` — every scoring constant and helper; the
  one place to tune ranking weights.
* `server/src/channel/command.rs` — protocol dispatcher.
* `server/src/query/` — the typed `Query` enum + builder + dispatch shim.
* `scripts/perf/` — perf rig that drives the TCP protocol end-to-end.
