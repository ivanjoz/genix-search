package key5

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
)

func TestWordKeyOrderIsStringOrder(t *testing.T) {
	words := []string{"pollo", "polo", "polvo", "postre", "p", "po", "pol", "chocolate", "chocolatera", "zz", "a", "pi{a", "pi|"}

	keys := make([]Key, len(words))
	cut := make([]string, len(words))

	for i, w := range words {
		keys[i] = Word(w).Key
		cut[i] = w[:min(len(w), MaxUnits)]

		if got := keys[i].String(); got != cut[i] {
			t.Errorf("Word(%q).String() = %q, want %q", w, got, cut[i])
		}
	}

	slices.Sort(keys)
	slices.Sort(cut) // '{' and '|' sort after 'z', like codes 30 and 31

	for i := range keys {
		if keys[i].String() != cut[i] {
			t.Fatalf("key order %v differs from string order %v", keys, cut)
		}
	}
}

func TestNumbers(t *testing.T) {
	cases := []struct {
		digits string
		value  uint32
		units  int
	}{
		{"0", 0, 2}, {"007", 7, 2}, {"31", 31, 2}, {"32", 32, 3}, {"1023", 1023, 3},
		{"1024", 1024, 4}, {"32767", 32767, 4}, {"32768", 0, 2}, {"162641020", 162641020 & MaxNumber, 0},
	}

	for _, c := range cases {
		k := FromToken(c.digits).Key
		if !k.IsNumber() || k.Value() != c.value {
			t.Errorf("%s: key %s value %d, want %d", c.digits, k, k.Value(), c.value)
		}

		if c.units > 0 && k.Len() != c.units {
			t.Errorf("%s: %d units, want %d", c.digits, k.Len(), c.units)
		}
	}

	// Number keys sort by value, across NUM_1, NUM_2 and NUM_3.
	prev := Number(0)
	for v := uint32(1); v <= MaxNumber; v++ {
		k := Number(v)
		if k <= prev || k.Value() != v {
			t.Fatalf("Number(%d) = %s does not sort after %s", v, k, prev)
		}

		prev = k
	}
}

func TestNumberPrefixRanges(t *testing.T) {
	got := NumberPrefixRanges("50")
	want := [][2]uint32{{50, 50}, {500, 509}, {5000, 5099}}

	if !slices.Equal(got, want) {
		t.Errorf("NumberPrefixRanges(50) = %v, want %v", got, want)
	}

	if got := NumberPrefixRanges("3"); got[len(got)-1] != [2]uint32{30000, 32767} {
		t.Errorf("NumberPrefixRanges(3) ends at %v", got[len(got)-1])
	}
}

func TestHeaderCodes(t *testing.T) {
	seen := map[uint8]bool{}

	for l := range maxRest + 1 {
		for s := range l + 1 {
			code := HeaderCode(s, l)
			if code >= 32 || seen[code] || int(headerS[code]) != s || int(headerL[code]) != l {
				t.Fatalf("(s=%d, L=%d) → %d does not round-trip", s, l, code)
			}

			seen[code] = true
		}
	}

	if len(seen) != 28 {
		t.Fatalf("%d header codes, want 28", len(seen))
	}
}

func TestSlotStreamRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	bySlot := map[uint16][]Key{}

	for range 20000 {
		var k Key
		if r.IntN(5) == 0 {
			k = Number(uint32(r.IntN(MaxNumber + 1)))
		} else {
			n := 1 + r.IntN(12)
			var b strings.Builder

			for range n {
				b.WriteByte(byte('a' + r.IntN(4))) // few letters: many shared prefixes
			}

			k = Word(b.String()).Key
		}

		bySlot[k.Slot()] = append(bySlot[k.Slot()], k)
	}

	for slot, keys := range bySlot {
		slices.Sort(keys)
		keys = slices.Compact(keys)

		w := bits.NewWriter(nil)
		restarts, err := AppendSlot(w, keys)
		if err != nil {
			t.Fatal(err)
		}

		if want := (len(keys) - 1) / RestartEvery; len(restarts) != want {
			t.Fatalf("slot %d: %d restarts, want %d", slot, len(restarts), want)
		}

		got, err := ReadSlot(bits.NewReader(w.Bytes(), 0), slot, len(keys))
		if err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(got, keys) {
			t.Fatalf("slot %d does not round-trip", slot)
		}

		// A restart is a whole key: decoding from it gives the same keys.
		for i, off := range restarts {
			from := (i + 1) * RestartEvery

			got, err := ReadSlot(bits.NewReader(w.Bytes(), 5*uint64(off)), slot, len(keys)-from)
			if err != nil || !slices.Equal(got, keys[from:]) {
				t.Fatalf("slot %d: restart %d does not decode", slot, i)
			}
		}
	}
}

func TestSlotStreamExample(t *testing.T) {
	keys := []Key{Word("pollo").Key, Word("polo").Key, Word("polvo").Key, Word("postre").Key}
	w := bits.NewWriter(nil)

	if _, err := AppendSlot(w, keys); err != nil {
		t.Fatal(err)
	}

	r := bits.NewReader(w.Bytes(), 0)
	var units []uint8

	for range 14 {
		units = append(units, uint8(r.Read(5)))
	}

	l, o, v, s, tt, re := Unit('l'), Unit('o'), Unit('v'), Unit('s'), Unit('t'), Unit('r')
	want := []uint8{
		HeaderCode(0, 3), l, l, o,
		HeaderCode(1, 2), o,
		HeaderCode(1, 3), v, o,
		HeaderCode(0, 4), s, tt, re, Unit('e'),
	}

	if !slices.Equal(units, want) {
		t.Errorf("units %v, want %v", units, want)
	}
}

func TestTokensRoundTrip(t *testing.T) {
	words := []string{"polo", "spiderman", "rojo", "50", "ml", "x", "chocolatera", "32767", "0", "pi{a", "congelad"}

	tokens := make([]Token, len(words))
	for i, w := range words {
		tokens[i] = FromToken(w)
	}

	data := AppendTokens(nil, tokens)

	got, err := ReadTokens(data)
	if err != nil {
		t.Fatal(err)
	}

	if !slices.Equal(got, tokens) {
		t.Fatalf("ReadTokens = %v, want %v", got, tokens)
	}

	if empty, err := ReadTokens(AppendTokens(nil, nil)); err != nil || len(empty) != 0 {
		t.Fatalf("empty document: %v %v", empty, err)
	}

	if _, err := ReadTokens(data[:len(data)/2]); err == nil {
		t.Error("a cut token stream decoded")
	}
}

func TestTail(t *testing.T) {
	if Tail("chocolat") != 0 || Tail("polo") != 0 {
		t.Error("words of up to 8 letters have tail 0")
	}

	for _, w := range []string{"chocolate", "chocolatera", "chocolatina"} {
		if tail := Tail(w); tail < 1 || tail > 3 {
			t.Errorf("Tail(%q) = %d", w, tail)
		}
	}
}

func TestKeyDistance(t *testing.T) {
	units := func(s string) []uint8 {
		var u []uint8
		for i := range len(s) {
			u = append(u, Unit(s[i]))
		}

		return u
	}

	cases := []struct {
		key, query string
		budget     int
		want       int
		ok         bool
	}{
		{"chocolat", "chocoate", 1, 1, true},
		{"chocolat", "chocolate", 1, 0, true},
		{"spiderma", "spidreman", 2, 1, true},
		{"polo", "pola", 1, 1, true},
		{"polo", "plato", 1, 0, false},
		{"rojo", "rojo", 1, 0, true},
	}

	for _, c := range cases {
		d, ok := KeyDistance(units(c.key), units(c.query), c.budget)
		if ok != c.ok || (ok && d != c.want) {
			t.Errorf("KeyDistance(%s, %s) = %d %v, want %d %v", c.key, c.query, d, ok, c.want, c.ok)
		}
	}
}

// TestContinuations: a one-letter word is a key of every host word before it,
// in the host's slot, right after the host; every stream carries it whole.
func TestContinuations(t *testing.T) {
	str := func(tokens []Token) string {
		var out []string
		for _, tok := range tokens {
			out = append(out, tok.Key.String())
		}

		return strings.Join(out, " ")
	}

	cases := map[string]string{
		"polo rojo m":           "polo rojo rojo›m polo›m",
		"m polo":                "polo",                         // nothing before the letter
		"polo rojo azul m":      "polo rojo azul azul›m rojo›m", // the 2 nearest
		"polo rojo azul polo m": "polo rojo azul polo polo›m azul›m",
		"polo polo m":           "polo polo polo›m",          // one continuation per host
		"vitamina c 500 mg":     "vitamina vitami›c #500 mg", // the host cut to 6 units
		"talla 40 x l":          "talla #40 talla›x talla›l", // numbers and letters are not hosts
		"camiseta camisetas m":  "camiseta camiseta camise›m",
	}

	for in, want := range cases {
		tokens := FromTokens(strings.Fields(in))
		if got := str(tokens); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}

		back, err := ReadTokens(AppendTokens(nil, tokens))
		if err != nil || !slices.Equal(back, tokens) {
			t.Errorf("%q: stream %v %v", in, err, back)
		}
	}

	talla := Word("talla").Key
	m := Continuation(talla, Unit('m'))

	if !m.IsContinuation() || m.IsHost() || talla.IsContinuation() || !talla.IsHost() || Word("m").Key.IsHost() || m.Slot() != talla.Slot() {
		t.Fatal("continuation predicates")
	}

	if !(talla < m && m < Word("tallaa").Key) || m.Len() != 7 {
		t.Fatalf("talla›m sorts at %v, len %d", m, m.Len())
	}

	// The dictionary of a slot and a list of keys of any slot.
	keys := []Key{talla, m, Continuation(talla, Unit('x')), Word("tallas").Key, Continuation(Word("tallarin").Key, Unit('a'))}
	slices.Sort(keys)

	w := bits.NewWriter(nil)
	if err := AppendBucket(w, keys); err != nil {
		t.Fatal(err)
	}

	if got, err := ReadSlot(bits.NewReader(w.Bytes(), 0), talla.Slot(), len(keys)); err != nil || !slices.Equal(got, keys) {
		t.Fatalf("slot stream: %v %v", got, err)
	}

	w = bits.NewWriter(nil)
	if err := AppendKeys(w, keys); err != nil {
		t.Fatal(err)
	}

	if got, err := ReadKeys(bits.NewReader(w.Bytes(), 0), len(keys)); err != nil || !slices.Equal(got, keys) {
		t.Fatalf("key stream: %v %v", got, err)
	}
}
