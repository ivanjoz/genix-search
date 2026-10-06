package key5

import (
	"errors"
	"fmt"
	"math"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
)

// RestartEvery is how often a slot stream stores a whole key (s = 0), so a
// reader can start decoding there.
const RestartEvery = 16

// The header of a slot entry is one unit: code = L·(L+1)/2 + s, with L the
// units after the slot and s those shared with the previous key, 0 ≤ s ≤ L ≤ 6.
const maxRest = MaxUnits - 2

var headerS, headerL [32]int8

func init() {
	for i := range headerS {
		headerS[i], headerL[i] = -1, -1
	}

	for l := range maxRest + 1 {
		for s := range l + 1 {
			code := l*(l+1)/2 + s
			headerS[code], headerL[code] = int8(s), int8(l)
		}
	}
}

// HeaderCode is the header unit of an entry.
func HeaderCode(s, l int) uint8 {
	return uint8(l*(l+1)/2 + s)
}

// rest is the number of units after the slot.
func (k Key) rest() int {
	return max(k.Len()-2, 0)
}

// AppendSlot writes the keys of one slot, sorted and distinct, front-coded
// against the previous key. It returns the offset in units of every restart
// key (16, 32, …).
func AppendSlot(w *bits.Writer, keys []Key) ([]uint32, error) {
	var restarts []uint32
	var units uint32
	var prev Key

	for i, k := range keys {
		if i > 0 && (k <= prev || k.Slot() != prev.Slot()) {
			return nil, fmt.Errorf("key5: slot keys out of order: %s after %s", k, prev)
		}

		l := k.rest()
		s := 0

		if i%RestartEvery == 0 {
			if i > 0 {
				restarts = append(restarts, units)
			}
		} else {
			for s < l && s < prev.rest() && k.Unit(2+s) == prev.Unit(2+s) {
				s++
			}
		}

		w.Write(uint64(HeaderCode(s, l)), 5)

		for j := s; j < l; j++ {
			w.Write(uint64(k.Unit(2+j)), 5)
		}

		units += uint32(1 + l - s)
		prev = k
	}

	return restarts, nil
}

// AppendBucket writes the keys of one slot and one space as AppendSlot does,
// without restarts: what a bucket's dictionary holds, which is always decoded
// whole. ReadSlot reads it.
func AppendBucket(w *bits.Writer, keys []Key) error {
	var prev Key

	for i, k := range keys {
		if i > 0 && (k <= prev || k.Slot() != prev.Slot() || k.Space() != prev.Space()) {
			return fmt.Errorf("key5: slot keys out of order: %s after %s", k, prev)
		}

		l := k.rest()
		s := 0

		if i > 0 {
			for s < l && s < prev.rest() && k.Unit(2+s) == prev.Unit(2+s) {
				s++
			}
		}

		w.Write(uint64(HeaderCode(s, l)), 5)

		for j := s; j < l; j++ {
			w.Write(uint64(k.Unit(2+j)), 5)
		}

		prev = k
	}

	return nil
}

// AppendKeys writes keys of any slot and space, sorted and distinct,
// front-coded: a bit set when some keys are not words, and then how many
// group words and signatures end the keys (nibble varints); then per key the
// units it shares with the previous one (3 bits), its
// units − 1 (3 bits), and its other units.
func AppendKeys(w *bits.Writer, keys []Key) error {
	var spaces [Spaces]uint64

	for _, k := range keys {
		if k.Space() >= Spaces {
			return fmt.Errorf("key5: key %x of no space", uint64(k))
		}

		spaces[k.Space()]++
	}

	if spaces[SpaceGroupWord]+spaces[SpaceSignature] == 0 {
		w.Write(0, 1)
	} else {
		w.Write(1, 1)
		writeNibbles(w, spaces[SpaceGroupWord])
		writeNibbles(w, spaces[SpaceSignature])
	}

	var prev Key
	prevLen := 0

	for i, k := range keys {
		if i > 0 && k <= prev {
			return fmt.Errorf("key5: keys out of order: %s after %s", k, prev)
		}

		l := k.Len()
		if l == 0 {
			return errors.New("key5: empty key")
		}

		s := 0
		for s < l && s < prevLen && s < MaxUnits-1 && k.Unit(s) == prev.Unit(s) {
			s++
		}

		w.Write(uint64(s), 3)
		w.Write(uint64(l-1), 3)

		for j := s; j < l; j++ {
			w.Write(uint64(k.Unit(j)), 5)
		}

		prev, prevLen = k, l
	}

	return nil
}

// ReadKeys decodes n keys written by AppendKeys.
func ReadKeys(r *bits.Reader, n int) ([]Key, error) {
	keys := make([]Key, n)

	// The first key of each space: the keys are sorted, so by space.
	from := [Spaces]int{SpaceGroupWord: n, SpaceSignature: n}

	if r.Read(1) == 1 {
		groupWords, sigs := readNibbles(r), readNibbles(r)
		if groupWords+sigs > uint64(n) {
			return nil, ErrCorrupt
		}

		from[SpaceSignature] = n - int(sigs)
		from[SpaceGroupWord] = from[SpaceSignature] - int(groupWords)
	}

	var prev Key
	prevLen := 0

	for i := range keys {
		s, l := int(r.Read(3)), int(r.Read(3))+1
		if s > prevLen || s > l {
			return nil, ErrCorrupt
		}

		space := SpaceWord
		if i >= from[SpaceSignature] {
			space = SpaceSignature
		} else if i >= from[SpaceGroupWord] {
			space = SpaceGroupWord
		}

		k := prev.prefix(s).In(space)
		for j := s; j < l; j++ {
			k |= Key(r.Read(5)) << (35 - 5*j)
		}

		if r.Err() != nil || (i > 0 && k <= prev) {
			return nil, ErrCorrupt
		}

		keys[i] = k
		prev, prevLen = k, l
	}

	return keys, nil
}

// prefix keeps the first n units.
func (k Key) prefix(n int) Key {
	if n >= MaxUnits {
		return k
	}

	return k &^ (Key(1)<<(40-5*n) - 1)
}

// Prefix keeps the first n units of the key and zeroes the rest: the key of
// the group of keys sharing those units.
func (k Key) Prefix(n int) Key { return k.prefix(n) }

// ErrCorrupt is a slot stream that does not decode.
var ErrCorrupt = errors.New("key5: corrupt slot stream")

// ReadSlot decodes the n keys of slot in space.
func ReadSlot(r *bits.Reader, slot uint16, space Space, n int) ([]Key, error) {
	keys := make([]Key, n)
	base := (Key(slot) << 30).In(space)

	var prev Key

	for i := range n {
		code := r.Read(5)
		s, l := int(headerS[code]), int(headerL[code])

		if l < 0 || (i > 0 && s > prev.rest()) || (i == 0 && s > 0) {
			return nil, ErrCorrupt
		}

		k := base
		for j := range s {
			k |= Key(prev.Unit(2+j)) << (35 - 5*(2+j))
		}

		for j := s; j < l; j++ {
			k |= Key(r.Read(5)) << (35 - 5*(2+j))
		}

		if (i > 0 && k <= prev) || k.rest() != l {
			return nil, ErrCorrupt
		}

		keys[i] = k
		prev = k
	}

	if r.Err() != nil {
		return nil, r.Err()
	}

	return keys, nil
}

// Header units of the token stream that mark the space of the next token.
// A group word follows its mark as a word or a
// number does; a signature follows its mark as its length and its units.
const (
	GroupWordMark uint8 = 30
	SignatureMark uint8 = 31
)

// AppendTokens writes a document's tokens: per token a
// header unit (1..8 = a word of that many letters, NUM_1..NUM_3 = a number,
// after GroupWordMark or SignatureMark for the group's tokens) and its units,
// then End, then a 2-bit tail per word of the text of 8 letters, then padding
// to the byte.
func AppendTokens(dst []byte, tokens []Token) []byte {
	w := bits.NewWriter(dst)

	for _, t := range tokens {
		n := t.Key.Len()

		switch t.Key.Space() {
		case SpaceGroupWord:
			w.Write(uint64(GroupWordMark), 5)
		case SpaceSignature:
			w.Write(uint64(SignatureMark), 5)
		}

		if !t.Key.IsNumber() {
			w.Write(uint64(n), 5)
		}

		for i := range n {
			w.Write(uint64(t.Key.Unit(i)), 5)
		}
	}

	w.Write(uint64(End), 5)

	for _, t := range tokens {
		if hasTail(t.Key) {
			w.Write(uint64(t.Tail), 2)
		}
	}

	return w.Bytes()
}

// hasTail reports a key whose token stream keeps the tail: a word of the
// text cut at 8 letters.
func hasTail(k Key) bool {
	return k.Space() == SpaceWord && !k.IsNumber() && k.Len() == MaxUnits
}

// ReadTokens decodes what AppendTokens wrote.
func ReadTokens(data []byte) ([]Token, error) {
	r := bits.NewReader(data, 0)

	var tokens []Token
	var units [MaxUnits]uint8

	for {
		header := uint8(r.Read(5))
		if r.Err() != nil {
			return nil, ErrCorrupt
		}

		space := SpaceWord

		switch header {
		case GroupWordMark:
			space, header = SpaceGroupWord, uint8(r.Read(5))
		case SignatureMark:
			space, header = SpaceSignature, uint8(r.Read(5))
		}

		switch {
		case header == End && space == SpaceWord:
		case space == SpaceSignature && header >= 1 && header <= MaxUnits:
			// Any unit: 0 pads a word of one letter, NUM_n starts a number.
			for i := range int(header) {
				units[i] = uint8(r.Read(5))
			}

			if units[0] == End || units[header-1] == End {
				return nil, ErrCorrupt
			}

			tokens = append(tokens, Token{Key: FromUnits(units[:header]).In(space)})

			continue
		case header >= 1 && header <= MaxUnits && (space == SpaceWord || header >= 2):
			// End only as the mark of a continuation of the text: after a
			// host of two letters or more, before the last unit.
			for i := range int(header) {
				units[i] = uint8(r.Read(5))
				if u := units[i]; (u >= Num1 && u <= Num3) || (u == End && (i != int(header)-2 || header < 4 || space != SpaceWord)) {
					return nil, ErrCorrupt
				}
			}

			if units[header-1] == End {
				return nil, ErrCorrupt
			}

			tokens = append(tokens, Token{Key: FromUnits(units[:header]).In(space)})

			continue
		case header >= Num1 && header <= Num3 && space != SpaceSignature:
			n := int(header-Num1) + 1
			units[0] = header

			for i := 1; i <= n; i++ {
				units[i] = uint8(r.Read(5))
			}

			tokens = append(tokens, Token{Key: FromUnits(units[:n+1]).In(space)})

			continue
		default:
			return nil, ErrCorrupt
		}

		break
	}

	for i := range tokens {
		if hasTail(tokens[i].Key) {
			tokens[i].Tail = uint8(r.Read(2))
		}
	}

	if r.Err() != nil {
		return nil, ErrCorrupt
	}

	return tokens, nil
}

// writeNibbles writes v as a varint of nibbles: 3 value bits, low first, and
// a continue bit (postings.WriteNibbles, which key5 cannot import).
func writeNibbles(w *bits.Writer, v uint64) {
	for v >= 8 {
		w.Write(v&7|8, 4)
		v >>= 3
	}

	w.Write(v, 4)
}

// readNibbles reads what writeNibbles wrote; a stream that does not end
// reads as a huge value, which the caller rejects.
func readNibbles(r *bits.Reader) uint64 {
	var v uint64

	for shift := uint(0); shift <= 60; shift += 3 {
		nibble := r.Read(4)
		v |= (nibble & 7) << shift

		if nibble&8 == 0 || r.Err() != nil {
			return v
		}
	}

	return math.MaxUint64
}
