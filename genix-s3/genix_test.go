package genix

import (
	"context"
	"testing"

	"github.com/ivanjoz/colbin"
)

func TestPublicAPI(t *testing.T) {
	ctx := context.Background()

	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{Collection: "productos", Extra: [2]rune{'ñ', 0}}

	ix, err := NewIndexer(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := ix.Init(ctx, []Doc{{ID: 10, Text: "Polo Spiderman rojo"}, {ID: 20, Text: "Piña"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := ix.Put(ctx, Doc{ID: 30, Text: "Pinna marina"}); err != nil {
		t.Fatal(err)
	}

	s, err := NewSearcher(store, cfg, DefaultSearchOptions())
	if err != nil {
		t.Fatal(err)
	}

	body, _ := colbin.Marshal(&QueryRequest{Query: "piña", Limit: 10})

	out, err := s.Serve(ctx, body)
	if err != nil {
		t.Fatal(err)
	}

	var resp QueryResponse
	if err := colbin.Unmarshal(out, &resp); err != nil {
		t.Fatal(err)
	}

	if len(resp.Hits) != 1 || resp.Hits[0].ID != 20 {
		t.Fatalf("piña: %+v", resp)
	}

	if _, err := NewIndexer(store, Config{Collection: "x", Extra: [2]rune{'a', 0}}); err == nil {
		t.Error("an extra letter a-z was accepted")
	}
}
