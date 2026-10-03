package snapshot

import (
	"slices"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

func TestRoundTrip(t *testing.T) {
	var docs []Doc

	for i := range 2500 {
		tokens := key5.AppendTokens(nil, key5.FromTokens([]string{"polo", "rojo", "50"}))
		docs = append(docs, Doc{ID: uint32(i*1000 + 7), Tokens: tokens})
	}

	data, err := Encode(Header{NormID: 1, Gen: 4, LastSeq: 99}, docs)
	if err != nil {
		t.Fatal(err)
	}

	h, got, err := Decode(data, 1)
	if err != nil {
		t.Fatal(err)
	}

	if h.Gen != 4 || h.LastSeq != 99 || h.DocCount != 2500 || len(got) != len(docs) {
		t.Fatalf("header %+v, %d docs", h, len(got))
	}

	for i := range docs {
		if got[i].ID != docs[i].ID || !slices.Equal(got[i].Tokens, docs[i].Tokens) {
			t.Fatalf("doc %d differs", i)
		}
	}

	bad := slices.Clone(data)
	bad[len(bad)-3] ^= 1

	if _, _, err := Decode(bad, 1); err == nil {
		t.Error("a corrupt chunk decoded")
	}

	if _, err := Encode(Header{}, []Doc{{ID: 2}, {ID: 1}}); err == nil {
		t.Error("unsorted documents encoded")
	}

	if _, err := Encode(Header{}, []Doc{{ID: 1}, {ID: 1}}); err == nil {
		t.Error("a repeated ID encoded")
	}

	if _, empty, err := Decode(mustEncode(t, nil), 0); err != nil || len(empty) != 0 {
		t.Errorf("empty snapshot: %v", err)
	}
}

func mustEncode(t *testing.T, docs []Doc) []byte {
	data, err := Encode(Header{}, docs)
	if err != nil {
		t.Fatal(err)
	}

	return data
}
