package bucket

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"hash/fnv"
	"math"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/postings"
)

// Params tune how specific the index gets (PLAN_BUCKETS.md §5, §11). A
// reader needs none of them: the .idx says what was expanded.
type Params struct {
	// T = max(MinSplit, √(ExpandCost × D)), D being the bytes of every list
	// of the bucket: a word whose list passes T is expanded.
	MinSplit, ExpandCost float64
	// HeadSize is the documents of a head.
	HeadSize int
	// InlineBudget is the bytes of lists held in the .idx.
	InlineBudget int
	// ComboDictBudget is the size of a combinations directory over which
	// co-keys share blocks by prefix.
	ComboDictBudget int
}

// DefaultParams are the defaults.
func DefaultParams() Params {
	return Params{MinSplit: 2048, ExpandCost: 2048, HeadSize: 64, InlineBudget: 2048, ComboDictBudget: 2048}
}

// Threshold is T for a bucket whose lists take d bytes.
func (p Params) Threshold(d float64) float64 {
	return max(p.MinSplit, math.Sqrt(p.ExpandCost*d))
}

// Doc is a document: its ID and its tokens in position order.
type Doc struct {
	ID     uint32
	Tokens []key5.Token
}

// Built is the index of one bucket, ready to write.
type Built struct {
	Slot     uint16
	Keys     int
	sections [5][]byte // keys, fields, node heads, expansions, inline
	db       []byte    // the .db after its header
	dbCombos uint32
	// Hash is of the content, without the version: a bucket whose hash did
	// not change is not rewritten.
	Hash uint64
	// What it holds, for the CLI and the measurements.
	D, T       float64
	Expanded   int
	InlineSize int
}

// HasDB reports whether the bucket has a .db.
func (b *Built) HasDB() bool { return len(b.db) > 0 }

// DBSize is the size of the .db, 0 without one.
func (b *Built) DBSize() int {
	if !b.HasDB() {
		return 0
	}

	return DBHeader + len(b.db)
}

// EncodeDB writes the .db.
func (b *Built) EncodeDB(m Meta) []byte {
	out := make([]byte, DBHeader, DBHeader+len(b.db))
	copy(out, dbMagic)
	binary.LittleEndian.PutUint16(out[4:], Format)
	binary.LittleEndian.PutUint16(out[6:], m.Slot)
	binary.LittleEndian.PutUint32(out[8:], m.NormID)
	binary.LittleEndian.PutUint32(out[12:], m.Epoch)
	binary.LittleEndian.PutUint64(out[16:], m.Seq)
	binary.LittleEndian.PutUint32(out[24:], crc32.Checksum(b.db, castagnoli))

	return append(out, b.db...)
}

// EncodeIdx writes the .idx, with the ETag S3 gave the .db.
func (b *Built) EncodeIdx(m Meta, dbETag string) []byte {
	return encodeIdx(m, b.Keys, dbETag, uint32(b.DBSize()), b.dbCombos, b.sections)
}

// Tombstone is the .idx of a bucket left without keys: a reader tells it
// from a bucket that never existed (PLAN_BUCKETS.md §7).
func Tombstone(m Meta) []byte {
	return encodeIdx(m, 0, "", 0, DBHeader, [5][]byte{})
}

func encodeIdx(m Meta, keys int, etag string, dbSize, dbCombos uint32, sections [5][]byte) []byte {
	size := IdxHeader + len(etag)
	for _, s := range sections {
		size += len(s)
	}

	out := make([]byte, IdxHeader, size)
	copy(out, idxMagic)
	binary.LittleEndian.PutUint16(out[4:], Format)
	binary.LittleEndian.PutUint16(out[6:], uint16(len(etag)))
	binary.LittleEndian.PutUint32(out[8:], m.NormID)
	binary.LittleEndian.PutUint32(out[12:], m.Epoch)
	binary.LittleEndian.PutUint64(out[16:], m.Seq)
	binary.LittleEndian.PutUint16(out[24:], m.Slot)
	binary.LittleEndian.PutUint32(out[28:], uint32(keys))
	binary.LittleEndian.PutUint32(out[32:], dbSize)
	binary.LittleEndian.PutUint32(out[36:], dbCombos)

	for i, s := range sections {
		binary.LittleEndian.PutUint32(out[40+4*i:], uint32(len(s)))
	}

	out = append(out, etag...)
	for _, s := range sections {
		out = append(out, s...)
	}

	crc := crc32.Update(crc32.Checksum(out[:60], castagnoli), castagnoli, out[IdxHeader:])
	binary.LittleEndian.PutUint32(out[60:], crc)

	return out
}

// Result is the index of a whole collection.
type Result struct {
	Buckets  []*Built   // sorted by slot
	Expanded []key5.Key // sorted
	Docs     int
}

// word is a key while the index is built.
type word struct {
	key    key5.Key
	list   []postings.Posting
	shape  postings.Shape
	df     uint32
	loc    Loc
	expand bool
}

func (w *word) bytes() float64 { return float64(w.shape.Bits()) / 8 }

// Build indexes docs, sorted by ID. prev is the expanded words of the index
// it replaces: one stays expanded until its list falls below T/2.
func Build(docs []Doc, prev map[key5.Key]bool, p Params) (*Result, error) {
	words := map[key5.Key]*word{}

	for i, d := range docs {
		if i > 0 && d.ID <= docs[i-1].ID {
			return nil, errors.New("bucket: documents not sorted by ID")
		}

		for pos, t := range d.Tokens {
			w := words[t.Key]
			if w == nil {
				w = &word{key: t.Key}
				words[t.Key] = w
			}

			// Documents go in ID order and tokens in position order, so every
			// list comes out sorted by (ID, Pos).
			w.list = append(w.list, postings.Posting{ID: d.ID, Pos: uint8(min(pos, postings.MaxPos)), Tail: t.Tail})
		}
	}

	keys := make([]key5.Key, 0, len(words))
	for k, w := range words {
		keys = append(keys, k)

		sh, err := postings.ShapeOf(w.list)
		if err != nil {
			return nil, fmt.Errorf("bucket: key %s: %w", k, err)
		}

		w.shape = sh
		w.df = uint32(len(docIDs(w.list)))
	}

	slices.Sort(keys)

	res := &Result{Docs: len(docs)}
	type span struct{ from, to int }

	var slots []span

	for from := 0; from < len(keys); {
		to := from + 1
		for to < len(keys) && keys[to].Slot() == keys[from].Slot() {
			to++
		}

		slots = append(slots, span{from, to})
		from = to
	}

	// Which words expand depends only on their bucket; where a pair is kept
	// depends on both words, so every bucket is decided before any is built.
	thresholds := make([]float64, len(slots))
	ds := make([]float64, len(slots))

	for i, s := range slots {
		for _, k := range keys[s.from:s.to] {
			ds[i] += words[k].bytes()
		}

		thresholds[i] = p.Threshold(ds[i])

		for _, k := range keys[s.from:s.to] {
			w := words[k]
			w.expand = w.bytes() > thresholds[i] || (prev[k] && w.bytes() > thresholds[i]/2)

			if w.expand {
				res.Expanded = append(res.Expanded, k)
			}
		}
	}

	byID := make(map[uint32]int, len(docs))
	for i, d := range docs {
		byID[d.ID] = i
	}

	b := &builder{p: p, words: words, docs: docs, byID: byID}

	for i, s := range slots {
		built, err := b.bucket(keys[s.from:s.to])
		if err != nil {
			return nil, err
		}

		built.D, built.T = ds[i], thresholds[i]
		res.Buckets = append(res.Buckets, built)
	}

	return res, nil
}

type builder struct {
	p     Params
	words map[key5.Key]*word
	docs  []Doc
	byID  map[uint32]int
}

// anchored reports whether the combination of a and k is kept under a: the
// expanded word, or the smaller key when both are.
func (b *builder) anchored(a, k key5.Key) bool {
	return !b.words[k].expand || a < k
}

func (b *builder) bucket(keys []key5.Key) (*Built, error) {
	slot := keys[0].Slot()
	built := &Built{Slot: slot, Keys: len(keys)}
	ws := make([]*word, len(keys))

	for i, k := range keys {
		ws[i] = b.words[k]
	}

	// Lists in the .idx: the smallest first, while they fit the budget.
	var small []*word

	for _, w := range ws {
		switch {
		case w.shape.Count == 1:
			w.loc = LocField
		case w.expand:
			w.loc = LocExpanded
			built.Expanded++
		default:
			w.loc = LocDB
			small = append(small, w)
		}
	}

	slices.SortStableFunc(small, func(x, y *word) int { return cmp.Compare(x.shape.Bits(), y.shape.Bits()) })

	var inlineBits uint64

	for _, w := range small {
		if (inlineBits+w.shape.Bits()+7)/8 > uint64(b.p.InlineBudget) {
			break
		}

		inlineBits += w.shape.Bits()
		w.loc = LocInline
	}

	// Keys.
	kw := bits.NewWriter(nil)
	if err := key5.AppendBucket(kw, keys); err != nil {
		return nil, err
	}

	built.sections[0] = kw.Bytes()
	built.sections[1] = encodeFields(ws)

	// Lists: the inline ones in the .idx, the others in the words zone of the
	// .db, both in key order.
	iw, dw := bits.NewWriter(nil), bits.NewWriter(nil)

	for _, w := range ws {
		switch w.loc {
		case LocInline:
			postings.Write(iw, w.shape, w.list)
		case LocDB, LocExpanded:
			postings.Write(dw, w.shape, w.list)
		}
	}

	built.sections[4] = iw.Bytes()
	built.InlineSize = len(built.sections[4])
	db := dw.Bytes()
	built.dbCombos = uint32(DBHeader + len(db))

	// Expansions: heads in the .idx, combinations in the .db.
	var exps []byte

	for i, w := range ws {
		if w.loc != LocExpanded {
			continue
		}

		record, blocks, err := b.expansion(w)
		if err != nil {
			return nil, fmt.Errorf("bucket: expansion of %s: %w", w.key, err)
		}

		exps = binary.LittleEndian.AppendUint32(exps, uint32(i))
		exps = binary.LittleEndian.AppendUint32(exps, uint32(len(record)))
		exps = append(exps, record...)
		db = append(db, blocks...)
	}

	built.sections[3] = exps

	if slices.ContainsFunc(ws, func(w *word) bool { return w.loc == LocDB || w.loc == LocExpanded }) {
		built.db = db
	}

	if uint64(built.DBSize()) > math.MaxUint32 {
		return nil, fmt.Errorf("bucket %d: .db over 4 GiB: partition the collection", slot)
	}

	h := fnv.New64a()
	var scratch [8]byte

	binary.LittleEndian.PutUint64(scratch[:], uint64(slot)<<32|uint64(len(keys)))
	h.Write(scratch[:])

	for _, s := range built.sections {
		binary.LittleEndian.PutUint64(scratch[:], uint64(len(s)))
		h.Write(scratch[:])
		h.Write(s)
	}

	h.Write(built.db)
	built.Hash = h.Sum64()

	return built, nil
}

func encodeFields(ws []*word) []byte {
	var maxCount, maxDup, maxX uint32
	base, maxFirst := uint32(math.MaxUint32), uint32(0)

	for _, w := range ws {
		maxCount = max(maxCount, w.shape.Count)
		maxDup = max(maxDup, w.shape.Count-w.df)
		base, maxFirst = min(base, w.shape.First), max(maxFirst, w.shape.First)

		if w.shape.Count > 1 {
			maxX = max(maxX, w.shape.X)
		}
	}

	wCount, wDup := bits.Len(uint64(maxCount)), bits.Len(uint64(maxDup))
	wFirst, wX := bits.Len(uint64(maxFirst-base)), max(bits.Len(uint64(maxX)), postings.MinX)

	out := []byte{byte(wCount), byte(wDup), byte(wFirst), byte(wX)}
	out = binary.LittleEndian.AppendUint32(out, base)
	w := bits.NewWriter(out)

	for _, wd := range ws {
		w.Write(uint64(wd.shape.Count), wCount)
		w.Write(uint64(wd.shape.Count-wd.df), wDup)
		w.Write(uint64(wd.shape.First-base), wFirst)
		w.Write(wd.shape.V(), 2+wX)
		w.Write(uint64(wd.loc), 2)
	}

	return w.Bytes()
}

// pair is a combination being built: the postings of the expanded word and
// of the co-key in the documents with both.
type pair struct {
	key  key5.Key
	a, k []postings.Posting
}

// expansion builds the record of an expanded word for the .idx and its
// combination blocks for the .db.
func (b *builder) expansion(w *word) (record, blocks []byte, err error) {
	entries := groupByDoc(w.list)
	pairs := map[key5.Key]*pair{}

	for _, e := range entries {
		doc := b.docs[b.byID[e.ID]]

		for pos, t := range doc.Tokens {
			if t.Key == w.key || !b.anchored(w.key, t.Key) {
				continue
			}

			pr := pairs[t.Key]
			if pr == nil {
				pr = &pair{key: t.Key}
				pairs[t.Key] = pr
			}

			if n := len(pr.a); n == 0 || pr.a[n-1].ID != e.ID {
				pr.a = append(pr.a, e.Postings...)
			}

			pr.k = append(pr.k, postings.Posting{ID: e.ID, Pos: uint8(min(pos, postings.MaxPos)), Tail: t.Tail})
		}
	}

	coKeys := make([]key5.Key, 0, len(pairs))
	for k := range pairs {
		coKeys = append(coKeys, k)
	}

	slices.Sort(coKeys)

	p := b.comboUnits(coKeys)

	// Groups of co-keys sharing their first p units, each a block.
	var groupKeys []key5.Key
	var groupLens []uint64

	for from := 0; from < len(coKeys); {
		g := coKeys[from].Prefix(p)
		to := from + 1

		for to < len(coKeys) && coKeys[to].Prefix(p) == g {
			to++
		}

		block, err := encodeBlock(coKeys[from:to], pairs)
		if err != nil {
			return nil, nil, err
		}

		groupKeys = append(groupKeys, g)
		groupLens = append(groupLens, uint64(len(block)))
		blocks = append(blocks, block...)
		from = to
	}

	groups := b.heads(w, entries)

	var maxLen uint64
	for _, n := range groupLens {
		maxLen = max(maxLen, n)
	}

	wBytes := bits.Len(maxLen)

	record = []byte{byte(len(groups)), byte(p), byte(wBytes), 0}
	record = binary.LittleEndian.AppendUint32(record, uint32(len(groupKeys)))

	for _, g := range groups {
		record = appendGroup(record, g)
	}

	kw := bits.NewWriter(record)
	if err := key5.AppendKeys(kw, groupKeys); err != nil {
		return nil, nil, err
	}

	record = kw.Bytes()
	lw := bits.NewWriter(record)

	for _, n := range groupLens {
		lw.Write(n, wBytes)
	}

	return lw.Bytes(), blocks, nil
}

// comboUnits picks how many units of a co-key choose its block: all of them,
// unless the directory would pass ComboDictBudget; then the most that fit,
// down to the two of the bucket.
func (b *builder) comboUnits(coKeys []key5.Key) int {
	for p := key5.MaxUnits; p > 2; p-- {
		var groups []key5.Key

		for _, k := range coKeys {
			if g := k.Prefix(p); len(groups) == 0 || groups[len(groups)-1] != g {
				groups = append(groups, g)
			}
		}

		w := bits.NewWriter(nil)
		_ = key5.AppendKeys(w, groups)

		// Each block length takes about 20 bits.
		if (w.BitLen()+20*uint64(len(groups)))/8 <= uint64(b.p.ComboDictBudget) {
			return p
		}
	}

	return 2
}

// encodeBlock writes the combinations of one block:
// n u16 | 0 u16 | base u32 | keys | per key, the columns of the word and of
// the co-key.
func encodeBlock(keys []key5.Key, pairs map[key5.Key]*pair) ([]byte, error) {
	base := uint32(math.MaxUint32)
	for _, k := range keys {
		base = min(base, pairs[k].a[0].ID)
	}

	if len(keys) > math.MaxUint16 {
		return nil, fmt.Errorf("bucket: %d co-keys in one block", len(keys))
	}

	out := binary.LittleEndian.AppendUint16(nil, uint16(len(keys)))
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, base)

	w := bits.NewWriter(out)
	if err := key5.AppendKeys(w, keys); err != nil {
		return nil, err
	}

	w = bits.NewWriter(w.Bytes())

	for _, k := range keys {
		if err := postings.WriteColumns(w, base, postings.Columns{pairs[k].a, pairs[k].k}); err != nil {
			return nil, err
		}
	}

	return w.Bytes(), nil
}

// groupByDoc splits a list into one entry per document.
func groupByDoc(list []postings.Posting) []postings.Entry {
	var entries []postings.Entry

	for i := 0; i < len(list); {
		j := i + 1
		for j < len(list) && list[j].ID == list[i].ID {
			j++
		}

		entries = append(entries, postings.Entry{ID: list[i].ID, Postings: list[i:j:j]})
		i = j
	}

	return entries
}

// rank is the order of a head: documents where the word comes first, then
// second, then the rest — the position bonus of a query of that word alone
// — and by ID within each.
func rank(pos uint8) uint8 { return min(pos, 2) }

// FirstPos is the first position of an entry's postings with tail t, or of
// any for AllTails; ok is false when it has none.
func FirstPos(e *postings.Entry, t uint8) (pos uint8, ok bool) {
	pos = postings.MaxPos + 1

	for _, p := range e.Postings {
		if t == AllTails || p.Tail == t {
			pos, ok = min(pos, p.Pos), true
		}
	}

	return pos, ok
}

// heads builds the groups of an expanded word: every document, and with two
// tails or more one group per tail (PLAN_BUCKETS.md §3.3).
func (b *builder) heads(w *word, entries []postings.Entry) []Group {
	tails := map[uint8]bool{}
	for _, p := range w.list {
		tails[p.Tail] = true
	}

	order := []uint8{AllTails}

	if len(tails) > 1 {
		for t := range uint8(4) {
			if tails[t] {
				order = append(order, t)
			}
		}
	}

	groups := make([]Group, 0, len(order))

	for _, t := range order {
		type ranked struct {
			e    postings.Entry
			rank uint8
		}

		var in []ranked

		for _, e := range entries {
			if pos, ok := FirstPos(&e, t); ok {
				in = append(in, ranked{e, rank(pos)})
			}
		}

		slices.SortFunc(in, func(x, y ranked) int {
			return cmp.Or(cmp.Compare(x.rank, y.rank), cmp.Compare(x.e.ID, y.e.ID))
		})

		g := Group{Tail: t, Total: uint32(len(in))}
		for _, r := range in[:min(len(in), b.p.HeadSize)] {
			g.Entries = append(g.Entries, r.e)
		}

		groups = append(groups, g)
	}

	return groups
}

// appendGroup writes a head: tail u8 | flags u8 | wID u8 | 0 | m u16 | 0 u16
// | total u32 | base u32 | size u32 | entries.
func appendGroup(dst []byte, g Group) []byte {
	base, top := uint32(math.MaxUint32), uint32(0)
	tail := false

	for _, e := range g.Entries {
		base, top = min(base, e.ID), max(top, e.ID)

		for _, p := range e.Postings {
			tail = tail || p.Tail != 0
		}
	}

	if len(g.Entries) == 0 {
		base = 0
	}

	wID := bits.Len(uint64(top - base))
	w := bits.NewWriter(nil)
	postings.WriteEntries(w, base, wID, tail, g.Entries)
	body := w.Bytes()

	dst = append(dst, g.Tail, byte(b2i(tail)), byte(wID), 0)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(g.Entries)))
	dst = binary.LittleEndian.AppendUint16(dst, 0)
	dst = binary.LittleEndian.AppendUint32(dst, g.Total)
	dst = binary.LittleEndian.AppendUint32(dst, base)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(body)))

	return append(dst, body...)
}

func b2i(b bool) int {
	if b {
		return 1
	}

	return 0
}

func docIDs(list []postings.Posting) []uint32 {
	var ids []uint32

	for i, p := range list {
		if i == 0 || p.ID != list[i-1].ID {
			ids = append(ids, p.ID)
		}
	}

	return ids
}
