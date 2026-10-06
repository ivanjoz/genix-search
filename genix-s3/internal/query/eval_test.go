package query

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/parallel"
	"github.com/ivanjoz/genix-search/genix-s3/internal/pricing"
	"github.com/ivanjoz/genix-search/genix-s3/internal/tokens"
)

// TestEval measures the design on a real catalogue: storage, what a cold
// query reads (rounds, requests, bytes, dollars: every query runs in a fresh
// Lambda, so nothing is warm), what a write costs, and
// whether the top 10 match a ranking by the same rules over every document
// (must be identical) and a ranking of the full words (what cutting words
// at 8 letters changes).
//
//	GXS_EVAL_CSV=../../../scripts/perf/data/productos.csv go test ./internal/query -run Eval -v
//
// Scale k repeats the catalogue k times under new IDs: random uint32 (the
// general case, nothing assumed), or dense 1..N (an auto-increment key, and
// what the previous genix-s3 evaluation used). The rankings, typos and
// writes are measured with random IDs only. GXS_EVAL_GROUPING=es100 builds
// the index grouped. GXS_EVAL_ORIGINS=n spreads the documents at random over n
// origins (n tables in one index), each with its own IDs: dense 1..N per
// origin, or random. GXS_EVAL_TEXTGROUPS=n gives 2 of every 3 documents one
// of n synthetic groups, and measures the queries that
// name it.
func TestEval(t *testing.T) {
	path := os.Getenv("GXS_EVAL_CSV")
	if path == "" {
		t.Skip("set GXS_EVAL_CSV to a catalogue, one name per line")
	}

	names := readLines(t, path)
	queries := sampleQueries(names, 3000)

	origins := 1
	if n := os.Getenv("GXS_EVAL_ORIGINS"); n != "" {
		var err error
		if origins, err = strconv.Atoi(n); err != nil || origins < 1 || origins > 256 {
			t.Fatalf("GXS_EVAL_ORIGINS=%q: 1 to 256", n)
		}
	}

	for _, scale := range []int{1, 10} {
		for _, dense := range []bool{true, false} {
			mode := "random"
			if dense {
				mode = "dense"
			}

			t.Run(fmt.Sprintf("x%d/%s", scale, mode), func(t *testing.T) {
				r := rand.New(rand.NewPCG(uint64(scale), 99))
				docs := scaledDocs(names, scale, origins, dense, r)

				if n := evalTextGroups(t); n > 0 {
					groups := syntheticGroups(n)
					for i := range docs {
						if r.IntN(3) > 0 {
							// A few groups take most documents.
							docs[i].Group = groups[int(float64(n)*r.Float64()*r.Float64()*r.Float64())]
						}
					}
				}

				evalScale(t, docs, queries, r, !dense)
			})
		}
	}
}

// scaledDocs repeats the names scale times under new IDs, spread at random
// over origins: dense 1..N per origin, or random uint32.
func scaledDocs(names []string, scale, origins int, dense bool, r *rand.Rand) []collection.Doc {
	used := map[docid.Key]bool{}
	next := make([]uint32, origins) // the last dense ID of each origin

	var docs []collection.Doc

	for range scale {
		for _, name := range names {
			origin := uint8(0)
			if origins > 1 {
				origin = uint8(r.IntN(origins))
			}

			next[origin]++
			k := docid.New(origin, next[origin])

			for !dense && (k.ID() == next[origin] || used[k]) {
				k = docid.New(origin, r.Uint32())
			}

			used[k] = true
			docs = append(docs, collection.Doc{OriginID: origin, ID: k.ID(), Text: name})
		}
	}

	return docs
}

// evalTextGroups is GXS_EVAL_TEXTGROUPS, 0 without it.
func evalTextGroups(t *testing.T) int {
	v := os.Getenv("GXS_EVAL_TEXTGROUPS")
	if v == "" {
		return 0
	}

	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		t.Fatalf("GXS_EVAL_TEXTGROUPS=%q", v)
	}

	return n
}

// syntheticGroups are n group names: Chilean places, then the same with a
// sector after them.
func syntheticGroups(n int) []string {
	places := []string{"Santiago Centro", "Providencia", "Las Condes", "Ñuñoa", "La Florida", "Maipú", "Puente Alto",
		"Vitacura", "Lo Barnechea", "La Reina", "Peñalolén", "Macul", "San Miguel", "Estación Central", "Quinta Normal",
		"Recoleta", "Independencia", "Conchalí", "Huechuraba", "Quilicura", "Renca", "Cerro Navia", "Lo Prado",
		"Pudahuel", "Cerrillos", "Lo Espejo", "Pedro Aguirre Cerda", "San Joaquín", "La Granja", "La Pintana",
		"San Ramón", "La Cisterna", "El Bosque", "San Bernardo", "Colina", "Lampa", "Buin", "Paine", "Talagante",
		"Peñaflor", "Valparaíso", "Viña del Mar", "Concepción", "Temuco", "Antofagasta", "Iquique", "Arica",
		"La Serena", "Coquimbo", "Rancagua", "Talca", "Chillán", "Valdivia", "Osorno", "Puerto Montt",
		"Punta Arenas", "Calama", "Copiapó", "Curicó", "Los Ángeles"}
	sectors := []string{"", " Norte", " Sur", " Oriente", " Poniente", " Centro Cívico", " Villa Los Aromos", " Alto"}

	var out []string

	for _, sector := range sectors {
		for _, place := range places {
			if len(out) == n {
				return out
			}

			out = append(out, place+sector)
		}
	}

	for len(out) < n {
		out = append(out, fmt.Sprintf("%s %d", places[len(out)%len(places)], len(out)))
	}

	return out
}

// evalGroupQueries measures queries that name a document's group after the
// first words of its text, against the same words alone: what they read
// cold, and where the document ranks.
func evalGroupQueries(t *testing.T, f *fixture, docs []collection.Doc) {
	var requests, bytes, withRank, withoutRank []int
	var top1With, top1Without, n int

	rank := func(resp Response, d collection.Doc) int {
		for i, h := range resp.Hits {
			if h.OriginID == d.OriginID && h.ID == d.ID {
				return i + 1
			}
		}

		return len(resp.Hits) + 1
	}

	var picked []collection.Doc
	var with, without []Request

	for i, d := range docs {
		if d.Group == "" || i%7 != 0 || n == 1500 {
			continue
		}

		words := strings.Fields(d.Text)
		name := strings.Join(words[:min(2, len(words))], " ")
		n++

		picked = append(picked, d)
		with = append(with, Request{Query: name + " " + d.Group, Limit: 100})
		without = append(without, Request{Query: name, Limit: 100})
	}

	withResp, withoutResp := coldAll(t, f, DefaultOptions(), with), coldAll(t, f, DefaultOptions(), without)

	for i, d := range picked {
		resp := withResp[i]
		requests = append(requests, resp.Stats.Requests)
		bytes = append(bytes, int(resp.Stats.Bytes))
		withRank = append(withRank, rank(resp, d))

		if withRank[len(withRank)-1] == 1 {
			top1With++
		}

		withoutRank = append(withoutRank, rank(withoutResp[i], d))

		if withoutRank[len(withoutRank)-1] == 1 {
			top1Without++
		}
	}

	t.Logf("group queries (%d, cold): requests %.2f (max %d) · bytes %s · $%.4f per 1M · rank of the document %.1f with the group, %.1f without · first %d with, %d without",
		n, avgOf(requests), slices.Max(requests), dist(bytes), perMillion(requests, bytes), avgOf(withRank), avgOf(withoutRank), top1With, top1Without)
}

type dist []int

func (d dist) String() string {
	if len(d) == 0 {
		return "-"
	}

	s := slices.Sorted(slices.Values(d))
	total := 0

	for _, v := range s {
		total += v
	}

	return fmt.Sprintf("avg %s p50 %s p99 %s max %s", kib(total/len(s)), kib(s[len(s)/2]), kib(s[min(len(s)-1, len(s)*99/100)]), kib(s[len(s)-1]))
}

func avgOf(values []int) float64 {
	total := 0
	for _, v := range values {
		total += v
	}

	return float64(total) / float64(max(len(values), 1))
}

// perMillion is the S3 cost of a million queries that made these requests
// and read these bytes, on average.
func perMillion(requests, bytes []int) float64 {
	p := pricing.S3Express

	return 1e6 * (avgOf(requests)*p.GET + avgOf(bytes)*p.Retrieval)
}

// coldAll runs every request cold (an engine of its own, as in a fresh
// Lambda) on every core, and returns the responses in order.
func coldAll(t *testing.T, f *fixture, opts Options, reqs []Request) []Response {
	t.Helper()

	out := make([]Response, len(reqs))

	err := parallel.Run(context.Background(), len(reqs), runtime.GOMAXPROCS(0), func(ctx context.Context, i int) error {
		resp, err := f.engine(opts).Search(ctx, reqs[i])
		if err != nil {
			return fmt.Errorf("%q: %w", reqs[i].Query, err)
		}

		out[i] = resp

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return out
}

// eachQuery calls work for every index on every core.
func eachQuery(n int, work func(i int)) {
	_ = parallel.Run(context.Background(), n, runtime.GOMAXPROCS(0), func(_ context.Context, i int) error {
		work(i)

		return nil
	})
}

func evalScale(t *testing.T, docs []collection.Doc, queries []string, r *rand.Rand, all bool) {
	grouping := layout.GroupingNone
	if name := os.Getenv("GXS_EVAL_GROUPING"); name != "" {
		var err error
		if grouping, err = layout.GroupingByName(name); err != nil {
			t.Fatal(err)
		}
	}

	start := time.Now()
	f := newFixtureGrouped(t, normalize.Default, docs, testOptions(), grouping)
	buildTime := time.Since(start)

	puts, wrote := f.store.Writes.Load(), f.store.BytesWritten.Load()
	t.Logf("%d docs · build %v · %d PUTs %s written · $%.4f", len(docs), buildTime.Round(time.Millisecond),
		puts, kib(int(wrote)), pricing.S3Express.Write(int(puts), wrote))

	logIndex(t, f)

	base := DefaultOptions()
	noCoalesce := base
	noCoalesce.CoalesceGap = 0

	variants := []struct {
		name string
		opts Options
	}{{"cold", base}, {"cold, no coalescing", noCoalesce}}

	for _, v := range variants {
		for _, words := range []int{1, 2, 3} {
			var reqs []Request

			for i, q := range queries {
				// 1 word: the first word of every other query, also as a
				// prefix like any last word.
				if words == 1 {
					if i%2 != 0 {
						continue
					}

					q = strings.Fields(q)[0]
				} else if len(strings.Fields(q)) != words {
					continue
				}

				reqs = append(reqs, Request{Query: q, Limit: 10})
			}

			var rounds, requests, bytes, idxBytes, dbBytes []int
			var approx, n, headed, deep, pairs int

			for _, resp := range coldAll(t, f, v.opts, reqs) {
				s := resp.Stats
				n++
				rounds = append(rounds, s.Rounds)
				requests = append(requests, s.Requests)
				bytes = append(bytes, int(s.Bytes))
				idxBytes = append(idxBytes, int(s.IdxBytes))
				dbBytes = append(dbBytes, int(s.Bytes-s.LogBytes-s.IdxBytes))

				if s.Approx {
					approx++
				}

				if s.Heads > 0 {
					headed++
				}

				if s.Deep > 0 {
					deep++
				}

				pairs += s.Pairs
			}

			if n == 0 {
				continue
			}

			t.Logf("%-20s %d words (%4d queries): $%.4f per 1M · rounds %.2f (max %d) · requests %.2f (max %d) · bytes %s · .idx %s · .db %s · approx %d · heads %d · deep %d · pairs %d",
				v.name, words, n, perMillion(requests, bytes), avgOf(rounds), slices.Max(rounds), avgOf(requests), slices.Max(requests), dist(bytes), dist(idxBytes), dist(dbBytes), approx, headed, deep, pairs)
		}
	}

	if slices.ContainsFunc(docs, func(d collection.Doc) bool { return d.Group != "" }) {
		evalGroupQueries(t, f, docs)
	}

	if !all {
		return
	}

	state := map[docid.Key]string{}
	whole := map[docid.Key]collection.Doc{}

	for _, d := range docs {
		state[d.Key()] = d.Text
		whole[d.Key()] = d
	}

	ref := newReference(normalize.Default, whole, whole, nil, 0)
	full := newFullText(normalize.Default, state)

	// Ranking: against the same rules over every document, and against the
	// full words.
	unlimited := base
	unlimited.PrefixBudget = 1 << 40

	reqs := make([]Request, len(queries))
	for i, q := range queries {
		reqs[i] = Request{Query: q, Limit: 10}
	}

	resps := coldAll(t, f, unlimited, reqs)
	sameAt, sameFullAt, sameFullSetAt, overlapAt := make([]int, len(queries)), make([]int, len(queries)), make([]int, len(queries)), make([]int, len(queries))

	eachQuery(len(queries), func(i int) {
		parsed := Parse(normalize.Default, queries[i])

		if slices.Equal(resps[i].Hits, ref.top(parsed, 0, 10, false)) {
			sameAt[i] = 1
		}

		want := full.top(parsed, 10, len(docs))
		got := keys(resps[i].Hits)

		if slices.Equal(got, want) {
			sameFullAt[i] = 1
		}

		if slices.Equal(slices.Sorted(slices.Values(got)), slices.Sorted(slices.Values(want))) {
			sameFullSetAt[i] = 1
		}

		for _, id := range want {
			if slices.Contains(got, id) {
				overlapAt[i]++
			}
		}
	})

	n := len(queries)
	same, sameFull, sameFullSet, overlap := sum(sameAt), sum(sameFullAt), sum(sameFullSetAt), sum(overlapAt)
	t.Logf("top-10 vs the same rules over every document: identical %d/%d", same, n)
	t.Logf("top-10 vs full words (no 8-letter cut): identical order %d/%d (%.1f%%) · same IDs %d/%d (%.1f%%) · overlap %.1f%%",
		sameFull, n, 100*float64(sameFull)/float64(n), sameFullSet, n, 100*float64(sameFullSet)/float64(n), 100*float64(overlap)/float64(10*n))

	if same != n {
		t.Errorf("%d queries rank differently from the reference", n-same)
	}

	evalFuzzy(t, f, docs, state, r)
	evalWrites(t, f, docs, r, queries)
}

// evalFuzzy types two words of a document's name, one of them with a typo,
// and looks for a document with that name in the top 10.
func evalFuzzy(t *testing.T, f *fixture, docs []collection.Doc, textOf map[docid.Key]string, r *rand.Rand) {
	// The same queries with no budget: what the budget costs.
	unlimited := DefaultOptions()
	unlimited.PrefixBudget = 1 << 40

	var reqs []Request
	var texts []string
	total, early := 0, 0

	for total < 500 {
		doc := docs[r.IntN(len(docs))]
		tokens := normalize.Tokens(doc.Text)

		var long []int
		for i, tok := range tokens {
			if len(tok) >= 5 && !normalize.IsNumber(tok) {
				long = append(long, i)
			}
		}

		if len(long) < 2 {
			continue
		}

		i := long[r.IntN(len(long))]
		typo := mutate(tokens[i], r)
		other := tokens[long[0]]

		if i == long[0] {
			other = tokens[long[1]]
		}

		q := typo + " " + other
		total++

		if len(typo) < 2 || typo[:2] != tokens[i][:2] {
			early++ // a typo in the first two letters: not searched, by design
		}

		reqs = append(reqs, Request{Query: q, Limit: 10, Fuzzy: true})
		texts = append(texts, doc.Text)
	}

	resps, wholes := coldAll(t, f, DefaultOptions(), reqs), coldAll(t, f, unlimited, reqs)

	var requests, bytes, rounds []int
	found, covered, foundAll, approx := 0, 0, 0, 0

	for i, resp := range resps {
		if slices.ContainsFunc(resp.Hits, func(h Hit) bool { return textOf[docid.New(h.OriginID, h.ID)] == texts[i] }) {
			found++
		}

		if len(resp.Hits) > 0 && resp.Hits[0].Coverage == 2 {
			covered++
		}

		if resp.Stats.Approx {
			approx++
		}

		if slices.ContainsFunc(wholes[i].Hits, func(h Hit) bool { return textOf[docid.New(h.OriginID, h.ID)] == texts[i] }) {
			foundAll++
		}

		requests = append(requests, resp.Stats.Requests)
		bytes = append(bytes, int(resp.Stats.Bytes))
		rounds = append(rounds, resp.Stats.Rounds)
	}

	t.Logf("typos (%d queries of 2 words, one with a typo, %d of them in the first two letters): the document in the top 10 %d/%d (%d/%d with no budget, %d cut by it) · top hit covers both words %d/%d · cold: $%.4f per 1M · rounds %.2f · requests %.2f · bytes %s",
		total, early, found, total, foundAll, total, approx, covered, total, perMillion(requests, bytes), avgOf(rounds), avgOf(requests), dist(bytes))
}

// mutate makes one typo: a swap, a deletion, an insertion or a substitution.
func mutate(word string, r *rand.Rand) string {
	b := []byte(word)
	i := r.IntN(len(b) - 1)

	switch r.IntN(4) {
	case 0:
		b[i], b[i+1] = b[i+1], b[i]
	case 1:
		b = append(b[:i], b[i+1:]...)
	case 2:
		b = slices.Insert(b, i, byte('a'+r.IntN(26)))
	default:
		b[i] = byte('a' + r.IntN(26))
	}

	return string(b)
}

// evalWrites measures appends, a query with a full p.log, and a compaction.
func evalWrites(t *testing.T, f *fixture, docs []collection.Doc, r *rand.Rand, queries []string) {
	ctx := context.Background()

	f.store.Reset()

	res, err := f.w.Put(ctx, []collection.Doc{{ID: 1, Text: docs[r.IntN(len(docs))].Text}})
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("put 1 document: %d reads %s · %d writes %s · $%.7f", f.store.Requests.Load(), kib(int(f.store.BytesRead.Load())),
		f.store.Writes.Load(), kib(int(f.store.BytesWritten.Load())), storeCost(f.store))

	// Fill p.log up to the compaction threshold, one document per append.
	f.store.Reset()

	appends := 1
	for !res.CompactRequested {
		doc := docs[r.IntN(len(docs))]
		if res, err = f.w.Put(ctx, []collection.Doc{{OriginID: doc.OriginID, ID: doc.ID, Text: doc.Text + " nuevo"}}); err != nil {
			t.Fatal(err)
		}

		appends++
	}

	t.Logf("p.log reached %s after %d appends of one document · $%.6f for them", kib(int(res.Size)), appends, storeCost(f.store))

	reqs := make([]Request, 500)
	for i, q := range queries[:500] {
		reqs[i] = Request{Query: q, Limit: 10}
	}

	var requests, bytes, logBytes []int

	for _, resp := range coldAll(t, f, DefaultOptions(), reqs) {
		requests = append(requests, resp.Stats.Requests)
		bytes = append(bytes, int(resp.Stats.Bytes))
		logBytes = append(logBytes, int(resp.Stats.LogBytes))
	}

	t.Logf("cold with a full p.log: $%.4f per 1M · bytes %s (p.log %s)", perMillion(requests, bytes), dist(bytes), kib(int(avgOf(logBytes))))

	f.store.Reset()
	start := time.Now()

	cres, err := f.w.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}

	t.Logf("compaction of %d ops: %v · %d buckets rewritten · %d reads %s · %d writes %s · $%.6f",
		cres.Ops, time.Since(start).Round(time.Millisecond), cres.Buckets, f.store.Requests.Load(), kib(int(f.store.BytesRead.Load())),
		f.store.Writes.Load(), kib(int(f.store.BytesWritten.Load())), storeCost(f.store))
}

// storeCost is the S3 cost of what went through the store since its last
// Reset: reads and writes.
func storeCost(c *blob.Counting) float64 {
	p := pricing.S3Express

	return p.Read(int(c.Requests.Load()), c.BytesRead.Load()) + p.Write(int(c.Writes.Load()), c.BytesWritten.Load())
}

// fullText ranks by whole words, the way the index would if it kept every
// letter: exact is the same word, a prefix is a prefix of the whole word.
type fullText struct {
	ids    []docid.Key
	tokens [][]string
	byWord map[string][]int
}

func newFullText(norm *normalize.Normalizer, docs map[docid.Key]string) *fullText {
	f := &fullText{byWord: map[string][]int{}}

	for id, text := range docs {
		tokens := norm.Tokens(text)
		for _, tok := range slices.Compact(slices.Sorted(slices.Values(tokens))) {
			f.byWord[tok] = append(f.byWord[tok], len(f.ids))
		}

		f.ids = append(f.ids, id)
		f.tokens = append(f.tokens, tokens)
	}

	return f
}

func (f *fullText) matches(t *Term, tok string) (matchKind, uint8) {
	if !t.Explicit && tok == t.Text {
		return exactMatch, 0
	}

	if t.Auto || t.Explicit {
		if tok == t.Text || (t.prefixable() && strings.HasPrefix(tok, t.Text)) {
			return prefixMatch, 0
		}
	}

	// A near word, with every letter.
	if t.Explicit || t.Number || normalize.IsNumber(tok) || t.letter() || t.virtual() {
		return noMatch, 0
	}

	short, long, kind := tok, t.Text, shorterMatch
	if len(tok) > len(t.Text) {
		short, long, kind = t.Text, tok, longerMatch
	}

	d := len(long) - len(short)
	if d > 0 && d <= NearExtra && len(short) >= NearMinLetters && strings.HasPrefix(long, short) && (d == 1 || strings.HasSuffix(long, "es")) {
		return kind, uint8(d)
	}

	return noMatch, 0
}

func (f *fullText) top(q Parsed, limit, docs int) []docid.Key {
	cand := map[int]bool{}

	for word, list := range f.byWord {
		for i := range q.Terms {
			if kind, _ := f.matches(&q.Terms[i], word); kind != noMatch {
				for _, d := range list {
					cand[d] = true
				}

				break
			}
		}
	}

	cands := newCandidates(len(q.Terms), len(cand))
	df := make([]int, len(q.Terms))

	for d := range cand {
		matches := cands.get(f.ids[d])

		for pos, tok := range f.tokens[d] {
			for i := range q.Terms {
				kind, dist := f.matches(&q.Terms[i], tok)
				matches[i].add(kind, dist, uint8(min(pos, 255)))
			}
		}

		for i := range matches {
			if matches[i].kind != noMatch {
				df[i]++
			}
		}
	}

	results := rankAll(q.Terms, cands, idfs(df, docs), 1<<30)
	out := make([]docid.Key, 0, limit)

	for _, r := range results[:min(limit, len(results))] {
		out = append(out, r.id)
	}

	return out
}

// sampleQueries takes two or three consecutive words of random names.
func sampleQueries(names []string, n int) []string {
	r := rand.New(rand.NewPCG(42, 7))

	var queries []string

	for len(queries) < n {
		tokens := normalize.Tokens(names[r.IntN(len(names))])
		if len(tokens) < 2 {
			continue
		}

		size := min(len(tokens), 2+r.IntN(2))
		start := r.IntN(len(tokens) - size + 1)
		queries = append(queries, strings.Join(tokens[start:start+size], " "))
	}

	return queries
}

func readLines(t *testing.T, path string) []string {
	t.Helper()

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var lines []string

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	return lines
}

// logIndex prints what the index takes: the buckets, their .idx and .db.
func logIndex(t *testing.T, f *fixture) {
	ctx := context.Background()

	data, _, err := f.store.Get(ctx, f.col.Tokens())
	if err != nil {
		t.Fatal(err)
	}

	tok, err := tokens.Decode(data, f.norm.ID())
	if err != nil {
		t.Fatal(err)
	}

	var idx, db []int
	var biggest string
	var biggestIdx int

	for group := range tok.Buckets {
		x, _, err := f.store.Get(ctx, f.col.Idx(group))
		if err != nil {
			t.Fatal(err)
		}

		idx = append(idx, len(x))

		if len(x) > biggestIdx {
			biggest, biggestIdx = f.col.Grouping().Name(group), len(x)
		}

		if d, _, err := f.store.Get(ctx, f.col.DB(group)); err == nil {
			db = append(db, len(d))
		}
	}

	t.Logf("%d buckets: .idx %s, total %s · %d .db: %s, total %s · %d expanded words · tokens %s · biggest .idx %s (%s)",
		len(idx), dist(idx), kib(sum(idx)), len(db), dist(db), kib(sum(db)), len(tok.Expanded), kib(len(data)), biggest, kib(biggestIdx))
}

func sum(values []int) int {
	total := 0
	for _, v := range values {
		total += v
	}

	return total
}

func kib(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}

	if n < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}

	return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
}
