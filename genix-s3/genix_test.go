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

// TestGroupingFlag: the writers group as their Config says; a searcher
// configured otherwise follows the index.
func TestGroupingFlag(t *testing.T) {
	ctx := context.Background()

	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{Collection: "productos", MinIndexDocs: 1, Grouping: GroupingES100}

	ix, err := NewIndexer(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	if err := ix.Init(ctx, []Doc{{ID: 1, Text: "xilofono de madera"}, {ID: 2, Text: "camisa"}}); err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"vo-y0.idx", "ca.idx"} {
		if _, _, err := store.Get(ctx, "genix/productos/default/"+key); err != nil {
			t.Errorf("%s: %v", key, err)
		}
	}

	if _, _, err := store.Get(ctx, "genix/productos/default/xi.idx"); err == nil {
		t.Error("xi.idx was written: the grouping was ignored")
	}

	s, err := NewSearcher(store, Config{Collection: "productos"}, DefaultSearchOptions())
	if err != nil {
		t.Fatal(err)
	}

	resp, err := s.Search(ctx, Request{Query: "xilofono", Limit: 10})
	if err != nil || len(resp.Hits) != 1 || resp.Hits[0].ID != 1 {
		t.Fatalf("xilofono: %+v %v", resp, err)
	}

	if _, err := NewIndexer(store, Config{Collection: "x", Grouping: 7}); err == nil {
		t.Error("an unknown grouping was accepted")
	}
}
