package query

import (
	"context"
	"os"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/index"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

// The benchmarks need the catalogue:
//
//	GXS_EVAL_CSV=../../../scripts/perf/data/productos.csv go test ./internal/query -run '^$' -bench .
func benchDocs(b *testing.B) ([]collection.Doc, []string) {
	path := os.Getenv("GXS_EVAL_CSV")
	if path == "" {
		b.Skip("set GXS_EVAL_CSV")
	}

	t := &testing.T{}
	names := readLines(t, path)

	docs := make([]collection.Doc, len(names))
	for i, name := range names {
		docs[i] = collection.Doc{ID: uint32(i+1) * 2654435761, Text: name}
	}

	return docs, sampleQueries(names, 1000)
}

// BenchmarkSearchWarm is the CPU of a warm search (k cached; the store is a
// local directory, so it includes reading files, not network).
func BenchmarkSearchWarm(b *testing.B) {
	docs, queries := benchDocs(b)
	f := newFixture(b, normalize.Default, docs)

	opts := DefaultOptions()
	opts.Preload = true
	e := f.engine(opts)
	ctx := context.Background()

	for _, q := range queries {
		if _, err := e.Search(ctx, Request{Query: q, Limit: 10}); err != nil {
			b.Fatal(err)
		}
	}

	i := 0

	for b.Loop() {
		if _, err := e.Search(ctx, Request{Query: queries[i%len(queries)], Limit: 10}); err != nil {
			b.Fatal(err)
		}

		i++
	}
}

// BenchmarkPlanAndRank is the CPU of a search without I/O: plan, decode,
// rank, over lists already read.
func BenchmarkPlanAndRank(b *testing.B) {
	docs, queries := benchDocs(b)
	f := newFixture(b, normalize.Default, docs)

	opts := DefaultOptions()
	opts.Preload = true
	e := f.engine(opts)
	ctx := context.Background()

	if _, err := e.Search(ctx, Request{Query: "polo"}); err != nil {
		b.Fatal(err)
	}

	g := e.state()
	p := g.head.PSize
	pData, err := f.store.GetRange(ctx, f.col.P(g.gen), 0, int64(p))
	if err != nil {
		b.Fatal(err)
	}

	parsed := make([]Parsed, len(queries))
	for i, q := range queries {
		parsed[i] = Parse(normalize.Default, q)
	}

	i := 0

	for b.Loop() {
		q := parsed[i%len(parsed)]
		plan, _ := planTerms(q.Terms, g.slot, opts.PrefixBudget, false)
		postings := make([][]index.Posting, len(plan.entries))

		for j, s := range plan.entries {
			postings[j], _ = index.Decode(s.entry, pData, 0, nil)
		}

		rankAll(q.Terms, gather(q.Terms, plan, postings), int(g.head.Docs), nil, 10)
		i++
	}
}

// BenchmarkBuild is building k, p and the snapshot of the catalogue.
func BenchmarkBuild(b *testing.B) {
	docs, _ := benchDocs(b)

	for b.Loop() {
		newFixture(b, normalize.Default, docs)
	}
}
