package query

import (
	"context"
	"fmt"
	"maps"
	"math/rand/v2"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
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

// testOptions always builds the index (MinIndexDocs 1) and does not space
// compactions: the tests that race queries with compactions set it.
func testOptions() collection.Options {
	opts := collection.DefaultOptions()
	opts.MinIndexDocs = 1
	opts.CompactSpacing = 0

	return opts
}

func newFixture(t testing.TB, norm *normalize.Normalizer, docs []collection.Doc) *fixture {
	t.Helper()

	return newFixtureOpts(t, norm, docs, testOptions())
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
		{query: "spidreman", fuzzy: true, first: 900001},                // typo after the first two letters
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

	// A typo in the first two letters is never matched: another bucket.
	if got := search(t, e, "psiderman", true).Hits; len(got) != 0 {
		t.Errorf("psiderman matched %v", got)
	}
}

// listsInDB keeps every list of two postings or more in the .db.
func listsInDB() collection.Options {
	opts := testOptions()
	opts.Params.InlineBudget = 0

	return opts
}

// TestBudget: an expansion over the budget is cut and flagged, but a term
// without an exact key still reads its best key.
func TestBudget(t *testing.T) {
	docs := append(slices.Clone(smallDocs), collection.Doc{ID: 60, Text: "Polar azul"}, collection.Doc{ID: 61, Text: "Polar verde"})
	f := newFixtureOpts(t, normalize.Default, docs, listsInDB())
	opts := DefaultOptions()
	opts.PrefixBudget = 1
	e := f.engine(opts)

	// polar and polo have lists in the .db (2 documents each, polar first by
	// key): only polar is read, forced. pollo and polera cost nothing.
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

// TestRoundsAndRequests: cold, p.log and the .idx of every bucket in one
// round, the .db in a second if a list is there; warm, one round.
func TestRoundsAndRequests(t *testing.T) {
	for _, inDB := range []bool{false, true} {
		opts := testOptions()
		if inDB {
			opts = listsInDB()
		}

		f := newFixtureOpts(t, normalize.Default, smallDocs, opts)
		e := f.engine(DefaultOptions())

		cold := search(t, e, "polo spiderman rojo", false)
		warm := search(t, e, "polo spiderman rojo", false)

		// po, sp, ro.
		wantRounds, wantRequests := 1, 4
		if inDB {
			wantRounds, wantRequests = 2, 7 // and po.db, sp.db, ro.db
		}

		if cold.Stats.Rounds != wantRounds || cold.Stats.Requests != wantRequests {
			t.Errorf("in .db %v: cold %d rounds %d requests, want %d and %d", inDB, cold.Stats.Rounds, cold.Stats.Requests, wantRounds, wantRequests)
		}

		wantWarm := 1
		if inDB {
			wantWarm = 4
		}

		if warm.Stats.Rounds != 1 || warm.Stats.Requests != wantWarm || !warm.Stats.Warm {
			t.Errorf("in .db %v: warm %d rounds %d requests (warm %v), want 1 and %d", inDB, warm.Stats.Rounds, warm.Stats.Requests, warm.Stats.Warm, wantWarm)
		}

		if !slices.Equal(ids(cold.Hits), ids(warm.Hits)) {
			t.Errorf("cold and warm results differ")
		}

		t.Logf("in .db %v: cold %d rounds %d requests %d B (.idx %d B) · warm %d requests %d B (p.log %d B)",
			inDB, cold.Stats.Rounds, cold.Stats.Requests, cold.Stats.Bytes, cold.Stats.IdxBytes, warm.Stats.Requests, warm.Stats.Bytes, warm.Stats.LogBytes)
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

	// Rewritten: the buckets of the old and new words of 77, 17 and 900001,
	// and nothing else.
	want := map[string]bool{}
	for _, w := range []string{"polo", "spiderman", "rojo", "azul", "gorra", "verde", "basico", "talla", "m"} {
		want[layout.BucketName(key5.FromToken(w).Key.Slot())] = true
	}

	if res.Seq != 3 || res.Ops != 3 || res.Full || res.Buckets != len(want) {
		t.Fatalf("compaction %+v, want %d buckets", res, len(want))
	}

	check("after compaction") // the engine had the old buckets cached

	if resp := search(t, e, "polo", false); resp.Seq != 3 || resp.Stats.LogEntries != 0 {
		t.Errorf("after compaction: seq %d, %d log entries", resp.Seq, resp.Stats.LogEntries)
	}

	// Gorra goes: its bucket becomes a tombstone, then goes away with the
	// next compaction.
	if _, err := f.w.Delete(ctx, 17); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	if got := search(t, e, "gorra", false).Hits; len(got) != 0 {
		t.Errorf("gorra after its delete: %v", got)
	}

	if _, err := f.w.Put(ctx, []collection.Doc{{ID: 5, Text: "Zapato"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	if _, _, err := f.store.Get(ctx, f.col.Idx(key5.FromToken("gorra").Key.Slot())); err == nil {
		t.Error("the tombstone of go was not deleted")
	}

	check2 := ids(search(t, e, "polo spiderman rojo", false).Hits)
	if len(check2) == 0 || check2[0] != 77 {
		t.Errorf("after the third compaction: %v", check2)
	}
}

// TestLogOnly: under MinIndexDocs there is no index and a query reads only
// p.log; growing past it builds the index, shrinking below drops it, and the
// answers match the reference at every stage.
func TestLogOnly(t *testing.T) {
	ctx := context.Background()
	opts := testOptions()
	opts.MinIndexDocs = 100
	f := newFixtureOpts(t, normalize.Default, smallDocs, opts)
	e := f.engine(DefaultOptions())

	state := map[uint32]string{}
	for _, d := range smallDocs {
		state[d.ID] = d.Text
	}

	base := maps.Clone(state)
	queries := []string{"polo spiderman rojo", "chocolate", "agua 500 ml", "pol", "agua 50", "spiderman*", "pina", "x m l", "talla m"}

	check := func(stage string, logOnly bool) {
		t.Helper()

		dict := base
		if logOnly {
			dict = state
		}

		ref := newReference(normalize.Default, dict, state)

		for _, q := range queries {
			resp := search(t, e, q, false)
			if resp.Stats.LogOnly != logOnly {
				t.Fatalf("%s: %q answered with LogOnly %v", stage, q, resp.Stats.LogOnly)
			}

			if want := ref.top(Parse(normalize.Default, q), 0, 10, false); !slices.Equal(resp.Hits, want) {
				t.Fatalf("%s: %q\n engine %v\n ref    %v", stage, q, resp.Hits, want)
			}
		}

		// Typos too: only the terms nobody has.
		if resp := search(t, e, "spidreman rojo", true); len(resp.Hits) == 0 || resp.Hits[0].Coverage != 2 {
			t.Fatalf("%s: spidreman rojo %v", stage, resp.Hits)
		}
	}

	if _, _, err := f.store.Get(ctx, f.col.Tokens()); err == nil {
		t.Fatal("an index was built for 9 documents")
	}

	check("9 documents", true)

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

			base = maps.Clone(state)

			if res.Indexed && len(state) < 100 {
				t.Fatalf("indexed with %d documents", len(state))
			}
		}

		if i == 40 {
			check("50 documents with pending writes", true)
		}
	}

	// The 100th document asked for the compaction that built the index.
	if !res.Indexed || !res.Full || len(state) != 104 {
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

	check("94 documents pending, still indexed", false)

	if res, err := f.w.Compact(ctx); err != nil || res.Indexed || res.Docs != 94 {
		t.Fatalf("compaction to 94 documents: %+v %v", res, err)
	}

	base = maps.Clone(state)
	check("94 documents, index dropped", true) // the engine had buckets cached

	// The buckets are kept for queries in flight, and deleted by the next
	// compaction.
	if _, _, err := f.store.Get(ctx, f.col.Tokens()); err != nil {
		t.Fatal("tokens deleted while queries may still read the index")
	}

	state[42] = "Gorra verde"
	if _, err := f.w.Put(ctx, []collection.Doc{{ID: 42, Text: "Gorra verde"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	base = maps.Clone(state)

	if _, _, err := f.store.Get(ctx, f.col.Tokens()); err == nil {
		t.Error("the dropped index was not deleted")
	}

	if _, _, err := f.store.Get(ctx, f.col.Idx(key5.FromToken("polo").Key.Slot())); err == nil {
		t.Error("po.idx was not deleted")
	}

	check("95 documents after another compaction", true)

	// Growing again builds the index in a new epoch.
	for i := range 10 {
		id := uint32(9000 + i)
		state[id] = "Zapato rojo"

		if _, err := f.w.Put(ctx, []collection.Doc{{ID: id, Text: "Zapato rojo"}}); err != nil {
			t.Fatal(err)
		}
	}

	res2, err := f.w.Compact(ctx)
	if err != nil || !res2.Indexed || res2.Epoch <= res.Epoch {
		t.Fatalf("compaction to 105 documents: %+v %v", res2, err)
	}

	base = maps.Clone(state)
	check("105 documents, indexed again", false)
}

// TestConcurrent runs searches in parallel with appends and compactions,
// with a cache small enough to be reset all the time, and with the
// compactions spaced as the design needs.
func TestConcurrent(t *testing.T) {
	ctx := context.Background()
	opts := listsInDB()
	opts.CompactSpacing = 60 * time.Millisecond
	f := newFixtureOpts(t, normalize.Default, smallDocs, opts)

	eopts := DefaultOptions()
	eopts.CacheBytes = 64
	eopts.RoundLimit = 30 * time.Millisecond
	e := f.engine(eopts)

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

// bruteWords are the words of the random documents: few, so that lists are
// long and words expand.
var bruteWords = []string{"polo", "polera", "pollo", "spiderman", "rojo", "azul", "chocolate", "chocolatera", "chocolatina",
	"agua", "mineral", "pack", "leche", "entera", "desnatada", "refrigerado", "refrigerante", "x", "l", "m", "ca", "casa", "cama"}

func bruteText(r *rand.Rand) string {
	n := 1 + r.IntN(12)
	s := ""

	for range n {
		if r.IntN(5) == 0 {
			s += fmt.Sprintf("%d ", r.IntN(2000))
		} else {
			s += bruteWords[r.IntN(len(bruteWords))] + " "
		}
	}

	return s
}

func bruteQueries(r *rand.Rand, n int) []string {
	var queries []string

	for range n {
		q := ""
		for range 1 + r.IntN(3) {
			q += bruteWords[r.IntN(len(bruteWords))] + " "
		}

		if r.IntN(3) == 0 {
			w := bruteWords[r.IntN(len(bruteWords))]
			q += w[:1+r.IntN(len(w))]
		}

		if r.IntN(4) == 0 {
			q += fmt.Sprint(r.IntN(200))
		}

		queries = append(queries, q)
	}

	return append(queries, "pol", "ca", "c", "chocolat*", "1", "19", "x m l", "chocolate", "chocolatera", "polo", "rojo azul",
		"refrigerado", "refrigerante", "chocolatina leche", "polo polo", "polo pol", "ca casa cama")
}

// TestMatchesBruteForce compares the engine with a ranking of every
// document's tokens by the same rules, with pending p.log ops, under index
// parameters small enough that most words expand: heads, combinations,
// grouped combination blocks and deep pages all take part.
func TestMatchesBruteForce(t *testing.T) {
	small := bucket.Params{MinSplit: 64, ExpandCost: 32, HeadSize: 8, InlineBudget: 64, ComboDictBudget: 16}

	for _, tc := range []struct {
		name   string
		params bucket.Params
	}{
		{"default", bucket.DefaultParams()},
		{"small", small},
		{"tiny heads", bucket.Params{MinSplit: 16, ExpandCost: 16, HeadSize: 1, InlineBudget: 0, ComboDictBudget: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			r := rand.New(rand.NewPCG(11, 12))

			var docs []collection.Doc
			used := map[uint32]bool{}

			for len(docs) < 3000 {
				id := r.Uint32()
				if !used[id] {
					used[id] = true
					docs = append(docs, collection.Doc{ID: id, Text: bruteText(r)})
				}
			}

			opts := testOptions()
			opts.Params = tc.params
			f := newFixtureOpts(t, normalize.Default, docs, opts)

			base := map[uint32]string{}
			for _, d := range docs {
				base[d.ID] = d.Text
			}

			state := maps.Clone(base)

			// Pending ops: new documents, new versions and deletes.
			var puts []collection.Doc
			for i := range 40 {
				id := docs[r.IntN(len(docs))].ID
				if i%3 == 0 {
					id = r.Uint32()
				}

				puts = append(puts, collection.Doc{ID: id, Text: bruteText(r)})
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

			ref := newReference(normalize.Default, base, state)
			e := f.engine(DefaultOptions())
			e.opts.PrefixBudget = 1 << 40

			var deep, headed, pairs int

			for i, q := range bruteQueries(r, 400) {
				offset := 0
				if i%5 == 0 {
					offset = 10 * r.IntN(6)
				}

				fuzzy := i%7 == 0

				resp, err := e.Search(ctx, Request{Query: q, Limit: 10, Offset: offset, Fuzzy: fuzzy})
				if err != nil {
					t.Fatalf("%q: %v", q, err)
				}

				want := ref.top(Parse(normalize.Default, q), offset, 10, fuzzy)
				if !slices.Equal(resp.Hits, want) {
					t.Fatalf("%q offset %d fuzzy %v:\n engine %v\n brute  %v", q, offset, fuzzy, resp.Hits, want)
				}

				if resp.Stats.Deep > 0 {
					deep++
				}

				if resp.Stats.Heads > 0 {
					headed++
				}

				pairs += resp.Stats.Pairs
			}

			t.Logf("%d queries used heads, %d read a whole list after them; %d combinations read", headed, deep, pairs)

			// And after a compaction, with nothing pending.
			if _, err := f.w.Compact(ctx); err != nil {
				t.Fatal(err)
			}

			ref = newReference(normalize.Default, state, state)

			for _, q := range bruteQueries(r, 100) {
				resp := search(t, e, q, false)
				if want := ref.top(Parse(normalize.Default, q), 0, 10, false); !slices.Equal(resp.Hits, want) {
					t.Fatalf("after compaction %q:\n engine %v\n brute  %v", q, resp.Hits, want)
				}
			}
		})
	}
}

// reference ranks every live document by the matching rules, without the
// index; the rarity of a term comes from the dictionary documents, those the
// index was built from.
type reference struct {
	ids    []uint32
	tokens [][]key5.Token
	dict   [][]key5.Token
}

func newReference(norm *normalize.Normalizer, dict, live map[uint32]string) *reference {
	ref := &reference{}

	for id, text := range live {
		ref.ids = append(ref.ids, id)
		ref.tokens = append(ref.tokens, key5.FromTokens(norm.Tokens(text)))
	}

	for _, text := range dict {
		ref.dict = append(ref.dict, key5.FromTokens(norm.Tokens(text)))
	}

	return ref
}

func (r *reference) top(q Parsed, offset, limit int, fuzzy bool) []Hit {
	enabled, df := dictMatch(q.Terms, r.dict, fuzzy)
	cands := newCandidates(len(q.Terms), 0)
	var scratch [key5.MaxUnits]uint8

	for i, tokens := range r.tokens {
		if matches, any := matchTokens(q.Terms, enabled, tokens, scratch[:]); any {
			cands.put(r.ids[i], matches)
		}
	}

	// Sort everything: the reference does not use the top-n selection.
	results := rankAll(q.Terms, cands, idfs(df, len(r.dict)), 1<<30)
	hits := []Hit{}

	for _, res := range results[min(offset, len(results)):min(offset+limit, len(results))] {
		hits = append(hits, Hit{ID: res.id, Coverage: res.score.coverage, Score: res.score.score})
	}

	return hits
}

// TestStaleBucket: a bucket of another version than p.log is detected, and
// the search starts over instead of mixing them.
func TestStaleBucket(t *testing.T) {
	ctx := context.Background()
	f := newFixtureOpts(t, normalize.Default, smallDocs, listsInDB())
	slot := key5.FromToken("polo").Key.Slot()

	old, _, err := f.store.Get(ctx, f.col.Idx(slot))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Put(ctx, []collection.Doc{{ID: 8, Text: "Polo verde"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	current, _, err := f.store.Get(ctx, f.col.Idx(slot))
	if err != nil {
		t.Fatal(err)
	}

	// Put the old po.idx back: the base says the last compaction rewrote it.
	if _, err := f.store.Put(ctx, f.col.Idx(slot), old, blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	e := f.engine(DefaultOptions())

	done := make(chan Response)
	go func() {
		resp, _ := e.Search(ctx, Request{Query: "polo verde", Limit: 10})
		done <- resp
	}()

	time.Sleep(20 * time.Millisecond)

	if _, err := f.store.Put(ctx, f.col.Idx(slot), current, blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	resp := <-done
	if resp.Stats.Retries == 0 || len(resp.Hits) == 0 || resp.Hits[0].ID != 8 {
		t.Errorf("stale po.idx: %d retries, hits %v", resp.Stats.Retries, resp.Hits)
	}

	// A .db of another version than its .idx: the ETag tells.
	dbSlot := key5.FromToken("spiderman").Key.Slot()

	db, _, err := f.store.Get(ctx, f.col.DB(dbSlot))
	if err != nil {
		t.Fatal(err)
	}

	e2 := f.engine(DefaultOptions())
	search(t, e2, "spiderman", false)

	if _, err := f.store.Put(ctx, f.col.DB(dbSlot), append(slices.Clone(db), 0), blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	go func() {
		resp, _ := e2.Search(ctx, Request{Query: "spiderman", Limit: 10})
		done <- resp
	}()

	time.Sleep(20 * time.Millisecond)

	if _, err := f.store.Put(ctx, f.col.DB(dbSlot), db, blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	if resp := <-done; resp.Stats.Retries == 0 || len(resp.Hits) != 2 {
		t.Errorf("sp.db of another version: %d retries, hits %v", resp.Stats.Retries, resp.Hits)
	}
}
