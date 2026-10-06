// Package postings is the bit-level codec of every list of the index:
// the posting lists of a word, whose size the
// dictionary computes from a few fields, and the self-delimiting lists of a
// combination of two words and of a head, which are decoded front to back.
package postings

import (
	"errors"
	"fmt"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
)

const (
	// PosBits is the width of a position, saturated at MaxPos.
	PosBits = 4
	// MaxPos is the largest position stored; later tokens share it.
	MaxPos   = 1<<PosBits - 1
	TailBits = 2
	// MinX keeps Tail|Codec|X at 6 bits or more, so a list of one posting
	// holds its position (4 bits) and tail (2) there.
	MinX = 4
)

// Codec is how the key gaps of a list are written (gaps.go).
const (
	CodecFixed  uint8 = 0 // every gap in W bits, a change of group escaped
	CodecNibble uint8 = 1 // varint of nibbles: 3 value bits + 1 continue bit
)

// escShift: the X of CodecFixed is W | escape nibbles << escShift.
const escShift = 6

// ErrCorrupt is a list that does not decode.
var ErrCorrupt = errors.New("postings: corrupt list")

// Posting is one occurrence of a key in a document.
type Posting struct {
	ID   docid.Key
	Pos  uint8 // position of the token in the document, saturated at MaxPos
	Tail uint8 // key5.Tail of the word
}

// Shape is what the dictionary records of a word's list: with it the list's
// size is known without reading it.
type Shape struct {
	Count   uint32    // postings, a document with the word twice counts twice
	First   docid.Key // key of the first posting
	HasTail bool
	Codec   uint8
	// X is W | escape nibbles << 6 for CodecFixed, the number of nibbles
	// for CodecNibble.
	X uint32
	// Pos and Tail of the only posting, when Count == 1.
	Pos, Tail uint8
}

// ShapeOf checks a list, sorted by (key, Pos), and picks the codec that
// writes its gaps in fewer bits.
func ShapeOf(list []Posting) (Shape, error) {
	if len(list) == 0 {
		return Shape{}, errors.New("postings: empty list")
	}

	sh := Shape{Count: uint32(len(list)), First: list[0].ID, Pos: list[0].Pos, Tail: list[0].Tail}

	for i, p := range list {
		if p.Pos > MaxPos || p.Tail > 3 {
			return sh, fmt.Errorf("postings: position %d or tail %d out of range", p.Pos, p.Tail)
		}

		sh.HasTail = sh.HasTail || p.Tail != 0

		if i > 0 && (p.ID < list[i-1].ID || (p.ID == list[i-1].ID && p.Pos < list[i-1].Pos)) {
			return sh, errors.New("postings: list not sorted by (key, Pos)")
		}
	}

	g := planGaps(len(list), func(i int) docid.Key { return list[i].ID })
	if g.escNibbles >= 1<<(32-escShift) || g.nibbles > 1<<32-1 {
		return sh, errors.New("postings: list too long")
	}

	if g.fixed {
		sh.Codec, sh.X = CodecFixed, uint32(g.w)|uint32(g.escNibbles)<<escShift
	} else {
		sh.Codec, sh.X = CodecNibble, uint32(g.nibbles)
	}

	return sh, nil
}

// gaps is how the list writes its gaps.
func (sh Shape) gaps() gaps {
	if sh.Codec == CodecNibble {
		return gaps{nibbles: uint64(sh.X)}
	}

	esc := uint64(sh.X >> escShift)

	return gaps{fixed: true, esc: esc > 0, w: uint(sh.X & (1<<escShift - 1)), escNibbles: esc}
}

// V is the Tail|Codec|X field of the dictionary: for a list of one posting it
// holds the posting's position and tail instead.
func (sh Shape) V() uint64 {
	if sh.Count == 1 {
		return uint64(sh.Pos) | uint64(sh.Tail)<<PosBits
	}

	v := uint64(sh.Codec)<<1 | uint64(sh.X)<<2
	if sh.HasTail {
		v |= 1
	}

	return v
}

// SetV is the inverse of V, once Count is set.
func (sh *Shape) SetV(v uint64) {
	if sh.Count == 1 {
		sh.Pos, sh.Tail = uint8(v&MaxPos), uint8(v>>PosBits&3)

		return
	}

	sh.HasTail, sh.Codec, sh.X = v&1 != 0, uint8(v>>1&1), uint32(v>>2)
}

// Bits is the size of the list; 0 for a list of one posting, which the
// dictionary holds.
func (sh Shape) Bits() uint64 {
	if sh.Count <= 1 {
		return 0
	}

	g := sh.gaps()
	gapBits := uint64(sh.Count-1)*uint64(g.w) + 4*g.escNibbles

	if !g.fixed {
		gapBits = 4 * g.nibbles
	}

	n := gapBits + PosBits*uint64(sh.Count)
	if sh.HasTail {
		n += TailBits * uint64(sh.Count)
	}

	return n
}

// Write writes the list (the gaps after the first ID, the positions, the
// tails). A list of one posting writes nothing.
func Write(w *bits.Writer, sh Shape, list []Posting) {
	if sh.Count <= 1 {
		return
	}

	g := sh.gaps()
	for i := 1; i < len(list); i++ {
		g.write(w, list[i-1].ID, list[i].ID)
	}

	for _, p := range list {
		w.Write(uint64(p.Pos), PosBits)
	}

	if sh.HasTail {
		for _, p := range list {
			w.Write(uint64(p.Tail), TailBits)
		}
	}
}

// Read decodes a list of shape sh from r, appending to dst.
func Read(r *bits.Reader, sh Shape, dst []Posting) ([]Posting, error) {
	if sh.Count == 0 {
		return dst, ErrCorrupt
	}

	if sh.Count == 1 {
		return append(dst, Posting{ID: sh.First, Pos: sh.Pos, Tail: sh.Tail}), nil
	}

	start := len(dst)
	id := sh.First
	dst = append(dst, Posting{ID: id})
	g := sh.gaps()

	if g.w > docid.Bits {
		return nil, fmt.Errorf("%w: gap width %d", ErrCorrupt, g.w)
	}

	for range sh.Count - 1 {
		var err error
		if id, err = g.read(r, id); err != nil {
			return nil, err
		}

		dst = append(dst, Posting{ID: id})
	}

	list := dst[start:]
	for i := range list {
		list[i].Pos = uint8(r.Read(PosBits))
	}

	if sh.HasTail {
		for i := range list {
			list[i].Tail = uint8(r.Read(TailBits))
		}
	}

	if r.Err() != nil {
		return nil, fmt.Errorf("%w: list is cut", ErrCorrupt)
	}

	return dst, nil
}

// NibbleCount is the nibbles of v as a varint of nibbles.
func NibbleCount(v uint64) uint {
	return max(1, (bits.Len(v)+2)/3)
}

// WriteNibbles writes v as a varint of nibbles: 3 value bits, low first, and
// a continue bit.
func WriteNibbles(w *bits.Writer, v uint64) {
	for {
		nibble := v & 7
		v >>= 3

		if v == 0 {
			w.Write(nibble, 4)

			return
		}

		w.Write(nibble|8, 4)
	}
}

// ReadNibbles reads what WriteNibbles wrote.
func ReadNibbles(r *bits.Reader) (uint64, error) {
	var v uint64

	for shift := uint(0); ; shift += 3 {
		if shift > 60 {
			return 0, fmt.Errorf("%w: varint too long", ErrCorrupt)
		}

		nibble := r.Read(4)
		v |= (nibble & 7) << shift

		if nibble&8 == 0 {
			break
		}

		if r.Err() != nil {
			return 0, fmt.Errorf("%w: varint is cut", ErrCorrupt)
		}
	}

	return v, r.Err()
}
