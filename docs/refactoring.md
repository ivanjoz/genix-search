# Refactoring Direction

This project (GenixSearch) is moving away from upstream Sonic's original goal of being a small exact-token identifier index. The new direction is a compact, lossy, ranked search engine optimized for Spanish product names and short commercial text.

The core idea is to replace plain term hashes as the main retrieval primitive with deterministic signatures built from statistically grouped Spanish letter pairs.

## Goals

- Rank results instead of only filtering by term intersection.
- Return ranking information in query responses, not only matching object IDs.
- Support compact lossy matching for product names.
- Boost adjacent two-word phrase matches.
- Support prefix-like queries such as `tequila sunr*`.
- Keep index values as compact `i32`/`u32` identifier lists.
- Keep storage predictable and RocksDB-friendly.
- Support collections where the client provides unique 32-bit integer object keys directly.

## Non-Goals

- Preserve the upstream Sonic project's exact original storage format.
- Preserve the upstream Sonic project's original minimal ranking behavior.
- Build a general multilingual full-text search engine.
- Guarantee exact phrase matching from lossy signatures alone.
- Keep the upstream Sonic project's OID-to-IID translation as a required storage layer.

## Rewrite Scope

This is a rewrite of the indexing core, not a small compatibility refactor. A large part of the upstream Sonic project's current code can be removed or isolated behind legacy compatibility because the new engine does not need the original exact-token flow.

Likely removal or replacement areas:

- Original term-hash search intersection as the primary query path.
- FST autocomplete and typo-correction path, unless kept only as a legacy feature.
- OID-to-IID and IID-to-OID translation for collections that use client-provided 32-bit keys.
- IID allocation metadata for direct-key collections.
- Language detection and large multilingual stopword tables if the product search domain stays Spanish/product-focused.
- Query alternate logic based on FST suggestions.
- The fixed 9-byte key layout when indexing `u64` signatures.

The new core should be organized around:

- normalization
- lossy token encoding
- numeric quantization
- pair-first indexing
- candidate collection
- scoring/ranking
- optional verification indexes

## Agent Instructions

The goal of this refactor is to build a new pair-first lossy ranked search engine, not to preserve the upstream Sonic project's original exact-token architecture.

Implementation should prioritize:

1. Direct `i32` document keys.
2. Pair-signature indexing as the main retrieval structure.
3. Range scans over pair signatures for both two-word and single-word queries.
4. Ranked output in `key|score` format.
5. Removing or isolating upstream paths that conflict with this model.

Do not start by extending the old `term_hash -> [IID]` search path. The old path is useful only as reference material for RocksDB access patterns and posting-list storage.

## Letter-Pair Signatures

The `docs/bigrams.md` table groups ordered two-letter combinations by observed Spanish product-name frequency.

The letter-pair encoder uses byte codes `0..254`. Byte `255` is reserved for numeric runs as `NUM_FLAG` and must never be emitted by a letter-pair group.

Byte codes are assigned deterministically from `docs/bigrams.md`:

```text
iterate first letters in row-major order: a..z
within each row, iterate groups left-to-right
assign byte codes incrementally from 0
skip byte 255
```

For example, the first group in row `a` gets the first code, the second group in row `a` gets the next code, and so on until row `z`. All characters inside a group share that group code. If a group contains `$`, that final-letter marker shares the same code as the other symbols in that group.

The encoding process is:

1. Normalize input text.
2. Normalize repeated noisy letters when useful, for example `aa` to `a`.
3. Split each word into non-overlapping two-letter pairs.
4. Map each pair to a deterministic `u8` group id.
5. Pack the group ids into fixed-width integer signatures.

For short words:

```text
first 8 normalized letters
=> 4 non-overlapping two-letter pairs
=> 4 u8 group ids
=> u32 word signature
```

Example shape:

```text
tequila
=> te qu il a$
=> [group(te), group(qu), group(il), group(a$)]
=> u32
```

This is intentionally lossy. Multiple different two-letter pairs can map to the same group id, especially for low-frequency combinations.

### Normalization

Normalization for v1 is intentionally simple and deterministic:

1. Convert text to lowercase.
2. Convert Spanish accented characters to ASCII:
   - `á à ä â` -> `a`
   - `é è ë ê` -> `e`
   - `í ì ï î` -> `i`
   - `ó ò ö ô` -> `o`
   - `ú ù ü û` -> `u`
   - `ñ` -> `n`
3. Keep only ASCII letters `a..z` and digits `0..9` inside tokens.
4. Treat all other characters as token separators.
5. Collapse repeated consecutive letters inside a token to one letter:
   - `aa` -> `a`
   - `ll` -> `l`
   - `rr` -> `r`
   - `coool` -> `col`
6. Keep repeated digits as written inside numeric runs.

Examples:

```text
CAÑA 100LTS     -> cana 100lts
Arroz--Extra    -> arroz extra
COOOLL          -> col
```

No stopword removal is required for v1.

## Odd Letters and Numbers

The word signature must remain fixed-width:

```text
word signature = 4 u8 units = u32
```

This means letters and numbers must both encode into the same four-byte budget.

### Odd Trailing Letters

If normalization leaves a single trailing letter, encode it as an end-of-word pair using the `$` marker from `docs/bigrams.md`:

```text
sol => so l$
a   => a$
```

The `$` marker is not a real input character. It only means "this first letter is the final single letter of the word" inside the encoder.

The marker is folded into existing low-frequency groups in `docs/bigrams.md`, so it does not create 26 new `u8` codes. For example, `a$` maps through the same row as pairs starting with `a`, and `$` is placed in a low-frequency group for that row.

### Numeric Runs

A numeric run must consume exactly two `u8` units:

```text
number run => [NUM_FLAG][NUM_BUCKET]
```

`NUM_FLAG` is a reserved byte that means the next byte is a lossy numeric bucket. `NUM_BUCKET` is not the original number. It is a `u8` bucket id from a numeric quantizer.

Example:

```text
100lts
=> [NUM_FLAG][bucket(100)][group(lt)][group(s$)]
=> u32
```

Large numbers still fit:

```text
2000lts
=> [NUM_FLAG][bucket(2000)][group(lt)][group(s$)]
=> u32
```

The number `2000` is not cast to `u8`. It is mapped to one of 255 possible numeric bucket codes. Byte `255` is reserved for `NUM_FLAG`, so `NUM_BUCKET` must be in `0..254`.

Numeric quantizer:

```text
bucket code 0..80      numeric step 1
bucket code 81..120    numeric step 5
bucket code 121..180   numeric step 15
bucket code 181..254   progressively coarser numeric steps
bucket code 255        reserved for NUM_FLAG
```

Bucket assignment is based on the nearest numeric boundary represented by the bucket-code table, clamped to the first or last bucket. The bucket-code ranges define how precise each part of the numeric space is. Lower bucket codes represent smaller numbers with finer precision; higher bucket codes represent larger numbers with coarser precision.

Numeric bucket boundary formula:

```text
value(code 0..80)    = code
value(code 81..120)  = 80 + ((code - 80) * 5)
value(code 121..180) = 280 + ((code - 120) * 15)
value(code 181..220) = 1180 + ((code - 180) * 25)
value(code 221..240) = 3180 + ((code - 220) * 50)
value(code 241..254) = 8180 + ((code - 240) * 100)
```

This produces boundaries from `0` through `9580`. Values above the largest boundary map to bucket code `254`. This is acceptable for product-name quantities because values rarely exceed the `5000` range.

Quantization rule:

```text
bucket(number) = code whose boundary value is nearest to number
ties choose the lower code
clamp below first boundary to 0
clamp above last boundary to 254
```

The invariant is that the quantizer returns a single `u8` in `0..254`; `255` is never a numeric bucket.

The important invariant is:

```text
any numeric run -> one bucket id -> one u8
```

This keeps mixed product tokens compact:

```text
100lts   => [NUM][100_bucket][lt][s$]
250ml    => [NUM][250_bucket][ml]
20kg     => [NUM][20_bucket][kg]
```

Prefix search over numbers is bucket-based, not decimal-prefix-exact. A query like `100l*` can match by `[NUM][bucket(100)]` plus a letter-pair prefix for `l`. A query like `10*` should not be expected to match all decimal expansions such as `100`, `1000`, and `10lts` unless a separate numeric-prefix feature is added.

## Proposed Indexes

The new search design should use a pair-first inverted index. Separate single-word indexes are not required as primary indexes because single-word queries can be served by range scans over the pair index.

### Word Signatures

Short normalized words use:

```text
u32 short_word_signature
```

Short signatures are packed in source order:

```text
[unit_1][unit_2][unit_3][unit_4] -> u32_be
```

If a word produces fewer than 4 units, the remaining bytes are zero-filled. This is an accepted lossy collision.

Long normalized words use:

```text
u64 long_word_signature
```

Long words are encoded as the first 16 normalized letters, split into 8 non-overlapping two-letter groups:

```text
first 16 normalized letters
=> 8 non-overlapping two-letter pairs
=> 8 u8 group ids
=> u64 word signature
```

Any remaining characters after the first 16 normalized letters are trimmed and not represented in the lossy signature.

Long signatures are packed in source order:

```text
[unit_1][unit_2][unit_3][unit_4][unit_5][unit_6][unit_7][unit_8] -> u64_be
```

If a long-signature path produces fewer than 8 units, the remaining bytes are zero-filled.

### Pair Index

For adjacent normalized word pairs, the main index is:

```text
pair_signature -> [DocID]
```

For two short-word signatures, the compact pair route is:

```text
pair_route = pack(short_sig(word_1), short_sig(word_2))
```

This index is used for phrase candidates, single-word candidates, prefix candidates, and ranking boosts.

Example:

```text
tequila sunrise
=> short_sig(tequila) + short_sig(sunrise)
=> u64
```

When long words are involved, the pair key must preserve lexicographic ordering and may use a wider encoded key route than `u64`. The required property is:

```text
[word_1_signature][word_2_signature] is sorted by word_1 first, then word_2
```

### Wrap Pair Entries

Every indexed text must add a wrap pair from the last word to the first word:

```text
last_word -> first_word
```

One-word records use a self-wrap pair:

```text
tequila
=> [tequila][tequila]
```

Multi-word records index adjacent pairs plus the wrap pair:

```text
tequila sunrise
=> [tequila][sunrise]
=> [sunrise][tequila]  // wrap
```

Longer example:

```text
agua mineral gas
=> [agua][mineral]
=> [mineral][gas]
=> [gas][agua]  // wrap
```

The wrap pair exists for retrieval coverage. It ensures the final word is also searchable as the first part of a pair without needing a reserved `END` signature.

Wrap pairs are not true phrase adjacency. The index must store enough metadata to distinguish pair kind:

```text
adjacent
wrap
```

Wrap matches should score lower than adjacent matches so a query like `sunrise tequila` does not rank like an actual adjacent phrase when the text is `tequila sunrise`.

### Single-Word Queries Through Pair Ranges

A single-word query does not need a separate `word -> [DocID]` index. It can scan all pair entries that begin with the queried word:

```text
query: agua
scan: [agua][00000000] .. [agua][FFFFFFFF]
```

This can be better for common words. Instead of reading one large posting list:

```text
agua -> [10000 DocIDs]
```

the engine reads split posting lists:

```text
agua mineral -> [DocIDs]
agua cielo   -> [DocIDs]
agua san     -> [DocIDs]
agua <first_word> wrap -> [DocIDs]
```

The query can stop scanning once it has collected enough candidates for ranking.

Important: RocksDB range order is not relevance order. A query should collect a candidate budget larger than the requested limit, score candidates, then return the top results.

Default candidate budget:

```text
candidate_budget = min(5000, max(500, limit * 10))
```

Example: `LIMIT 50` collects up to `500` candidates before ranking.

## Prefix Queries

Prefix-like queries can work if the signature layout preserves pair order.

Example:

```text
tequila sunr*
```

The query can encode:

```text
tequila => full short word signature
sunr    => prefix pair groups: su nr
```

Then search can retrieve pair-signature candidates whose second word begins with the encoded prefix groups.

Because the encoding is lossy, prefix results are candidates, not proof of exact text equality.

### Prefix Strategy

Prefix search should be implemented by expanding the unfinished final query unit into all compatible byte codes.

If the query prefix ends on a complete two-letter unit, compare the known encoded units directly:

```text
sunr* => su nr
```

If the query prefix ends on a single letter, expand to all groups in that row whose raw group contains that letter as a possible second symbol:

```text
sun* => su + all n? groups
100l* => [NUM][bucket(100)] + all l? groups
```

For a full-word prefix ending marker, use `$` only when the query term is complete, not when it is open-ended with `*`.

Implementation strategy:

1. Encode all complete two-byte units before the trailing single-letter prefix.
2. If there is a trailing single-letter prefix, enumerate compatible group codes from `docs/bigrams.md`.
3. Query all expanded signatures.
4. Merge candidates and score prefix matches with the `prefix pair match` weight.

This avoids RocksDB prefix scans for partial pair expansion and keeps prefix logic deterministic.

For single-word prefix queries, use the same pair-range strategy on the first word:

```text
query: agu*
scan expanded first-word prefix ranges:
[agu_prefix][00000000] .. [agu_prefix][FFFFFFFF]
```

## Collection Key Modes

Collections should support two object-key modes.

### Direct `i32` Key Mode

In direct-key collections, the client provides the object key as a unique signed 32-bit integer:

```text
object key = i32
```

The engine stores this value directly in posting lists:

```text
signature -> [i32 object_key]
```

There is no OID-to-IID translation:

```text
no oid -> iid
no iid -> oid
no iid allocator
```

The client is responsible for guaranteeing that each key is unique inside the collection or collection/bucket scope. This mode is the preferred mode for the new engine because it removes an entire storage layer and avoids extra RocksDB reads during result resolution.

### Legacy String Key Mode

If string object IDs are still needed, they can be handled by a compatibility layer:

```text
external string OID -> internal u32 DocID
```

This is not required for v1. The v1 implementation target is direct `i32` keys only. The ranking/indexing core should still use the neutral name `DocID` internally.

## Ranking Model

Search should collect candidate `DocID`s from multiple indexes and score them.

Initial scoring model:

```text
single-word pair-range match      +10
long-word pair-range match        +12
two-word pair signature match     +30
wrap pair match                   +6
prefix pair match                 +15
exact normalized token match      +extra, if stored
```

The two-word pair index should strongly boost adjacent phrase matches over independent word matches.

Example:

```text
query: tequila sunrise

record A: tequila sunrise  => word matches + pair match
record B: tequila promo sunrise => word matches only
```

Record A should rank higher.

## Query Output

The new query path must return ranked results, not only IDs. The wire format should stay compact and easy to parse.

Direct `i32` key collections should return whitespace-separated `key|score` pairs:

```text
123|92 987|77 42|50
```

Format:

```text
key|score key|score key|score
```

For legacy string-key collections, `key` can be the resolved external object ID:

```text
product:123|92 product:987|77
```

Rules:

- Results are separated by ASCII whitespace.
- Key and score are separated by `|`.
- Scores are unsigned integer ranking values.
- Direct-key collection keys are signed 32-bit integers.
- Legacy string keys must not contain whitespace or `|`, or they must be escaped by the protocol layer.

Results must be sorted by score descending:

```text
highest score first
```

Tie-breakers should be deterministic. Suggested order:

```text
score desc
pair-match count desc
single-word match count desc
doc_id asc
```

Scores should be treated as relative ranking values, not stable public metrics. Future scoring changes may alter exact numbers while preserving result ordering quality.

## Storage Implications

the upstream Sonic project's current RocksDB key format is:

```text
[idx: u8][bucket_hash: u32][route: u32]
```

That format supports only a `u32` route. The new design requires `u64` pair routes and wider extended-pair routes when long-word signatures are involved.

Proposed key formats use lexicographic ordering. Multi-byte numeric fields are encoded big-endian so RocksDB iteration order matches numeric byte order.

```text
compact pair:
[idx: u8][collection_hash: u32_be][bucket_hash: u32_be][word1_sig32: u32_be][word2_sig32: u32_be][pair_kind: u8]

extended pair:
[idx: u8][collection_hash: u32_be][bucket_hash: u32_be][word1_slot][word2_slot][pair_kind: u8]

doc reverse signatures:
[idx: u8][collection_hash: u32_be][bucket_hash: u32_be][doc_id: i32_be]
```

Extended pair `word_slot` format is fixed-width:

```text
word_slot = [word_kind: u8][sig64: u64_be]
```

`word_kind` values:

```text
1 = short signature stored in sig64 high 32 bits, low 32 bits zero
2 = long signature stored as full sig64
```

Examples:

```text
short sig32 0x11223344 -> sig64 0x1122334400000000
long  sig64 0x1122334455667788 -> sig64 0x1122334455667788
```

This keeps extended pair keys fixed-width and lexicographically sortable by `word1`, then `word2`.

Suggested index ids:

```text
1 = compact pair route -> [DocID]
2 = extended pair signature -> [DocID], for routes wider than compact u64
4 = DocID -> indexed signatures
5 = exact normalized token hash -> [DocID], optional
6 = exact normalized pair hash -> [DocID], optional
```

Values remain compact document-key lists:

```text
[doc_1: i32/u32][doc_2: i32/u32][doc_3: i32/u32]...
```

For direct-key collections, these values are the client-provided object keys. For legacy string-key collections, these values are internal document IDs that still require lookup before returning results.

Direct `i32` document keys must be encoded for lexicographic order as:

```text
sortable_doc_id = (doc_id as u32) ^ 0x80000000
store sortable_doc_id as u32_be
```

If the implementation chooses to reject negative keys, plain `u32_be` is also acceptable. V1 should accept full signed `i32`, so use the sortable transform.

### Replacement and Deletion

Every indexed object must store its reverse signature list:

```text
DocID -> [indexed signature refs]
```

Each signature ref must include the index id and signature value:

```text
{ index: compact_pair, word1_sig32, word2_sig32, pair_kind }
{ index: extended_pair, word1_sig, word2_sig, pair_kind }
{ index: exact_token, hash } optional
{ index: exact_pair, hash } optional
```

Reverse signature serialization alternatives:

1. **Fixed-width refs, recommended for v1**

   Simple and fast to parse. Each ref stores a full extended shape, even for compact pairs.

   ```text
   [count: u16_be]
   repeated count times:
     [index: u8]
     [pair_kind: u8]
     [word1_kind: u8][word1_sig64: u64_be]
     [word2_kind: u8][word2_sig64: u64_be]
   ```

   Pros: simplest deletion/replacement path; no branchy parsing.
   Cons: larger reverse values.

2. **Tagged compact refs**

   Store compact refs in a smaller form and extended refs in the full form.

   ```text
   [count: u16_be]
   compact ref:
     [index: u8][pair_kind: u8][word1_sig32: u32_be][word2_sig32: u32_be]
   extended ref:
     [index: u8][pair_kind: u8][word1_kind: u8][word1_sig64: u64_be][word2_kind: u8][word2_sig64: u64_be]
   ```

   Pros: smaller for common short-short product names.
   Cons: more parser complexity.

3. **Store normalized text and recompute**

   Store only normalized text and recompute old signatures on deletion/replacement.

   Pros: smallest reverse metadata if text is already needed.
   Cons: deletion correctness depends on encoder stability; not recommended for v1.

Use alternative 1 for v1.

On replacement:

1. Read `DocID -> indexed signature refs`.
2. Remove the `DocID` from all old posting lists.
3. Encode the new text.
4. Insert the `DocID` into new posting lists.
5. Replace the reverse signature list.

On deletion:

1. Read `DocID -> indexed signature refs`.
2. Remove the `DocID` from all posting lists.
3. Delete the reverse signature list.

## Candidate Verification

Lossy signatures are not exact. They are useful for compact retrieval and ranking, but collisions are expected.

If exact confirmation is needed, add one or more verification indexes:

```text
exact normalized token hash -> [DocID]
exact normalized pair hash  -> [DocID]
DocID -> compact stored token signatures
```

V1 decision: do not implement exact verification indexes. Ranking over lossy candidates is enough for the first implementation. Keep index ids `5` and `6` reserved for future exact verification.

## Protocol

The protocol should expose direct-key operations explicitly.

Create or configure a direct-key collection:

```text
CREATE <collection> KEY(INT32)
```

Success:

```text
OK
```

Push a direct-key object:

```text
PUSHI <collection> <bucket> <int32_key> "<text>"
```

Replace semantics are the default. If `<int32_key>` already exists in the reverse signature index, old postings are removed before new postings are inserted.

Success:

```text
OK
```

Delete a direct-key object:

```text
POPI <collection> <bucket> <int32_key>
```

Success:

```text
OK <count>
```

`count` is `1` if an object was removed and `0` if the key was not indexed.

Query:

```text
QUERY <collection> <bucket> "<terms>" [LIMIT(<count>)]? [OFFSET(<count>)]?
```

Query response payload:

```text
key|score key|score key|score
```

Example:

```text
123|92 987|77 42|50
```

Empty query result payload:

```text
EMPTY
```

Errors:

```text
ERR <code>
```

Suggested error codes:

```text
bad_request
bad_key
bad_collection
bad_limit
store_error
```

Legacy string-key commands are out of scope for v1. The new implementation target is direct `int32` key indexing.

## Migration Plan

1. Define the new storage model and direct `i32` collection mode.
2. Implement the Spanish letter-pair encoder from `docs/bigrams.md`.
3. Implement trailing-letter `$` handling.
4. Implement numeric-run encoding with `[NUM_FLAG][NUM_BUCKET]`.
5. Add unit tests for normalization, pair grouping, final-letter encoding, number bucketing, and integer packing.
6. Add RocksDB key builders for lexicographic `u32` and `u64` routes.
7. Add direct-key posting lists where values are client-provided `i32` keys.
8. Add the pair index, including wrap `[last_word][first_word]` entries.
9. Add pair-range single-word query support.
10. Add pair-prefix query support.
11. Add long-word signatures using the first 16 normalized letters.
12. Replace search intersection with candidate collection plus scoring.
13. Add reverse signature storage for replacement and deletion.
14. Add direct-key protocol commands.
15. Return ranked query output with score values.
16. Remove or isolate legacy upstream-only paths that are no longer part of the new core.
17. Add tests for ranking behavior:
   - exact single word
   - lossy single word
   - exact adjacent pair
   - separated words
   - prefix pair query
   - mixed number and letters, for example `100lts`
   - direct `i32` object keys without OID/IID translation
   - output ordering by descending score

## V1 Status

No v1 implementation blockers remain.
