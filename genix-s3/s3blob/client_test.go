package s3blob_test

import (
	"context"
	"slices"
	"testing"

	genix "github.com/ivanjoz/genix-search/genix-s3"
)

// TestClient runs the multi-partition Client on the bucket the way an ORM
// uses it: partitions created by their first write, mixed batches, a rebuild
// from the source with writes landing during it, sync compaction and a drop.
func TestClient(t *testing.T) {
	store, prefix := testStore(t)
	ctx := context.Background()

	t.Cleanup(func() { deletePrefix(t, prefix) })

	c, err := genix.NewClient(store, genix.ClientOptions{Root: prefix, Extra: [2]rune{'ñ', 0}, Compact: genix.CompactSync})
	if err != nil {
		t.Fatal(err)
	}

	search := func(part, query string) genix.Response {
		t.Helper()

		resp, err := c.Search(ctx, "productos", part, genix.Request{Query: query, Limit: 20})
		if err != nil {
			t.Fatalf("%s %q: %v", part, query, err)
		}

		t.Logf("%s %q: gen %d, %d hits, %d rounds, %d requests, %d B, log only %v", part, query, resp.Gen, len(resp.Hits), resp.Stats.Rounds, resp.Stats.Requests, resp.Stats.Bytes, resp.Stats.LogOnly)

		return resp
	}

	has := func(resp genix.Response, id uint32) bool {
		return slices.ContainsFunc(resp.Hits, func(h genix.Hit) bool { return h.ID == id })
	}

	// A small partition: created by its first write, LogOnly.
	res, err := c.Apply(ctx, "productos", "p1", []genix.Doc{{ID: 1, Text: "piña golden"}, {ID: 2, Text: "polo rojo"}}, nil)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("create p1: %d requests", res.Requests)

	if resp := search("p1", "piña"); !has(resp, 1) || !resp.Stats.LogOnly {
		t.Errorf("p1 piña: %+v", resp.Hits)
	}

	if resp := search("p9", "piña"); len(resp.Hits) != 0 {
		t.Errorf("missing partition: %+v", resp.Hits)
	}

	// A big partition, rebuilt from the source; a write lands meanwhile.
	source := func(ctx context.Context, yield func(genix.Doc) error) error {
		for id := uint32(1); id <= 400; id++ {
			if id == 200 {
				if _, err := c.Apply(ctx, "productos", "p2", []genix.Doc{{ID: 9000, Text: "gorra edicion limitada"}}, []uint32{11}); err != nil {
					return err
				}
			}

			if err := yield(genix.Doc{ID: id, Text: docText(id)}); err != nil {
				return err
			}
		}

		return nil
	}

	rebuilt, err := c.Rebuild(ctx, "productos", "p2", source)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("rebuild p2: %+v", rebuilt)

	if resp := search("p2", "gorra limitada"); !has(resp, 9000) {
		t.Errorf("the write during the rebuild was lost: %+v", resp.Hits)
	}

	if resp := search("p2", "gorra adidas amarillo talla 41"); has(resp, 11) {
		t.Errorf("the delete during the rebuild was lost: %+v", resp.Hits)
	}

	if resp := search("p1", "polo"); has(resp, 9000) || !has(resp, 2) {
		t.Errorf("partitions leak: %+v", resp.Hits)
	}

	if _, err := c.Drop(ctx, "productos", "p2"); err != nil {
		t.Fatal(err)
	}

	if resp := search("p2", "gorra"); len(resp.Hits) != 0 {
		t.Errorf("p2 after the drop: %+v", resp.Hits)
	}
}
