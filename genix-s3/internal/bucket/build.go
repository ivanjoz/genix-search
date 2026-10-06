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
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
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
	// GroupWordDocs is the documents of each group whose group words are
	// indexed: those of the smallest keys (PLAN_BUCKETS.md §17.1). 0 means
	// the default.
	GroupWordDocs int
}

// DefaultGroupWordDocs is the default of Params.GroupWordDocs.
const DefaultGroupWordDocs = 100

// DefaultParams are the defaults.
func DefaultParams() Params {
	return Params{MinSplit: 2048, ExpandCost: 2048, HeadSize: 64, InlineBudget: 2048, ComboDictBudget: 2048, GroupWordDocs: DefaultGroupWordDocs}
}

// Threshold is T for a bucket whose lists take d bytes.
func (p Params) Threshold(d float64) float64 {
	return max(p.MinSplit, math.Sqrt(p.ExpandCost*d))
}

// Doc is a document: its key and its tokens in position order.
type Doc struct {
	ID     docid.Key
	Tokens []key5.Token
}

// Built is the index of one bucket — one group of slots — ready to write.
type Built struct {
	Group    uint16
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
	binary.LittleEndian.PutUint16(out[6:], m.Group)
	binary.LittleEndian.PutUint32(out[8:], m.NormID)
	binary.LittleEndian.PutUint32(out[12:], m.Epoch)
	binary.LittleEndian.PutUint64(out[16:], m.Seq)
	binary.LittleEndian.PutUint32(out[24:], crc32.Checksum(b.db, castagnoli))
	out[28] = m.Grouping

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
	binary.LittleEndian.PutUint16(out[24:], m.Group)
	out[26] = m.Grouping
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
	Buckets  []*Built   // sorted by group
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

// Build indexes docs, sorted by key, into one bucket per group of g. prev is
// the expanded words of the index it replaces: one stays expanded until its
// list falls below T/2. T and the inline lists are decided per slot, so a
// group holds what its slots would hold apart.
func Build(docs []Doc, prev map[key5.Key]bool, p Params, g *layout.Grouping) (*Result, error) {
	for i, d := range docs {
		if i > 0 && d.ID <= docs[i-1].ID {
			return nil, errors.New("bucket: documents not sorted by key")
		}
	}

	docs = CapGroupWords(docs, p.GroupWordDocs)
	words := map[key5.Key]*word{}

	for _, d := range docs {
		for pos, t := range d.Tokens {
			w := words[t.Key]
			if w == nil {
				w = &word{key: t.Key}
				words[t.Key] = w
			}

			// Documents go in key order and tokens in position order, so every
			// list comes out sorted by (key, Pos).
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

	// By group, and within a group in the order of the keys: by space, then
	// slot (PLAN_BUCKETS.md §17.2).
	slices.SortFunc(keys, func(a, b key5.Key) int {
		return cmp.Or(cmp.Compare(g.Of(a.Slot()), g.Of(b.Slot())), cmp.Compare(a, b))
	})

	res := &Result{Docs: len(docs)}

	// Which words expand depends only on their slot, over every space; where
	// a pair is kept depends on both words, so every bucket is decided before
	// any is built.
	ds := map[uint16]float64{}
	for _, k := range keys {
		ds[k.Slot()] += words[k].bytes()
	}

	for _, k := range keys {
		w, t := words[k], p.Threshold(ds[k.Slot()])
		w.expand = w.bytes() > t || (prev[k] && w.bytes() > t/2)

		if w.expand {
			res.Expanded = append(res.Expanded, k)
		}
	}

	slices.Sort(res.Expanded)

	byID := make(map[docid.Key]int, len(docs))
	for i, d := range docs {
		byID[d.ID] = i
	}

	b := &builder{p: p, words: words, docs: docs, byID: byID}

	for from := 0; from < len(keys); {
		group := g.Of(keys[from].Slot())
		to := from + 1

		for to < len(keys) && g.Of(keys[to].Slot()) == group {
			to++
		}

		built, err := b.bucket(group, keys[from:to])
		if err != nil {
			return nil, err
		}

		slots := map[uint16]bool{}
		for _, k := range keys[from:to] {
			if !slots[k.Slot()] {
				slots[k.Slot()] = true
				built.D += ds[k.Slot()]
				built.T = max(built.T, p.Threshold(ds[k.Slot()]))
			}
		}

		res.Buckets = append(res.Buckets, built)
		from = to
	}

	return res, nil
}

// CapGroupWords keeps the group words of the first n documents of each group,
// in key order, and drops those of the rest (PLAN_BUCKETS.md §17.4): a group
// is the sequence of its group words and its signature. n ≤ 0 means the
// default. docs is not modified.
func CapGroupWords(docs []Doc, n int) []Doc {
	if n <= 0 {
		n = DefaultGroupWordDocs
	}

	var out []Doc
	members := map[string]int{}
	var scratch []byte

	for i, d := range docs {
		scratch = scratch[:0]

		for _, t := range d.Tokens {
			if t.Key.Space() != key5.SpaceWord {
				scratch = binary.LittleEndian.AppendUint64(scratch, uint64(t.Key))
			}
		}

		if len(scratch) == 0 {
			continue
		}

		members[string(scratch)]++
		if members[string(scratch)] <= n {
			continue
		}

		if out == nil {
			out = slices.Clone(docs)
		}

		out[i].Tokens = slices.DeleteFunc(slices.Clone(d.Tokens), func(t key5.Token) bool { return t.Key.Space() == key5.SpaceGroupWord })
	}

	if out == nil {
		return docs
	}

	return out
}

type builder struct {
	p     Params
	words map[key5.Key]*word
	docs  []Doc
	byID  map[docid.Key]int
}

// anchored reports whether the combination of a and k is kept under a: the
// expanded word, or the smaller key when both are.
func (b *builder) anchored(a, k key5.Key) bool {
	return !b.words[k].expand || a < k
}

func (b *builder) bucket(group uint16, keys []key5.Key) (*Built, error) {
	built := &Built{Group: group, Keys: len(keys)}
	ws := make([]*word, len(keys))

	for i, k := range keys {
		ws[i] = b.words[k]
	}

	// Lists in the .idx: per slot, the smallest first, while they fit the
	// budget.
	small := map[uint16][]*word{}

	for _, w := range ws {
		switch {
		case w.shape.Count == 1:
			w.loc = LocField
		case w.expand:
			w.loc = LocExpanded
			built.Expanded++
		default:
			w.loc = LocDB
			small[w.key.Slot()] = append(small[w.key.Slot()], w)
		}
	}

	for _, list := range small {
		slices.SortStableFunc(list, func(x, y *word) int { return cmp.Compare(x.shape.Bits(), y.shape.Bits()) })

		var inlineBits uint64

		for _, w := range list {
			if (inlineBits+w.shape.Bits()+7)/8 > uint64(b.p.InlineBudget) {
				break
			}

			inlineBits += w.shape.Bits()
			w.loc = LocInline
		}
	}

	// Keys: the directory of runs of one space and one slot, then the keys of
	// each run.
	var counts []int
	var dir []byte

	for from := 0; from < len(keys); {
		to := from + 1
		for to < len(keys) && keys[to].Slot() == keys[from].Slot() && keys[to].Space() == keys[from].Space() {
			to++
		}

		dir = binary.LittleEndian.AppendUint16(dir, uint16(keys[from].Space())<<spaceShift|keys[from].Slot())
		dir = binary.LittleEndian.AppendUint32(dir, uint32(to-from))
		counts = append(counts, to-from)
		from = to
	}

	kw := bits.NewWriter(append(binary.LittleEndian.AppendUint16(nil, uint16(len(counts))), dir...))

	at := 0
	for _, n := range counts {
		if err := key5.AppendBucket(kw, keys[at:at+n]); err != nil {
			return nil, err
		}

		at += n
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
		return nil, fmt.Errorf("bucket %d: .db over 4 GiB: partition the collection", group)
	}

	h := fnv.New64a()
	var scratch [8]byte

	binary.LittleEndian.PutUint64(scratch[:], uint64(group)<<32|uint64(len(keys)))
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
	firsts := make([]docid.Key, len(ws))

	for i, w := range ws {
		maxCount = max(maxCount, w.shape.Count)
		maxDup = max(maxDup, w.shape.Count-w.df)
		firsts[i] = w.shape.First

		if w.shape.Count > 1 {
			maxX = max(maxX, w.shape.X)
		}
	}

	wCount, wDup := bits.Len(uint64(maxCount)), bits.Len(uint64(maxDup))
	wX := max(bits.Len(uint64(maxX)), postings.MinX)
	first := postings.SplitOf(firsts...)

	out := []byte{byte(wCount), byte(wDup), byte(first.WID), byte(wX), byte(first.WO), first.Origin, 0, 0}
	out = binary.LittleEndian.AppendUint32(out, first.ID)
	w := bits.NewWriter(out)

	for _, wd := range ws {
		w.Write(uint64(wd.shape.Count), wCount)
		w.Write(uint64(wd.shape.Count-wd.df), wDup)
		first.Write(w, wd.shape.First)
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
// n u16 | base origin u8 | 0 u8 | base ID u32 | keys | per key, the columns of
// the word and of the co-key.
func encodeBlock(keys []key5.Key, pairs map[key5.Key]*pair) ([]byte, error) {
	base := docid.Max
	for _, k := range keys {
		base = min(base, pairs[k].a[0].ID)
	}

	if len(keys) > math.MaxUint16 {
		return nil, fmt.Errorf("bucket: %d co-keys in one block", len(keys))
	}

	out := binary.LittleEndian.AppendUint16(nil, uint16(len(keys)))
	out = append(out, base.Origin(), 0)
	out = binary.LittleEndian.AppendUint32(out, base.ID())

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
// — and by key within each. A group word or a signature scores the same
// wherever it is (PLAN_BUCKETS.md §17.5): its head goes by key alone.
func rank(k key5.Key, pos uint8) uint8 {
	if k.Space() != key5.SpaceWord {
		return 0
	}

	return min(pos, 2)
}

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
				in = append(in, ranked{e, rank(w.key, pos)})
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

// appendGroup writes a head: tail u8 | flags u8 | wID u8 | base origin u8 |
// m u16 | wOrigin u8 | 0 u8 | total u32 | base ID u32 | size u32 | entries.
func appendGroup(dst []byte, g Group) []byte {
	keys := make([]docid.Key, len(g.Entries))
	tail := false

	for i, e := range g.Entries {
		keys[i] = e.ID

		for _, p := range e.Postings {
			tail = tail || p.Tail != 0
		}
	}

	s := postings.SplitOf(keys...)
	w := bits.NewWriter(nil)
	postings.WriteEntries(w, s, tail, g.Entries)
	body := w.Bytes()

	dst = append(dst, g.Tail, byte(b2i(tail)), byte(s.WID), s.Origin)
	dst = binary.LittleEndian.AppendUint16(dst, uint16(len(g.Entries)))
	dst = append(dst, byte(s.WO), 0)
	dst = binary.LittleEndian.AppendUint32(dst, g.Total)
	dst = binary.LittleEndian.AppendUint32(dst, s.ID)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(body)))

	return append(dst, body...)
}

func b2i(b bool) int {
	if b {
		return 1
	}

	return 0
}

func docIDs(list []postings.Posting) []docid.Key {
	var ids []docid.Key

	for i, p := range list {
		if i == 0 || p.ID != list[i-1].ID {
			ids = append(ids, p.ID)
		}
	}

	return ids
}
