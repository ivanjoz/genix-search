package genix

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
)

func newTestClient(t *testing.T, opts ClientOptions) (*Client, Blob) {
	t.Helper()

	store, err := NewFSStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewClient(store, opts)
	if err != nil {
		t.Fatal(err)
	}

	return c, store
}

func hitIDs(t *testing.T, c *Client, collection, partition, query string) []uint32 {
	t.Helper()

	resp, err := c.Search(context.Background(), collection, partition, Request{Query: query, Limit: 50})
	if err != nil {
		t.Fatalf("%s/%s %q: %v", collection, partition, query, err)
	}

	ids := make([]uint32, len(resp.Hits))
	for i, h := range resp.Hits {
		ids[i] = h.ID
	}

	slices.Sort(ids)

	return ids
}

func TestClientPartitions(t *testing.T) {
	ctx := context.Background()
	c, store := newTestClient(t, ClientOptions{Extra: [2]rune{'ñ', 0}, Compact: CompactCallback})

	if _, err := c.Apply(ctx, "productos", "p1", []Doc{{ID: 1, Text: "piña golden"}, {ID: 2, Text: "polo rojo"}}, nil); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Apply(ctx, "productos", "p2", []Doc{{ID: 1, Text: "polo azul"}}, nil); err != nil {
		t.Fatal(err)
	}

	if got := hitIDs(t, c, "productos", "p1", "polo"); !slices.Equal(got, []uint32{2}) {
		t.Errorf("p1 polo: %v", got)
	}

	if got := hitIDs(t, c, "productos", "p2", "polo"); !slices.Equal(got, []uint32{1}) {
		t.Errorf("p2 polo: %v", got)
	}

	if got := hitIDs(t, c, "productos", "p3", "polo"); len(got) != 0 {
		t.Errorf("missing partition: %v", got)
	}

	// Deletes alone do not create a partition.
	if res, err := c.Apply(ctx, "productos", "p4", nil, []Ref{{ID: 1}}); err != nil || res.Size != 0 {
		t.Errorf("delete on a missing partition: %+v %v", res, err)
	}

	if _, _, err := store.Get(ctx, "genix/productos/p4/p.log"); err == nil {
		t.Error("a delete created partition p4")
	}

	// Puts and deletes in one batch: one read and one append.
	res, err := c.Apply(ctx, "productos", "p1", []Doc{{ID: 3, Text: "polo verde"}}, []Ref{{ID: 2}})
	if err != nil || res.Requests != 2 {
		t.Fatalf("mixed batch: %+v %v", res, err)
	}

	if got := hitIDs(t, c, "productos", "p1", "polo"); !slices.Equal(got, []uint32{3}) {
		t.Errorf("p1 polo after the batch: %v", got)
	}

	if _, err := c.Apply(ctx, "productos", "../x", []Doc{{ID: 1, Text: "x"}}, nil); err == nil {
		t.Error("an invalid partition name was accepted")
	}
}

func TestClientCompactModes(t *testing.T) {
	ctx := context.Background()

	docs := func(from, n int) []Doc {
		var out []Doc
		for i := from; i < from+n; i++ {
			out = append(out, Doc{ID: uint32(i), Text: fmt.Sprintf("producto numero %d", i)})
		}

		return out
	}

	t.Run("Sync", func(t *testing.T) {
		c, _ := newTestClient(t, ClientOptions{MinIndexDocs: 5, Compact: CompactSync})

		res, err := c.Apply(ctx, "c", "p", docs(0, 5), nil)
		if err != nil || res.Compacted == nil || res.CompactErr != nil || !res.Compacted.Indexed || res.Compacted.Seq != 5 {
			t.Fatalf("the write that reaches MinIndexDocs: %+v %v", res, err)
		}

		if resp, _ := c.Search(ctx, "c", "p", Request{Query: "producto 3"}); resp.Seq != 5 || resp.Stats.LogOnly || len(resp.Hits) == 0 {
			t.Errorf("search after the sync compaction: %+v", resp)
		}
	})

	t.Run("Async", func(t *testing.T) {
		var mu sync.Mutex
		var errs []error

		c, _ := newTestClient(t, ClientOptions{MinIndexDocs: 5, Compact: CompactAsync, OnCompactError: func(_, _ string, err error) {
			mu.Lock()
			errs = append(errs, err)
			mu.Unlock()
		}})

		var wg sync.WaitGroup

		for w := range 4 {
			wg.Go(func() {
				for i := range 10 {
					if _, err := c.Apply(ctx, "c", "p", docs(w*100+i*3, 3), nil); err != nil {
						t.Error(err)
					}
				}
			})
		}

		wg.Wait()
		c.Wait()

		if len(errs) > 0 {
			t.Fatalf("async compactions: %v", errs)
		}

		resp, err := c.Search(ctx, "c", "p", Request{Query: "producto", Limit: 200})
		if err != nil || resp.Seq == 0 || resp.Stats.LogOnly || len(resp.Hits) != 120 {
			t.Errorf("after async compactions: seq %d, %d hits, %v", resp.Seq, len(resp.Hits), err)
		}
	})

	t.Run("Callback", func(t *testing.T) {
		var asked []string

		c, _ := newTestClient(t, ClientOptions{MinIndexDocs: 5, Compact: CompactCallback, OnCompact: func(col, part string) {
			asked = append(asked, col+"/"+part)
		}})

		if _, err := c.Apply(ctx, "c", "p", docs(0, 5), nil); err != nil {
			t.Fatal(err)
		}

		if !slices.Equal(asked, []string{"c/p"}) {
			t.Errorf("OnCompact calls: %v", asked)
		}

		if res, err := c.Compact(ctx, "c", "p"); err != nil || !res.Indexed {
			t.Errorf("compaction run by the caller: %+v %v", res, err)
		}
	})

	t.Run("AutoOnLambda", func(t *testing.T) {
		t.Setenv("AWS_LAMBDA_FUNCTION_NAME", "genix")

		c, _ := newTestClient(t, ClientOptions{})
		if c.mode != CompactSync {
			t.Errorf("mode on Lambda: %d", c.mode)
		}
	})
}

func TestClientRebuildAndDrop(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestClient(t, ClientOptions{MinIndexDocs: 2, Compact: CompactSync})

	if _, err := c.Apply(ctx, "c", "p", []Doc{{ID: 1, Text: "polo viejo"}, {ID: 2, Text: "casaca vieja"}}, nil); err != nil {
		t.Fatal(err)
	}

	source := func(_ context.Context, yield func(Doc) error) error {
		for _, d := range []Doc{{ID: 2, Text: "casaca nueva"}, {ID: 3, Text: "polo nuevo"}} {
			if err := yield(d); err != nil {
				return err
			}
		}

		return nil
	}

	if _, err := c.Rebuild(ctx, "c", "p", source); err != nil {
		t.Fatal(err)
	}

	if got := hitIDs(t, c, "c", "p", "polo"); !slices.Equal(got, []uint32{3}) {
		t.Errorf("polo after the rebuild: %v", got)
	}

	if got := hitIDs(t, c, "c", "p", "vieja"); len(got) != 0 {
		t.Errorf("vieja after the rebuild: %v", got)
	}

	if _, err := c.Drop(ctx, "c", "p"); err != nil {
		t.Fatal(err)
	}

	if got := hitIDs(t, c, "c", "p", "polo"); len(got) != 0 {
		t.Errorf("polo after the drop: %v", got)
	}

	if _, err := c.Rebuild(ctx, "c", "p", func(context.Context, func(Doc) error) error { return errors.New("db down") }); err == nil {
		t.Error("a failing source did not fail the rebuild")
	}
}

func TestClientSearcherLRU(t *testing.T) {
	ctx := context.Background()
	c, _ := newTestClient(t, ClientOptions{MaxSearchers: 2})

	for _, p := range []string{"a", "b", "c", "a"} {
		if _, err := c.Search(ctx, "col", p, Request{Query: "polo"}); err != nil {
			t.Fatal(err)
		}
	}

	if len(c.searchers) != 2 || c.recent.Len() != 2 {
		t.Fatalf("%d searchers kept; want 2", len(c.searchers))
	}

	if _, ok := c.searchers[partKey{"col", "b"}]; ok {
		t.Error("b, the least recently used, was kept")
	}
}
