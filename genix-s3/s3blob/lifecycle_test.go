package s3blob_test

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	genix "github.com/ivanjoz/genix-search/genix-s3"
	"github.com/ivanjoz/genix-search/genix-s3/s3blob"
)

var (
	colors = []string{"rojo", "azul", "verde", "negro", "blanco", "amarillo"}
	things = []string{"polo", "camiseta", "pantalon", "zapatilla", "gorra", "casaca", "mochila"}
	brands = []string{"spiderman", "adidas", "nike", "puma", "reebok"}
)

func docText(id uint32) string {
	return fmt.Sprintf("%s %s %s talla %d", things[id%7], brands[id%5], colors[id%6], 30+id%12)
}

// TestLifecycle runs the write and query paths of the public API on the
// bucket: the first build, appends from concurrent writers until a compaction
// is requested, the compaction (claim with If-None-Match, rebase of p.log with
// If-Match) and searches before and after it.
func TestLifecycle(t *testing.T) {
	store, prefix := testStore(t)
	ctx := context.Background()

	t.Cleanup(func() { deletePrefix(t, prefix) })

	cfg := genix.Config{Root: prefix, Collection: "productos", Extra: [2]rune{'ñ', 0}}

	ix, err := genix.NewIndexer(store, cfg)
	if err != nil {
		t.Fatal(err)
	}

	docs := make([]genix.Doc, 300)
	for i := range docs {
		id := uint32(i + 1)
		docs[i] = genix.Doc{ID: id, Text: docText(id)}
	}

	if err := ix.Init(ctx, docs); err != nil {
		t.Fatal(err)
	}

	s, err := genix.NewSearcher(store, cfg, genix.DefaultSearchOptions())
	if err != nil {
		t.Fatal(err)
	}

	expectTop := func(query string, id uint32) {
		t.Helper()

		resp, err := s.Search(ctx, genix.Request{Query: query, Limit: 10})
		if err != nil {
			t.Fatalf("%q: %v", query, err)
		}

		if !slices.ContainsFunc(resp.Hits, func(h genix.Hit) bool { return h.ID == id }) {
			t.Errorf("%q (seq %d): %d not in %+v", query, resp.Seq, id, resp.Hits)
		}

		t.Logf("%q: seq %d, %d rounds, %d requests, %d B, %v", query, resp.Seq, resp.Stats.Rounds, resp.Stats.Requests, resp.Stats.Bytes, resp.Stats.Elapsed)
	}

	expectTop("gorra adidas", 11)
	expectTop("gorra adidas", 11) // warm: k cached

	// Concurrent writers: every append must land exactly once.
	const writers, perWriter = 4, 10

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		requested int
		errs      []error
	)

	for w := range writers {
		wg.Go(func() {
			for i := range perWriter {
				id := uint32(10_000 + w*100 + i)

				res, err := ix.Put(ctx, genix.Doc{ID: id, Text: fmt.Sprintf("piña colada edicion %d", id)})

				mu.Lock()
				if err != nil {
					errs = append(errs, err)
				}

				if res.CompactRequested {
					requested++
				}
				mu.Unlock()
			}
		})
	}

	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent puts: %v", errs)
	}

	if _, err := ix.Delete(ctx, 11); err != nil {
		t.Fatal(err)
	}

	expectTop("piña colada 10203", 10_203) // from p.log

	t.Logf("compactions requested: %d", requested)

	res, err := ix.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("compact: %+v", res)

	expectTop("piña colada 10203", 10_203) // from the rewritten buckets
	expectTop("mochila nike negro", 27)

	resp, err := s.Search(ctx, genix.Request{Query: "gorra adidas amarillo talla 41", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}

	if slices.ContainsFunc(resp.Hits, func(h genix.Hit) bool { return h.ID == 11 }) {
		t.Errorf("deleted document 11 is still found: %+v", resp.Hits)
	}
}

// deletePrefix removes everything under prefix. Only the tests list: the
// library never does.
func deletePrefix(t *testing.T, prefix string) {
	ctx := context.Background()

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		t.Error(err)
		return
	}

	client := s3.NewFromConfig(cfg)
	bucket := aws.String(testBucket())
	store := s3blob.New(client, *bucket)

	var keys []string

	pages := s3.NewListObjectsV2Paginator(client, &s3.ListObjectsV2Input{Bucket: bucket, Prefix: &prefix})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			t.Error(err)
			return
		}

		for _, o := range page.Contents {
			keys = append(keys, aws.ToString(o.Key))
		}
	}

	if len(keys) > 0 {
		if err := store.Delete(ctx, keys...); err != nil {
			t.Error(err)
		}
	}
}
