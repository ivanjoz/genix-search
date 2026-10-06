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

	if err := ix.Init(ctx, []Doc{{ID: 10, Text: "Polo Spiderman rojo"}, {OriginID: 9, ID: 20, Text: "Piña"}}); err != nil {
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

	if len(resp.Hits) != 1 || resp.Hits[0].ID != 20 || resp.Hits[0].OriginID != 9 {
		t.Fatalf("piña: %+v", resp)
	}

	if _, err := NewIndexer(store, Config{Collection: "x", Extra: [2]rune{'a', 0}}); err == nil {
		t.Error("an extra letter a-z was accepted")
	}
}

// TestGroupingFlag: the writers group as their Config says, es100 by
// default; a searcher configured otherwise follows the index.
func TestGroupingFlag(t *testing.T) {
	ctx := context.Background()

	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	exists := func(key string) bool {
		_, _, err := store.Get(ctx, "genix/productos/default/"+key)

		return err == nil
	}

	search := func(g Grouping) {
		t.Helper()

		s, err := NewSearcher(store, Config{Collection: "productos", Grouping: g}, DefaultSearchOptions())
		if err != nil {
			t.Fatal(err)
		}

		resp, err := s.Search(ctx, Request{Query: "xilofono", Limit: 10})
		if err != nil || len(resp.Hits) != 1 || resp.Hits[0].ID != 1 {
			t.Fatalf("xilofono: %+v %v", resp, err)
		}
	}

	ix, err := NewIndexer(store, Config{Collection: "productos", MinIndexDocs: 1})
	if err != nil {
		t.Fatal(err)
	}

	if err := ix.Init(ctx, []Doc{{ID: 1, Text: "xilofono de madera"}, {ID: 2, Text: "camisa"}}); err != nil {
		t.Fatal(err)
	}

	if !exists("vo-y0.idx") || !exists("ca.idx") || exists("xi.idx") {
		t.Error("the default config did not group as es100")
	}

	search(GroupingNone)

	// Opting out rewrites the index one bucket per slot.
	plain, err := NewIndexer(store, Config{Collection: "productos", MinIndexDocs: 1, Grouping: GroupingNone})
	if err != nil {
		t.Fatal(err)
	}

	if res, err := plain.Compact(ctx); err != nil || !res.Full {
		t.Fatalf("regroup to none: %+v %v", res, err)
	}

	if !exists("xi.idx") {
		t.Error("GroupingNone did not write xi.idx")
	}

	search(GroupingES100)

	if _, err := NewIndexer(store, Config{Collection: "x", Grouping: 7}); err == nil {
		t.Error("an unknown grouping was accepted")
	}
}
