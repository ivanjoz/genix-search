package query

import (
	"context"
	"os"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
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

// BenchmarkSearchWarm is the CPU of a warm search (.idx cached; the store is
// a local directory, so it includes reading files, not network).
func BenchmarkSearchWarm(b *testing.B) {
	docs, queries := benchDocs(b)
	f := newFixture(b, normalize.Default, docs)
	e := f.engine(DefaultOptions())
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

// BenchmarkBuild is building every bucket of the catalogue.
func BenchmarkBuild(b *testing.B) {
	docs, _ := benchDocs(b)

	for b.Loop() {
		newFixture(b, normalize.Default, docs)
	}
}
