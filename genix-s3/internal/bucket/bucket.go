// Package bucket is the index of one bucket, the keys that share their first
// two letters: <xy>.idx, the dictionary with the small lists, the heads and
// the combinations directory, and <xy>.db, the lists that do not fit in it
// (PLAN_BUCKETS.md §3–4). Both are plain bytes; every offset is computed
// from sizes, never stored per key.
package bucket

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/postings"
)

// Format is the layout version of .idx and .db. A reader rejects any other.
const Format uint16 = 1

const (
	idxMagic = "GXI1"
	dbMagic  = "GXD1"
	// IdxHeader and DBHeader are the fixed headers.
	IdxHeader = 64
	DBHeader  = 32
	// AllTails is the head group of every document of a word.
	AllTails = 255
)

// Loc is where a word's list is.
type Loc uint8

const (
	// LocField: one posting, held in the dictionary fields.
	LocField Loc = iota
	// LocInline: in the Inline section of the .idx.
	LocInline
	// LocDB: in the words zone of the .db.
	LocDB
	// LocExpanded: in the words zone of the .db, and the word has heads and
	// combinations.
	LocExpanded
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is a file that fails its checks.
var ErrCorrupt = errors.New("bucket: corrupt")

// Word is a key of the bucket and what the dictionary says of its list.
type Word struct {
	Key   key5.Key
	Shape postings.Shape
	DF    uint32 // documents with the word
	Loc   Loc
	// BitOff is where the list starts: in the Inline section for LocInline,
	// in the .db (from its byte 0) for LocDB and LocExpanded.
	BitOff uint64
	Exp    *Expansion
}

// Bytes is the byte range of the list in the .db.
func (w *Word) Bytes() (off, end uint64) {
	return w.BitOff / 8, (w.BitOff + w.Shape.Bits() + 7) / 8
}

// Group is a head: the first documents of a word in the order of a query of
// that word alone (PLAN_BUCKETS.md §3.3–3.4).
type Group struct {
	Tail    uint8  // the tail of the documents in it, or AllTails
	Total   uint32 // documents of the group, Entries being the first of them
	Entries []postings.Entry
}

// Complete reports a group whose head holds every document.
func (g *Group) Complete() bool { return len(g.Entries) == int(g.Total) }

// Combo is an entry of a combinations directory: the block of the .db with
// the lists of the expanded word together with every co-key whose first P
// units are Key.
type Combo struct {
	Key      key5.Key
	Off, Len uint32 // byte range of the block in the .db
}

// Expansion is what an expanded word has besides its full list.
type Expansion struct {
	Groups []Group // AllTails first
	// P is the units of a co-key that pick its combination block: 8 when
	// every co-key has a block of its own.
	P      uint8
	Combos []Combo // sorted by Key
}

// Group returns the head of documents with tail t, or of every document
// for AllTails.
func (e *Expansion) Group(t uint8) *Group {
	for i := range e.Groups {
		if e.Groups[i].Tail == t {
			return &e.Groups[i]
		}
	}

	return nil
}

// Combo returns the block holding the combination with co-key k.
func (e *Expansion) Combo(k key5.Key) (Combo, bool) {
	k = k.Prefix(int(e.P))
	i := sort.Search(len(e.Combos), func(i int) bool { return e.Combos[i].Key >= k })

	if i < len(e.Combos) && e.Combos[i].Key == k {
		return e.Combos[i], true
	}

	return Combo{}, false
}

// Meta is what both files say of the version they belong to.
type Meta struct {
	NormID uint32
	Epoch  uint32
	Seq    uint64 // the compaction that wrote them includes the ops up to Seq
	Slot   uint16
}

// Index is a decoded .idx.
type Index struct {
	Meta
	DBETag   string
	DBSize   uint32
	DBCombos uint32
	Words    []Word // sorted by key
	Inline   []byte
	Size     int // bytes of the .idx, for cache accounting
}

// Find returns the word of key k.
func (x *Index) Find(k key5.Key) (*Word, bool) {
	i := sort.Search(len(x.Words), func(i int) bool { return x.Words[i].Key >= k })
	if i < len(x.Words) && x.Words[i].Key == k {
		return &x.Words[i], true
	}

	return nil, false
}

// Range returns the words with lo ≤ key ≤ hi.
func (x *Index) Range(lo, hi key5.Key) []Word {
	from := sort.Search(len(x.Words), func(i int) bool { return x.Words[i].Key >= lo })
	to := sort.Search(len(x.Words), func(i int) bool { return x.Words[i].Key > hi })

	return x.Words[from:max(from, to)]
}

// Postings decodes the list of w. data holds bytes of the .db from byte
// dataOff and must cover w.Bytes() for LocDB and LocExpanded.
func (x *Index) Postings(w *Word, data []byte, dataOff uint64, dst []postings.Posting) ([]postings.Posting, error) {
	switch w.Loc {
	case LocField:
		return postings.Read(nil, w.Shape, dst)
	case LocInline:
		return x.read(w, x.Inline, w.BitOff, dst)
	}

	if w.BitOff < 8*dataOff {
		return nil, fmt.Errorf("bucket: list of %s at bit %d, before the data at byte %d", w.Key, w.BitOff, dataOff)
	}

	return x.read(w, data, w.BitOff-8*dataOff, dst)
}

func (x *Index) read(w *Word, data []byte, at uint64, dst []postings.Posting) ([]postings.Posting, error) {
	r := bits.NewReader(data, at)

	out, err := postings.Read(r, w.Shape, dst)
	if err != nil {
		return nil, fmt.Errorf("bucket %d: list of %s: %w", x.Slot, w.Key, err)
	}

	if r.Pos()-at != w.Shape.Bits() {
		return nil, fmt.Errorf("%w: list of %s has %d bits, the dictionary says %d", ErrCorrupt, w.Key, r.Pos()-at, w.Shape.Bits())
	}

	return out, nil
}

// ReadMeta reads the version of a .idx from its header alone: a reader
// decides from it whether the bucket belongs to the p.log it read, before it
// decodes the rest (PLAN_BUCKETS.md §7).
func ReadMeta(data []byte) (Meta, error) {
	if len(data) < IdxHeader || string(data[:4]) != idxMagic {
		return Meta{}, fmt.Errorf("%w: not a .idx", ErrCorrupt)
	}

	return Meta{
		NormID: binary.LittleEndian.Uint32(data[8:]),
		Epoch:  binary.LittleEndian.Uint32(data[12:]),
		Seq:    binary.LittleEndian.Uint64(data[16:]),
		Slot:   binary.LittleEndian.Uint16(data[24:]),
	}, nil
}

// Decode reads a whole .idx. normID is the reader's normalizer.
func Decode(data []byte, normID uint32) (*Index, error) {
	if len(data) < IdxHeader || string(data[:4]) != idxMagic {
		return nil, fmt.Errorf("%w: not a .idx", ErrCorrupt)
	}

	if f := binary.LittleEndian.Uint16(data[4:]); f != Format {
		return nil, fmt.Errorf("bucket: format %d, this build reads %d", f, Format)
	}

	crc := crc32.Update(crc32.Checksum(data[:60], castagnoli), castagnoli, data[IdxHeader:])
	if crc != binary.LittleEndian.Uint32(data[60:]) {
		return nil, fmt.Errorf("%w: .idx CRC", ErrCorrupt)
	}

	x := &Index{
		Meta: Meta{
			NormID: binary.LittleEndian.Uint32(data[8:]),
			Epoch:  binary.LittleEndian.Uint32(data[12:]),
			Seq:    binary.LittleEndian.Uint64(data[16:]),
			Slot:   binary.LittleEndian.Uint16(data[24:]),
		},
		DBSize:   binary.LittleEndian.Uint32(data[32:]),
		DBCombos: binary.LittleEndian.Uint32(data[36:]),
		Size:     len(data),
	}

	if x.NormID != normID {
		return nil, fmt.Errorf("bucket: NormID %08x, this normalizer is %08x: the collection was indexed with another normalizer or other extra letters", x.NormID, normID)
	}

	if x.Slot >= key5.Slots {
		return nil, fmt.Errorf("%w: slot %d", ErrCorrupt, x.Slot)
	}

	keys := int(binary.LittleEndian.Uint32(data[28:]))
	etagLen := int(binary.LittleEndian.Uint16(data[6:]))

	var lens [5]int
	at := IdxHeader + etagLen

	for i := range lens {
		lens[i] = int(binary.LittleEndian.Uint32(data[40+4*i:]))
		at += lens[i]
	}

	if at != len(data) || keys > len(data)*2 {
		return nil, fmt.Errorf("%w: sections do not add up", ErrCorrupt)
	}

	x.DBETag = string(data[IdxHeader : IdxHeader+etagLen])

	if keys == 0 {
		return x, nil
	}

	var sections [5][]byte
	at = IdxHeader + etagLen

	for i, n := range lens {
		sections[i] = data[at : at+n]
		at += n
	}

	words, err := key5.ReadSlot(bits.NewReader(sections[0], 0), x.Slot, keys)
	if err != nil {
		return nil, fmt.Errorf("bucket %d: keys: %w", x.Slot, err)
	}

	x.Words = make([]Word, keys)
	for i, k := range words {
		x.Words[i].Key = k
	}

	if err := x.decodeFields(sections[1]); err != nil {
		return nil, err
	}

	// sections[2] is reserved for the heads of letter nodes.

	if err := x.decodeExpansions(sections[3]); err != nil {
		return nil, err
	}

	x.Inline = sections[4]

	// Every list starts where the one before it in the same place ends.
	inline, db := uint64(0), uint64(8*DBHeader)

	for i := range x.Words {
		w := &x.Words[i]

		switch w.Loc {
		case LocInline:
			w.BitOff = inline
			inline += w.Shape.Bits()
		case LocDB, LocExpanded:
			w.BitOff = db
			db += w.Shape.Bits()
		}

		if (w.Loc == LocExpanded) != (w.Exp != nil) {
			return nil, fmt.Errorf("%w: word %s expanded without its record", ErrCorrupt, w.Key)
		}
	}

	if (inline+7)/8 != uint64(len(x.Inline)) {
		return nil, fmt.Errorf("%w: inline lists of %d bits in %d bytes", ErrCorrupt, inline, len(x.Inline))
	}

	if (db+7)/8 != uint64(x.DBCombos) || x.DBCombos > x.DBSize && x.DBSize > 0 {
		return nil, fmt.Errorf("%w: .db words zone of %d bits, combinations at %d", ErrCorrupt, db, x.DBCombos)
	}

	return x, nil
}

func (x *Index) decodeFields(b []byte) error {
	if len(b) < 8 {
		return fmt.Errorf("%w: fields", ErrCorrupt)
	}

	wCount, wDup, wFirst, wX := uint(b[0]), uint(b[1]), uint(b[2]), uint(b[3])
	base := binary.LittleEndian.Uint32(b[4:])

	if wCount > 32 || wDup > 32 || wFirst > 32 || wX < postings.MinX || wX > 32 {
		return fmt.Errorf("%w: field widths", ErrCorrupt)
	}

	r := bits.NewReader(b[8:], 0)

	for i := range x.Words {
		w := &x.Words[i]
		w.Shape.Count = uint32(r.Read(wCount))
		dup := uint32(r.Read(wDup))
		first := uint64(base) + r.Read(wFirst)
		w.Shape.SetV(r.Read(2 + wX))
		w.Loc = Loc(r.Read(2))

		if w.Shape.Count == 0 || dup >= w.Shape.Count || first > 1<<32-1 || (w.Loc == LocField) != (w.Shape.Count == 1) {
			return fmt.Errorf("%w: fields of %s", ErrCorrupt, w.Key)
		}

		w.Shape.First = uint32(first)
		w.DF = w.Shape.Count - dup
	}

	if r.Err() != nil {
		return fmt.Errorf("%w: fields are cut", ErrCorrupt)
	}

	return nil
}

func (x *Index) decodeExpansions(b []byte) error {
	combo := uint64(x.DBCombos)

	for len(b) > 0 {
		if len(b) < 8 {
			return fmt.Errorf("%w: expansion header", ErrCorrupt)
		}

		word, size := int(binary.LittleEndian.Uint32(b)), int(binary.LittleEndian.Uint32(b[4:]))
		if word >= len(x.Words) || size > len(b)-8 || x.Words[word].Exp != nil {
			return fmt.Errorf("%w: expansion of word %d", ErrCorrupt, word)
		}

		e, err := decodeExpansion(b[8 : 8+size])
		if err != nil {
			return fmt.Errorf("bucket %d: expansion of %s: %w", x.Slot, x.Words[word].Key, err)
		}

		for i := range e.Combos {
			e.Combos[i].Off = uint32(combo)
			combo += uint64(e.Combos[i].Len)
		}

		x.Words[word].Exp = e
		b = b[8+size:]
	}

	if x.DBSize > 0 && combo != uint64(x.DBSize) {
		return fmt.Errorf("%w: combinations end at %d, the .db has %d bytes", ErrCorrupt, combo, x.DBSize)
	}

	return nil
}

func decodeExpansion(b []byte) (*Expansion, error) {
	if len(b) < 8 {
		return nil, ErrCorrupt
	}

	groups, p, wBytes := int(b[0]), b[1], uint(b[2])
	n := int(binary.LittleEndian.Uint32(b[4:]))

	if groups == 0 || p < 2 || p > key5.MaxUnits || wBytes > 32 || n > len(b)*2 {
		return nil, ErrCorrupt
	}

	e := &Expansion{P: p, Groups: make([]Group, groups)}
	b = b[8:]

	for i := range e.Groups {
		if len(b) < groupHeader {
			return nil, ErrCorrupt
		}

		g := &e.Groups[i]
		g.Tail = b[0]
		tail, wID := b[1]&1 != 0, uint(b[2])
		m := int(binary.LittleEndian.Uint16(b[4:]))
		g.Total = binary.LittleEndian.Uint32(b[8:])
		base := binary.LittleEndian.Uint32(b[12:])
		size := int(binary.LittleEndian.Uint32(b[16:]))

		if wID > 32 || m > int(g.Total) || size > len(b)-groupHeader {
			return nil, ErrCorrupt
		}

		var err error
		if g.Entries, err = postings.ReadEntries(bits.NewReader(b[groupHeader:groupHeader+size], 0), base, wID, tail, m); err != nil {
			return nil, err
		}

		b = b[groupHeader+size:]
	}

	r := bits.NewReader(b, 0)

	keys, err := key5.ReadKeys(r, n)
	if err != nil {
		return nil, err
	}

	r = bits.NewReader(b[(r.Pos()+7)/8:], 0)
	e.Combos = make([]Combo, n)

	for i := range e.Combos {
		e.Combos[i] = Combo{Key: keys[i], Len: uint32(r.Read(wBytes))}
	}

	if r.Err() != nil {
		return nil, ErrCorrupt
	}

	return e, nil
}

// groupHeader is tail u8 | flags u8 | wID u8 | 0 | m u16 | 0 u16 | total u32
// | base u32 | size u32 (bytes of the entries).
const groupHeader = 20

// Block is a decoded combination block: per co-key, the lists of the
// expanded word and of the co-key over the documents with both.
type Block map[key5.Key]postings.Columns

// DecodeBlock reads a combination block, data being exactly its bytes.
func DecodeBlock(data []byte) (Block, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("%w: combination block", ErrCorrupt)
	}

	n := int(binary.LittleEndian.Uint16(data))
	base := binary.LittleEndian.Uint32(data[4:])
	r := bits.NewReader(data[8:], 0)

	keys, err := key5.ReadKeys(r, n)
	if err != nil {
		return nil, fmt.Errorf("%w: combination keys", ErrCorrupt)
	}

	r = bits.NewReader(data[8+(r.Pos()+7)/8:], 0)
	block := make(Block, n)

	for _, k := range keys {
		cols, err := postings.ReadColumns(r, base, 2)
		if err != nil {
			return nil, fmt.Errorf("bucket: combination with %s: %w", k, err)
		}

		block[k] = cols
	}

	return block, nil
}

// CheckDB checks a whole .db against the Meta of its .idx. Queries never
// read the header; the tests and the CLI do.
func CheckDB(data []byte, m Meta) error {
	if len(data) < DBHeader || string(data[:4]) != dbMagic {
		return fmt.Errorf("%w: not a .db", ErrCorrupt)
	}

	if f := binary.LittleEndian.Uint16(data[4:]); f != Format {
		return fmt.Errorf("bucket: .db format %d, this build reads %d", f, Format)
	}

	got := Meta{
		Slot:   binary.LittleEndian.Uint16(data[6:]),
		NormID: binary.LittleEndian.Uint32(data[8:]),
		Epoch:  binary.LittleEndian.Uint32(data[12:]),
		Seq:    binary.LittleEndian.Uint64(data[16:]),
	}

	if got != m {
		return fmt.Errorf("%w: .db of %+v, the .idx is %+v", ErrCorrupt, got, m)
	}

	if crc32.Checksum(data[DBHeader:], castagnoli) != binary.LittleEndian.Uint32(data[24:]) {
		return fmt.Errorf("%w: .db CRC", ErrCorrupt)
	}

	return nil
}
