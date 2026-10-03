package postings

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
)

// randomList is a list sorted by (ID, Pos): sparse or dense IDs, some
// documents with the word twice, tails or not. n × spread must stay under
// 2³² − 2²⁰.
func randomList(r *rand.Rand, n int, spread uint32, tails bool) []Posting {
	var list []Posting

	id := r.Uint32N(1 << 20)

	for len(list) < n {
		id += 1 + r.Uint32N(spread)

		repeats := 1
		if r.IntN(8) == 0 {
			repeats = 2 + r.IntN(3)
		}

		pos := uint8(r.IntN(4))

		for range repeats {
			p := Posting{ID: id, Pos: min(pos, MaxPos)}
			if tails {
				p.Tail = uint8(r.IntN(4))
			}

			list = append(list, p)
			pos += uint8(1 + r.IntN(6))
		}
	}

	return list
}

func TestListRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))

	for i := range 400 {
		spread := []uint32{1, 30, 1 << 12, 1 << 23}[i%4]
		list := randomList(r, 1+r.IntN(300), spread, i%3 == 0)

		sh, err := ShapeOf(list)
		if err != nil {
			t.Fatal(err)
		}

		// The dictionary keeps Count, First and V; the reader rebuilds the rest.
		var back Shape
		back.Count, back.First = sh.Count, sh.First
		back.SetV(sh.V())

		w := bits.NewWriter([]byte{0xff}) // not at bit 0
		Write(w, sh, list)

		if got := w.BitLen() - 8; got != back.Bits() {
			t.Fatalf("list %d: wrote %d bits, the shape says %d", i, got, back.Bits())
		}

		out, err := Read(bits.NewReader(w.Bytes(), 8), back, nil)
		if err != nil || !slices.Equal(out, list) {
			t.Fatalf("list %d (%+v): %v\n got %v\nwant %v", i, sh, err, out, list)
		}
	}
}

func TestColumnsRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))

	for i := range 300 {
		a := randomList(r, 1+r.IntN(100), []uint32{1, 50, 1 << 25}[i%3], false)

		// A second column over the same documents, with its own repeats and
		// tails.
		var b []Posting
		for _, id := range docIDs(a) {
			if n := len(b); n > 0 && b[n-1].ID == id {
				continue
			}

			for k := range 1 + r.IntN(2) {
				b = append(b, Posting{ID: id, Pos: uint8(3 + 2*k), Tail: uint8(r.IntN(4)) * uint8(i%2)})
			}
		}

		base := a[0].ID - r.Uint32N(min(a[0].ID+1, 1000))
		w := bits.NewWriter(nil)

		if err := WriteColumns(w, base, Columns{a, b}); err != nil {
			t.Fatal(err)
		}

		// Self-delimiting: a second copy right after decodes the same.
		if err := WriteColumns(w, base, Columns{a, b}); err != nil {
			t.Fatal(err)
		}

		rd := bits.NewReader(w.Bytes(), 0)

		for copy := range 2 {
			cols, err := ReadColumns(rd, base, 2)
			if err != nil || !slices.Equal(cols[0], a) || !slices.Equal(cols[1], b) {
				t.Fatalf("columns %d, copy %d: %v\n got %v\nwant %v %v", i, copy, err, cols, a, b)
			}
		}
	}

	if err := WriteColumns(bits.NewWriter(nil), 0, Columns{{{ID: 1}}, {{ID: 2}}}); err == nil {
		t.Error("columns over different documents were accepted")
	}
}

func TestEntriesRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))

	for i := range 200 {
		list := randomList(r, 1+r.IntN(80), 1<<16, i%2 == 0)

		var entries []Entry
		for j := 0; j < len(list); {
			k := j + 1
			for k < len(list) && list[k].ID == list[j].ID {
				k++
			}

			entries = append(entries, Entry{ID: list[j].ID, Postings: list[j:k]})
			j = k
		}

		// A head is not in ID order.
		r.Shuffle(len(entries), func(x, y int) { entries[x], entries[y] = entries[y], entries[x] })

		base, top := entries[0].ID, entries[0].ID
		for _, e := range entries {
			base, top = min(base, e.ID), max(top, e.ID)
		}

		wID := bits.Len(uint64(top - base))
		w := bits.NewWriter(nil)
		WriteEntries(w, base, wID, i%2 == 0, entries)

		out, err := ReadEntries(bits.NewReader(w.Bytes(), 0), base, wID, i%2 == 0, len(entries))
		if err != nil || !slices.EqualFunc(out, entries, func(x, y Entry) bool { return x.ID == y.ID && slices.Equal(x.Postings, y.Postings) }) {
			t.Fatalf("entries %d: %v", i, err)
		}
	}
}

func TestNibbles(t *testing.T) {
	values := []uint64{0, 1, 7, 8, 63, 64, 1 << 20, 1<<32 - 1, 1 << 40}
	w := bits.NewWriter(nil)

	for _, v := range values {
		WriteNibbles(w, v)
	}

	r := bits.NewReader(w.Bytes(), 0)

	for _, v := range values {
		if got, err := ReadNibbles(r); err != nil || got != v {
			t.Errorf("%d: got %d, %v", v, got, err)
		}
	}

	var total uint64
	for _, v := range values {
		total += 4 * uint64(NibbleCount(v))
	}

	if total != w.BitLen() {
		t.Errorf("NibbleCount says %d bits, %d written", total, w.BitLen())
	}
}
