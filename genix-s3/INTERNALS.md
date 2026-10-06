# genix-s3 internals

The specification of genix-s3: what it stores, in which byte layouts, and the
algorithms that write and read it. The code is the reference implementation;
where this document and the code disagree, one of them has a bug.

## Contents

1. [Overview](#1-overview)
2. [Vocabulary](#2-vocabulary)
3. [Text normalization](#3-text-normalization)
4. [Keys](#4-keys)
5. [Encoding rules](#5-encoding-rules)
6. [Token stream](#6-token-stream)
7. [List codecs](#7-list-codecs)
8. [Objects of a partition](#8-objects-of-a-partition)
9. [`p.log`](#9-plog)
10. [`.idx`](#10-idx)
11. [`.db`](#11-db)
12. [`tokens`](#12-tokens)
13. [`compact.lease`](#13-compactlease)
14. [Building the index](#14-building-the-index)
15. [Write path](#15-write-path)
16. [Read path](#16-read-path)
17. [Matching](#17-matching)
18. [Ranking](#18-ranking)
19. [Consistency](#19-consistency)
20. [Parameters](#20-parameters)
21. [Public API](#21-public-api)
22. [Code map](#22-code-map)
23. [Tests and tools](#23-tests-and-tools)
24. [Known limitations](#24-known-limitations)
25. [Measurements and rationale](#25-measurements-and-rationale)

---

## 1. Overview

genix-s3 is a search index kept in an S3 Express One Zone directory bucket. It
answers a text query with a ranked list of document keys; the caller looks
the rows up in its own database.

**What it optimizes, in order:**

1. S3 requests: GETs per query, PUTs and POSTs per write;
2. bytes read per query.

File size and CPU are not goals: any CPU work that saves a request or bytes
is worth it.

**What it never assumes:** anything about the corpus. Document IDs are
arbitrary `uint32` values within each origin (never assumed dense), no table
is derived from the data, and no global statistics are kept besides the
document count.

**What it stores:** no text. A word is its first 8 letters in 5-bit units
plus 2 bits about the rest; a number is its binary value; a document is a
40-bit key. Changing how text becomes keys therefore means reprocessing the
collection from its source system.

**Typical cost of a search:**

| Search | GETs | Rounds |
|---|---|---|
| Cold, 1 word whose list or head is in the `.idx` | 2 (`p.log`, `.idx`) | 1 |
| Cold, 1 word whose list is in the `.db` | 3 | 2 |
| Cold, N words | 1 + distinct `.idx` + `.db` ranges | 2 |
| Warm (`.idx` cached) | 1 + `.db` ranges | 1 |
| Collection under 100 documents | 1 (`p.log`) | 1 |

---

## 2. Vocabulary

| Term | Meaning |
|---|---|
| **Collection** | A named index (`productos`). |
| **Partition** | An independent index of a collection, with its own objects (one per tenant, say). Default `default`. |
| **Document** | `(origin u8, ID u32)`, a text, and optionally a group. |
| **Origin** | The table a document comes from. The same ID in two origins is two documents. It never changes, is returned with every hit, takes no part in the score, and is not a filter. |
| **Document key** | `origin << 32 \| ID`, 40 bits. Everything in the index is sorted by it. |
| **Group** | A parent text many documents share (`"Juan Carlos"` lives in `"centro civico"`). It adds to the score of a search that names it, below the text. |
| **Token** | A normalized word or number of a document, with its position. |
| **Key** | The 5-bit form of a token (§4). |
| **Tail** | 2 bits that tell apart words sharing their first 8 letters. |
| **Posting** | One occurrence of a key in a document: `(document key, position, tail)`. |
| **List** | The postings of one key, sorted by `(document key, position)`. |
| **DF** | The distinct documents of a list. |
| **Slot** | The first two units of a key: 1024 slots. |
| **Bucket group** | A set of contiguous slots stored as one `.idx`/`.db` pair. Called "bucket" in the code. |
| **Expanded word** | A word with a large list: it gets a head and combinations. |
| **Head** | The first 64 documents of an expanded word, in the order a query of that word alone ranks them. |
| **Combination** | The lists of two words over the documents that have both. |
| **Seq** | The sequence number of a change in `p.log`. |
| **Epoch** | Raised by every full rewrite of the index; buckets of different epochs never mix. |

---

## 3. Text normalization

`internal/normalize`. The same function runs on documents and on queries.

`Normalize(text)`:

1. Each character is mapped:
   - `a`–`z` stay; `A`–`Z` are lowercased; digits stay;
   - the two configured **extra letters** (`Config.Extra`) become letters of
     their own (bytes `{` and `|` internally, which sort after `z`);
   - Spanish accented vowels fold to the plain vowel (`á à ä â` → `a`, also
     uppercase); `ñ` → `n` unless `ñ` is an extra letter;
   - anything else is a separator.
2. A change between letters and digits ends a token: `"50ml"` → `50`, `ml`.
   A token is either all letters or all digits.
3. A run of the same letter is cut to one (`"buenooo"` → `bueno`), except
   `l`, which keeps two (`pollo` ≠ `polo`). Digits are never cut.

`Tokens(text)` is `Normalize` minus the Spanish stopwords, unless
`KeepStopwords` is set:

```
a al con de del desde el en es esa ese eso esos esta esto fue ha la las le les
lo los me mi mis ni no nos o para por que se ser si son soy su sus tu y
```

`sin` is not a stopword: "sin gluten" and "con gluten" are different products.

**Extra letters:** at most two, lowercase, distinct, and none of `a`–`z`,
digits, whitespace, control characters or `*`.

**NormID** = CRC32C(`Version` u16 ‖ extra[0] u32 ‖ extra[1] u32 [‖ 1 if
`KeepStopwords`]). Every file stores it, and every reader rejects a file with
another NormID. `Version` (currently 4) must be bumped whenever the output of
`Tokens` changes for any input; collections must then be rebuilt from their
source. `testdata/normalize_golden.cb` records the output over a catalogue, so
a change shows up as a reviewed diff (`go test ./internal/normalize -update`).

---

## 4. Keys

`internal/key5`.

### 4.1 Alphabet

| Unit | Meaning |
|---|---|
| 0 | End of word. Inside a key, the mark of a continuation (§4.6). |
| 1–26 | `a`–`z` |
| 27, 28, 29 | `NUM_1`, `NUM_2`, `NUM_3`: a number of 1, 2 or 3 value units follows |
| 30, 31 | The two extra letters |

### 4.2 Key layout

A key is a `uint64`:

```
bits 35–39  unit 0
bits 30–34  unit 1
...
bits  0–4   unit 7          unused units are 0
bits 40–41  space           0 word, 1 group word, 2 signature
```

Comparing two keys as integers compares the space, then unit by unit, a
shorter word before a longer one that extends it. That is the order of every
key list of the index. A prefix of n units is the contiguous range
`[k with units n.. set to 0, k with units n.. set to 31]` (`Key.PrefixRange`).

`Key.Len()` is the number of units: for a number `n + 1`; otherwise the index
of the last nonzero unit, plus one.

**Slot** = `unit0 << 5 | unit1` (`Key.Slot`). The name of a slot is two
characters of `0abcdefghijklmnopqrstuvwxyz12345` (unit 0 = `0`, 1–26 =
`a`–`z`, 27–31 = `1`–`5`): `camisa` → `ca`, the number 50 → `2a`.

### 4.3 Words and tails

A word's key is its first 8 letters. Its **tail** is:

- 0 for a word of up to 8 letters;
- `1 + CRC32C(letters 9 and on) mod 3` for a longer one.

The tail tells apart most words that share their first 8 letters
(`chocolate`, `chocolatera`, `chocolatina` share `chocolat`). Every posting
stores the tail of its word. Same key and same tail is an **exact** match;
same key and another tail is a **stem** match (it still matches, with fewer
points). A typo after the 8th letter does not change the key.

### 4.4 Numbers

A number is its value, not its text: `007` is 7. The key is `NUM_n` followed
by n units of the value in binary, most significant first, with the smallest
n that fits:

| Key | Values |
|---|---|
| `NUM_1` + 1 unit | 0–31 |
| `NUM_2` + 2 units | 32–1023 |
| `NUM_3` + 3 units | 1024–32767 |

Values above 32767 keep their low 15 bits (on purpose: saturating would put
every barcode in one key). Number keys sort by value.

**Number prefix:** an open number term `50` matches the values whose decimal
form starts with `50`: 50, 500–509, 5000–5099 (ranges stop at 32767;
`NumberPrefixRanges`). Each range is a contiguous range of keys. A term
starting with `0` matches only itself, and one of more than 5 digits matches
only its value.

### 4.5 Positions

A token's position is its index in the document's token stream (§6),
saturated at 15 (4 bits). Group tokens go after the text tokens, and the
continuations of a letter take consecutive positions. Ranking uses only the
first and second positions and the distance between two words, so 15 is
enough.

### 4.6 Continuations: one-letter words

A one-letter word (`M` in "Polo rojo M") has no key of its own: every key of
a letter would be one of 26 enormous lists. Instead it is stored as a
**continuation** of the 2 nearest distinct **host** words before it
(`FromTokens`):

- a host is a word of the text of 2 letters or more; numbers and other
  letters are not hosts;
- a repeated host counts at its most recent occurrence;
- the nearest host goes first; the continuations take consecutive positions
  from the letter's, and later words shift;
- with no host before it, the letter is dropped.

The key of a continuation is the host's first 6 units, unit 0 as the mark, and
the letter (`Continuation`): it lives in the host's slot, right after the host
(`talla` < `talla›m` < `tallaa`). Two hosts that share their first 6 letters
share the continuation.

"Polo rojo M" stores `polo`, `rojo`, `rojo›m`, `polo›m`. "Polo rojo azul M"
stores `azul›m` and `rojo›m`.

### 4.7 Groups: group words and signatures

The group text is normalized like the text, then becomes (`GroupTokens`):

- **group words:** every word of 2 letters or more and every number, each with
  its own key, in space 1. They have no tail. They let a query find a group by
  one of its words or by prefix.
- **the signature:** one key in space 2 that identifies the group:
  - one segment of 2 units per word, up to 4 words (`Segments`);
  - the first word gives its 1st and 2nd letters, so the signature lives in
    that word's slot; the others their 1st and 3rd;
  - a 2-letter word gives both letters; a 1-letter word gives unit 0 and the
    letter; a number gives the first 2 units of its key;
  - 1-letter words at the start are skipped;
  - **a one-word group's signature is that word's key** (its first 8
    letters), so it does not collide with every word sharing 2 letters.

| Group | Signature |
|---|---|
| centro civico | `cecv` |
| centro civico norte | `cecvnr` |
| condes vuelan alto | `coveat` |
| Las Condes (`las` is a stopword) | `condes` |

Spaces keep these keys apart from words: a word prefix range never includes a
group word or a signature, because the space is in the high bits.

**Group word cap:** only the first `GroupWordDocs` (100) members of each group
(in document key order) keep their group words in the index; every member
keeps the signature. A group is identified by the sequence of its group-word
keys and signature. Documents still in `p.log` keep all their group words.

---

## 5. Encoding rules

- **Raw bytes.** Fixed headers, then sections whose offsets come from sizes.
  No self-describing messages, no per-field tags. colbin is used only for the
  wire API (§21), never in a stored file.
- Integers are **little-endian**.
- Bit streams are **LSB-first**: the first value sits in the lowest bits of
  the first byte (`internal/bits`). A stream is padded to the byte only where
  stated.
- Integrity: **CRC32C** (Castagnoli).
- Every file starts with a 4-byte magic and a `Format uint16`. A reader
  rejects an unknown `Format`; changing a layout means bumping it and
  rebuilding.
- **Nibble varint:** groups of 4 bits, low first; bits 0–2 are value, bit 3
  means "another nibble follows". 0–7 take 1 nibble, 8–63 take 2.

| File | Magic | Format |
|---|---|---|
| `p.log` | `GXL1` | 3 |
| `.idx` | `GXI1` | 4 |
| `.db` | `GXD1` | 4 |
| `tokens` | `GXT1` | 3 |
| `compact.lease` | `GXC1` | — |

A document key on disk outside bit streams is 5 bytes: `ID u32 | origin u8`.

---

## 6. Token stream

What `p.log` and `tokens` store per document (`AppendTokens`, `ReadTokens`),
in 5-bit units, LSB-first:

```
per token, in position order:
  [mark]      1 unit, only for group tokens: 30 = group word, 31 = signature
  header      1 unit: 1..8 = a word (or signature) of that many units;
                      27..29 = NUM_1..NUM_3 (the header is the number's first unit)
  body        the units (for a number, its n value units)
End           unit 0
tails         2 bits per text word of exactly 8 units, in order (0 if it had 8 letters)
padding       to the byte
```

Rules a reader enforces: unit 0 appears inside a word only as the
continuation mark (second-to-last unit, key of 4 units or more, space 0); a
word never contains `NUM_n`; a signature's first and last units are nonzero.

---

## 7. List codecs

`internal/postings`.

### 7.1 A word's list

A list of `Count` postings sorted by `(document key, position)`. A document
with the word twice has two postings. The list is a bit stream, **not**
self-delimiting: its size comes from the dictionary fields (§10.3).

```
gaps       Count − 1 key differences (the first key is in the dictionary)
positions  Count × 4 bits
tails      Count × 2 bits, only if the list's HasTail flag is set
```

A list of one posting writes nothing: its key, position and tail live in the
dictionary.

**Gap codecs.** A list is a run of IDs per origin; the jump between runs is
huge (≈ Δorigin · 2³²). The builder computes the three forms and keeps the
smallest:

- **Codec 0, plain:** every gap in `W` bits, `W` the width of the largest gap.
- **Codec 0 with escapes:** `W` is the width of the largest gap inside a run,
  plus one. The all-ones value of `W` bits is an **escape**, followed by two
  nibble varints: the origins skipped (`Δorigin − 1`) and the ID in the new
  origin. With a single origin there are no escapes and it costs the plain
  form.
- **Codec 1:** every gap as a nibble varint.

**Shape and `V`.** The dictionary records `Count`, the first key and `V`:

```
Count = 1:  V = pos | tail << 4
Count > 1:  V = HasTail | Codec << 1 | X << 2
            Codec 0: X = W | escapeNibbles << 6   (escapeNibbles = total nibbles after the escapes)
            Codec 1: X = total nibbles of the gaps
```

The size in bits is then computable: Codec 0 is `(Count − 1)·W +
4·escapeNibbles`, Codec 1 is `4·X`, plus `4·Count` for positions, plus
`2·Count` with tails. `X` is at least 4 bits wide, so `V` always has room for
a position and a tail.

### 7.2 Split keys

Keys that are not in order (the first key of every word, head entries) are
written as `origin − baseOrigin` in `WO` bits and `ID − baseID` in `WID` bits,
with the bases the smallest origin and smallest ID of the set. The IDs of
every origin share one range. With one origin, `WO` = 0.

### 7.3 Combination columns

Two lists (the expanded word and a co-occurring word) over the same
documents. Self-delimiting, decoded front to back:

```
docs        nibble varint     documents
first       nibble varint     first document key − block base
codec       1 bit             0: escapes (1 bit) | W (6 bits) | W bits per gap
                              1: a nibble varint per gap
per column (2):
  dup       1 bit             some document has the word more than once
  tail      1 bit
  [counts]  with dup: per document, its postings − 1 (nibble varint)
  positions 4 bits per posting
  [tails]   with tail: 2 bits per posting
```

### 7.4 Head entries

Per entry, in head order (not key order): the split key (§7.2), its postings
− 1 (nibble varint), a 4-bit position per posting, and with the group's tail
flag a 2-bit tail per posting. Every posting of the word in the document is
kept, so a reader scores the entry exactly.

---

## 8. Objects of a partition

```
<root><collection>/<partition>/p.log          change log; every query reads it
<root><collection>/<partition>/<g>.idx        index of bucket group g
<root><collection>/<partition>/<g>.db         lists of g that do not fit in its .idx (only if any)
<root><collection>/<partition>/tokens         document → tokens state; only the compaction reads it
<root><collection>/<partition>/compact.lease  mutual exclusion between compactions
```

`<root>` defaults to `genix/`.

**Grouping** (`internal/layout`): how the 1024 slots are grouped into
objects. The table is fixed in the code; it is never derived from the corpus,
so a query computes the object name of a word from the word itself.

- **`GroupingES100`** (default, ID 1): 100 groups of contiguous slots, cut so
  that they weigh about the same in Spanish (half by distinct words, half by
  occurrences of the 50 000 most frequent words; numbers weighted 1/(n+1), 20 %
  of the total). 79 groups of letters and 21 of numbers. A group of one slot
  is named after it (`ca`, `co`, `pa`); a run is named `<first>-<last>`
  (`vo-y0`, `ya-z5`). At most 200 objects.
- **`GroupingNone`** (ID 0): one group per slot, named after it. Smallest
  `.idx` per cold read, up to 2048 objects.

The writers choose the grouping; readers follow the one recorded in the
`p.log` base. All writers of a partition must agree: a compaction with
another grouping rewrites every bucket.

A partition with fewer than `MinIndexDocs` (100) documents has no `.idx`,
`.db` or `tokens`: its documents live in `p.log` (§9.3).

---

## 9. `p.log`

`internal/plog`.

### 9.1 Base (176 bytes)

```
0    magic "GXL1"
4    Format     u16 = 3
6    flags      u8      bit 0 = LogOnly
7    grouping   u8      the grouping ID of the index
8    NormID     u32
12   Epoch      u32
16   LastSeq    u64     the ops ≤ LastSeq are in the index (or in the Docs frame)
24   PrevSeq    u64     the LastSeq of the base this one replaced
32   UnixMs     i64     when it was written
40   Docs       u32     live documents as of LastSeq
44   Touched    [128]B  bit g = the compaction that wrote this base rewrote group g
172  CRC32C     u32     of bytes 0..172
```

### 9.2 Frames

```
frame := tag u8 | len u32 | crc32c u32 (of body) | body[len]

tag 1  Batch    FirstSeq u64 | n u16 | n × op
                op := key (ID u32 | origin u8) | flags u8 (bit 0 = delete) | tokLen u16 | tokens[tokLen]
                the seq of op i is FirstSeq + i; a delete has tokLen 0
tag 2  Compact  TargetSeq u64 | UnixMs i64        a compaction request
tag 3  Docs     n u32 | n × (key | tokLen u16 | tokens)   sorted by key; only right after a LogOnly base
```

The appends since the last compaction are the bytes after the base and the
`Docs` frame. A reader stops at the first frame that is cut or fails its CRC
(a torn append); a writer refuses to append after a torn tail.

**Pending ops:** the last op of every document with `seq > LastSeq`.

### 9.3 Collections without an index

A `LogOnly` base is followed by a `Docs` frame with every document as of
`LastSeq`, then normal appends. A search reads `p.log` only (1 request) and
matches every document directly (§16.9). The compaction moves the partition
in and out of this mode by its live document count.

---

## 10. `.idx`

`internal/bucket`. One per bucket group that has keys. Its name comes from
the group, which comes from the query, so no directory is needed.

### 10.1 Header (64 bytes)

```
0    magic "GXI1"
4    Format     u16 = 4
6    ETagLen    u16     length of the .db ETag that follows the header
8    NormID     u32
12   Epoch      u32
16   Seq        u64     written by the compaction up to this seq
24   Group      u16     the group of slots (the slot, without grouping)
26   Grouping   u8
27   reserved   u8
28   Keys       u32     keys of the object; 0 = tombstone
32   DBSize     u32     size of the .db, 0 = no .db
36   DBCombos   u32     byte of the .db where the combinations zone starts
40   KeysLen | FieldsLen | HeadsLen | ExpLen | InlineLen    5 × u32
60   CRC32C     u32     of bytes 0..60 and of everything after the header
```

Then: `ETag[ETagLen]` (the ETag S3 returned when the `.db` was written), and
the five sections in order.

### 10.2 Keys section

```
n u16
n × run:  space << 14 | slot  u16  |  keys u32        ascending: by space, then slot
per run, its keys (bit stream, each run continuing the previous one):
  per key: header 1 unit = L·(L+1)/2 + s, then L − s units
           L = units after the slot (0..6), s = units shared with the previous key of the run
```

Every slot of a run must belong to the group. The header fits one 5-bit unit
because there are 28 valid `(s, L)` pairs.

### 10.3 Fields section

```
wCount u8 | wDup u8 | wFirst u8 | wX u8 | wOrigin u8 | base origin u8 | 0 u16 | base ID u32
then per key, in key order, in bits:
  count[wCount] | dup[wDup] | first key (split: origin[wOrigin], ID[wFirst]) | V[2 + wX] | loc[2]
```

- `count`: postings; `dup = count − DF`.
- `V`: §7.1.
- `loc`:

| `loc` | Where the list is |
|---|---|
| 0 field | One posting, entirely in the fields (`count` = 1). |
| 1 inline | In the Inline section. |
| 2 db | In the words zone of the `.db`. |
| 3 expanded | In the words zone of the `.db`, and the word has an Exp record. |

**List offsets are not stored.** A list starts where the previous list with
the same `loc` ends: inline lists from bit 0 of the Inline section, `.db`
lists (loc 2 and 3) from bit 256 of the `.db` (right after its header).
Lists are packed bit after bit, not byte-aligned. Since keys are sorted, the
lists of a prefix are a contiguous stretch of the words zone.

### 10.4 Heads section

Reserved for heads of prefixes (heads of letter nodes such as `cam`). Always
empty today.

### 10.5 Exp section: expansions

One record per expanded word, in key order:

```
word u32 | size u32                      index of the key in this .idx; bytes of the rest
groups u8 | p u8 | wLen u8 | 0 u8 | n u32
groups × head group (below)
n directory keys (any-bucket key stream, below), padded to the byte
n × block length [wLen bits]             bytes of each combination block in the .db
```

**Head group:**

```
tail u8 | flags u8 (bit 0 = tails stored) | wID u8 | base origin u8 | m u16 | wOrigin u8 | 0 u8
| total u32 | base ID u32 | size u32 | m head entries (§7.4) in size bytes
```

- The first group has `tail = 255`: every document of the word. If the
  word's postings have 2 tails or more, one more group per tail follows, so
  an exact query (one tail) and a stem query (all tails) both have an exact
  top.
- **Order:** by `min(first position of the word with that tail, 2)`, then by
  document key. That is the score order of a query of that word alone (first
  word, second word, the rest). A group word's or signature's head is ordered
  by key alone, since their score does not depend on position.
- `m ≤ HeadSize` (64); `total` is the documents of the group, so a reader
  knows whether the head is complete.

**Combination directory:** the `n` keys are the first `p` units of the
co-occurring keys (co-keys) of the word; each names one block of the `.db`
holding the combinations with every co-key that starts that way. `p` is 8
(one block per co-key) unless the directory would exceed `ComboDictBudget`
(2 KB); then the largest `p` that fits, down to 2. Block offsets are the
running sum of the lengths from `DBCombos`, in the order of the records and
their directory entries.

**Any-bucket key stream** (directory keys and block keys):

```
1 bit: some keys are not words
  if set: count of group words (nibble varint) | count of signatures (nibble varint)
          — they are the last keys, in that order
per key: s (3 bits: units shared with the previous key) | L − 1 (3 bits) | the L − s new units
```

**Where a pair lives:** the combination of `{a, k}` is stored under the
expanded word; if both are expanded, under the smaller key. A reader knows
after round 1, having read both `.idx`.

### 10.6 Inline section

The lists with `loc = 1`, packed in key order. Per slot, the smallest
non-expanded lists go inline while they fit `InlineBudget` (2 KB).

---

## 11. `.db`

```
Header (32 bytes):
0    magic "GXD1"
4    Format     u16 = 4
6    Group      u16
8    NormID     u32
12   Epoch      u32
16   Seq        u64
24   CRC32C     u32     of the body
28   Grouping   u8
29   reserved   3 B
Words zone          from byte 32: the lists with loc 2 or 3, packed in bits in key order
Combinations zone   from DBCombos: the blocks, in Exp record order and directory order
```

**Combination block** (read whole):

```
n u16 | base origin u8 | 0 u8 | base ID u32
n co-keys (any-bucket key stream), padded to the byte
per co-key, in order: the columns (§7.3) of the expanded word and of the co-key
```

A query never reads the header, only ranges. Consistency comes from the ETag:
each range must come back with the `.db` ETag the `.idx` recorded. The header
and its CRC are checked by tests and the CLI.

---

## 12. `tokens`

The state the index was built from. Only the compaction reads it.

```
Header (48 bytes):
0    magic "GXT1"
4    Format     u16 = 3
8    NormID     u32
12   Epoch      u32
16   Seq        u64     the ops ≤ Seq are in the documents
24   docs       u32
28   chunks     u32
32   expanded   u32
36   entries    u32     bucket and garbage entries
40   Grouping   u8
44   CRC32C     u32     of bytes 0..44 and the tables
Tables:
  chunks × (first key u64 | Off u32 | Len u32 | CRC32C u32)
  expanded × key u64                              the expanded words, sorted
  entries × (group u16 | flags u8 | grouping u8 | hash u64)
            flags: 1 = garbage (grouping = its grouping), 2 = the bucket has a .db
Chunks of 1000 documents, sorted by key:
  per document: key (ID u32 | origin u8) | tokLen u16 | tokens
```

The bucket entries hold the content hash of every bucket written, so an
incremental compaction knows which ones changed. The garbage entries are
objects to delete at the next compaction (§15.2).

---

## 13. `compact.lease`

24 bytes: `"GXC1" | owner u64 | expiry ms i64 | epoch u32`. `epoch` is the
highest epoch the holder may have written buckets in. Only the lease holder
writes buckets.

---

## 14. Building the index

`bucket.Build` builds **every** bucket of the partition in memory from the
`document → tokens` state; the compaction then writes only the buckets whose
content changed.

1. Cap group words (§4.7).
2. Collect each key's postings. Documents are visited in key order and tokens
   in position order, so every list comes out sorted.
3. Compute each list's shape (§7.1) and DF.
4. **Expansion threshold**, per slot: with D the bytes of all lists of the
   slot, `T = max(MinSplit, √(ExpandCost · D))`, both 2 KB. A word expands if
   its list exceeds T, or if it was expanded before and its list still
   exceeds T/2 (hysteresis; the previous set is in `tokens`).

   Why the square root: an expansion adds about `ExpandCost` to the `.idx`,
   which every query of the bucket pays, while only the queries of that word
   save. At most D/T words expand, so the `.idx` grows by about T and the
   largest list a query reads from the `.db` is about T.
5. Per bucket group, in key order (space, then slot):
   - `loc`: field for one posting, expanded, otherwise db; then per slot the
     smallest db lists move inline within `InlineBudget`;
   - write the Keys, Fields and Inline sections and the words zone;
   - per expanded word: heads (§10.5) and combinations: for every document of
     the word, every other token anchored under it (§10.5) becomes a pair;
     co-keys are grouped by their first `p` units into blocks;
   - hash the content (FNV-64a of the group, key count, sections and `.db`
     body). The hash excludes epoch and seq.

---

## 15. Write path

### 15.1 Append

`Collection.Apply(puts, dels)` → `plog.Append`:

1. Each put is normalized (§3) and turned into its token stream: the text
   tokens, then the group tokens (§6). That is all that is stored.
2. GET `p.log`, parse it, build one `Batch` with the puts first and the
   deletes last (a document in both ends up deleted), seqs following the last
   one.
3. **Append at the exact current size** of the object: on S3 Express a
   `PutObject` with `WriteOffsetBytes`, which fails if the object grew in
   between. On failure: back off (random, doubling, scaled by the round trip
   of the lost attempt) and retry from step 2, up to 16 times. Writers are
   serialized without locks.
4. **Lazy creation:** if `p.log` does not exist and there is at least one put,
   it is created with `If-None-Match`: an empty `LogOnly` base plus the batch.
   Deletes alone on a missing collection write nothing.
5. **Compaction request:** if the appends since the base exceed
   `CompactBytes` (4 KB), or a `LogOnly` log reaches `MinIndexDocs` live
   documents, the same append carries a `Compact` frame and the result has
   `CompactRequested`. While a request is pending no other is written, unless
   it is older than `StaleCompact` (1 min): its compactor must have died.

Cost: 1 GET and 1 PUT per `Apply`, whatever the number of documents.

**Who compacts** (`Client`, `CompactMode`):

| Mode | Who |
|---|---|
| `CompactAuto` (default) | `Sync` on AWS Lambda (it freezes goroutines after the response), `Async` elsewhere |
| `CompactSync` | the `Apply` that asked, before returning |
| `CompactAsync` | a goroutine, one at a time per partition; a request arriving meanwhile makes it run once more |
| `CompactCallback` | the caller, notified through `OnCompact` (e.g. to enqueue it) |

### 15.2 Compaction

`Collection.Compact`:

1. **Lease.** Create `compact.lease` with `If-None-Match`. If it exists and has
   expired (`LeaseTimeout`, 2 min), take it over with `If-Match`: that is a
   **recovery**. If it has not expired: `ErrConflict`.
2. **Read** `p.log` and `tokens` in parallel. The current state is `tokens` if
   it is at or ahead of the base (epoch and seq), else the `Docs` frame of a
   `LogOnly` base; an indexed base with `tokens` missing or behind is an error
   (rebuild from the source). Apply the pending ops.
3. **Nothing to do** (no ops, no mode change, no leftovers, same grouping, no
   garbage, no recovery): release and return.
4. **Mode:**
   - fewer than `MinIndexDocs` live documents: rewrite `p.log` as `LogOnly`
     with a `Docs` frame; a previous index is dropped in a new epoch;
   - **full** rewrite in a new epoch if: recovery, the base was `LogOnly`,
     `tokens` missing or of another epoch, or another grouping;
   - otherwise **incremental**.
5. **Spacing:** wait until `CompactSpacing` (1 s) after the base's `UnixMs`,
   except for the first indexing of a collection that never had buckets.
6. A new epoch is `max(base, tokens, lease) + 1`, recorded in the lease
   (`If-Match`) **before** writing any bucket, so an epoch a previous holder may
   have half-written is never reused.
7. **Build** the index (§14).
8. **Write** the buckets whose hash differs from `tokens` (all, if full), in
   parallel: first `<g>.db` (to get its ETag), then `<g>.idx` with that ETag
   and `Seq = TargetSeq`. A group left without keys gets a **tombstone**
   `.idx` (`Keys = 0`) and its `.db` is deleted.
9. **Delete the garbage of the previous compaction:** its tombstones, and on a
   full rewrite the objects of the old epoch or grouping that the new index
   does not have. No query reads them anymore (§19).
10. **Write `tokens`:** the state, the expanded words, the hashes, and the new
    garbage.
11. **Commit:** rewrite `p.log` with `If-Match` on the version read in step 2:
    a new base (`LastSeq = TargetSeq`, `PrevSeq`, `Docs`, `Touched` = the
    groups written, tombstones included) followed by the ops and requests that
    arrived meanwhile. If a writer appended in between, re-read and retry the
    rebase.
12. **Release** the lease (delete it if still ours). If the compaction failed
    after writing a bucket, the lease is left expired instead, so the next
    compaction recovers with a full rewrite.

Cost: up to 2 PUTs per changed bucket group, plus `tokens` and `p.log`. Every
`.db` that changes is rewritten whole.

### 15.3 Rebuild, Drop, Init

- **`Rebuild(source)`** reprocesses a partition from the system of record; it
  is also how a partition moves to another normalizer or extra letters.
  1. Take the lease; if held, wait (250 ms, 500 ms, …) and retry, up to 4
     attempts.
  2. Note the log's last seq, S0, **before** reading the source (creating an
     empty log if missing).
  3. Read the source (`Source`, which may be called more than once) and do a
     full rewrite from it, or a `LogOnly` log under `MinIndexDocs`.
  4. Rebase `p.log` keeping the ops after S0.

  Writers append to `p.log` **after** their write to the source commits, so
  an op ≤ S0 is already in the source, and later ones stay pending on top;
  ops are idempotent. A log of another normalizer is replaced whole.
- **`Drop`** leaves an empty `LogOnly` log in a new epoch and deletes `tokens`,
  every `.idx` and `.db`.
- **`Init(docs)`** writes a `LogOnly` log with every document (`If-None-Match`;
  `ErrExists` if there is one) and compacts if there are `MinIndexDocs` or
  more.

---

## 16. Read path

`internal/query`, `Engine.Search`. An `Engine` serves one partition and is
safe for concurrent use.

### 16.1 Attempts

A search runs up to 8 **attempts**. An attempt that finds the index changed
under it (`errStale`) starts over after a random backoff below `25 ms << min(attempt, 5)`
(about 3 s in total, enough for a rebuild to finish writing). An attempt
that finds another grouping starts over at once.

### 16.2 Parse

`Parse` turns the query into **terms**:

1. Split on whitespace. `foo*` makes `foo` an **explicit prefix**: required,
   and matched only as a prefix. Only the first one counts; later ones are
   dropped. (`m*` is just `m`: a letter is never a prefix.)
2. Normalize each piece (§3); keep at most 16 words.
3. One term per **distinct** word or number: a word typed twice is one term.
4. Without an explicit prefix, the **last** word is **open** (`Auto`): it also
   matches as a prefix.
5. One-letter words move behind the others: each becomes a term matching its
   continuation (§4.6) of **every host word of the query** (any one suffices).
   With no host in the query, the letter is dropped. `m talla` is `talla m`;
   in `m tal`, `tal` is still open.
6. `Position` is the order of the terms; position 0 earns `QueryFirstWord`.
7. **Signatures:** the query cannot know which words are a group, so for every
   run of 1 to 4 consecutive words not starting with a letter, it adds a
   **virtual term**: the run's signature (§4.7), and which terms it covers. An
   open last word joins a multi-word run only if its segment is complete (3
   letters), and a one-word run with 2 letters or more; an open number never.
   Each signature lives in the slot of the run's first word, which the query
   reads anyway: **0 extra requests**.

### 16.3 Slots

The slots a search needs: each term's exact key (a letter: its continuations'
slots, i.e. the hosts'), and every slot its prefix ranges span. They map to
bucket groups through the grouping.

### 16.4 Round 1

In parallel:

- `p.log`;
- the `.idx` of every needed group that is not cached;
- if every `.idx` was cached: the `.db` ranges the cached plan needs (§16.6),
  speculatively. A warm search is a single round.

Then:

1. **Too slow:** a round 1 that read some `.idx` and took longer than
   `RoundLimit` (500 ms) is discarded (§19).
2. **`LogOnly` base:** answer from the log alone (§16.9).
3. **Another grouping** in the base: adopt it, clear the cache, start over.
4. **Validate** every `.idx` against the base by its header alone:

| The `.idx` | Group in `Touched` | Group not in `Touched` |
|---|---|---|
| missing (404) | stale | empty |
| epoch above the base | stale | stale |
| epoch below the base | stale | empty (garbage of a replaced index) |
| same epoch, `Seq < LastSeq` | stale | valid |
| same epoch, `Seq ≥ LastSeq` | valid | valid |

   A valid tombstone is an empty bucket. Stale starts the attempt over.
5. **Cache sync:** the cache holds decoded `.idx` valid as of `(epoch, seq)`:
   - same epoch and `LastSeq`: all valid;
   - same epoch and the base's `PrevSeq` equals the cached seq: valid except
     the groups in `Touched`, which are dropped and re-read (one more round);
   - a base older than the cache (a lagging replica): the cache is neither
     used nor filled;
   - anything else: cleared.

   The cache is bounded by `CacheBytes`; when full, a random half is dropped.

### 16.5 Plan

`planQuery` chooses, for every term, the dictionary words to read. A **pick**
is a word with the terms it serves (`use`: term and key kind).

| Kind | Which keys | Budget |
|---|---|---|
| exact | the term's key; a letter's continuations; a signature's key | none |
| prefix | open term of 2+ characters: every key in its prefix range (not continuations, not the exact key) | `PrefixBudget` |
| near | any word term: keys with the same letters plus or minus 1 letter at the end, or plus or minus `es` (§17.3) | `PrefixBudget` |
| group word | the term's key in space 1, and its prefix range if open | `PrefixBudget` |
| signature prefix | a signature of 2+ segments also matches longer signatures that start with it | `PrefixBudget` |
| fuzzy | only with `Request.Fuzzy`, only for a term with no exact, prefix or near key, never numbers or explicit prefixes: keys of the term's own slot within its edit budget (§17.4) | `PrefixBudget` |

**Budget:** `PrefixBudget` (32 KB) bounds the bytes of `.db` an expansion may
read. Lists in the fields or inline cost nothing, and expanded words are read
through their heads; only db lists of non-expanded words cost. Candidates go
most frequent first (fuzzy: nearest first). A term with no exact match always
gets its best key, even over the budget. If some key was left out, the
response is flagged `Approx`.

**DF of a term:** the sum of the DF of every key that matches it, from the
fields alone. No list is read for IDF.

**Full or headed:** a pick is **headed** (read through its head, no list
read) when it is an expanded word serving a single term, matched exactly or
as a prefix, or as a near word of fewer than 8 letters. Every other pick is
**full**: its whole list is read.

**Pairs:** for every two picks of different terms where at least one is
headed, the combination block of the pair, if the anchor's directory has one.

Why this is exact: any document with two query words or more is seen whole,
through a full list or a combination. A document with only one headed word is
either in that word's head or cannot rank above the head's last entry
(§16.8).

### 16.6 Round 2

The full lists in the `.db` and the combination blocks become byte ranges,
sorted, and merged when less than `CoalesceGap` (4 KB) apart, up to
`MaxRequest` (1 MB) per range: S3 has no multi-range GET. They are read in
parallel. Each range must come back with the ETag the `.idx` recorded and the
requested length; otherwise (or on a 404) the bucket was rewritten between
rounds and the attempt starts over. If everything is in the `.idx`, there is
no round 2.

### 16.7 Assemble

1. Every document of every full list, of every combination column, and of
   every head entry, with its postings per pick.
2. **Overlay:** every document with a pending op in `p.log` is removed; if the
   op is a put, its new tokens are matched against the terms directly
   (`matchTokens`), with the same rules.
3. Each posting resolves to a match kind (§17.1). Per term, a document keeps
   its best kind and the positions where it matched with that kind (for fuzzy
   and near: the nearest, earliest one).

### 16.8 Rank and verify (round 3)

Every candidate is scored (§18). A document whose explicit prefix term did
not match is dropped, as is any with score 0. The top `offset + limit` is
kept with a heap and sorted: **score descending, then document key
ascending**.

Then `shortHeads`: for every headed pick whose head is incomplete, the bound
is the head's last entry scored as a document with only that word. If that
bound would rank at or above result number `offset + limit` (or there are
fewer results), the word could be hiding a document that belongs in the
answer: its whole list is read (**round 3**), it becomes a full pick, and the
answer is assembled again. With the defaults this is rare (deep pages, or a
head whose documents almost all have other query words).

The result is identical to scoring every document of the collection by the
same rules. `TestMatchesBruteForce` and the eval check it.

**Rule for score changes:** a head is ordered by the score of its term alone,
which may depend only on the match kind and the first position (plus
constants per term). A bonus that depends on anything else breaks the heads,
and must be accounted for in `headBounds` too.

### 16.9 Collections without an index

`searchLog`: the documents of the `Docs` frame not pending, plus the pending
puts, form the dictionary. DF and fuzzy eligibility come from it
(`dictMatch`); every document is matched with `matchTokens` and ranked as
above. One request.

---

## 17. Matching

`Term.classifyKey` decides how a key matches a term; `resolve` turns it into
a match kind per posting, with the posting's tail.

### 17.1 Match kinds

Better kinds first; a document keeps the best per term.

| Kind | When |
|---|---|
| exact | same key, same tail |
| stem | same key, another tail (the word goes on past its 8th letter differently) |
| prefix | a key in the term's prefix range |
| longer | a near word with letters the term lacks (`panes` for `pan`) |
| shorter | a near word lacking letters of the term (`bolsa` for `bolsas`) |
| fuzzy | a typo (§17.4) |
| group word | only through a group word of the document's group |

### 17.2 Who matches what

- A signature term matches only signatures: its own key exactly, and with 2+
  segments longer signatures that start with it.
- A word term matches words of space 0 and, as a group word, its own key or
  prefix in space 1. Never signatures.
- A one-letter term matches only its continuations, and no other term
  matches a continuation: `talla›m` is neither a prefix nor a near word nor a
  typo of `talla`, and does not add to its DF.
- An explicit prefix matches only as a prefix (never exactly, near or fuzzy).

### 17.3 Near words (singular and plural)

A key is **near** a word term when:

- neither is a number, the term is not an explicit prefix, the key is a word
  (space 0);
- their lengths differ by 1 or 2 units, all the units of the shorter one are
  the first units of the longer one, and the shorter has at least 3 letters;
- a difference of 2 is allowed only when the longer one ends in `es`
  (`pan`/`panes`, `color`/`colores`; not `sal`/`salsa`, `bot`/`boton`);
- a shorter key must have fewer than 8 units (a key of 8 may be cut);
- a longer key of 8 units with a nonzero tail on a posting is not near (the
  word goes on further).

Near words are tried for every word term, open or not, found in the
dictionary or not, with or without `Fuzzy`. They are in the term's own slot:
no extra request. When a term has no exact key, near words count as found and
typos are not tried.

### 17.4 Typos

Only with `Request.Fuzzy`, and only for terms with no exact, prefix or near
key. Edit budget by the term's length: under 4 letters none, 4–7 letters 1,
8 or more 2. The distance is optimal string alignment (Levenshtein plus
adjacent transpositions) over 5-bit units, with all the query's letters (not
cut at 8). A key of 8 units may be a longer word cut short: it is compared
with the query's prefixes of `8 − budget` to `8 + budget` letters, keeping
the best (`chocoate` is 1 edit from `chocolat`). Typos are looked for only in
the term's own slot: **there are no typos in the first two letters**.

---

## 18. Ranking

`query/score.go`. Results are ordered by points, then by document key.
`Hit.Coverage` (terms covered by the text or the group) is reported but does
not order.

### 18.1 Constants

| Constant | Value | Earned by |
|---|---|---|
| `LetterFirst` | 5 | the first letter that matches |
| `LetterNext` | 3 | each further letter that matches, up to the 8th (a number counts its digits) |
| `WordFull` | 7 | the whole word: same key and same tail |
| `LetterExtra` | 3 | subtracted per letter of difference with a near word |
| `GroupSegment` | 4 | each segment of a matching signature |
| `GroupFull` | 2 | the signature is the document's whole signature |
| `GroupWord` | 4 | each term matched only by a group word |
| `PairAdjacent` | 6 | two text-matched terms adjacent, in query order |
| `PairAdjacentReversed` | 3 | adjacent, reversed |
| `PairNear` | 2 | one word apart |
| `DocFirstWord` | 2 | the term matched the document's first word |
| `DocSecondWord` | 1 | the term matched the document's second word |
| `QueryFirstWord` | 2 | the term is the first word the user typed (it usually leads the intent: "cafe organico" looks for coffee) |
| `IDFWeight`, `IDFMax` | 2, 8 | rarity: `round(2 · ln(N / df))`, at most 8 |

`N` is the base's document count, `df` the term's DF (§16.5). Any word in
fewer than about 1.8 % of the documents earns the maximum.

### 18.2 Points of a term matched by the text

With `n = min(letters of the term, 8)` and `letters(n) = 5 + 3·(n − 1)`:

| Match | Points |
|---|---|
| exact | `letters(n) + 7` ("juan" 21, "carlos" 27, "maximiliano" 33) |
| stem | `letters(n)` |
| prefix | `letters(typed letters)` ("carl" 14) |
| near | `letters(min(shared letters, 8)) − 3 · difference` ("bolsas" against "Bolsa": 17 − 3 = 14) |
| fuzzy | `letters(n − distance)` ("carlso" 17) |
| continuation (a letter) | `5 + 7` = 12 |

Plus `DocFirstWord`/`DocSecondWord` by the term's first matching position,
`QueryFirstWord` if it is the first term, and the IDF bonus.

**Pairs:** for every two text-matched terms, the closest pair of their
positions (preferring query order at equal distance) earns `PairAdjacent`,
`PairAdjacentReversed` or `PairNear`.

### 18.3 The group

- **Signature points:** `4 × segments of the query run`, `+2` when it is the
  document's whole signature ("centro civico" whole = 10; against "centro
  civico norte" = 8; a one-word group such as "condes" = 6). The document's
  best signature counts (at equal points, the one covering more terms).
- **Group word points:** 4 per term matched only through a group word. A term
  matched by the text earns nothing as a group word.
- The group adds `max(signature, group words)`.

**Counting the group's words once.** Let *alone* be the text points (terms
and pairs) ignoring the group, and *grouped* the text points of the terms
outside the best signature's run (and pairs with neither term in it):

- If the run covers **every** word of the query (the query is the group:
  "limpieza facial"), name and group both count:
  `score = alone + max(signature, group words)`. A member whose name also
  says it ranks first.
- Otherwise the query is a name plus a group, and the run's words count
  **once**, by the text or by the group, whichever earns more:
  `score = max(alone, grouped + max(signature, group words))`. For
  "hidratante cuidado corporal", "Crema corporal @ cuidado corporal" does not
  earn "corporal" twice, and "Crema hidratante @ cuidado corporal" ranks
  first.

The group never lowers a score.

### 18.4 Example

"juan carlos centro civico", without IDF:

| Document | Text | Group | Pair | Position | Total |
|---|---|---|---|---|---|
| Juan Carlos @ centro civico | 21 + 27 | 10 | 6 | 2 + 1 + 2 | **69** |
| "Centro Civico" | 27 + 27 | — | 6 | 2 + 1 | **63** |
| Juan Carlos @ los condes | 21 + 27 | — | 6 | 2 + 1 + 2 | **59** |
| Juan @ centro civico norte | 21 | 8 | — | 2 + 2 | **33** |
| Pedro @ centro civico | — | 10 | — | — | **10** |

---

## 19. Consistency

There are no locks, no generations and no manifest. The rules:

1. **Writers to `p.log`** are serialized by the append offset (§15.1).
2. **Compactions** are serialized by the lease. The commit is one `If-Match`
   PUT of `p.log`.
3. **Buckets are written before the log that announces them**, and each `.db`
   before its `.idx`. A reader that sees an `.idx` newer than its log is
   fine: the pending ops it applies on top are idempotent, so a document
   already in the bucket comes out the same.
4. **The dangerous case is a bucket older than its log.** A reader detects it
   with the base's epoch, `LastSeq` and `Touched` (§16.4): a group the last
   compaction rewrote must have `Seq ≥ LastSeq`.
5. **Two compactions back is undetectable** by rule 4, so it is prevented:
   compactions are at least `CompactSpacing` (1 s) apart, and a reader
   discards a round 1 longer than `RoundLimit` (500 ms). `RoundLimit` must stay
   below `CompactSpacing`.
6. **`.db` ranges** carry the object's ETag, which must equal the one in the
   `.idx`.
7. **Garbage is deleted one compaction later**, after at least
   `CompactSpacing`: no query can still be using an object it just validated.
   For the current base, those objects already read as empty (epoch below,
   group not touched).
8. **Epochs** go up on every full rewrite. Buckets of a higher epoch than the
   base are a rewrite in progress (stale); of a lower epoch, garbage.
9. **A crash mid-compaction:** written `.idx` newer than the log are valid
   (rule 3); the expired lease makes the next compaction recover with a full
   rewrite in a new epoch, so no half-written bucket is compared against a
   hash in `tokens` that no longer matches it.

---

## 20. Parameters

### Build (`bucket.Params`, through `collection.Options.Params`)

| Parameter | Default | |
|---|---|---|
| `MinSplit` | 2048 | minimum T |
| `ExpandCost` | 2048 | cost of an expansion in the `.idx` |
| `HeadSize` | 64 | entries of a head |
| `InlineBudget` | 2048 | bytes of inline lists per slot |
| `ComboDictBudget` | 2048 | combination directory size before grouping co-keys by prefix |
| `GroupWordDocs` | 100 | members per group that keep their group words |

A reader needs none of them: the `.idx` says what is expanded and where
everything is. Changing them only affects what is written from then on.

### Write (`collection.Options`)

| Parameter | Default | |
|---|---|---|
| `CompactBytes` | 4 KB | appends since the base that ask for a compaction |
| `MinIndexDocs` | 100 | below it, no index (1 always indexes) |
| `LeaseTimeout` | 2 min | an older lease can be taken over |
| `CompactSpacing` | 1 s | minimum time between compactions; must exceed `RoundLimit` |
| `StaleCompact` | 1 min | a pending request older than this is asked again |
| `Concurrency` | 32 | buckets written at a time |

### Read (`query.Options`, `genix.SearchOptions`)

| Parameter | Default | |
|---|---|---|
| `PrefixBudget` | 32 KB | `.db` bytes a prefix, near, group-word or typo expansion may read |
| `CoalesceGap` | 4 KB | largest hole merged into one range read |
| `MaxRequest` | 1 MB | largest merged range |
| `Concurrency` | 16 | requests in flight per round |
| `CacheBytes` | 64 MB (8 MB per partition under `Client`) | decoded `.idx` cache |
| `RoundLimit` | 500 ms | a slower cold round 1 is repeated |

### Fixed

| | Value |
|---|---|
| Letters per key | 8 |
| Positions | 4 bits, saturated at 15 |
| Signature segments | 4 |
| Query words | 16 |
| Search attempts | 8, backoff base 25 ms |
| Append attempts | 16 |
| `tokens` chunk | 1000 documents |

---

## 21. Public API

Package `genix`.

- **`Client`** (`NewClient(store, ClientOptions)`): writes and searches every
  partition of every collection in a store; keep one per process.
  - `Apply(ctx, collection, partition, puts []Doc, dels []Ref)`: one batch,
    one GET and one PUT; the first put creates the partition. Runs the
    compaction per `CompactMode`.
  - `Search(ctx, collection, partition, Request)`: a missing partition has no
    hits.
  - `Compact`, `Rebuild(source)`, `Drop`, `Wait` (for async compactions).
  - Keeps one search engine per partition, LRU-bounded by `MaxSearchers`
    (256).
- **`Indexer`** and **`Searcher`** (`NewIndexer`, `NewSearcher`, with a
  `Config`): the same for a single partition.
- **`Config`** / **`ClientOptions`**: `Root`, `Collection`, `Partition`,
  `Extra`, `KeepStopwords`, `MinIndexDocs`, `Grouping`. Writers and searchers
  of a collection must agree on `Extra` and `KeepStopwords` (the NormID checks
  it).
- **`Doc{OriginID, ID, Text, Group}`**, **`Ref{OriginID, ID}`**.
- **`Request{Query, Limit (default 10), Offset, Fuzzy}`**.
- **`Response{Seq, Hits, Stats}`**: `Seq` is the `LastSeq` of the index the
  answer comes from. `Hit{OriginID, ID, Coverage, Score}`. `Stats` reports
  rounds, requests, bytes (and of which `p.log` and `.idx`), candidates,
  pending ops, `Approx`, `LogOnly`, `Warm`, heads and pairs read, deep reads
  (round 3) and retries.
- **`Searcher.Serve(body)`**: a colbin `QueryRequest{Query, Limit, Offset,
  Fuzzy}` in, a colbin `QueryResponse{Seq, Hits[{ID, Coverage, Score,
  OriginID}], Approx}` out.
- **Stores:** `s3blob.New(client, bucket)` (S3; on a directory bucket the
  append is native, on a general purpose bucket it is emulated with a read and
  an `If-Match` put) and `NewFSStore(dir)` (a directory, same semantics, for
  tests and the CLI).

---

## 22. Code map

```
genix (genix.go, client.go)    public API
internal/collection            write side: Apply, Compact, Rebuild, Drop, Init, lease
internal/plog                  p.log format and Append
internal/tokens                tokens format
internal/bucket                .idx/.db formats, Decode, Build
internal/query                 Parse, plan, rounds, assemble, match, score, cache
internal/postings              list, column and head-entry codecs
internal/key5                  keys, numbers, continuations, signatures, key and token streams, edit distance
internal/normalize             text → tokens, NormID
internal/layout                object names, groupings
internal/docid                 the 40-bit document key
internal/bits                  LSB-first bit reader and writer
internal/parallel              bounded concurrency
internal/blob                  store interface, FS store, request counter
s3blob                         S3 store
cmd/gxs                        CLI
```

The read side never imports the write side, and vice versa; they meet in the
formats.

| To change | Where | Watch out |
|---|---|---|
| Ranking points | `query/score.go` | `headBounds` (§16.8); run the benchmark and `TestMatchesBruteForce` |
| What a word matches | `classifyKey`, `nearKey`, `resolve` (`score.go`), `planQuery` (`plan.go`) | the same rule must hold in `matchTokens` (overlay) and in the brute-force reference (`eval_test.go`) |
| Query parsing | `query/parse.go` | `querySlots` decides which objects are read |
| Normalization | `internal/normalize` | bump `Version`; collections need `Rebuild` |
| A file layout | `bucket`, `plog`, `tokens`, `postings`, `key5/stream.go` | bump that file's `Format`; indexes need `Rebuild` |
| Index shape (T, heads, inline) | `bucket.Params` | readers need no change |
| Bucket grouping | `internal/layout` | another grouping forces a full rewrite |

---

## 23. Tests and tools

| What | How |
|---|---|
| Unit tests | `go test ./...` (`-race` recommended) |
| Ranking equals brute force | `TestMatchesBruteForce` (`internal/query`) |
| Requests and rounds per search | `TestRoundsAndRequests`, `TestStaleBucket` |
| Cost at scale (requests, bytes, sizes) | `GXS_EVAL_CSV=../../../scripts/perf/data/productos.csv go test ./internal/query -run Eval -v`; vary with `GXS_EVAL_GROUPING`, `GXS_EVAL_ORIGINS`, `GXS_EVAL_TEXTGROUPS` |
| Effectiveness against BM25 | `GXS_BENCH=1 go test ./internal/query -run 'TestBench$' -v`; writes `testdata/bench/report.md` and fails on regressions against `testdata/bench/baseline.json` |
| Real S3 | `GENIX_S3_TEST_BUCKET=<bucket> go test ./s3blob` |
| Normalizer golden | `go test ./internal/normalize [-update]` |
| CPU benchmarks | `go test ./internal/query -bench .` |

**Benchmark.** 10 010 products of a real catalogue (`testdata/bench/productos.tsv`),
with their 315 categories as groups; 225 fixed queries in categories (exact,
partial, typing, typo, accents, brand, numbers, plural, name+group, group,
negative) with graded relevance judgments from a rule over the full text, and
two references, BM25 (SQLite FTS5) and trigram BM25. Base A indexes names,
base B names plus category groups. Metrics: S@1, S@10, MRR, nDCG@10, P@10,
false hits, plus cold and warm GETs and KB per query. The gate fails if global
nDCG or MRR drop more than 0.01, a category's more than 0.03, or cold GET/KB
p50 rise more than 5 %. The scripts that build the data and references are in
`scripts/bench/`; to accept a change, copy `results.json` over
`baseline.json`.

- *Data.* `queries.tsv`, `reference.tsv`, `qrels.tsv`, `productos.tsv`,
  `baseline.json`, `report.md` and `results.json` are versioned (`results.json`
  has no timings, so it is deterministic). The two local databases
  `bench/db/{names,groups}` are not; they are rebuilt when the corpus hash or
  the index-writing code changes, and can be read with
  `gxs query -root bench/db/groups -collection productos`. To regenerate the
  data: `prepare.py <xlsx>` (needs `openpyxl`; the catalogue is outside the
  repo), then `queries.py`, `queries_plural.py` (rewrites only q201–q225),
  `reference.py` (needs Python `sqlite3` with FTS5 and trigram) and
  `judge.py`. A query is never regenerated once it exists.
- *References.* R1 is BM25 and R2 an OR of trigrams; both drop the query
  stopwords and break ties by ID. In base B, R1 weighs the category 0.3. R3 is
  our own rules by brute force over all documents; it must equal the engine on
  every query (if not, use `TestEval`).
- *Metrics.* S@1, S@10 and MRR use only queries with a target; nDCG and P@10
  leave out the negatives. P@10 divides by 10, so its maximum is low (about
  0.2) and it is only for comparing runs; false hits divide by the number
  returned. The gate compares per base, on "all" and per category. Per-category
  cost is not gated: 10–25 queries are too noisy.
- *`-random-ids`* indexes `uint32` IDs drawn with a fixed seed (they cost more
  bytes), writes `*.random-ids.*` and compares only if
  `baseline.random-ids.json` exists.
- *Limits.* 10 010 documents is a small scale: cost at 100k needs `TestEval`.
  With 200–225 queries, a difference of 1–3 queries is noise, so the benchmark
  rejects regressions but cannot tune constants. The judge is lexical: no
  synonyms, and it does not order within a grade. It barely sees
  `QueryFirstWord` (queries follow the name's word order), and it has too few
  group queries to tune the group constants.

`blob.Counting` wraps any store and counts requests and bytes.

**CLI** (`go run ./cmd/gxs`): `build -csv FILE`, `query TERMS…`, `put REF TEXT…
[@ GROUP…]`, `del REF`, `compact`, `log`, `inspect [WORD]` (dumps a bucket:
keys, expansions, heads); common flags `-root`, `-collection`, `-partition`,
`-extra`, `-grouping`, `-keep-stopwords`, `-min-index-docs`.

---

## 24. Known limitations

- **Typos in the first two letters** are not corrected: typos are only looked
  for in the term's own slot.
- **A group is worth less than a name word.** A group-only query ranks
  documents that have the words in their name above members of the group
  whose name does not.
- **Heads of prefixes and combinations of 3 words** are not implemented
  (the `.idx` has a reserved section for the former).
- **A full rewrite that fails half way** leaves buckets of a higher epoch than
  the base; searches that need them fail ("the index kept changing") until
  another compaction recovers. Nothing triggers it if there are no more
  writes.
- **The lease is not renewed.** A compaction or rebuild longer than
  `LeaseTimeout` can lose it; it notices when it records the epoch (before
  writing) or at the commit (after writing buckets).
- **Write cost:** every `.db` that changes is rewritten whole, and the
  largest buckets change in almost every compaction. Idea, not implemented:
  make the `.db` append-only with S3 Express append; a compaction would append
  only the changed lists and the `.idx` would point to the new offsets, with
  ranges validated by size instead of ETag and a rewrite when the garbage
  passes half.
- **A failed full rewrite** could be recovered by the `Client` retrying the
  compaction after a failure, or by `Search` returning a typed error so the
  caller asks for one.
- **Typos in the first two letters** could be found by computing candidate
  slots from the query (transposition, deleted first letter, inserted letter,
  substituted second letter: about 80 slots at distance 1). The slots are in
  the cached dictionary, so checking them costs no request; postings would be
  read only for the keys that pass the distance check. The previous design
  did this through neighbouring slots (482 of 500 typo queries had both words
  in the top 1).
- **Numbers above 32 767** keep only their low 15 bits (§4.4): a catalogue
  with many SKUs or barcodes collides about 1 time in 32 768. There is no
  collision report.
- **Prefix heads** are deferred because the measurements showed no query
  needing them.
- **First-two-letter typos** score S@1 0.17 in the benchmark, against 0.89 for
  the other typos.

---

## 25. Measurements and rationale

Why the design is the way it is, with the numbers measured when it was built
(`productos.csv`, 10k and 100k products, `GroupingNone` unless said; the
eval and tests default to `GroupingNone` so numbers stay comparable).

**IDs.** A translation table from client IDs to dense numbers was rejected
(an object to read and keep). Random `uint32` IDs cost 29.8 bits per posting
against 9.4 for dense ones (10k; `p` 266 KB against 89 KB, bytes per query ×2.4–2.8),
close to the minimum `log2(2^32/df)`; dense IDs shrink by themselves through
the per-list width. With random IDs the query words land in different
buckets, so requests do not drop and `CoalesceGap` changes less than 0.1
request.

**Gap codecs (§7.1).** With random IDs the fixed width wins in 4 014 of 4 118
lists; with name-ordered IDs 42 % of gaps are 1 and nibbles win. Choosing per
list costs 1 bit per key. Rejected: 5-bit varint, byte varint, 9+5, and
colbin per list (216 KB against 125 KB: 5–7 bytes of structure per message,
and 42 % of lists hold one posting). Page CRCs on `.db` ranges were rejected
too: verifying 4 KB pages raised the bytes read from 1.0 KB to 10.9 KB per
query at 10k (7.2 to 18.0 KB at 100k), since a list is far smaller than a
page; the ETag does the job.

**Origins (§7.1).** With random IDs the bytes grow 5 % at 8 origins and 9 % at
50. With dense per-origin IDs the escapes cut the overhead from 14 % and
26 % to 7 % and 15 % (100k). With one origin the bytes are identical to
having none; without the escapes the 33-bit `W` would apply to every gap.

**Keys.**
- *8-letter cut:* 86.4 % of tokens and 77.6 % of distinct words fit (96.9 % of
  tokens in 10). 7 136 words give 6 753 keys; 302 keys merge 685 words, 240 of
  them the same word with another ending (free stemming, applied equally to
  the query) and 62 different roots (`chocolate`/`chocolatera`). The 2-bit
  tail separates 72 % of the 179 different-root pairs. If that is not enough,
  widen the tail to 3–4 bits, paid only by lists with `HasTail`.
- *Digits split from letters:* `500ml` must equal `500 ml`; only 0.65 % of
  tokens are affected, and it frees the codes so all 26 letters stay
  unfolded. Digits kept as letters would sort `100` before `20`.
- *`NUM_n`* costs 27 860 units against 33 284 for decimal digits (−16 %). A
  nibble varint was rejected because it does not preserve order.
- *Positions:* 99.8 % are under 16 (maximum 29), hence 4 bits.
- *Phonetic folding* of the slot only (`v→b`, `z→s`, `w→u`, `k→c`, keeping
  the letters in the key) was considered and rejected.
- *Signature:* letters 1st+2nd of a group raised false hits from 0.033 to
  0.041 and made one negative query return documents, with no gain, so 1st+3rd
  stays. A 2-letter signature for a one-word group made "producto" read all
  of "Providencia": cold requests 3.36/5.96/9.38 for 1/2/3 words against
  2.95/5.04/7.64 with the signature equal to the word. The cap of 100 group
  words means a new member does not rewrite buckets. Group queries rank at a
  median 4.1 at 100k (33.3 without the group).

**One-letter continuations (§4.6).** 36 % of the names in the catalogue have a
lone letter (`g` 2 434 times, `l` 516). Postings: 19 049 for all hosts, 3 877
for the nearest only. Cold bytes at 100k: nearest only about equal to before,
2 hosts +4–5 %, all hosts +16 % (+30 % for one word). Nearest only misses
`polo m` for "Polo rojo M"; 2 covers the words that usually describe the
letter. Slots drop from 379 to 356.

**Sizes (§10, §11).** The largest `.idx` is always `ca`: 4.0, 4.5, 5.4 and
5.8 KB for 10k/100k with dense/random IDs. From 10k to 100k it grows 1.3×,
the effect of the square root in T. `tokens` is 338 KB at 10k and 3.27 MB at
100k. Buckets with a `.db`, of 379: 3 and 42 at 10k, 126 and 172 at 100k.

**Grouping (§8).** `es100` against `none` at 100k: objects 551 → 185, build
PUTs 557 → 191, cold bytes ×1.7 (an `.idx` brings its neighbours; the largest
goes from 5.8 to 15.8 KB), requests unchanged, warm queries byte-identical,
a compaction 20 % fewer writes but 10 % more bytes. It helps with many
partitions or mostly warm queries; `none` is better if most queries are cold
and bytes are paid. The table comes from `hermitdave/FrequencyWords`
(`es_50k`, OpenSubtitles 2018); the cut is a bisection on the maximum weight
per group and `TestES100` rebuilds it. Without the weighting of numbers, all
of them fell into one 102 KB group.

**Reads per query (§16).** Cold at 100k with random IDs: about 2 rounds, 2.9,
5.1 and 7.6 requests and 4.8, 10.8 and 17.8 KB for 1, 2 and 3 words; warm:
1 round, 1.3–4.4 requests. The previous design needed 4 rounds and twice the
bytes. Round 3 (`shortHeads`) never fires with the defaults (932 of 1 488
3-word queries used a head) and `Approx` was never set; tests with tiny
parameters do exercise it. A full `p.log` (4.2 KB, 72 appends) costs 10.4 KB
warm at 100k, which is why `CompactBytes` is small. IDF is the sum of the
`DF` values rather than a count after the overlay: pending ops are few.
`HeadSize` 64 is three pages of 20 plus margin; depth 2 is word and pair.
Heads store all postings of a word in a document, not a tail mask; a
combination directory is all exact (p = 8) or all prefix-grouped, with no
`df`.

**Ranking and typos.** The engine equals brute force on 3 000 of 3 000
queries. Against whole-word ranking 89.5 % of the orders are identical at 10k
(99.2 % at 100k); the differences come from free stemming and the top hit
still covers every query word. A typo match never outranks an exact one. Cases
worth keeping as tests: `spiderman polo` against `polo spiderman rojo`,
`pimienta moli`, `sunrise tequila`, `carrefour` ≡ `carefour`, `pollo` ≠ `polo`,
`sin gluten`.

**Near words (§17.3).** Before the rule, 0 of 330 benchmark query words were
plural against singular, hence the `plural` category. Allowing any 2 letters
instead of `es` raised false hits (0.023/0.029) for no gain; 1 letter alone
leaves plural at 0.76. The false positives that motivated `es` were
`sal`/`salsa`, `bot`/`boton`, `uni`/`unica`, `smile`/`smileat`. Result: plural
S@1 0.56 → 0.80, base B nDCG 0.912 → 0.929, no extra cost.

**Write cost (§15).** A one-document `Put` is 1 GET and 1 append. A
compaction at 100k with 72 ops touches 62 of 379 buckets: 128 writes and
11.3 MB (the previous design: 5 writes and 6.0 MB, rewriting everything).
About 7.9 of the 9.9 MB of `.db` are rewritten, the largest `.db` is 757 KB,
and `tokens` (3.3 MB) is rewritten every time. Buckets are rewritten when
their content hash differs rather than by computing which were touched; this
also covers the pairs that move when an expansion changes. Recovering an
expired lease does a full rewrite in a new epoch, because the alternative
needs to know what the previous owner wrote.

**Rebuilding after a code change.** Changes after the keys (layout, slots,
codecs, ranking, grouping) rebuild from the stored `tokens`, which holds no
text. Changes before the keys (normalizer, 5-bit table, extra letters) need
the source system.

**Real S3 Express.** An `If-Match` on a missing key counts as a failed
precondition (412); with 8 concurrent writers on the same offset or a new
key, exactly one wins, so the SQS FIFO fallback is not needed. A range that
starts at the object's end returns 416 on S3 and empty locally; `s3blob`
resolves it with a HEAD. With 4 writers at about 100 ms per request a fixed
64 ms backoff exhausted the 16 retries, because an attempt lasts a round; a
backoff proportional to the lost attempt's duration passed 3 of 3 runs (hence
"scaled by the round trip", §15.1). Latency from outside AWS is about 100 ms
per request; it is not measured from the same AZ. A cold "gorra adidas" took
1 round, 3 requests and 421 B; warm, 1 request and 176 B. A compaction of 41
ops rewrote 10 buckets with 14 writes; a new partition takes 2 requests; an
add and a delete during a rebuild were both kept. Appends on a general-purpose
bucket are untested against real S3.

**Benchmark results and tuning (2026-10-05).** Base A/B: S@1 0.72/0.78,
nDCG@10 0.90/0.92 (BM25 0.86/0.91). Better than BM25 on typing, typo, numbers,
brand and plural; behind on `group` (0.60 against 0.80, a group is worth less
than a name word: q172 and q176 are examples). `name+group` S@1 goes from
0.16 to 0.52 with the group. Cold cost: 3.8 GETs and 7–8 KB in 1.1 rounds
(about 2.7 `.idx`, 1 `p.log`, 0.1 `.db`); warm: 1 GET; groups add about 1 KB
and no GET.
- `IDFMax` 15 → 8: nDCG B 0.910 → 0.917, `name+group` S@1 0.44 → 0.52, same
  cost. The cap is a fraction of the corpus, and a very rare word no longer
  rewards a document that misses another. `IDFWeight` 0 or 4 equals changing
  the cap.
- Group counted once when the query has words outside the run (§18.3). Always
  once: `group` nDCG 0.599 → 0.534; once only if the name has part of the run:
  0.468. It fixes q161 and q165; q172 and q176 stay.
- `QueryFirstWord` 2 and `DocFirstWord` 3 → 2 were asked for, not measured:
  85 top-10s change, quality flat (B nDCG 0.929 → 0.930).
- No gain (1–2 queries, noise): `PairNear` 2→4, `DocSecondWord` 1→0,
  `GroupSegment`, `GroupFull`, `GroupWord`, `LetterNext`, `WordFull`. Worse:
  `PairAdjacent` 6→2, `PairAdjacentReversed` 3→6. `LetterExtra` 1, 2, 3 or 5
  give identical results.
- `PrefixBudget` gives identical results at 4 KB, 8 KB, 128 KB and unlimited:
  it never cuts at 10k and needs 100k+ to tune.
