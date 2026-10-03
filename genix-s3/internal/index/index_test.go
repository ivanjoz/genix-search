package index

import (
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// randomLists builds lists over random words and numbers, with random uint32
// IDs: dense runs, sparse IDs, repeated IDs (a word twice in a document) and
// lists of one posting.
func randomLists(r *rand.Rand, n int) []List {
	byKey := map[key5.Key][]Posting{}

	for range n {
		var k key5.Key
		var tail uint8

		if r.IntN(6) == 0 {
			k = key5.Number(uint32(r.IntN(key5.MaxNumber + 1)))
		} else {
			var b strings.Builder
			for range 1 + r.IntN(11) {
				b.WriteByte(byte('a' + r.IntN(6)))
			}

			t := key5.Word(b.String())
			k, tail = t.Key, t.Tail
		}

		count := 1
		if r.IntN(2) == 0 {
			count = 1 + r.IntN(200)
		}

		dense := r.IntN(2) == 0
		id := r.Uint32() >> uint(r.IntN(32))

		for range count {
			byKey[k] = append(byKey[k], Posting{ID: id, Pos: uint8(r.IntN(MaxPos + 1)), Tail: tail})

			switch {
			case r.IntN(10) == 0: // the same document again
			case dense:
				id += uint32(1 + r.IntN(3))
			default:
				id += r.Uint32() >> 8
			}
		}
	}

	lists := make([]List, 0, len(byKey))
	for k, postings := range byKey {
		slices.SortFunc(postings, func(a, b Posting) int {
			if a.ID != b.ID {
				return cmpU32(a.ID, b.ID)
			}

			return int(a.Pos) - int(b.Pos)
		})

		lists = append(lists, List{Key: k, Postings: postings})
	}

	slices.SortFunc(lists, func(a, b List) int { return cmpU64(uint64(a.Key), uint64(b.Key)) })

	return lists
}

func cmpU32(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

func cmpU64(a, b uint64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}

const testNormID = 0xC0FFEE

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(5, 6))
	lists := randomLists(r, 5000)

	k, p, err := Build(Meta{Gen: 7, Docs: 1234, NormID: testNormID}, lists)
	if err != nil {
		t.Fatal(err)
	}

	h, err := ParseHead(k[:HeadSize], testNormID)
	if err != nil {
		t.Fatal(err)
	}

	if h.Gen != 7 || h.Docs != 1234 || int(h.Keys) != len(lists) || h.PSize != uint64(len(p)) || int(h.KSize()) != len(k) {
		t.Fatalf("head %+v", h)
	}

	if _, err := ParseHead(k[:HeadSize], testNormID+1); err == nil {
		t.Fatal("a k with another NormID was accepted")
	}

	crcs := PageCRCs(k[h.PageCRCOff:])
	if err := CheckPages(p, 0, h.PSize, crcs, 0); err != nil {
		t.Fatal(err)
	}

	at := 0
	single := 0

	for s := range uint16(key5.Slots) {
		off, end := h.Block(s)

		slot, err := ParseSlot(h, s, k[off:end])
		if err != nil {
			t.Fatal(err)
		}

		for i := range slot.Entries {
			e := &slot.Entries[i]
			want := lists[at]
			at++

			if e.Key != want.Key {
				t.Fatalf("slot %d entry %d: key %s, want %s", s, i, e.Key, want.Key)
			}

			// Decode from exactly the bytes of the list, as a query does.
			from, to := e.Bytes()

			got, err := Decode(e, p[from:to], from, nil)
			if err != nil {
				t.Fatalf("%s: %v", e.Key, err)
			}

			if !slices.Equal(got, want.Postings) {
				t.Fatalf("%s: postings differ", e.Key)
			}

			if e.Count == 1 {
				single++

				if e.Bits != 0 {
					t.Fatalf("%s: a list of one posting takes %d bits in p", e.Key, e.Bits)
				}
			}

			checkCodecIsSmaller(t, e, want.Postings)
		}

		if found, ok := slot.Find(lists[max(at-1, 0)].Key); len(slot.Entries) > 0 && (!ok || found.Key != lists[at-1].Key) {
			t.Fatalf("Find(%s) failed", lists[at-1].Key)
		}
	}

	if at != len(lists) || single == 0 {
		t.Fatalf("read %d of %d lists, %d single", at, len(lists), single)
	}
}

// checkCodecIsSmaller recomputes both encodings of the gaps.
func checkCodecIsSmaller(t *testing.T, e *Entry, postings []Posting) {
	t.Helper()

	if e.Count == 1 {
		return
	}

	var maxGap uint32
	var nibbles uint64

	for i := 1; i < len(postings); i++ {
		gap := postings[i].ID - postings[i-1].ID
		maxGap = max(maxGap, gap)
		nibbles += uint64(nibbleCount(gap))
	}

	fixed := uint64(len(postings)-1) * uint64(len32(maxGap))
	got := listBits(e.Count, e.HasTail, e.Codec, e.X)
	smallest := listBits(e.Count, e.HasTail, CodecFixed, uint32(len32(maxGap)))

	if 4*nibbles < fixed {
		smallest = listBits(e.Count, e.HasTail, CodecNibble, uint32(nibbles))
	}

	if got != smallest {
		t.Fatalf("%s: codec %d takes %d bits, the smallest is %d", e.Key, e.Codec, got, smallest)
	}
}

func len32(v uint32) int {
	n := 0
	for v != 0 {
		n++
		v >>= 1
	}

	return n
}

func TestGoldenBytes(t *testing.T) {
	// Slot "po": pollo (2 postings), polo (1). IDs 10 and 13, 1000.
	lists := []List{
		{Key: key5.Word("pollo").Key, Postings: []Posting{{ID: 10, Pos: 0}, {ID: 13, Pos: 2}}},
		{Key: key5.Word("polo").Key, Postings: []Posting{{ID: 1000, Pos: 1}}},
	}

	k, p, err := Build(Meta{Gen: 1, Docs: 3, NormID: testNormID}, lists)
	if err != nil {
		t.Fatal(err)
	}

	// p: gap 3 in 2 bits (fixed, W = 2), positions 0 and 2 in 4 bits each,
	// LSB-first: bits 0-1 = 11, 2-5 = 0000, 6-9 = 0100 (2) → 0x83 0x00.
	if want := []byte{0x83, 0x00}; !slices.Equal(p, want) {
		t.Errorf("p = % x, want % x", p, want)
	}

	slot := key5.Word("po").Key.Slot()
	off := binary.LittleEndian.Uint32(k[headerSize+8*int(slot):])
	block := k[off:binary.LittleEndian.Uint32(k[headerSize+8*int(slot+1):])]

	// n = 2 | wCount 2 | wFirst 10 (990) | wX 4 | 0 | base 10.
	wantHeader := []byte{2, 0, 0, 0, 2, 10, 4, 0, 10, 0, 0, 0}
	if !slices.Equal(block[:blockHeader], wantHeader) {
		t.Errorf("block header % x, want % x", block[:blockHeader], wantHeader)
	}

	// Keys: [0,3] l l o, [1,2] o → 6 units = 30 bits = 4 bytes.
	keysLen := 4

	// Fields, 18 bits each: Count | Tail Codec X(4) | First−base(10).
	// pollo: 2 | 0 0 2 | 0; polo: 1 | pos 1, tail 0 | 990.
	fields := block[blockHeader+keysLen : len(block)-4]
	if len(fields) != 5 {
		t.Fatalf("fields take %d bytes, want 5 (36 bits)", len(fields))
	}

	h, err := ParseHead(k[:HeadSize], testNormID)
	if err != nil {
		t.Fatal(err)
	}

	s, err := ParseSlot(h, slot, block)
	if err != nil {
		t.Fatal(err)
	}

	if e := s.Entries[1]; e.Count != 1 || e.First != 1000 || e.Pos != 1 || e.Bits != 0 {
		t.Errorf("polo entry %+v", e)
	}

	if e := s.Entries[0]; e.Count != 2 || e.Codec != CodecFixed || e.X != 2 || e.Bits != 10 {
		t.Errorf("pollo entry %+v", e)
	}
}

func TestCorruption(t *testing.T) {
	r := rand.New(rand.NewPCG(8, 9))
	lists := randomLists(r, 300)

	k, p, err := Build(Meta{Gen: 1, NormID: testNormID}, lists)
	if err != nil {
		t.Fatal(err)
	}

	h, err := ParseHead(k[:HeadSize], testNormID)
	if err != nil {
		t.Fatal(err)
	}

	// Any flipped byte of the head is caught.
	for i := 0; i < HeadSize; i += 97 {
		bad := slices.Clone(k[:HeadSize])
		bad[i] ^= 0x10

		if _, err := ParseHead(bad, testNormID); err == nil {
			t.Fatalf("flipped head byte %d accepted", i)
		}
	}

	// Any flipped byte of a slot block is caught.
	for s := range uint16(key5.Slots) {
		off, end := h.Block(s)
		if end == off {
			continue
		}

		bad := slices.Clone(k[off:end])
		bad[r.IntN(len(bad))] ^= 0x01

		if _, err := ParseSlot(h, s, bad); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("slot %d: flipped byte gave %v", s, err)
		}
	}

	// A flipped byte of p is caught by its page.
	bad := slices.Clone(p)
	bad[len(bad)/2] ^= 0x01

	if err := CheckPages(bad, 0, h.PSize, PageCRCs(k[h.PageCRCOff:]), 0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("flipped p byte gave %v", err)
	}
}

func BenchmarkDecode(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 1))
	lists := randomLists(r, 2000)

	k, p, err := Build(Meta{NormID: testNormID}, lists)
	if err != nil {
		b.Fatal(err)
	}

	h, _ := ParseHead(k[:HeadSize], testNormID)

	var entries []Entry

	for s := range uint16(key5.Slots) {
		off, end := h.Block(s)

		slot, err := ParseSlot(h, s, k[off:end])
		if err != nil {
			b.Fatal(err)
		}

		entries = append(entries, slot.Entries...)
	}

	postings := 0
	for _, e := range entries {
		postings += int(e.Count)
	}

	b.ResetTimer()

	var buf []Posting

	for b.Loop() {
		for i := range entries {
			buf, _ = Decode(&entries[i], p, 0, buf[:0])
		}
	}

	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(postings), "ns/posting")
}

func BenchmarkParseSlots(b *testing.B) {
	r := rand.New(rand.NewPCG(1, 1))

	k, _, err := Build(Meta{NormID: testNormID}, randomLists(r, 2000))
	if err != nil {
		b.Fatal(err)
	}

	h, _ := ParseHead(k[:HeadSize], testNormID)

	for b.Loop() {
		for s := range uint16(key5.Slots) {
			off, end := h.Block(s)
			if _, err := ParseSlot(h, s, k[off:end]); err != nil {
				b.Fatal(err)
			}
		}
	}
}
