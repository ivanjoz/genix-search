// Package key5 turns tokens into keys of 5-bit units (PLAN_5BIT.md §2–4):
// a word is its first 8 letters, a number is NUM_n plus n units holding its
// value in binary. The first two units are the key's slot.
//
// A word of one letter has no key of its own: it is a continuation of the
// two words before it in the document (PLAN_BUCKETS.md §16). "Polo rojo M"
// holds rojo›m and polo›m, each in the slot of its host word.
package key5

import (
	"hash/crc32"
	"slices"
	"strconv"
	"strings"

	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

// The 5-bit table.
const (
	End    uint8 = 0  // end of word; inside a key, the mark of a continuation
	Num1   uint8 = 27 // followed by 1 unit: uint5
	Num2   uint8 = 28 // followed by 2 units: uint10
	Num3   uint8 = 29 // followed by 3 units: uint15
	Extra1 uint8 = 30 // configurable letter (normalize.Extra1)
	Extra2 uint8 = 31 // configurable letter (normalize.Extra2)
)

// MaxUnits is the longest key: 8 letters, or NUM_3 and its 3 units.
const MaxUnits = 8

// MaxNumber is the largest value a number key holds; larger values keep
// their low 15 bits.
const MaxNumber = 1<<15 - 1

// Slots is the number of slots: every pair of units.
const Slots = 1024

// Key is a key of up to 8 units, unit 0 in the highest 5 of 40 bits and the
// unused units at 0. Comparing two Keys as integers compares them unit by
// unit, a shorter word first: the order of the index.
type Key uint64

// Token is a key and the tail of the word it came from (§3.4).
type Token struct {
	Key  Key
	Tail uint8
}

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Unit is the code of a normalized letter, 0 for anything else.
func Unit(c byte) uint8 {
	switch {
	case c >= 'a' && c <= 'z':
		return c - 'a' + 1
	case c == normalize.Extra1:
		return Extra1
	case c == normalize.Extra2:
		return Extra2
	}

	return 0
}

// Letter is the normalized byte of a letter unit.
func Letter(u uint8) byte {
	switch {
	case u >= 1 && u <= 26:
		return 'a' + u - 1
	case u == Extra1:
		return normalize.Extra1
	case u == Extra2:
		return normalize.Extra2
	}

	return '?'
}

// FromUnits packs up to 8 units.
func FromUnits(units []uint8) Key {
	var k Key
	for i, u := range units[:min(len(units), MaxUnits)] {
		k |= Key(u&31) << (35 - 5*i)
	}

	return k
}

// Unit returns unit i (0..7).
func (k Key) Unit(i int) uint8 {
	return uint8(k>>(35-5*i)) & 31
}

// Slot is the first two units.
func (k Key) Slot() uint16 {
	return uint16(k>>30) & (Slots - 1)
}

// IsNumber reports a NUM_n key.
func (k Key) IsNumber() bool {
	u := k.Unit(0)

	return u >= Num1 && u <= Num3
}

// Len is the number of units: n+1 for a number, the letters for a word,
// and for a continuation its host's, the mark and the letter.
func (k Key) Len() int {
	if k.IsNumber() {
		return int(k.Unit(0)-Num1) + 2
	}

	n := MaxUnits
	for n > 0 && k.Unit(n-1) == End {
		n--
	}

	return n
}

// MaxHost is the units of the host word a continuation keeps.
const MaxHost = MaxUnits - 2

// Continuation is the key of the one-letter word letter after the word host
// (§16): host's first MaxHost units, End and the letter. It sorts right after
// host and lives in host's slot.
func Continuation(host Key, letter uint8) Key {
	n := min(host.Len(), MaxHost)

	return host.prefix(n) | Key(letter&31)<<(35-5*(n+1))
}

// IsContinuation reports the key of a one-letter word after a host word.
func (k Key) IsContinuation() bool {
	n := k.Len()

	return !k.IsNumber() && n >= 4 && k.Unit(n-2) == End
}

// IsHost reports a key a one-letter word continues: a word of two letters
// or more.
func (k Key) IsHost() bool {
	return !k.IsNumber() && !k.IsContinuation() && k.Len() >= 2
}

// Units appends the key's units to dst.
func (k Key) Units(dst []uint8) []uint8 {
	for i := range k.Len() {
		dst = append(dst, k.Unit(i))
	}

	return dst
}

// Value is the number a NUM_n key holds.
func (k Key) Value() uint32 {
	var v uint32
	for i := 1; i < k.Len(); i++ {
		v = v<<5 | uint32(k.Unit(i))
	}

	return v
}

// String is the word, or "#" and the value of a number.
func (k Key) String() string {
	if k.IsNumber() {
		return "#" + strconv.Itoa(int(k.Value()))
	}

	var b strings.Builder
	for i := range k.Len() {
		if u := k.Unit(i); u == End {
			b.WriteString("›")
		} else {
			b.WriteByte(Letter(u))
		}
	}

	return b.String()
}

// PrefixRange is the range of keys whose first n units are those of k:
// lo ≤ key ≤ hi.
func (k Key) PrefixRange(n int) (lo, hi Key) {
	if n >= MaxUnits {
		return k, k
	}

	rest := Key(1)<<(40-5*n) - 1

	return k &^ rest, k | rest
}

// Word is the token of a normalized word (letters only): its first 8
// letters, and the tail of the rest.
func Word(word string) Token {
	units := make([]uint8, 0, MaxUnits)
	for i := 0; i < len(word) && i < MaxUnits; i++ {
		units = append(units, Unit(word[i]))
	}

	return Token{Key: FromUnits(units), Tail: Tail(word)}
}

// Tail is 0 for a word of up to 8 letters and 1 + CRC32C(letters 9 on) mod 3
// for a longer one: 2 bits that tell apart most words sharing a key.
func Tail(word string) uint8 {
	if len(word) <= MaxUnits {
		return 0
	}

	return 1 + uint8(crc32.Checksum([]byte(word[MaxUnits:]), castagnoli)%3)
}

// Number is the key of a value, in the smallest NUM_n it fits; a value over
// MaxNumber keeps its low 15 bits.
func Number(v uint32) Key {
	v &= MaxNumber

	switch {
	case v < 1<<5:
		return FromUnits([]uint8{Num1, uint8(v)})
	case v < 1<<10:
		return FromUnits([]uint8{Num2, uint8(v >> 5), uint8(v & 31)})
	}

	return FromUnits([]uint8{Num3, uint8(v >> 10), uint8(v >> 5 & 31), uint8(v & 31)})
}

// NumberValue is the value of a run of digits, mod 2^15: leading zeros do
// not count ("007" is 7).
func NumberValue(digits string) uint32 {
	var v uint32
	for i := 0; i < len(digits); i++ {
		v = (v*10 + uint32(digits[i]-'0')) & MaxNumber
	}

	return v
}

// FromToken is the token of a normalized token: a number or a word.
func FromToken(token string) Token {
	if normalize.IsNumber(token) {
		return Token{Key: Number(NumberValue(token))}
	}

	return Word(token)
}

// IsLetter reports a normalized token of one letter: a continuation, not a
// word of its own.
func IsLetter(token string) bool {
	return len(token) == 1 && !normalize.IsNumber(token)
}

// MaxContinued is how many host words a one-letter word continues: the
// nearest before it. "Polo rojo azul M" holds azul›m and rojo›m, not polo›m.
const MaxContinued = 2

// FromTokens converts a document's normalized tokens. A one-letter word
// becomes a continuation of the MaxContinued nearest distinct host words
// before it, the nearest first, at consecutive positions; with no host
// before it, it is dropped.
func FromTokens(tokens []string) []Token {
	out := make([]Token, 0, len(tokens))
	var hosts []Key // distinct, the most recent last

	for _, token := range tokens {
		if !IsLetter(token) {
			t := FromToken(token)
			if t.Key.IsHost() {
				hosts = append(slices.DeleteFunc(hosts, func(k Key) bool { return k == t.Key }), t.Key)
			}

			out = append(out, t)

			continue
		}

		letter := Unit(token[0])
		var done []Key

		for i := len(hosts) - 1; i >= 0 && len(done) < MaxContinued; i-- {
			if k := Continuation(hosts[i], letter); !slices.Contains(done, k) {
				done = append(done, k)
				out = append(out, Token{Key: k})
			}
		}
	}

	return out
}

// NumberPrefixRanges are the values whose decimal form starts with digits,
// up to MaxNumber: "50" is 50, 500–509, 5000–5099, … Each is a contiguous
// range of keys, since number keys sort by value.
func NumberPrefixRanges(digits string) [][2]uint32 {
	if len(digits) > 5 {
		v := NumberValue(digits)

		return [][2]uint32{{v, v}}
	}

	p, _ := strconv.Atoi(digits)
	ranges := [][2]uint32{{uint32(p), uint32(p)}}

	if digits[0] == '0' {
		return ranges // "05" is 5, not a prefix of other numbers
	}

	for scale := 10; p*scale <= MaxNumber; scale *= 10 {
		lo, hi := p*scale, (p+1)*scale-1
		ranges = append(ranges, [2]uint32{uint32(lo), uint32(min(hi, MaxNumber))})
	}

	return ranges
}
