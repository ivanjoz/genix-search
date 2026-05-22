# Position-Aware Postings

This document captures the design and final shape of two related changes
to the pair index storage. Both are now implemented; see
[`INNER_WORKINGS.md`](../INNER_WORKINGS.md) for the as-built description
that ties this into the rest of the engine.

1. **`pair_kind` removed from the pair-index key.** It was a design
   error that bloated the key by one byte and split postings for the
   same `(word1, word2)` pair across two RocksDB keys.
2. **Per-doc-id posting widened from a bare `u32` to a `u32 + u8`
   tuple.** The new metadata byte carries the pair kind (Adjacent /
   Wrap) *and* the doc-side position of each of the two words. This
   unlocks a "match position" signal at ranking time — a query word
   that appears at the start of a product name outranks the same word
   buried at position 5.

No data migration was performed. Existing collections were wiped
(`FLUSHC` / delete the on-disk directories) and re-ingested.

## 1. Goals

- Free a byte per pair key and consolidate postings for a given
  `(word1, word2)` pair under a single RocksDB key, regardless of kind.
- Add a doc-side position signal to the ranking input without changing
  the posting count (still one 5-byte entry per pair occurrence).
- Keep postings lex-sortable on `sortable_doc_id`, so range scans, dedup,
  and the existing reverse-ref deletion path keep working unchanged.
- No on-disk version flag, no migration code. Wipe and reindex.

## 2. Key layout — before / after

### Compact pair index (`idx = 1`)

Before — 18 bytes:

```text
idx<1B> | col_hash<u32_be> | bucket_hash<u32_be>
       | word1_sig<u32_be> | word2_sig<u32_be> | pair_kind<1B>
```

After — 17 bytes:

```text
idx<1B> | col_hash<u32_be> | bucket_hash<u32_be>
       | word1_sig<u32_be> | word2_sig<u32_be>
```

### Extended pair index (`idx = 2`)

Before — 28 bytes:

```text
idx<1B> | col_hash<u32_be> | bucket_hash<u32_be>
       | word1_slot<9B>    | word2_slot<9B>     | pair_kind<1B>
```

After — 27 bytes:

```text
idx<1B> | col_hash<u32_be> | bucket_hash<u32_be>
       | word1_slot<9B>    | word2_slot<9B>
```

### Doc reverse refs (`idx = 4`)

Unchanged — the 20-byte ref record still stores
`(index, pair_kind, word1_slot, word2_slot)`. Deletion does not need
the kind (it strips every 5-byte posting whose first 4 bytes match the
target `sortable_doc_id`, regardless of the trailing meta byte), but
the field is kept because it costs nothing on disk and leaves room for
a future deletion path that only removes Adjacent *or* Wrap postings
for a given doc.

## 3. Value layout

A postings list is a packed sequence of fixed-width 5-byte entries:

```text
[ doc_id<u32_be> | meta<u8> ] [ doc_id<u32_be> | meta<u8> ] ...
```

Entries are sorted by `sortable_doc_id(doc_id) = doc_id as u32 ^ 0x80000000`,
i.e. the same sort order as the pre-refactor format. Because `doc_id`
occupies the *high* 4 bytes of every entry, the byte stream is
lex-sortable on `doc_id` and the metadata byte trails it. The codec
helpers `encode_postings` / `decode_postings` in `src/pair/mod.rs` read
and write 5-byte chunks (`POSTING_WIDTH = 5`).

### Meta byte

Bit layout (high bit first):

```text
bit 7    : kind        (0 = Adjacent, 1 = Wrap)
bits 6-5 : word1_pos   (0-2 = exact, 3 = saturated "≥3")
bits 4-3 : word2_pos   (0-2 = exact, 3 = saturated "≥3")
bits 2-0 : reserved    (must be 0)
```

5 bits used, 3 reserved. The reserved bits leave room for future flags
(e.g. exact-token vs. lossy-signature, or extra ranking signals)
without another value-format version.

#### Position semantics

`word1_pos` and `word2_pos` are the **doc-side** positions of the two
words in the source document text, after stopword filtering and
encoding. Position 0 is the first non-stopword token.

- **Adjacent** pair at token positions `(i, i+1)`:
  - `word1_pos = min(i, 3)`
  - `word2_pos = min(i+1, 3)`
- **Wrap** pair `(last_token, first_token)` in a doc of `N` encoded tokens:
  - `word1_pos = min(N-1, 3)`
  - `word2_pos = 0`
  - Single-word doc degenerate case `[w][w]`: `word1_pos = 0`,
    `word2_pos = 0`.

Saturation at 3 means anything from the fourth token onward is bucketed
together. This is acceptable because the ranking benefit of doc-side
position is concentrated in the first few tokens of a product name —
"Pimienta" at position 0 versus position 1 or 2 is the signal we want;
position 3+ all behave the same way for ranking.

#### Why include `kind` in the value when it's already encoded by
`adjacent` vs `wrap` postings being collapsed under the same key?

Because the key no longer carries it. After this change, the same
`(word1, word2)` pair has exactly one RocksDB key holding both Adjacent
and Wrap occurrences. The ranker needs to distinguish them — Adjacent
should still score `+5`, Wrap should still score `+1`. The kind bit in
the meta byte is what makes that distinction recoverable.

## 4. Ranking integration

All scoring constants — including the doc-position bonus — live in
`src/executor/scoring.rs`. That file is the single tuning surface; this
doc names the constants and explains what each one means, but the
authoritative values are in code.

How the meta byte is consumed:

- **Doc-side position bonus on single-word scans.** Reading a posting
  yields `(doc_id, kind, word1_pos, word2_pos)`. For a single-word
  lookup by `word1`, the relevant position is `word1_pos`; the executor
  aggregates the **minimum** `word1_pos` per doc across all postings
  (a doc that has the token in many pairs only earns the bonus once,
  at its earliest occurrence) and adds
  `doc_position_bonus(min_w1_pos)`.
- **Doc-side position bonus on prefix scans.** Same idea. Anchored
  explicit prefixes (`scan_pair_postings_by_second_word_units`) use
  `word2_pos`; unanchored explicit and auto-prefix scans use
  `word1_pos`.
- **Kind-aware merge on adjacent-pair lookups.** Adjacent and Wrap
  occurrences for the same `(word1, word2)` live under a single RocksDB
  key. Each posting's kind bit picks `ADJACENT_PAIR_SCORE` or
  `WRAP_PAIR_SCORE`. No second wrap-keyed lookup.

Current tuned values (commit-time snapshot — read `scoring.rs` for the
live numbers):

| Constant | Value | Where it applies |
|----------|------:|------------------|
| `QUERY_FIRST_WORD_BONUS`  | `4` | token at query position 0 |
| `QUERY_SECOND_WORD_BONUS` | `2` | token at query position 1 |
| `DOC_FIRST_WORD_BONUS`    | `4` | matched word at doc position 0 |
| `DOC_SECOND_WORD_BONUS`   | `2` | matched word at doc position 1 |
| `ADJACENT_PAIR_SCORE`     | `5` | adjacent posting under `(X,Y)` key |
| `WRAP_PAIR_SCORE`         | `1` | wrap posting under `(X,Y)` key |
| `PREFIX_SCORE`            | `5` | explicit or auto prefix hit |
| `WORD_BASE_SCORE_BASE`    | `6` | base term in `base + len` |
| `WORD_BASE_SCORE_FLOOR`   | `8` | floor of clamped base |
| `WORD_BASE_SCORE_CEIL`    | `12` | ceiling of clamped base |

## 5. Out of scope

- Per-doc length normalisation. The meta byte does not encode `N`; if
  ranking later needs it, the reverse-ref record is the natural place
  to put it.
- Exact-token verification (`idx = 5`, `idx = 6`). Untouched by this
  change.
