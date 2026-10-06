# Genix Search

Genix Search is a ranked, typo-tolerant search engine for **short text**:
records of a few words that people find by typing part of them. It stores
**no text**. Every word becomes a compact integer key, and a query answers
with a ranked list of document ids. Your own database keeps the rows.

The repository holds two engines. They share one idea (compact keys, lossy by
design, ranked results) and differ in where the index lives:

| | **Genix Search S3** | **Genix Search Server** |
|---|---|---|
| Directory | [`genix-s3/`](genix-s3) | [`server/`](server) |
| Language | Go library (+ `gxs` CLI) | Rust daemon |
| Index lives in | S3 Express One Zone directory bucket | RocksDB on local disk |
| You talk to it | Go API (`genix.Client`) | TCP text protocol |
| Optimized for | Fewest S3 requests and bytes read per query | Low latency on one box |
| Key format | 5-bit letters (`uint5`) | Spanish letter-pair signatures (moving to `uint5`) |
| Status | Current focus | Working; being migrated |

For anything new, start with **Genix Search S3**. That is where the design
work goes.

---

# Genix Search S3

A search index that lives in an S3 Express One Zone directory bucket, with no
server. A Lambda function, a container or a CLI opens the bucket, searches
and exits. Any number of processes can read and write the same index at once.

## The problem

Most search engines assume a long-lived server with a warm disk or a large
heap. On object storage the costs are different:

* every GET has a price and a round trip of several milliseconds, so the
  number of **requests**, and how many of them must wait for each other, is
  what makes a query slow or expensive;
* a cold process (a new Lambda, a new container) has nothing cached;
* there is no lock service, no multi-range GET and no transaction across
  objects. All you get is conditional writes on single objects.

Genix S3 is built to minimize the **S3 bill of a cold search**, then of the
writes. On S3 Express One Zone (us-east-1) a GET costs $0.00003 per 1000 and
every byte read $0.0006 per GB, so one GET costs as much as reading 52 KiB.
A search reads a few KB, so its bill is mostly GETs: reading tens of KB more
to save one GET pays. Every trade between requests and bytes is decided in
dollars (`internal/pricing`). File size and CPU are not goals.

It also keeps three constraints that rule out the usual shortcuts:

* **It assumes nothing about the data.** Document ids are arbitrary `uint32`
  values (never assumed dense), no table is derived from the corpus, and the
  only global statistic is the document count.
* **It stores no text.** A word is reduced to a key, and the index never holds
  the original string.
* **It has no directory, manifest or coordinator.** A query computes the name
  of every object it needs from the query words alone.

What that buys, measured on a 10 000-document collection
([Measurements](#measurements)):

| Operation | Cost |
|---|---|
| Cold search | about 3.7 GETs and 7 KB, in 1 round trip: $0.11 per million |
| Collection under 100 documents | 1 GET, no index at all |
| Write, any batch size | 1 GET + 1 PUT |

## The design

### 1. A word is up to 8 units of 5 bits

The alphabet has 32 symbols, so each one fits in 5 bits (a *unit*):

| Unit | Meaning |
|---|---|
| 0 | end of word |
| 1–26 | `a`–`z` |
| 27–29 | a number of 1, 2 or 3 value units follows |
| 30–31 | the two configurable extra letters (`ñ`, for instance) |

A word is its first **8 units**. A number is its value in binary, not its
text (`007` is 7, `50` is `27 1 18`, since 50 = 1·32 + 18).

The same word has two representations: a fixed-width integer while the
engine works with it, and a stream of units in the files.

**In memory: a `uint64` key.** The 8 units are packed with the first letter in
the highest bits, followed by zeros once the word ends. Bits 40–41 hold a
*space* (word, group word or group signature), so the three kinds never mix.

```
polo  →  p=16 o=15 l=12 o=15, then four 0 units
         bits 39–35  34–30  29–25  24–20  19–0
              10000  01111  01100  01111  0…0     = 566 280 323 072
```

**In S3: a stream of 5-bit units, never the `uint64`.** No file stores a word
as 8 bytes. After a fixed byte header, words live in bit streams (LSB-first)
that write a word's units and only what cannot be inferred:

| Where | How a word is written | `polo` costs |
|---|---|---|
| `.idx` dictionary | Keys are sorted and grouped in runs by slot (the first 2 units). A run header (`u16`) names the slot once. Each key then writes a 1-unit header (how many units follow the slot, how many it shares with the previous key) and only its new units. | 2 units (10 bits) if the previous key is `pollo` |
| `p.log`, `tokens` | Per token, a 1-unit length, then its units. The 2-bit tails of 8-unit words go at the end of the document. | 5 units (25 bits) |
| `.db` lists | No word at all. The `.idx` says where a word's list starts and how long it is. A list holds document-key gaps, 4-bit positions and 2-bit tails. | 0 |
| `.db` combination blocks | The co-occurring words as a shared-prefix stream: 3 bits shared, 3 bits length, then the new units. | a few units |

Document keys work the same way. In memory a document is a 40-bit integer
(`origin << 32 | ID`). In `p.log` and `tokens` it takes 5 bytes. In a list it
becomes the gap from the previous document, a few bits wide. (The one raw
`uint64` word key on disk is the list of expanded words in `tokens`, which
only the compaction reads.)

Why this split:

* **In memory, integer order is alphabetical order.** Comparing two keys as
  integers sorts them the way the words sort, a shorter word first
  (`pol` < `polo` < `polos`). A prefix (`rodam`) is one contiguous range of
  keys, from the prefix followed by 0s to the prefix followed by 31s. Prefix
  search is a range scan, and lookups are binary searches with no string
  handling.
* **In S3, bytes are what a query pays for.** The files keep the same order,
  so a key only spells what it does not share with its neighbour, and each
  field of a bucket is as wide as its largest value in that bucket. A decoded `.idx` is cached, so the
  translation from units to `uint64` happens once per bucket version, not
  once per query.
* **Eight letters are enough for short text.** Two more bits, the *tail*,
  hash the rest of a long word, so `chocolate`, `chocolatera` and
  `chocolatina` stay apart. They are stored per posting, not in the key. Same
  key and tail is an exact match; same key and another tail still matches,
  with fewer points. In the benchmark, the 8-letter cut does not cost quality:
  the same rules over whole words score nDCG 0.91, and over 8-letter keys
  0.93 (a typo or a different ending after the 8th letter still matches).

The price is that the index is **lossy**: results are candidates ranked by
evidence, never confirmed text equality. And changing how text becomes keys
means reprocessing the collection from its source.

### 2. The query names the objects it reads

The first two units of a key are its **slot**: 1024 slots. A fixed table in
the code groups the slots into **100 buckets** of about the same weight in
Spanish (by distinct words and by occurrences of frequent words; numbers get
their own 21 buckets). Each bucket is a pair of objects:

```
genix/<collection>/<partition>/p.log         change log; every query reads it
genix/<collection>/<partition>/<g>.idx       dictionary and small lists of bucket g
genix/<collection>/<partition>/<g>.db        large lists of bucket g, read by byte range
genix/<collection>/<partition>/tokens        the state the index was built from (compaction only)
genix/<collection>/<partition>/compact.lease mutual exclusion between compactions
```

A query turns each word into its key, the key into its slot, and the slot
into an object name. There is no directory, no manifest and no statistics
file to read first, so **the first round trip already fetches the data**.

The table is tuned for Spanish but never derived from your corpus, so it
cannot go stale and needs no coordination. Why 100: fewer, larger buckets
make every cold read bigger; more, smaller ones mean more objects per query
and per compaction. One bucket per slot is also available (`GroupingNone`).

### 3. Two tiers per bucket: `.idx` read whole, `.db` read by range

The `.idx` holds the bucket's keys and, for each key, its posting count, its
first document and the shape of its list. Lists of one posting live entirely
in those fields. A slot whose other lists fit in 52 KiB (what one GET costs
in bytes) keeps them all **inline**, so its searches never need the `.db`; a
bigger slot keeps none, because its searches mostly ask for its big lists and
every search of the slot would pay the bytes. The `.idx` is read whole, and a
long-lived process can cache it (a Lambda starts cold every time).

Everything larger goes to the `.db`, which is never read whole. **List
offsets are not stored**: each list's size is computable from its fields, so
an offset is the running sum of the sizes before it, and the lists of a
prefix are one contiguous stretch. The reader asks for exactly those bytes.

Lists are bit streams: document-key gaps at the narrowest width that fits (or
nibble varints, whichever is smaller), then a 4-bit position per posting,
then 2-bit tails only when the list needs them.

### 4. Common words are pre-answered: heads and combinations

A word in thousands of documents would make every query that uses it read a
long list. Instead it is **expanded**: its `.idx` entry carries

* a **head**: the 64 documents a query of that word alone ranks first, with
  their postings, so the head scores exactly; and
* **combinations**: the pre-joined lists of that word with every word that
  co-occurs with it, grouped into blocks in the `.db`.

A one-word query for a common word reads its head and nothing else. A
two-word query reads one combination block instead of two long lists.

**When to expand** is a cost balance. An expansion makes the `.idx` larger,
and every query of the bucket pays that. Only queries of that word save. With
`D` the bytes of all lists of a slot, a word expands when its list exceeds
`T = max(32 KB, √(32 KB · D))`, with hysteresis so it doesn't flip on every
write. At most `D/T` words expand, so the `.idx` grows by about `T`, and the
largest list a query still reads from the `.db` is also about `T`.

**Why the result stays exact.** A document with two or more query words is
seen whole, through a full list or a combination. A document with only one
query word, an expanded one, is either in that word's head or scores no
higher than the head's last entry. The engine checks that bound after
ranking. If the last head entry could still reach the requested page, it
reads the full list in a third round (rare: deep pages, or a head whose
documents almost all have other query words). The tests compare every result
with a brute-force scoring of the whole collection, and they must be equal.

### 5. Writes append to a log; compaction folds them in

Every write is one batch appended to `p.log` **at its exact current size**
(`WriteOffsetBytes` on S3 Express). If another writer appended first, the PUT
fails, and the writer re-reads and retries. Writers are serialized without
locks, and a batch costs 1 GET + 1 PUT whatever its size.

Every query reads `p.log` anyway (round 1, in parallel with the `.idx`
files), so the **pending operations** are applied on top of the index at
query time: a deleted or replaced document is removed from the candidates,
and a new one is matched directly against the query. A write is searchable
immediately.

When the appends pass 4 KB, the append that crosses the threshold also asks
for a **compaction**. The compaction runs under a lease, rebuilds the index
in memory from the `tokens` state plus the pending operations, and **rewrites
only the buckets whose content hash changed**. Then it commits by rewriting
`p.log` with a new base, using one conditional PUT. Compaction runs inline on
AWS Lambda (which freezes goroutines after the response) and in a background
goroutine elsewhere. You can also hand it to your own queue.

Below 100 documents an index costs more than it saves. The partition then
keeps all its documents inside `p.log`, and a search is that single GET.
Compaction moves a partition in and out of this mode by its live document
count.

### 6. Consistency without a manifest

S3 offers no transaction across objects, so the order of writes and a few
numbers in the `p.log` base carry the consistency:

* **Buckets are written before the log that announces them**, and each `.db`
  before its `.idx`. An `.idx` newer than the log is harmless, because the
  pending operations applied on top are idempotent.
* The dangerous case is **a bucket older than its log**. The base records an
  **epoch** (raised by every full rewrite), the last sequence number folded
  in, and a bitmap of the buckets the last compaction rewrote. The reader
  validates every `.idx` against it from the header alone. A stale bucket
  restarts the search.
* A bucket two compactions old would slip through that check, so it is
  prevented by timing. Compactions are at least 1 s apart, and a reader
  discards a first round slower than 500 ms.
* `.db` ranges must come back with the ETag the `.idx` recorded.
* Replaced objects are deleted **one compaction later**, once no query can
  still be using them.

The same base also validates the `.idx` cache. If the log moved by exactly
one compaction, only the buckets it touched are re-read, and a warm search
stays a single request.

## Anatomy of a search

1. **Parse.** Normalize (fold accents, collapse repeated letters, drop
   Spanish stopwords), one term per distinct word, at most 16. The last word
   is open (`rodam` finds `Rodamiento`), and `foo*` makes a word an explicit
   prefix. The query cannot know which words name a group, so every run of 1
   to 4 words also becomes a *signature* term. It lives in the slot of the
   run's first word, which the query reads anyway, so it costs no request.
2. **Round 1**, in parallel: `p.log` and every needed `.idx` not in the
   cache. When all of them are cached, the `.db` ranges are fetched
   speculatively in the same round.
3. **Plan.** For every term, choose the keys to read: the exact key, the
   prefix range, near words (singular and plural), group words, and typos if
   asked. The `.db` bytes these expansions may read are capped at 32 KB per
   term, and the answer is flagged approximate if a key is left out. Common
   words go through their heads. Pairs of terms go through combination
   blocks.
4. **Round 2**, in parallel: the `.db` byte ranges, sorted and merged when
   less than 52 KiB apart (S3 has no multi-range GET, and a smaller hole
   costs less to read than a GET). Often there is nothing to read.
5. **Assemble, overlay, rank.** Collect postings per document, apply the
   pending operations from `p.log`, score, keep the top `offset + limit`.
6. **Verify.** Check every head bound. In the rare case a bound fails, a
   third round reads the full list and ranks again.

## Ranking

Short text breaks the assumptions of BM25. Term frequency is almost always 1,
length normalization mostly penalizes the one extra word, and prefixes, typos
and plurals need partial credit, which BM25 does not give. Genix scores with
**points**, chosen so a human can follow why one result beats another:

* **letters matched**: 5 for the first, 3 for each further one up to the 8th,
  so a longer word typed in full outweighs a short one;
* **the whole word**: +7 when key and tail both match. Prefix, stem, plural
  and typo matches get the letter points they earned and less;
* **where the word is**: +2 for the document's first word and +1 for its
  second, and +2 to the first word the user typed (it usually leads the
  intent: "cafe organico" looks for coffee);
* **proximity**: +6 for two query words adjacent in order, +3 reversed, +2
  one word apart;
* **rarity**: `round(2 · ln(N / df))`, capped at 8, so a rare word helps
  without letting a single rare word outweigh a missing one;
* **the group**: 4 per signature segment matched (+2 if it is the document's
  whole group), or 4 per word found only in the group. When the query names
  something and its group, the group's words count once, by the text or by
  the group, whichever earns more. The group never lowers a score.

Ties are broken by document key, so results are deterministic. The ranking
has one structural rule: the score of a term alone may depend only on its
match kind and first position. That is what lets a head be sorted in
advance.

"juan carlos centro civico" (without rarity):

| Document | Text | Group | Pair | Position | Total |
|---|---|---|---|---|---|
| Juan Carlos @ centro civico | 21 + 27 | 10 | 6 | 2 + 1 + 2 | **69** |
| "Centro Civico" | 27 + 27 | — | 6 | 2 + 1 | **63** |
| Juan Carlos @ los condes | 21 + 27 | — | 6 | 2 + 1 + 2 | **59** |
| Juan @ centro civico norte | 21 | 8 | — | 2 + 2 | **33** |
| Pedro @ centro civico | — | 10 | — | — | **10** |

## What it handles

* **Typing as you go.** The last word is a prefix; a number prefix `50`
  matches 50, 500–509 and 5000–5099 as key ranges.
* **Singular and plural.** `rodamientos` finds `Rodamiento` and `panes` finds
  `pan`: keys one letter or `es` apart, in the same slot, at no extra request.
* **Typos**, opt-in (`Request.Fuzzy`): 1 edit for words of 4–7 letters, 2 for
  8 or more, searched only inside the word's own slot.
* **Accents and repeated letters.** `á` → `a`, `buenooo` → `bueno` (`ll` is
  kept: `pollo` is not `polo`). `ñ` folds to `n` unless it is configured as
  one of the two extra letters. `sin` is not a stopword: "sin gluten" is not
  "con gluten".
* **One-letter words.** `D` in "Vitamina D" would be a huge list of its own,
  so it is stored as a continuation of the 2 nearest words before it, inside
  their slots: `vitamina d` and `d vitamina` both find it.
* **Origins.** A document is `(origin uint8, ID uint32)`, so several tables
  share one index and every hit says which table it comes from.
* **Groups.** A document may belong to a group, a parent text many documents
  share (`Doc{Text: "Juan Carlos", Group: "centro civico"}`). A query that
  names the group ranks its members higher.
* **Partitions.** One `genix.Client` serves every partition (per tenant,
  project or dataset) of every collection in a bucket. A partition is created
  by its first write.

## What it is not

* **Not a text store.** It returns ids and scores, never text.
* **Not exact.** 8 letters and 2 bits per word. Two words that agree on all
  of that are the same word to the index.
* **Not a full-text engine.** No phrases, filters, field weights or long
  documents.
* **Not multilingual.** Letters are `a`–`z` (plus two extra letters),
  digits and separators; other scripts count as separators. The bucket table
  and the stopwords are tuned for Spanish.

## Use it from Go

```go
cfg, _ := config.LoadDefaultConfig(ctx)
store := s3blob.New(s3.NewFromConfig(cfg), "genix--use1-az4--x-s3")
c, _ := genix.NewClient(store, genix.ClientOptions{Extra: [2]rune{'ñ', 0}})

// write: one GET and one PUT, whatever the batch size
c.Apply(ctx, "piezas", "taller-12",
    []genix.Doc{
        {OriginID: 1, ID: 7, Text: "Rodamiento de bolas 6204", Group: "rodamientos"},
        {OriginID: 1, ID: 8, Text: "Perno hexagonal M8 zincado"},
    },
    []genix.Ref{{OriginID: 1, ID: 9}}, // deletes
)

// search
resp, _ := c.Search(ctx, "piezas", "taller-12", genix.Request{Query: "rodamiento 6204", Limit: 20})
for _, h := range resp.Hits {
    fmt.Println(h.OriginID, h.ID, h.Score)
}
// resp.Stats: rounds, requests, bytes, warm or cold, approximate or exact

// reprocess a partition from your database (also how you change normalization)
c.Rebuild(ctx, "piezas", "taller-12", source)
```

On a directory bucket the append is native. On a general-purpose bucket it is
emulated with a read and an `If-Match` PUT (it works, at a higher cost).
`NewFSStore` gives the same semantics over a local directory, for tests and
the CLI.

## Try it without S3

```sh
cd genix-s3
go test -race ./...
go run ./cmd/gxs build -csv ../scripts/perf/data/productos.csv
go run ./cmd/gxs query polo spiderman rojo
```

`gxs` also has `put`, `del`, `compact`, `log` and `inspect` (dumps a bucket:
keys, expansions, heads). Integration tests against a real bucket:

```sh
GENIX_S3_TEST_BUCKET=<bucket> go test ./s3blob
```

## Measurements

The benchmark is 10 010 product names from a real catalogue, with their 315
categories as groups, and 225 fixed queries (exact, partial, typing, typos,
accents, brand-like words, numbers, plurals, name + group, group only, and
queries that must return nothing) with graded relevance judgments. The
reference is BM25 on SQLite FTS5.

| | S@1 | MRR | nDCG@10 |
|---|---|---|---|
| Genix S3, names only | 0.75 | 0.82 | 0.93 |
| BM25, names only | 0.61 | 0.71 | 0.85 |
| Genix S3, names + category groups | 0.78 | 0.86 | 0.93 |
| BM25, names + category groups | 0.69 | 0.79 | 0.90 |

* **Where it wins.** It matches BM25 on exact, partial and accented queries,
  and beats it on typing as you go, typos, numbers, plurals and brand-like
  words. A trigram BM25 also tolerates typos, but about 0.17 of its top 10
  are false hits, against 0.02–0.03.
* **Where it loses.** Queries that are only a group name (nDCG 0.59 against
  0.80): a group is worth less than a word of the name, so documents that
  mention the words outrank the group's members.
* **Cost.** Every search is cold (as in a fresh Lambda): 3.7 GETs, 7 KB and
  1 round on average, $0.11 per million searches; p95 5 GETs and 12 KB.
* **Random ids cost a little more.** With random `uint32` ids instead of row
  numbers a posting takes about 30 bits instead of 9, so the `.idx` holds
  more bytes (15.7 KB read per search instead of 7.1), but the GETs do not
  change: $0.12 per million. The quality does not change.
* **At scale** (the catalogue copied under new random ids): $0.13 per million
  searches at 100k documents, $0.17 at 400k and $0.19 at 1M. Taking the
  lists out of the `.idx` to read fewer bytes costs 30–58 % more at 10k–100k:
  each GET saved is worth 52 KiB.

The benchmark doubles as a regression gate. A change that lowers global nDCG
or MRR by more than 0.01, or raises the average dollars per search by more
than 5 %, fails (`GXS_BENCH=1 go test ./internal/query -run 'TestBench$'`).
`TestCostSweep` measures the shapes of the index against each other.

## Limitations

* **Typos in the first two letters are not corrected.** They would move the
  word to another slot, and searching other slots is what a query avoids.
* **A group is worth less than a word of the name** (see above).
* **No prefix heads or three-word combinations yet.** A short prefix of a
  common stem, or three common words together, reads more than it should.
* **Write cost grows with the largest buckets.** Every `.db` that changes is
  rewritten whole, and the largest buckets change in almost every
  compaction.
* **The compaction lease is not renewed.** A compaction longer than 2 min can
  lose it. The next compaction recovers.

## Code map

```
genix              public API: Client, Indexer, Searcher
internal/key5      keys, numbers, continuations, group signatures, edit distance
internal/normalize text → tokens; the normalizer ID every file is stamped with
internal/layout    slot → bucket table, object names
internal/postings  list, combination and head codecs
internal/bucket    .idx/.db formats and the index builder
internal/plog      p.log format and the conditional append
internal/collection  write side: Apply, Compact, Rebuild, lease
internal/query     read side: parse, plan, rounds, assemble, score, cache
s3blob             S3 store
cmd/gxs            CLI
```

The read side never imports the write side; they meet only in the file
formats. [`genix-s3/INTERNALS.md`](genix-s3/INTERNALS.md) is the full
specification: byte layouts, every algorithm, the ranking constants, the
consistency rules, parameters and known limitations.

---

# Genix Search Server

A standalone Rust daemon that keeps the index in RocksDB on local disk and
speaks a line-based protocol over TCP. It takes short UTF-8 text keyed by
client-supplied `int32` ids and answers `QUERY` with a ranked `key|score`
list. Use it when you want a search process next to your application and low
latency on one machine, rather than an index in object storage.

> **Direction:** the Server is moving to the same 5-bit (`uint5`) keys as
> Genix Search S3. Today it encodes each word as a deterministic Spanish
> letter-pair signature and indexes two-word pair signatures, which collide by
> design. Until the migration, its behaviour and index format are those in
> [`INNER_WORKINGS.md`](server/INNER_WORKINGS.md). They will change with the
> migration, and existing data will need to be re-ingested.

## What it is today

* **Pair-first index.** Every word is a letter-pair signature, and numeric
  runs are quantized into one byte. Two-word pair signatures are the main
  retrieval primitive.
* **Direct `i32` keys.** `PUSHI` / `POPI` take a 32-bit id from the client,
  with no id translation and no string interning.
* **Ranked output.** `QUERY` returns `key|score …` by descending score,
  combining single-word, adjacent-pair, wrap-pair and prefix matches.
* **Prefix queries.** A trailing `*` (`tequila sunr*`) expands to compatible
  signatures.
* **Compact storage.** Postings are `i32` arrays, and reverse refs are
  fixed-width per document, so a delete is one RocksDB read.

## Install

Prebuilt, fully static Linux binaries (x86_64, generic arm64, and arm64 tuned
for Ampere Altra / Oracle Cloud `VM.Standard.A1.Flex`) are attached to every
[release](https://github.com/ivanjoz/genix-search/releases):

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install.sh | sh
```

Or straight into a hardened systemd service (needs only Python 3):

```sh
curl -fsSL https://raw.githubusercontent.com/ivanjoz/genix-search/main/scripts/install_service.py \
    | sudo python3 - install --bind 0.0.0.0:1491 --auth-password '…'
```

See [`PACKAGING.md`](server/PACKAGING.md) for the asset table and local release builds.

## Quick start

```sh
cd server
cargo build --release
./target/release/genixsearch -c ./genixsearch.cfg
```

Connect a TCP client to `[::1]:1491`:

```text
START ingest SecretPassword
PUSHI products default 1 "tequila sunrise"
OK
PUSHI products default 2 "agua mineral 100lts"
OK
QUIT
```

```text
START search SecretPassword
QUERY products default "tequila sunrise" LIMIT(10)
PENDING Bt2m2gYa
EVENT QUERY Bt2m2gYa 1|50
```

[`PROTOCOL.md`](server/PROTOCOL.md) has the full command set. A scripted perf rig
(`./scripts/perf/run_perf.sh`) drives it with 50k Spanish product names.

---

# Documentation

**Genix Search S3**

* [`genix-s3/INTERNALS.md`](genix-s3/INTERNALS.md): the specification: keys,
  file layouts, write and read paths, matching, ranking, consistency,
  parameters, tests and tools.

**Genix Search Server**

* [`PROTOCOL.md`](server/PROTOCOL.md): channel commands, syntax, responses.
* [`CONFIGURATION.md`](server/CONFIGURATION.md): every `genixsearch.cfg` knob.
* [`INNER_WORKINGS.md`](server/INNER_WORKINGS.md): index, ingest and query walkthrough.
* [`PACKAGING.md`](server/PACKAGING.md): release assets and how to cut a release.

# License

Mozilla Public License v2.0. See [`LICENSE.md`](LICENSE.md).
