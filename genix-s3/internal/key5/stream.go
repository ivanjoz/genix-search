package key5

import (
	"errors"
	"fmt"

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

// ErrCorrupt is a slot stream that does not decode.
var ErrCorrupt = errors.New("key5: corrupt slot stream")

// ReadSlot decodes the n keys of slot.
func ReadSlot(r *bits.Reader, slot uint16, n int) ([]Key, error) {
	keys := make([]Key, n)
	base := Key(slot) << 30

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

// AppendTokens writes a document's tokens (PLAN_5BIT.md §5.4): per token a
// header unit (1..8 = a word of that many letters, NUM_1..NUM_3 = a number)
// and its units, then End, then a 2-bit tail per word of 8 letters, then
// padding to the byte.
func AppendTokens(dst []byte, tokens []Token) []byte {
	w := bits.NewWriter(dst)

	for _, t := range tokens {
		n := t.Key.Len()

		if t.Key.IsNumber() {
			for i := range n {
				w.Write(uint64(t.Key.Unit(i)), 5)
			}

			continue
		}

		w.Write(uint64(n), 5)

		for i := range n {
			w.Write(uint64(t.Key.Unit(i)), 5)
		}
	}

	w.Write(uint64(End), 5)

	for _, t := range tokens {
		if !t.Key.IsNumber() && t.Key.Len() == MaxUnits {
			w.Write(uint64(t.Tail), 2)
		}
	}

	return w.Bytes()
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

		switch {
		case header == End:
		case header <= MaxUnits:
			for i := range int(header) {
				units[i] = uint8(r.Read(5))
				if u := units[i]; u == End || (u >= Num1 && u <= Num3) {
					return nil, ErrCorrupt
				}
			}

			tokens = append(tokens, Token{Key: FromUnits(units[:header])})

			continue
		case header >= Num1 && header <= Num3:
			n := int(header-Num1) + 1
			units[0] = header

			for i := 1; i <= n; i++ {
				units[i] = uint8(r.Read(5))
			}

			tokens = append(tokens, Token{Key: FromUnits(units[:n+1])})

			continue
		default:
			return nil, ErrCorrupt
		}

		break
	}

	for i := range tokens {
		if !tokens[i].Key.IsNumber() && tokens[i].Key.Len() == MaxUnits {
			tokens[i].Tail = uint8(r.Read(2))
		}
	}

	if r.Err() != nil {
		return nil, ErrCorrupt
	}

	return tokens, nil
}
