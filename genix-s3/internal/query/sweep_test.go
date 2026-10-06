package query

import (
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/pricing"
)

// TestCostSweep builds the index under several shapes and measures what each
// costs on S3 Express: a cold search (the queries of TestEval: 2 and 3 words,
// and the first word alone), the build, a compaction and the storage. It is
// how the defaults were chosen; the rankings must not change.
//
//	GXS_SWEEP=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv go test ./internal/query -run CostSweep -v -timeout 60m
//
// The grid comes from comma-separated lists: GXS_SWEEP_GROUPING (none,es100),
// GXS_SWEEP_SLOT (InlineSlot), GXS_SWEEP_INLINE (InlineBudget),
// GXS_SWEEP_EXPAND (ExpandCost, also MinSplit),
// GXS_SWEEP_GAP (CoalesceGap), GXS_SWEEP_SCALE (copies of the catalogue) and
// GXS_SWEEP_IDS (dense,random).
func TestCostSweep(t *testing.T) {
	if os.Getenv("GXS_SWEEP") == "" {
		t.Skip("set GXS_SWEEP=1 and GXS_EVAL_CSV")
	}

	path := os.Getenv("GXS_EVAL_CSV")
	if path == "" {
		t.Skip("set GXS_EVAL_CSV to a catalogue, one name per line")
	}

	names := readLines(t, path)

	var reqs []Request

	for i, q := range sampleQueries(names, 3000) {
		reqs = append(reqs, Request{Query: q, Limit: 10})

		if i%2 == 0 {
			reqs = append(reqs, Request{Query: strings.Fields(q)[0], Limit: 10})
		}
	}

	type shape struct {
		grouping     *layout.Grouping
		slot, inline int
		expandCost   float64
	}

	var shapes []shape

	for _, name := range sweepList(t, "GXS_SWEEP_GROUPING", "none,es100") {
		g, err := layout.GroupingByName(name)
		if err != nil {
			t.Fatal(err)
		}

		for _, slot := range sweepInts(t, "GXS_SWEEP_SLOT", fmt.Sprintf("0,%d", pricing.S3Express.BytesPerGET())) {
			for _, inline := range sweepInts(t, "GXS_SWEEP_INLINE", "0,2048,8192") {
				for _, expand := range sweepInts(t, "GXS_SWEEP_EXPAND", "2048,8192,32768,131072") {
					shapes = append(shapes, shape{g, int(slot), int(inline), float64(expand)})
				}
			}
		}
	}

	gaps := sweepInts(t, "GXS_SWEEP_GAP", fmt.Sprintf("4096,%d", pricing.S3Express.BytesPerGET()))

	for _, scale := range sweepInts(t, "GXS_SWEEP_SCALE", "1,10") {
		for _, mode := range sweepList(t, "GXS_SWEEP_IDS", "dense,random") {
			dense := mode == "dense"

			t.Run(fmt.Sprintf("x%d/%s", scale, mode), func(t *testing.T) {
				docs := scaledDocs(names, int(scale), 1, dense, rand.New(rand.NewPCG(uint64(scale), 99)))

				var want [][]Hit
				var lines []string

				for _, s := range shapes {
					opts := testOptions()
					opts.Params = bucket.DefaultParams()
					opts.Params.InlineSlot, opts.Params.InlineBudget = s.slot, s.inline
					opts.Params.ExpandCost, opts.Params.MinSplit = s.expandCost, s.expandCost

					f := newFixtureGrouped(t, normalize.Default, docs, opts, s.grouping)
					build, stored := storeCost(f.store), f.store.BytesWritten.Load() // the build writes every object once

					var searches []string

					for _, gap := range gaps {
						qopts := DefaultOptions()
						qopts.CoalesceGap = gap

						var requests, bytes, rounds []int
						changed := 0

						for i, resp := range coldAll(t, f, qopts, reqs) {
							requests = append(requests, resp.Stats.Requests)
							bytes = append(bytes, int(resp.Stats.Bytes))
							rounds = append(rounds, resp.Stats.Rounds)

							if len(want) < len(reqs) {
								want = append(want, resp.Hits)
							} else if !slices.Equal(resp.Hits, want[i]) {
								changed++
							}
						}

						if changed > 0 {
							t.Errorf("%s slot %d inline %d expand %.0f gap %d: %d rankings changed", groupingName(s.grouping), s.slot, s.inline, s.expandCost, gap, changed)
						}

						searches = append(searches, fmt.Sprintf("%-6s %6d %6d %6.0f %7d | %8.4f %6.2f %7.1f %5.2f",
							groupingName(s.grouping), s.slot, s.inline, s.expandCost, gap, perMillion(requests, bytes), avgOf(requests), avgOf(bytes)/1024, avgOf(rounds)))
					}

					// The compaction changes the index: last.
					compaction := compactionCost(t, f, docs)

					for _, line := range searches {
						lines = append(lines, fmt.Sprintf("%s | %9.6f %9.6f %8.1f", line, build, compaction, float64(stored)/1024))
					}
				}

				slices.SortStableFunc(lines, func(a, b string) int { return strings.Compare(a[strings.Index(a, "|"):], b[strings.Index(b, "|"):]) })

				t.Logf("%d docs, %d cold searches per shape; sorted by $ per 1M searches\n%-6s %6s %6s %6s %7s | %8s %6s %7s %5s | %9s %9s %8s\n%s",
					len(docs), len(reqs), "group", "slot", "inline", "expand", "gap", "$/1M", "GET", "KB", "rnds", "build $", "compact $", "KB kept", strings.Join(lines, "\n"))
			})
		}
	}
}

// sweepList is the comma-separated value of an environment variable, or def.
func sweepList(t *testing.T, env, def string) []string {
	t.Helper()

	v := os.Getenv(env)
	if v == "" {
		v = def
	}

	return strings.Split(v, ",")
}

// sweepInts is sweepList as integers.
func sweepInts(t *testing.T, env, def string) []int64 {
	t.Helper()

	var out []int64

	for _, s := range sweepList(t, env, def) {
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			t.Fatalf("%s: %q is not a number", env, s)
		}

		out = append(out, n)
	}

	return out
}

func groupingName(g *layout.Grouping) string {
	if g == layout.GroupingNone {
		return "none"
	}

	return "es100"
}

// compactionCost fills p.log up to the compaction threshold with one-document
// appends on a copy of the index's state, then compacts: the dollars of both.
func compactionCost(t *testing.T, f *fixture, docs []collection.Doc) float64 {
	t.Helper()

	ctx := context.Background()
	r := rand.New(rand.NewPCG(7, 7))

	f.store.Reset()

	for {
		doc := docs[r.IntN(len(docs))]

		res, err := f.w.Put(ctx, []collection.Doc{{OriginID: doc.OriginID, ID: doc.ID, Text: doc.Text + " nuevo"}})
		if err != nil {
			t.Fatal(err)
		}

		if res.CompactRequested {
			break
		}
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	return storeCost(f.store)
}
