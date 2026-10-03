package tokens

import (
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

func TestRoundTrip(t *testing.T) {
	for _, n := range []int{0, 1, ChunkDocs, 2*ChunkDocs + 7} {
		s := &State{
			NormID:   0xabcd,
			Epoch:    3,
			Seq:      41,
			Expanded: []key5.Key{key5.FromToken("camisa").Key, key5.FromToken("polo").Key},
			Buckets:  map[uint16]Bucket{5: {Hash: 1 << 60, HasDB: true}, 77: {Hash: 9}},
			Garbage:  []uint16{900, 12, 900},
		}

		for i := range n {
			s.Docs = append(s.Docs, Doc{ID: uint32(i * 3), Tokens: []byte(fmt.Sprint("tokens of ", i))[:i%15]})
		}

		data, err := Encode(s)
		if err != nil {
			t.Fatal(err)
		}

		got, err := Decode(data, s.NormID)
		if err != nil {
			t.Fatalf("%d docs: %v", n, err)
		}

		if got.Epoch != s.Epoch || got.Seq != s.Seq || !slices.Equal(got.Expanded, s.Expanded) ||
			!maps.Equal(got.Buckets, s.Buckets) || !slices.Equal(got.Garbage, []uint16{12, 900}) {
			t.Errorf("%d docs: %+v", n, got)
		}

		if !slices.EqualFunc(got.Docs, s.Docs, func(a, b Doc) bool { return a.ID == b.ID && slices.Equal(a.Tokens, b.Tokens) }) {
			t.Errorf("%d docs: documents differ", n)
		}

		if _, err := Decode(data, s.NormID+1); err == nil {
			t.Error("another normalizer was accepted")
		}

		if n > 0 {
			data[len(data)-1] ^= 1
			if _, err := Decode(data, s.NormID); err == nil {
				t.Errorf("%d docs: a flipped bit in the last chunk passed", n)
			}
		}
	}

	if _, err := Encode(&State{Docs: []Doc{{ID: 2}, {ID: 1}}}); err == nil {
		t.Error("unsorted documents were accepted")
	}
}
