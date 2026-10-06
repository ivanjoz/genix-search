# Compression study: can the `.idx` and `.db` be made smaller, and does it pay? (2026-10-06)

The question: is there a cheap compression (the keys use 5-bit letters) that
shrinks the `.idx` and `.db`; would ordering the document IDs help, in the
index or in another object. The format was not changed: every alternative is
measured as the bits it would take on the same index, and priced per cold
search with the method of [COST_STUDY.md](COST_STUDY.md).

## 1. Method

- `bucket.TestCompressStudy` (`internal/bucket/compress_study_test.go`)
  builds the index and counts the bits of every part (header, keys, fields,
  lists: gaps, positions, tails), then what other codecs would take on the
  same data. `GXS_COMPRESS_OUT` writes the objects for external compressors.
- `query.TestCompressCost` (`internal/query/compress_cost_test.go`) runs the
  4 500 cold searches of `TestCostSweep` and records, per search, the GETs by
  object and the bytes by section of every `.idx` read. It prices:
  - the index as it is, under each way of giving IDs;
  - **codecs:** the bytes of each section times the ratio the study measured,
    with the index rebuilt with `InlineSlot / ratio`, so the slots that would
    fit once compressed are inline (their searches lose the `.db` GET);
  - **ID table:** the documents numbered 1..N by the engine, and the real IDs
    of the 10 hits read from a table in another object, after the search.
- **IDs:** `random` uint32 (the case the design must hold); `shuffled`, dense
  1..N in random order (an autoincrement); `csv`, dense in the order of the
  catalogue (grouped by category and brand); `sorted`, dense in the order of
  the normalized names (what the engine could do if it numbered the
  documents itself).
- Corpus `productos.csv` (10 010 names); 100k and 400k are copies of it, so
  the vocabulary does not grow and the copies of a name are identical (see
  §6).

```sh
cd genix-s3
GXS_COMPRESS=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv GXS_COMPRESS_SCALE=1,10 \
  go test ./internal/bucket -run CompressStudy -v
GXS_COMPRESS=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv GXS_COMPRESS_SCALE=1,10 \
  go test ./internal/query -run CompressCost -v -timeout 60m
```

## 2. Where the bytes are

`.idx` total, 10k documents, random IDs, `none` (every slot fits `InlineSlot`,
so there is no `.db`):

| Part | KB | Share |
|---|---|---|
| Header (64 B per object) | 22.2 | 6 % |
| Keys (the 5-bit letters) | 23.0 | 6 % |
| Fields | 63.2 | 17 % |
| — of which the first key of each word (32 bits with random IDs) | 37.5 | 10 % |
| Inline lists | 272.0 | 71 % |
| — gaps between document keys | 234.3 | 62 % |
| — positions (4 bits) | 35.4 | 9 % |
| — tails | 2.2 | 1 % |
| **Total** | **380.5** | |

At 100k (random): `.idx` 2 455 KB + `.db` 1 413 KB; the gaps are 2 439 KB of
it (63 %). At 400k: 25.9 MB, of which the gaps are 9.2 MB in the lists plus
the combination blocks.

**The letters are 6 % of the `.idx` at 10k and under 1 % at 100k.** With
random IDs, the bytes are the document IDs: a random uint32 carries 32 bits
of information that no codec can remove.

What a cold search reads (bytes per search, `none`):

| | 10k random | 100k random | 400k random |
|---|---|---|---|
| GETs (`.idx` · `.db` · `p.log`) | 3.16 (2.17 · 0 · 1) | 3.51 (2.17 · 0.35 · 1) | 4.76 (2.17 · 1.59 · 1) |
| KB read | 8.2 | 40.2 | 47.5 |
| of which lists (inline + `.db`) | 6.2 | 38.1 | 44.8 |
| of which keys | 0.4 | 0.4 | 0.4 |
| Share of the bill that is bytes | 5 % | 18 % | 16 % |

## 3. Codecs

### 3.1 Gaps (62–70 % of the bytes)

KB of the gaps of every list with more than one posting, by codec. `bound`
is `log2 C(u, df)`, the minimum any codec of the documents can reach on
average.

| | now (fixed W / nibbles) | Rice, best k per list | Elias-Fano | Interpolative (BIC) | bound |
|---|---|---|---|---|---|
| 10k random | 234.3 | 228.2 (−2.6 %) | 232.2 | 226.2 (−3.5 %) | 220.9 (−5.7 %) |
| 100k random, inline | 2 010.6 | 1 893.9 (−5.8 %) | 1 927.8 | 1 909.2 | 1 867.6 (−7.1 %) |
| 400k random, `.db` | 5 601.0 | 5 188.5 (−7.4 %) | 5 287.2 | 5 250.5 | 5 149.0 (−8.1 %) |
| 10k shuffled | 77.6 | 74.2 (−4.4 %) | 78.1 | 72.6 | 68.0 |
| 10k csv | 53.8 | 66.1 (+23 %) | 68.4 | 51.1 (−5 %) | 58.1 ¹ |

¹ Below the bound because the documents of a word are clustered in the
catalogue order, which the bound (uniform) does not assume.

**The gaps are already within 6–8 % of the minimum.** Rice gets most of that
(the fixed width loses on long lists, where the largest gap is `ln(df)` times
the mean); with clustered IDs only BIC improves on the current codecs.

### 3.2 Positions (9–27 % of the bytes)

Distribution: 0–6 hold 82 %, ≥ 8 hold 12 %; entropy 3.44 bits against 4.

| | 4 bits | width per list (2 bits + max position width) | Elias gamma | order-0 entropy |
|---|---|---|---|---|
| 10k | 35.4 | 31.6 (−11 %) | 38.4 | 30.4 (−14 %) |
| 100k random, inline | 308.4 | 260.7 (−15 %) | 332.1 | 264.8 |
| 400k shuffled, inline | 792.5 | 665.3 (−16 %) | 846.9 | 680.6 |

A width per list is as good as entropy coding and costs 2 bits in `V`.
Positions cannot drop to 3 bits: 18.5 % are ≥ 7, and the distance between
words is scored.

### 3.3 Fields, keys, heads

- **Fields:** `count` as Elias gamma instead of a fixed width per object:
  8.4 → 3.8 KB at 10k (−55 % of `count`, −1.2 % of the `.idx`); nothing at
  100k. The first key is the bulk (37.5 KB) and is a random ID.
- **Keys, the 5-bit letters:** 23.0 KB = slot directory 2.8 + headers 5.9 +
  letters 14.1. Entropy of a letter: 4.25 bits (order 0), 3.52 (given the
  previous letter); header 4.28 bits. The bound with an order-1 code is
  17.9 KB (−22 % of the keys, **−1.4 % of the `.idx` at 10k, −0.2 % at
  100k**). A Huffman table fixed in the code would get somewhat less.
- **Heads:** 4.5 KB at 400k; gaps inside each rank run instead of split keys:
  3.7 KB. Negligible.

### 3.4 General-purpose compressors

Each object compressed on its own (the `.idx` is read whole, so it could be):

| `.idx` total, 10k | raw | gzip -9 | zstd -19 | brotli 11 | xz -9e |
|---|---|---|---|---|---|
| random | 389 674 | −0.9 % | −2.4 % | −2.4 % | +2.8 % |
| csv | 185 342 | −6.7 % | −8.6 % | −9.3 % | +1.1 % |

They find almost nothing: the data are bit streams not aligned to bytes, and
the random gaps are noise. The `.db` is read by ranges, so it could not use
them anyway.

### 3.5 Codecs priced

Rice gaps + a position width per list + gamma counts + keys at their order-1
entropy (optimistic), with the `InlineSlot` effect simulated. $ per million
cold searches, `none`:

| | 10k | 100k | 400k |
|---|---|---|---|
| random, now | 0.0995 | 0.1285 | 0.1699 |
| random, codecs | 0.0993 (−0.2 %) | 0.1269 (−1.2 %) | 0.1674 (−1.5 %) |
| shuffled, now | 0.0973 | 0.1131 | 0.1484 |
| shuffled, codecs | 0.0971 (−0.2 %) | 0.1113 (−1.6 %) | 0.1433 (−3.4 %) |

With `es100` at 100k: random 0.1417 → 0.1391 (−1.8 %). At 400k with dense
IDs the gain is larger because more slots fit in `InlineSlot` (`.db` GETs
0.89 → 0.71).

## 4. Ordering the IDs

### 4.1 What the IDs cost

The same index, only the IDs change:

| Objects (`.idx` + `.db`) | random | shuffled | csv | sorted |
|---|---|---|---|---|
| 10k | 380 KB | 203 KB (−47 %) | 181 KB (−52 %) | 187 KB (−51 %) |
| 100k | 3 868 KB | 1 477 KB (−62 %) | 1 221 KB (−68 %) | 1 273 KB (−67 %) |
| 400k | 25 909 KB | 7 900 KB (−70 %) | | |

| $ per 1M cold searches | random | shuffled | csv | sorted |
|---|---|---|---|---|
| 10k, `none` | 0.0995 | 0.0973 (−2 %) | 0.0970 (−3 %) | 0.0970 (−3 %) |
| 100k, `none` | 0.1285 | 0.1131 (−12 %) | 0.1104 (−14 %) | 0.1109 (−14 %) |
| 100k, `es100` | 0.1417 | 0.1198 (−15 %) | 0.1158 (−18 %) | 0.1166 (−18 %) |
| 400k, `none` | 0.1699 | 0.1484 (−13 %) | | |

**Most of the gain comes from density, not from order:** going from a
uint32 range to 1..N saves `log2(2^32/N)` bits per posting (15 at 100k).
Ordering by name adds 2–3 points on top.

### 4.2 Numbering the documents in the engine (an ID table)

If the engine gave its own numbers 1..N, a search would still have to return
the client's IDs. The hits are known only after scoring, so their IDs come
from a table in **one more round**:

| $ per 1M cold searches | 10k | 100k `none` | 100k `es100` | 400k `none` |
|---|---|---|---|---|
| random IDs, now | 0.0995 | 0.1285 | 0.1417 | 0.1699 |
| numbered + table of u32 | 0.1418 (+43 %) | 0.2294 (+79 %) | 0.2360 (+67 %) | 0.2671 (+57 %) |
| numbered + Elias-Fano table in chunks of 64 | 0.1368 (+37 %) | 0.1991 (+55 %) | 0.2058 (+45 %) | 0.2314 (+36 %) |
| numbered in name order + EF table | 0.1322 (+33 %) | 0.2019 (+57 %) | 0.2074 (+46 %) | |

It loses in every case, and it cannot win: **at 100k all the bytes a search
reads cost $0.022 per million, less than one GET ($0.030).** A table in
another object adds at least one GET, and the 10 hits are scattered over it
(1.0–2.1 GETs and 80–190 KB per search). In `p.log`, which every search
already reads, the table would add its whole size to every search (24 KB at
10k, 208 KB at 100k): it loses too. It also has write costs this study does
not price: ordinals that move on every compaction, and a put that must look
up the ordinal of its ID. This confirms the earlier rejection of a
translation table (INTERNALS §25, "IDs").

### 4.3 A local ID table in each `.idx`

Each object would store its distinct documents once (Elias-Fano), and its
lists would point into that table. It adds no request (the `.idx` is read
anyway), but it only gains when a document appears in several lists of the
same object: 1.14 lists per document with `none`, 1.16 with `es100`.

Bits of the document keys (first keys + gaps, `none`; the table is the first
key in full and the rest Elias-Fano over the object's own range of keys):

| | now | local table (table + first + gaps) | |
|---|---|---|---|
| 20k, sparse5 (IDs up to 100 000) | 237.5 KB | 141.7 + 11.9 + 84.1 = 237.7 KB | 0 % |
| 20k, sparse10 (IDs up to 200 000) | 257.0 KB | 158.0 + 11.9 + 84.1 = 254.0 KB | −1.2 % |
| 20k, dense | 191.5 KB | 199.7 KB | +4 % |
| 20k, random uint32 | 528.6 KB | 488.6 KB | −7.6 % |
| 100k, sparse5 | 1 230 KB | 1 186 KB | −3.6 % |
| 100k, sparse10 | 1 330 KB | 1 268 KB | −4.6 % |
| 100k, dense | 994 KB | 996 KB | 0 % |
| 100k, random uint32 | 2 475 KB | 2 254 KB | −8.9 % |

With `es100` the results are within a point of these.

The bits do not disappear: they move into the table, which must hold the
real ID of every document of the object (to return hits and to match the
words of other objects). The local numbers save `log2(range / n)` bits per
reference to a document, and the table costs about `2 + log2(range / n)` per
document, so the table pays only when `(R − 1) · log2(range / n) > 2`, R
being the references per document. A slot holds one or two words of a
name, so R is 1.14. A fixed width per number (10 bits for 1 000 documents)
would be worse than the gaps of today for any list of 4 documents or more.

With realistic IDs (an autoincrement with 4 to 9 deletions in 10) it saves
0–5 % of the ID bits, under 0.5 % of the bill. Not recommended. (A first
version of this table priced the table over a range of 2³² whatever the IDs,
and reported +123 % for dense IDs; that was wrong.)

### 4.4 IDs with holes: an autoincrement after deletions

Real IDs are neither dense nor random: they are an autoincrement with
deletions. Two cases, IDs in no order of the names:

- `sparse10`: N documents among IDs 1..10N (one in ten left; 10k documents
  with IDs up to 100 000; the largest gap between consecutive IDs is 79 at
  10k and 105 at 100k, under a `uint8`);
- `del10`: N documents among IDs 1..N/0.9 (one in ten deleted).

| | random | sparse10 | del10 | dense (shuffled) |
|---|---|---|---|---|
| Objects, 10k | 380 KB | 235 KB (−38 %) | 205 KB (−46 %) | 203 KB (−47 %) |
| Objects, 100k | 3 868 KB | 1 809 KB (−53 %) | 1 491 KB (−61 %) | 1 477 KB (−62 %) |
| $ per 1M, 10k `none` | 0.0995 | 0.0977 (−2 %) | 0.0973 (−2 %) | 0.0973 (−2 %) |
| $ per 1M, 10k `es100` | 0.1004 | 0.0978 (−3 %) | 0.0973 (−3 %) | 0.0972 (−3 %) |
| $ per 1M, 100k `none` | 0.1285 | 0.1166 (−9 %) | 0.1132 (−12 %) | 0.1131 (−12 %) |
| $ per 1M, 100k `es100` | 0.1417 | 0.1251 (−12 %) | 0.1200 (−15 %) | 0.1198 (−15 %) |
| KB per search, 100k `none` | 40.2 | 32.7 | 26.8 | 26.5 |

The cost of an ID is `log2(range / df)` bits per posting: each factor 10 of
holes adds 3.3 bits, against 15 for random IDs at 100k. So one deletion in
ten is as good as dense, and nine in ten still keep about 75 % of the gain of
dense IDs. Nothing in the engine has to change for it: the gap codecs adapt
per list. The codecs of §3.5 on `sparse10`: 100k `none` 0.1166 → 0.1145
(−1.8 %), `es100` 0.1251 → 0.1220 (−2.5 %), 10k 0.0977 → 0.0974.

## 5. `tokens` (write path)

### 5.1 What it weighs in the bill

`tokens` is read and rewritten whole by every compaction; searches never
read it. Measured with `TestCompressCost` (one-document puts up to the
compaction threshold, then the compaction):

| | 10k | 100k random | 100k sparse10 |
|---|---|---|---|
| `tokens` | 382 KB | 3 786 KB | 3 786 KB |
| Puts up to the compaction (appends to `p.log`) | $7.43 × 10⁻⁵ | $7.78 × 10⁻⁵ | $7.78 × 10⁻⁵ |
| The compaction alone | $0.60 × 10⁻⁵ | $2.60 × 10⁻⁵ | $1.84 × 10⁻⁵ |
| `tokens` (read + upload) | $0.14 × 10⁻⁵ | $1.37 × 10⁻⁵ | $1.37 × 10⁻⁵ |
| `tokens` in the compaction alone | 23 % | 53 % | 75 % |
| `tokens` in puts + compaction | 2 % | 13 % | 14 % |

The appends cost more than the compaction (each one is a write request,
$1.13 × 10⁻⁶). In this workload the puts change few buckets; a compaction
that rewrites more buckets makes `tokens` a smaller share. (A first estimate
of "a third of a compaction", taken from another workload, was wrong.)

### 5.2 Layouts

`internal/tokens/compress_study_test.go`. Documents part of the object (the
tables are small), 10k:

| Layout | random raw | random + flate | sparse10 raw | sparse10 + flate |
|---|---|---|---|---|
| Now: key 5 B, length u16, 5-bit tokens | 378 KB | 295 KB (−22 %) | 378 KB | 279 KB (−26 %) |
| Key as a gap (uvarint), length uvarint, 5-bit tokens | 348 KB (−8 %) | 276 KB (−27 %) | 329 KB (−13 %) | 253 KB (−33 %) |
| Vocabulary + rows (gap, count, token numbers as uvarints) | 205 KB (−46 %) | 179 KB (−53 %) | 186 KB (−51 %) | 155 KB (−59 %) |
| **Vocabulary + columns** (all gaps, all counts, all numbers) | 205 KB (−46 %) | **176 KB (−54 %)** | 186 KB (−51 %) | **154 KB (−59 %)** |
| At 100k (`flate`, columns) | | 1 150 KB (−70 %) | | 970 KB (−74 %) |

- **Vocabulary:** every distinct token (key and tail) once, the most
  frequent first, as one 5-bit token stream; a document is the numbers of
  its tokens. 10k names have 77 198 tokens and 9 972 distinct ones: the
  numbers take 10.75 bits each (order-0 entropy), against 20–40 bits for the
  units of a word.
- **Gaps:** with `sparse10` a gap has 4.7 bits of entropy and is at most 105,
  so a uvarint takes 1 byte (a fixed `uint8` would break on the first gap of
  256; a uvarint does not). The IDs are only about 4 % of the object: the
  vocabulary is what matters.
- **flate** (`compress/flate`, standard library) is close to what a custom
  entropy coder would get: the gaps, counts and numbers have 111 KB of
  order-0 entropy (sparse10, 10k), and flate's whole body, vocabulary
  included, is 154 KB. On the columns layout zstd -19 is 5 % better (150 KB
  against 158 KB with gzip -9) and xz 9 %: no new dependency is needed.
- The 100k objects are copies of the catalogue: the vocabulary does not grow,
  so their −70 % is optimistic. flate's 32 KB window cannot reach the
  copies, which are hundreds of KB apart.

### 5.3 Implemented: `tokens` Format 4

```
header, tables (expanded words, buckets, garbage)     as before; the header holds the body's CRC32C
body = flate(
  vocabulary: len uvarint | the distinct tokens as one token stream (key5.AppendTokens), most frequent first
  N gaps:    key − previous key, uvarint (the first from 0; a change of origin is just a large gap)
  N counts:  tokens of the document, uvarint
  numbers:   per document, per token in position order, its number in the vocabulary, uvarint
)
```

The chunks of 1 000 documents are gone: the compaction already read every
chunk. Decoding builds the vocabulary once and then each document's token
stream; `ReadTokens`/`AppendTokens` stay for `p.log`. Specified in INTERNALS
§12.

Measured after the change:

| | before | after |
|---|---|---|
| `TestBench`, base A (10k names, row IDs) | 379.4 KB | 134.4 KB (−65 %) |
| `TestBench`, base B (names + categories) | 515.9 KB | 145.1 KB (−72 %) |
| 100k, IDs up to 1M (sparse10) | 3 786 KB | 971 KB (−74 %) |
| Compaction alone, 100k sparse10 | $1.84 × 10⁻⁵ | $0.82 × 10⁻⁵ (−55 %) |
| Puts before it + compaction, 100k sparse10 | $9.62 × 10⁻⁵ | $8.60 × 10⁻⁵ (−11 %) |

`TestBench` passed against its baseline with every top 10, every quality
figure and every search cost identical; only the size of `tokens` and the
bytes written by the build changed (921 → 676 KB for base A).

## 6. Caveats

- 100k and 400k are copies of 10k: every list grows in proportion and the
  vocabulary does not. Real catalogues have more distinct words (more
  one-posting keys, whose first key is a full random ID).
- The codec rows are estimates: bytes scaled by the measured ratio and
  `InlineSlot` stretched; the rankings were checked to be identical with the
  stretched `InlineSlot`. A real implementation would give the same GETs and
  bytes within a few percent.
- The keys' entropy is a bound computed on the corpus itself; a table fixed
  in the code would do a bit worse.

## 7. Conclusions

1. **Compressing the letters (the 5-bit units) is not worth it:** they are
   6 % of the `.idx` at 10k and under 1 % at 100k; the best possible code
   saves 0.1 % of the bill.
2. **The bytes are the random IDs, and they are already near the minimum.**
   No codec can save more than 6–8 % of the gaps. The best set (Rice + a
   position width per list + gamma counts) saves 0.2 % of the bill at 10k,
   1.2–1.8 % at 100k and 1.5 % at 400k. Possible, but it changes the format
   of every list for little.
3. **General-purpose compressors** (zstd, brotli, xz) save 0–2.4 % of the
   `.idx` with random IDs. Rejected.
4. **Ordering the IDs pays only if the IDs are dense, and that is the
   client's choice:** dense IDs (an autoincrement per origin) make a cold
   search 12–18 % cheaper at 100k–400k and the objects 47–70 % smaller,
   with no change to the engine. An autoincrement with deletions keeps most
   of it: one in ten deleted equals dense, one in ten left (IDs up to 10N)
   gives −9 to −12 % at 100k (§4.4). Numbering the documents in the engine
   with an ID table costs 33–79 % more, at every size, because it adds a GET.
5. **Where compression does pay is `tokens`, on the write side:** a
   vocabulary of tokens, document gaps and token numbers in columns, then
   flate from the standard library, cuts it by 65–72 % at 10k and 74 % at
   100k (§5.3, implemented). That is −55 % of a compaction at 100k, but
   −11 % once the puts before it are counted.

### Recommendations

- Document for the clients (Genix ORM first) that **compact IDs per origin**
  are the biggest saving available: up to 18 % per search; deletions do not
  undo it. Check which IDs the ORM sends today.
- The `tokens` layout of §5.3: **implemented** (Format 4).
- Optional, small: a position width per list (2 bits in `V`; −11 to −16 % of
  the positions, about −0.4 % of the bill at 100k; a third of the codec gain)
  and Rice gaps. Only if the format is changed for another reason.
- Do not implement: an ID table in another object or in `p.log`; a local ID
  table per `.idx`; general-purpose compression of `.idx`/`.db`; a new code
  for the letters.
