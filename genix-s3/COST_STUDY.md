# Cost study: the shape of the index on S3 Express (2026-10-06)

What a cold search costs in dollars under each shape of the index, why the
defaults are what they are, and how to measure it again. The specification is
[INTERNALS.md](INTERNALS.md) (§1 cost model, §10.6 inline, §25 summary); this
file keeps the full study.

## 1. Questions

1. Does it pay to make **more GETs and read fewer bytes**? For example, an
   `.idx` that is a pure index (keys and pointers) with every list in the
   `.db`.
2. Which **shape of the index** (inline lists, expansions, range merging,
   grouping of slots into objects) gives the lowest bill?

Searches run in Lambdas, so **every search is cold**: nothing is cached
between searches, and warm figures play no part.

## 2. Prices

S3 Express One Zone, us-east-1, from the AWS Price List API
(`pricing.us-east-1.amazonaws.com`, offer `AmazonS3`, published 2026-09-28;
the pricing web page renders its tables with JavaScript):

| Usage type | Price | Per unit |
|---|---|---|
| `Requests-XZ-Tier2`: GET and every request but the writes | $0.00003 per 1000 | 3 × 10⁻⁸ |
| `Requests-XZ-Tier1`: PUT, COPY, POST, LIST | $0.00113 per 1000 | 1.13 × 10⁻⁶ |
| `Retrieval-XZ`: bytes read (every byte) | $0.0006 per GB | 5.6 × 10⁻¹³ per byte |
| `Upload-XZ`: bytes written | $0.0032 per GB | 3.0 × 10⁻¹² per byte |
| `TimedStorage-XZ` | $0.11 per GB-month | |
| DELETE | free | |

GB is 2³⁰ bytes. Derived figures (`internal/pricing`):

- **One GET costs as much as reading 53 687 bytes (52 KiB)**: `BytesPerGET`.
- One PUT costs as much as uploading 379 165 bytes (370 KiB): `BytesPerPUT`.

A search reads 4 to 70 KB, so **GETs are 80–97 % of its bill**. Saving one
GET is worth reading tens of KB more.

## 3. Method

- **Tool:** `TestCostSweep` (`internal/query/sweep_test.go`). It builds the
  index once per shape, runs every search cold (a fresh engine per search, as
  in a new Lambda) on every core, and reports dollars per million searches,
  GETs, KB and rounds per search, plus the dollars of the build, of one
  compaction (one-document appends up to `CompactBytes`, then `Compact`) and
  the bytes stored. It fails if any shape changes a ranking: none did.
- **Workload:** 4 500 searches per shape: 3 000 queries of 2 or 3 consecutive
  words of random product names, and the first word alone of every other one
  (as a prefix, like any last word).
- **Corpus:** `scripts/perf/data/productos.csv`, 10 010 product names. Larger
  sizes are copies of the catalogue under new IDs: x10 = 100k, x40 = 400k,
  x100 = 1M documents.
- **IDs:** dense (1..N) or random `uint32`.

```sh
cd genix-s3
GXS_SWEEP=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv \
  GXS_SWEEP_SCALE=1,10 GXS_SWEEP_IDS=dense,random GXS_SWEEP_GROUPING=none,es100 \
  GXS_SWEEP_SLOT=0,53687 GXS_SWEEP_INLINE=0,2048 GXS_SWEEP_EXPAND=8192,32768 \
  GXS_SWEEP_GAP=4096,53687 \
  go test ./internal/query -run CostSweep -v -timeout 60m
```

`GXS_SWEEP_SLOT` is `InlineSlot`, `GXS_SWEEP_INLINE` is `InlineBudget`,
`GXS_SWEEP_EXPAND` is `ExpandCost` (and `MinSplit`), `GXS_SWEEP_GAP` is
`CoalesceGap`. At 1M a run takes about 2.5 min and 1.8 GB of memory per
handful of shapes.

**Caveats.**

- 100k to 1M are the same catalogue repeated: the vocabulary does not grow
  as it would in a real catalogue of that size, and every list grows in
  proportion.
- The queries come from the names, so a word is searched in proportion to
  the documents that hold it. The `InlineSlot` rule (§5.2) rests on that.
- Rounds are reported but not priced: a round is S3 latency, which a Lambda
  pays in duration.

## 4. Where the GETs go

Before the study, 10k documents, `es100`, the benchmark queries: 3.76 GETs
per search, of which `.idx` 2.68, `p.log` 1.00, `.db` 0.08. The `p.log` GET
is fixed (it is how a search sees the pending writes) and there is one `.idx`
per distinct slot group of the query. The `.db` GETs are what the shape of
the index can still remove, and what a pure index would add.

## 5. Results

All figures are dollars per million cold searches unless said otherwise.

### 5.1 Overview

| Shape | 10k dense | 10k random | 100k dense | 100k random | 400k random | 1M random |
|---|---|---|---|---|---|---|
| Before: es100, `InlineBudget` 2 KB, `ExpandCost` 2 KB, `CoalesceGap` 4 KB | 0.0998 | 0.1194 | 0.1546 | 0.1694 | | |
| Pure index: no inline lists, none ¹ | 0.1530 | 0.1547 | 0.1620 | 0.1675 | 0.1800 | 0.1976 |
| Small inline only: 2 KB smallest first, none, expand 32 KB | 0.0999 | 0.1165 | 0.1470 | 0.1584 | 0.1792 | 0.1980 |
| Fixed inline 52 KiB per slot, none, expand 32 KB | 0.0970 | 0.0995 | 0.1111 | 0.1317 | 0.1893 | 0.2252 |
| **Now: `InlineSlot` 52 KiB, `InlineBudget` 0, expand 32 KB, gap 52 KiB, none** | **0.0970** | **0.0995** | **0.1104** | **0.1285** | **0.1699** | **0.1932** |
| Now, es100 | 0.0968 | 0.1004 | 0.1158 | 0.1417 | | |

¹ `ExpandCost` 2 KB at 10k and 100k, 32 KB at 400k and 1M.

GETs, KB and rounds per search, random IDs:

| Shape | 100k: GET · KB · rounds | 1M: GET · KB · rounds |
|---|---|---|
| Before (es100, 2 KB, 2 KB, 4 KB) | 5.28 · 19.2 · 1.89 | |
| Pure index | 5.26 · 17.3 · 1.94 | 5.45 · 59.6 · 1.94 |
| Now | 3.51 · 40.2 · 1.30 | 5.18 · 66.2 · 1.90 |

### 5.2 Inline lists: `InlineSlot` and `InlineBudget`

**Fixed budget, smallest lists first** (the rule before the study, `none`,
gap 52 KiB; `ExpandCost` 2 KB for the rows 0 and 2 KB at 10k and 100k, 32 KB
everywhere else):

| `InlineBudget` | 10k random | 100k dense | 100k random | 1M random |
|---|---|---|---|---|
| 0 | 0.1547 | 0.1620 | 0.1675 | 0.1976 |
| 2 KB | 0.1170 | | 0.1608 | 0.1980 |
| 16 KB | 0.0995 | 0.1135 | 0.1416 | |
| 52 KiB | 0.0995 | 0.1111 | 0.1317 | 0.2252 |
| 64 KiB | | 0.1100 | 0.1321 | |
| 128 KiB | | 0.1100 | 0.1339 | |
| 256 KiB | | 0.1100 | 0.1336 | |

A fixed budget fails at one end. Up to 100k, 52 KiB holds the whole slot, so
the search ends in round 1. At 1M no slot fits, so the budget holds the
smallest lists, which are the rarest words: few searches ask for them, and
every search of the slot pays their bytes.

**The model behind `InlineSlot`.** Take a slot whose db lists take D bytes,
and assume a search asks for a word in proportion to its list. Holding a list
of L bytes inline costs L bytes to every search of the slot, and saves the
searches of that word (a share L/D) one GET plus nothing in bytes (they would
read it from the `.db` anyway). Holding it pays when D − L < `BytesPerGET`. So
a slot under `BytesPerGET` keeps everything, and a bigger one keeps nearly
nothing. Implemented as: all of them if D ≤ `InlineSlot`, otherwise the
smallest within `InlineBudget`.

**`InlineSlot`** (`InlineBudget` 2 KB, expand 32 KB, none):

| `InlineSlot` | 10k dense | 10k random | 100k dense | 100k random |
|---|---|---|---|---|
| 0 | 0.0999 | 0.1165 | 0.1470 | 0.1584 |
| 26 KiB | 0.0970 | 0.0995 | 0.1119 | 0.1364 |
| **52 KiB** | **0.0970** | **0.0995** | **0.1105** | **0.1287** |
| 105 KiB | 0.0970 | 0.0995 | 0.1100 | 0.1298 |

**`InlineBudget` for slots over `InlineSlot`** (`InlineSlot` 52 KiB, expand
32 KB, none):

| `InlineBudget` | 100k dense | 100k random | 400k random | 1M random |
|---|---|---|---|---|
| **0** | **0.1104** | **0.1285** | **0.1699** | **0.1932** |
| 2 KB | 0.1105 | 0.1287 | 0.1708 | 0.1946 |
| 8 KB | | | 0.1735 | 0.1985 |

At 10k it makes no difference: every slot fits.

### 5.3 Expansions: `ExpandCost` (and `MinSplit`)

`T = max(MinSplit, √(ExpandCost · D))`: a word whose list passes T gets a head
in the `.idx` and combination blocks in the `.db`.

| `ExpandCost` | 10k random ² | 100k random ² | 400k random ² | 1M random ³ | 400k, rule ⁴ | 1M, rule ⁴ |
|---|---|---|---|---|---|---|
| 2 KB | 0.1040 | | 0.1908 | 0.1971 | | |
| 8 KB | | | | 0.1982 | 0.1715 | 0.1947 |
| **32 KB** | 0.0995 | 0.1317 | **0.1893** | 0.1980 | **0.1699** | **0.1932** |
| 128 KB | 0.0995 | 0.1306 | 0.1922 | 0.2145 | | |
| 512 KB | | 0.1306 | 0.1950 | | | |

² fixed inline 52 KiB. ³ inline 2 KB. ⁴ `InlineSlot` 52 KiB, `InlineBudget` 0.

32 KB is within 1 % of the best at every size. 2 KB is 4.5 % worse at 10k;
128 KB is 8 % worse at 1M, where the lists grow too long to read whole.

Dollars of one compaction, by `ExpandCost` (fewer expansions, fewer
combination blocks to rewrite):

| `ExpandCost` | 400k random | 1M random |
|---|---|---|
| 2 KB | 0.000419 | 0.000708 |
| 8 KB | 0.000336 | 0.000613 |
| 32 KB | 0.000223 | 0.000511 |
| 128 KB | 0.000151 | 0.000314 |

### 5.4 Range merging: `CoalesceGap`

Two `.db` ranges closer than the gap are read in one GET. A hole costs less to
read than a GET while it is under `BytesPerGET`.

| Shape | 4 KB | 52 KiB |
|---|---|---|
| 10k random, inline 2 KB, expand 2 KB, none | 0.1187 | 0.1170 |
| 100k random, inline 32 KiB, expand 8 KB, none | 0.1402 | 0.1387 |

1–2 % when lists live in the `.db`; the same when every slot is inline.

### 5.5 Grouping: `none` against `es100`

`es100` packs the 1024 slots into 100 objects cut by Spanish word frequency
(`internal/layout/grouping.go`); `none` is one object per slot. With the new
parameters:

| | 10k dense | 10k random | 100k dense | 100k random |
|---|---|---|---|---|
| none, per search | 0.0970 | 0.0995 | 0.1104 | 0.1285 |
| es100, per search | 0.0968 | 0.1004 | 0.1158 | 0.1417 |
| none, build ($) | 0.000411 | 0.000412 | 0.000440 | 0.000455 |
| es100, build ($) | 0.000119 | 0.000119 | 0.000148 | 0.000162 |
| compaction ($), both | 0.000080 | 0.000080 | 0.000096 | 0.000104 |

- Both make the same GETs: two words of a query rarely share a group.
- es100 reads more bytes, because each `.idx` holds several slots, each with
  its own inline lists: +10 % per search at 100k, nothing at 10k.
- es100 builds cost a third: 102 PUTs instead of 361.
- Break-even at 100k random: $0.000293 more per build for none against
  $1.32 × 10⁻⁸ saved per search, so about **22 000 searches per full build**.
  Compactions cost the same.

**es100 stays the default.** none is the better choice for partitions that
serve more than about 22k searches between full builds; es100 for many small
partitions with few searches.

## 6. Answers

1. **More GETs for fewer bytes does not pay.** The pure index reads less than
   half the bytes and costs 30–58 % more at 10k–100k, and 2 % more at 1M.
   Fewer GETs per search (and fewer rounds) are worth tens of KB more read.
2. **The shape now in the defaults** (`bucket.DefaultParams`,
   `query.DefaultOptions`):
   - `InlineSlot` = `BytesPerGET` (52 KiB): a slot whose lists fit keeps them
     all in the `.idx`;
   - `InlineBudget` = 0: a bigger slot keeps none;
   - `ExpandCost` = `MinSplit` = 32 KB;
   - `CoalesceGap` = `BytesPerGET` (52 KiB);
   - grouping es100 (unchanged; see §5.5).

   It is the cheapest shape measured at every size, or within 1 % of it.

**What it looks like on disk.** At 10k every slot fits in `InlineSlot`, so
there is no `.db` at all: the `.idx` total is 181 KB with dense IDs and
380 KB with random ones. At 100k random the `.idx` total is 2.4 MB and the
`.db` 1.4 MB; at 1M the `.db` holds most of the bytes. The size of the files
is not the cost: a search reads one `.idx` per word, whole (0.5–50 KB), and
nothing else when its slots fit.

## 7. Effect on the benchmarks

| Benchmark | Before | Now |
|---|---|---|
| `TestBench`, names, row IDs (es100, 10k) | 0.1167 | 0.1145 |
| `TestBench`, names + groups, row IDs | 0.1197 | 0.1151 |
| `TestBench`, names, random IDs | about 0.145 (4.6 GETs, 11.5 KB) | 0.1194 |
| `TestEval` 100k random, none: 1 · 2 · 3 words | 0.0924 · 0.1611 · 0.2467 | 0.0800 · 0.1279 · 0.1778 |
| `TestEval` 100k random: one compaction ($) | 0.000160 | 0.000037 |

Quality does not change: nDCG@10 0.93 on both bases, no query changes its
top 10, and the engine still equals the brute-force ranking on all 3 000
`TestEval` queries at 10k and 100k. At 100k random, 8 of the 500 typo queries
are now flagged approximate by `PrefixBudget`; they find the same document as
with no budget.

The benchmarks were also changed to fit this cost model: they report dollars,
measure only cold searches, and run the searches in parallel (`TestBench`
7 s → 2 s, `TestEval` more than 10 min → under 2 min). The `TestBench` gate
is the average dollars per search (+5 %) instead of GETs and KB apart.
`blob.Counting` counts DELETEs apart from the writes, since they are free.

## 8. Open questions

- The grouping default (§5.5).
- es100 gives each slot of a group its own `InlineSlot`. A budget per group
  instead could remove most of its extra bytes; not measured.
- A real catalogue of 1M distinct names (growing vocabulary) and a real query
  log would test the two assumptions of §3.
- Lambda duration per round is not priced. Fewer rounds (1.30 against 1.89 at
  100k) also make the new shape faster.
