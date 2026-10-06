package tokens

import (
	"encoding/binary"
	"hash/crc32"
	"maps"
	"slices"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// testTokens is a valid token stream that varies with i: words of the text
// (one longer than 8 letters, so it has a tail), a number, a one-letter word
// and, for some, a group; every 15th document has no tokens.
func testTokens(i int) []byte {
	words := []string{"polo", "rojo", "chocolatera", "camisa", "500", "ml", "spiderman", "agua", "m", "pack"}

	var text []string
	for j := range i % 15 {
		text = append(text, words[(i+j*7)%len(words)])
	}

	toks := key5.FromTokens(text)
	if i%4 == 0 {
		toks = append(toks, key5.GroupTokens([]string{"las", "condes", "norte"})...)
	}

	return key5.AppendTokens(nil, toks)
}

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, 1000, 2007} {
		s := &State{
			NormID:   0xabcd,
			Epoch:    3,
			Seq:      41,
			Expanded: []key5.Key{key5.FromToken("camisa").Key, key5.FromToken("polo").Key},
			Grouping: 1,
			Buckets:  map[uint16]Bucket{5: {Hash: 1 << 60, HasDB: true}, 77: {Hash: 9}},
			Garbage:  []Garbage{{0, 900}, {1, 12}, {0, 900}, {0, 12}},
		}

		for i := range n {
			s.Docs = append(s.Docs, Doc{ID: docid.New(uint8(i/700), uint32(i*3)), Tokens: testTokens(i)})
		}

		data, err := Encode(s)
		if err != nil {
			t.Fatal(err)
		}

		got, err := Decode(data, s.NormID)
		if err != nil {
			t.Fatalf("%d docs: %v", n, err)
		}

		if got.Epoch != s.Epoch || got.Seq != s.Seq || got.Grouping != 1 || !slices.Equal(got.Expanded, s.Expanded) ||
			!maps.Equal(got.Buckets, s.Buckets) || !slices.Equal(got.Garbage, []Garbage{{0, 12}, {0, 900}, {1, 12}}) {
			t.Errorf("%d docs: %+v", n, got)
		}

		if !slices.EqualFunc(got.Docs, s.Docs, func(a, b Doc) bool { return a.ID == b.ID && slices.Equal(a.Tokens, b.Tokens) }) {
			t.Errorf("%d docs: documents differ", n)
		}

		if _, err := Decode(data, s.NormID+1); err == nil {
			t.Error("another normalizer was accepted")
		}

		data[len(data)-1] ^= 1
		if _, err := Decode(data, s.NormID); err == nil {
			t.Errorf("%d docs: a flipped bit in the body passed", n)
		}
	}

	if _, err := Encode(&State{Docs: []Doc{{ID: 2, Tokens: testTokens(1)}, {ID: 1, Tokens: testTokens(2)}}}); err == nil {
		t.Error("unsorted documents were accepted")
	}

	if _, err := Encode(&State{Docs: []Doc{{ID: 1, Tokens: testTokens(1)}, {ID: 1, Tokens: testTokens(2)}}}); err == nil {
		t.Error("a repeated document was accepted")
	}

	if _, err := Encode(&State{Docs: []Doc{{ID: 1, Tokens: []byte{0xff, 0xff}}}}); err == nil {
		t.Error("a corrupt token stream was accepted")
	}
}

// TestDecodeCorrupt feeds bodies that pass the CRC but do not decode.
func TestDecodeCorrupt(t *testing.T) {
	s := &State{Docs: []Doc{{ID: 5, Tokens: testTokens(3)}, {ID: 9, Tokens: testTokens(4)}}}

	data, err := Encode(s)
	if err != nil {
		t.Fatal(err)
	}

	// The same body, claiming more documents than it holds: the header CRC
	// covers the count, so rewrite it too.
	bad := slices.Clone(data)
	bad[24] = 3
	fixHeaderCRC(bad)

	if _, err := Decode(bad, 0); err == nil {
		t.Error("a body with fewer documents than the header passed")
	}

	bad = slices.Clone(data)
	bad[24] = 1
	fixHeaderCRC(bad)

	if _, err := Decode(bad, 0); err == nil {
		t.Error("a body with more documents than the header passed")
	}
}

func fixHeaderCRC(data []byte) {
	// No tables in these states: the CRC covers the first 44 bytes only.
	binary.LittleEndian.PutUint32(data[44:], crc32.Checksum(data[:44], castagnoli))
}
