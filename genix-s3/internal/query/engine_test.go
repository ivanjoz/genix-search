package query

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

type fixture struct {
	store *blob.Counting
	col   layout.Collection
	norm  *normalize.Normalizer
	w     *collection.Collection
}

// newFixture always builds the index (MinIndexDocs 1), whatever the size.
func newFixture(t testing.TB, norm *normalize.Normalizer, docs []collection.Doc) *fixture {
	t.Helper()

	opts := collection.DefaultOptions()
	opts.MinIndexDocs = 1

	return newFixtureOpts(t, norm, docs, opts)
}

func newFixtureOpts(t testing.TB, norm *normalize.Normalizer, docs []collection.Doc, opts collection.Options) *fixture {
	t.Helper()

	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	col, err := layout.New("genix/", "test", "default")
	if err != nil {
		t.Fatal(err)
	}

	f := &fixture{store: &blob.Counting{Blob: fs}, col: col, norm: norm}
	f.w = collection.New(f.store, col, norm, opts)

	if _, err := f.w.Init(context.Background(), docs); err != nil {
		t.Fatal(err)
	}

	return f
}

func (f *fixture) engine(opts Options) *Engine {
	return New(f.store, f.col, f.norm, opts)
}

func ids(hits []Hit) []uint32 {
	out := make([]uint32, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}

	return out
}

var smallDocs = []collection.Doc{
	{ID: 900001, Text: "Polo Spiderman rojo talla M"},
	{ID: 17, Text: "Polo rojo básico"},
	{ID: 4000000000, Text: "Polera Spiderman azul"},
	{ID: 3, Text: "Pollo asado 1 kg"},
	{ID: 250, Text: "Chocolate negro 70% cacao"},
	{ID: 251, Text: "Chocolatera eléctrica 500 ml"},
	{ID: 252, Text: "Agua mineral 500ml pack 6"},
	{ID: 253, Text: "Agua mineral 5 l"},
	{ID: 254, Text: "Piña en almíbar"},
}

func search(t *testing.T, e *Engine, query string, fuzzy bool) Response {
	t.Helper()

	resp, err := e.Search(context.Background(), Request{Query: query, Limit: 10, Fuzzy: fuzzy})
	if err != nil {
		t.Fatalf("%q: %v", query, err)
	}

	return resp
}

func TestSearchBasics(t *testing.T) {
	f := newFixture(t, normalize.Default, smallDocs)
	e := f.engine(DefaultOptions())

	cases := []struct {
		query string
		fuzzy bool
		first uint32
		has   []uint32
		not   []uint32
	}{
		{query: "polo spiderman rojo", first: 900001, has: []uint32{17, 4000000000}},
		{query: "chocolate", first: 250, has: []uint32{251}},            // same key, other tail: after
		{query: "chocolatera", first: 251, has: []uint32{250}},          // and the other way round
		{query: "agua 500 ml", first: 252, has: []uint32{251, 253}},     // 500ml is 500 ml
		{query: "pol", first: 3, has: []uint32{17, 900001, 4000000000}}, // a tie: by ID
		{query: "agua 50", first: 252},                                  // numeric prefix: 50 → 500
		{query: "spidreman", fuzzy: true, first: 900001},                // typo after the slot
		{query: "psiderman", fuzzy: true, first: 900001},                // typo in the first two letters
		{query: "pina", first: 254},                                     // ñ folds to n by default
		{query: "spiderman*", first: 900001, not: []uint32{17, 3}},      // explicit prefix is required
	}

	for _, c := range cases {
		resp := search(t, e, c.query, c.fuzzy)
		got := ids(resp.Hits)

		if len(got) == 0 || got[0] != c.first {
			t.Errorf("%q: hits %v, want %d first", c.query, resp.Hits, c.first)

			continue
		}

		for _, id := range c.has {
			if !slices.Contains(got, id) {
				t.Errorf("%q: %d missing from %v", c.query, id, got)
			}
		}

		for _, id := range c.not {
			if slices.Contains(got, id) {
				t.Errorf("%q: %d should not be in %v", c.query, id, got)
			}
		}
	}
}

// TestBudget: an expansion over the budget is cut and flagged, but a term
// without an exact key still reads its best key.
func TestBudget(t *testing.T) {
	docs := append(slices.Clone(smallDocs), collection.Doc{ID: 60, Text: "Polar azul"}, collection.Doc{ID: 61, Text: "Polar verde"})
	f := newFixture(t, normalize.Default, docs)
	opts := DefaultOptions()
	opts.PrefixBudget = 1
	e := f.engine(opts)

	// polar and polo have lists in p (2 documents each, polar first by key):
	// only polar is read, forced. pollo and polera cost nothing: always in.
	resp := search(t, e, "pol", false)
	got := ids(resp.Hits)

	if !resp.Stats.Approx || !slices.Contains(got, 60) || !slices.Contains(got, 3) || slices.Contains(got, 17) {
		t.Errorf("pol with a 1-byte budget: %v, approx %v", got, resp.Stats.Approx)
	}

	resp = search(t, e, "spidermn", true)
	if len(resp.Hits) == 0 || resp.Hits[0].ID != 900001 {
		t.Errorf("spidermn with a 1-byte budget: %v", resp.Hits)
	}
}

func TestExtraLetters(t *testing.T) {
	norm, err := normalize.New([2]rune{'ñ', 0})
	if err != nil {
		t.Fatal(err)
	}

	docs := []collection.Doc{{ID: 1, Text: "Piña en almíbar"}, {ID: 2, Text: "Pinna marina"}, {ID: 3, Text: "Campaña"}, {ID: 4, Text: "Campana"}}
	f := newFixture(t, norm, docs)

	if got := ids(search(t, f.engine(DefaultOptions()), "piña", false).Hits); !slices.Equal(got, []uint32{1}) {
		t.Errorf("piña: %v", got)
	}

	if got := ids(search(t, f.engine(DefaultOptions()), "campaña", false).Hits); !slices.Equal(got, []uint32{3}) {
		t.Errorf("campaña: %v", got)
	}

	// An engine with another configuration refuses the collection.
	other := New(f.store, f.col, normalize.Default, DefaultOptions())
	if _, err := other.Search(context.Background(), Request{Query: "pina"}); err == nil {
		t.Error("an engine with other extra letters read the collection")
	}
}

func TestRoundsAndRequests(t *testing.T) {
	f := newFixture(t, normalize.Default, smallDocs)

	for _, preload := range []bool{false, true} {
		opts := DefaultOptions()
		opts.Preload = preload
		e := f.engine(opts)

		cold := search(t, e, "polo spiderman rojo", false)
		warm := search(t, e, "polo spiderman rojo", false)

		// Cold: p.log; manifest + head of k (or all of k); [slots]; lists.
		wantCold := 4
		if preload {
			wantCold = 3
		}

		if cold.Stats.Rounds != wantCold {
			t.Errorf("preload %v: cold search took %d rounds, want %d", preload, cold.Stats.Rounds, wantCold)
		}

		// Warm: one round, p.log and the lists.
		if warm.Stats.Rounds != 1 {
			t.Errorf("preload %v: warm search took %d rounds", preload, warm.Stats.Rounds)
		}

		if !slices.Equal(ids(cold.Hits), ids(warm.Hits)) {
			t.Errorf("cold and warm results differ")
		}

		t.Logf("preload %v: cold %d rounds %d requests %d B · warm %d requests %d B (p.log %d B)",
			preload, cold.Stats.Rounds, cold.Stats.Requests, cold.Stats.Bytes, warm.Stats.Requests, warm.Stats.Bytes, warm.Stats.LogBytes)
	}

	// A word of one document is answered from the dictionary: only p.log.
	opts := DefaultOptions()
	opts.Preload = true
	e := f.engine(opts)
	search(t, e, "pollo", false)

	if resp := search(t, e, "asado", false); resp.Stats.Requests != 1 || resp.Hits[0].ID != 3 {
		t.Errorf("asado: %d requests, hits %v", resp.Stats.Requests, resp.Hits)
	}
}

func TestOverlayAndCompaction(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, normalize.Default, smallDocs)
	e := f.engine(DefaultOptions())

	search(t, e, "polo", false)

	if _, err := f.w.Put(ctx, []collection.Doc{{ID: 77, Text: "Polo Spiderman rojo y azul"}, {ID: 17, Text: "Gorra verde"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Delete(ctx, 900001); err != nil {
		t.Fatal(err)
	}

	check := func(stage string) {
		got := ids(search(t, e, "polo spiderman rojo", false).Hits)
		if len(got) == 0 || got[0] != 77 || slices.Contains(got, 900001) || slices.Contains(got, 17) {
			t.Errorf("%s: %v", stage, got)
		}

		if got := ids(search(t, e, "gorra", false).Hits); !slices.Equal(got, []uint32{17}) {
			t.Errorf("%s: gorra %v", stage, got)
		}
	}

	check("overlay")

	res, err := f.w.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}

	if res.Gen != 2 || res.Ops != 3 {
		t.Fatalf("compaction %+v", res)
	}

	check("after compaction") // the engine had generation 1 cached

	if resp := search(t, e, "polo", false); resp.Gen != 2 || resp.Stats.LogEntries != 0 {
		t.Errorf("after compaction: gen %d, %d log entries", resp.Gen, resp.Stats.LogEntries)
	}

	// A second compaction collects generation 1.
	if _, err := f.w.Put(ctx, []collection.Doc{{ID: 5, Text: "Zapato"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.store.Get(ctx, f.col.K(1)); err == nil {
		t.Error("generation 1 was not collected")
	}

	check("after the second compaction")
}

// TestLogOnly: under MinIndexDocs there is no index and a query reads only
// p.log; growing past it builds the index, shrinking below drops it, and the
// answers match the reference at every stage.
func TestLogOnly(t *testing.T) {
	ctx := context.Background()
	opts := collection.DefaultOptions() // MinIndexDocs 100
	f := newFixtureOpts(t, normalize.Default, smallDocs, opts)
	e := f.engine(DefaultOptions())

	state := map[uint32]string{}
	for _, d := range smallDocs {
		state[d.ID] = d.Text
	}

	queries := []string{"polo spiderman rojo", "chocolate", "agua 500 ml", "pol", "agua 50", "spiderman*", "pina", "x m l", "talla m"}

	check := func(stage string, logOnly bool) {
		t.Helper()

		ref := newReference(normalize.Default, state)

		for i, q := range queries {
			resp := search(t, e, q, false)
			if resp.Stats.LogOnly != logOnly {
				t.Fatalf("%s: %q answered with LogOnly %v", stage, q, resp.Stats.LogOnly)
			}

			// The first query after the index is dropped still reads the
			// generation it had cached, alongside p.log, in the same round.
			if logOnly && i > 0 && (resp.Stats.Rounds != 1 || resp.Stats.Requests != 1) {
				t.Fatalf("%s: %q took %d rounds, %d requests", stage, q, resp.Stats.Rounds, resp.Stats.Requests)
			}

			docs := len(state)
			if !logOnly {
				docs = int(e.state().head.Docs)
			}

			if want := ref.top(Parse(normalize.Default, q), 10, docs); !slices.Equal(resp.Hits, want) {
				t.Fatalf("%s: %q\n engine %v\n ref    %v", stage, q, resp.Hits, want)
			}
		}

		// Typos too: only the terms nobody has.
		if resp := search(t, e, "spidreman rojo", true); len(resp.Hits) == 0 || resp.Hits[0].Coverage != 2 {
			t.Fatalf("%s: spidreman rojo %v", stage, resp.Hits)
		}
	}

	if _, _, err := f.store.Get(ctx, f.col.K(1)); err == nil {
		t.Fatal("an index was built for 9 documents")
	}

	check("9 documents", true)

	// Writes on top: still one request.
	var res collection.CompactResult

	for i := range 95 {
		id := uint32(5000 + i)
		text := fmt.Sprintf("Polo básico color %d talla m", i)
		state[id] = text

		put, err := f.w.Put(ctx, []collection.Doc{{ID: id, Text: text}})
		if err != nil {
			t.Fatal(err)
		}

		if put.CompactRequested {
			if res, err = f.w.Compact(ctx); err != nil {
				t.Fatal(err)
			}

			if res.Gen != 0 && len(state) < 100 {
				t.Fatalf("indexed with %d documents", len(state))
			}
		}

		if i == 40 {
			check("50 documents with pending writes", true)
		}
	}

	// The 100th document asked for the compaction that built the index.
	if res.Gen != 1 || len(state) != 104 {
		t.Fatalf("after 104 documents: compaction %+v", res)
	}

	check("104 documents, indexed", false)

	// Deletes below the threshold drop the index.
	for i := range 10 {
		id := uint32(5000 + i)
		delete(state, id)

		if _, err := f.w.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}
	}

	if res, err := f.w.Compact(ctx); err != nil || res.Gen != 0 || res.Docs != 94 {
		t.Fatalf("compaction to 94 documents: %+v %v", res, err)
	}

	check("94 documents, index dropped", true) // the engine had generation 1 cached

	// Generation 1 is kept for queries in flight, and collected by the next
	// compaction.
	if _, _, err := f.store.Get(ctx, f.col.K(1)); err != nil {
		t.Fatal("generation 1 deleted while queries may still read it")
	}

	state[42] = "Gorra verde"
	if _, err := f.w.Put(ctx, []collection.Doc{{ID: 42, Text: "Gorra verde"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.store.Get(ctx, f.col.K(1)); err == nil {
		t.Error("generation 1 was not collected")
	}

	check("95 documents after another compaction", true)

	// Growing again claims generation 2, not the collected 1.
	for i := range 10 {
		id := uint32(9000 + i)
		state[id] = "Zapato rojo"

		if _, err := f.w.Put(ctx, []collection.Doc{{ID: id, Text: "Zapato rojo"}}); err != nil {
			t.Fatal(err)
		}
	}

	if res, err := f.w.Compact(ctx); err != nil || res.Gen != 2 {
		t.Fatalf("compaction to 105 documents: %+v %v", res, err)
	}

	check("105 documents, indexed again", false)
}

// TestConcurrent runs searches in parallel with appends and compactions,
// with a cache small enough to be reset all the time.
func TestConcurrent(t *testing.T) {
	ctx := context.Background()
	f := newFixture(t, normalize.Default, smallDocs)

	opts := DefaultOptions()
	opts.CacheBytes = 64
	opts.VerifyPages = true
	e := f.engine(opts)

	stop := make(chan struct{})
	writerDone := make(chan error, 1)

	go func() {
		defer close(writerDone)

		for i := range 30 {
			if _, err := f.w.Put(ctx, []collection.Doc{{ID: uint32(1000 + i), Text: "Polo Spiderman edición " + fmt.Sprint(i)}}); err != nil {
				writerDone <- err

				return
			}

			if i%5 == 4 {
				if _, err := f.w.Compact(ctx); err != nil {
					writerDone <- err

					return
				}
			}
		}

		close(stop)
	}()

	var wg sync.WaitGroup

	for range 8 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}

				resp, err := e.Search(ctx, Request{Query: "polo spiderman rojo", Limit: 10, Fuzzy: true})
				if err != nil {
					t.Error(err)

					return
				}

				if len(resp.Hits) == 0 || resp.Hits[0].ID != 900001 {
					t.Errorf("hits %v", resp.Hits)

					return
				}
			}
		})
	}

	wg.Wait()

	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
}

// TestMatchesBruteForce compares the engine with a ranking of every
// document's tokens by the same rules, with pending p.log ops.
func TestMatchesBruteForce(t *testing.T) {
	ctx := context.Background()
	r := rand.New(rand.NewPCG(11, 12))
	words := []string{"polo", "polera", "pollo", "spiderman", "rojo", "azul", "chocolate", "chocolatera", "chocolatina",
		"agua", "mineral", "pack", "leche", "entera", "desnatada", "refrigerado", "refrigerante", "x", "l", "m", "ca", "casa", "cama"}

	text := func() string {
		n := 1 + r.IntN(12)
		s := ""

		for range n {
			if r.IntN(5) == 0 {
				s += fmt.Sprintf("%d ", r.IntN(2000))
			} else {
				s += words[r.IntN(len(words))] + " "
			}
		}

		return s
	}

	var docs []collection.Doc
	used := map[uint32]bool{}

	for len(docs) < 3000 {
		id := r.Uint32()
		if !used[id] {
			used[id] = true
			docs = append(docs, collection.Doc{ID: id, Text: text()})
		}
	}

	f := newFixture(t, normalize.Default, docs)
	state := map[uint32]string{}

	for _, d := range docs {
		state[d.ID] = d.Text
	}

	// Pending ops: new documents, new versions and deletes.
	var puts []collection.Doc
	for i := range 40 {
		id := docs[r.IntN(len(docs))].ID
		if i%3 == 0 {
			id = r.Uint32()
		}

		puts = append(puts, collection.Doc{ID: id, Text: text()})
		state[id] = puts[len(puts)-1].Text
	}

	if _, err := f.w.Put(ctx, puts); err != nil {
		t.Fatal(err)
	}

	for range 10 {
		id := docs[r.IntN(len(docs))].ID
		if _, err := f.w.Delete(ctx, id); err != nil {
			t.Fatal(err)
		}

		delete(state, id)
	}

	ref := newReference(normalize.Default, state)
	e := f.engine(DefaultOptions())
	e.opts.PrefixBudget = 1 << 40

	var queries []string
	for range 400 {
		q := ""
		for range 1 + r.IntN(3) {
			q += words[r.IntN(len(words))] + " "
		}

		if r.IntN(3) == 0 {
			w := words[r.IntN(len(words))]
			q += w[:1+r.IntN(len(w))]
		}

		if r.IntN(4) == 0 {
			q += fmt.Sprint(r.IntN(200))
		}

		queries = append(queries, q)
	}

	queries = append(queries, "pol", "ca", "c", "chocolat*", "1", "19", "x m l")

	for _, q := range queries {
		resp := search(t, e, q, false)
		want := ref.top(Parse(normalize.Default, q), 10, int(len(docs)))

		if !slices.Equal(resp.Hits, want) {
			t.Fatalf("%q:\n engine %v\n brute  %v", q, resp.Hits, want)
		}
	}
}

// reference ranks every document by the matching rules, without the index.
type reference struct {
	ids    []uint32
	tokens [][]key5.Token
}

func newReference(norm *normalize.Normalizer, docs map[uint32]string) *reference {
	ref := &reference{}

	for id, text := range docs {
		ref.ids = append(ref.ids, id)
		ref.tokens = append(ref.tokens, key5.FromTokens(norm.Tokens(text)))
	}

	return ref
}

func (r *reference) top(q Parsed, limit, docs int) []Hit {
	cands := newCandidates(len(q.Terms), 0)
	fuzzy := make([]bool, len(q.Terms))
	var scratch [key5.MaxUnits]uint8

	for i, tokens := range r.tokens {
		if matches, any := matchTokens(q.Terms, fuzzy, tokens, scratch[:]); any {
			cands.put(r.ids[i], matches)
		}
	}

	// Sort everything: the reference does not use the top-n selection.
	results := rankAll(q.Terms, cands, docs, nil, 1<<30)
	hits := make([]Hit, 0, limit)

	for _, res := range results[:min(limit, len(results))] {
		hits = append(hits, Hit{ID: res.id, Coverage: res.score.coverage, Score: res.score.score})
	}

	return hits
}
