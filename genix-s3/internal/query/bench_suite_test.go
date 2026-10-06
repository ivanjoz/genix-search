package query

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

// TestBench is the standard benchmark of PLAN_BENCH.md: how well the engine
// finds (against relevance judgments and against BM25) and what it reads from
// S3 (requests, bytes and rounds, cold and warm), on a fixed corpus of 10 010
// products and 200 fixed queries. It writes testdata/bench/report.md and
// results.json, and fails when the result is worse than baseline.json.
//
//	GXS_BENCH=1 go test ./internal/query -run Bench -v
//	GXS_BENCH=1 go test ./internal/query -run Bench -v -random-ids
//
// To accept a result, copy results.json over baseline.json in the same commit.
// The databases stay in bench/db/{names,groups}, so they can be queried by
// hand: gxs query -root bench/db/groups -collection productos ...
var benchRandomIDs = flag.Bool("random-ids", false, "TestBench: random uint32 IDs instead of the row number (the results go to *.random-ids files)")

const benchDir = "../../testdata/bench"

// The categories of the queries, in the order of the report.
var benchCategories = []string{"exacta", "parcial", "escribiendo", "typo", "tildes", "marca", "numeros", "plural", "nombre+grupo", "grupo", "negativa"}

const benchAll = "todas"

type benchProduct struct {
	ID              int
	Name, Cat, Mark string
}

type benchQuery struct {
	ID, Cat, Text string
	Target        int // the product, 0 when there is none
	Bases         string
}

// fuzzy: typos are typed by the people who make them, so the app asks for it.
func (q benchQuery) fuzzy() bool { return q.Cat == "typo" }

type benchData struct {
	products []benchProduct
	queries  []benchQuery
	qrels    map[string]map[int]int      // query -> product -> grade
	ref      map[string]map[string][]int // system/base -> query -> top 10
}

func readTSV(t *testing.T, name string) [][]string {
	t.Helper()

	file, err := os.Open(filepath.Join(benchDir, name))
	if err != nil {
		t.Fatalf("%v (python3 scripts/bench/*.py makes it)", err)
	}
	defer file.Close()

	var rows [][]string

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for scanner.Scan() {
		rows = append(rows, strings.Split(scanner.Text(), "\t"))
	}

	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}

	return rows[1:] // the header
}

func atoi(t *testing.T, s string) int {
	t.Helper()

	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatal(err)
	}

	return n
}

func loadBench(t *testing.T) *benchData {
	d := &benchData{qrels: map[string]map[int]int{}, ref: map[string]map[string][]int{}}

	for _, r := range readTSV(t, "productos.tsv") {
		d.products = append(d.products, benchProduct{ID: atoi(t, r[0]), Name: r[1], Cat: r[2], Mark: r[3]})
	}

	for _, r := range readTSV(t, "queries.tsv") {
		q := benchQuery{ID: r[0], Cat: r[1], Text: r[2], Bases: r[4]}
		if r[3] != "" {
			q.Target = atoi(t, r[3])
		}

		d.queries = append(d.queries, q)
	}

	for _, r := range readTSV(t, "qrels.tsv") {
		if d.qrels[r[0]] == nil {
			d.qrels[r[0]] = map[int]int{}
		}

		d.qrels[r[0]][atoi(t, r[1])] = atoi(t, r[2])
	}

	for _, r := range readTSV(t, "reference.tsv") {
		key := r[0] + "/" + r[1]
		if d.ref[key] == nil {
			d.ref[key] = map[string][]int{}
		}

		var top []int
		for _, id := range strings.Fields(r[3]) {
			top = append(top, atoi(t, id))
		}

		d.ref[key][r[2]] = top
	}

	return d
}

// ids maps products to the IDs of the documents, and back.
type benchIDs struct {
	of  map[int]uint32
	row map[uint32]int
}

func newBenchIDs(d *benchData, random bool) benchIDs {
	ids := benchIDs{of: map[int]uint32{}, row: map[uint32]int{}}
	r := rand.New(rand.NewPCG(2026, 1005))

	for _, p := range d.products {
		id := uint32(p.ID)

		for random {
			id = r.Uint32()
			if _, taken := ids.row[id]; !taken && id != 0 {
				break
			}
		}

		ids.of[p.ID] = id
		ids.row[id] = p.ID
	}

	return ids
}

func (d *benchData) docs(ids benchIDs, grouped bool) []collection.Doc {
	docs := make([]collection.Doc, len(d.products))

	for i, p := range d.products {
		docs[i] = collection.Doc{ID: ids.of[p.ID], Text: p.Name}
		if grouped {
			docs[i].Group = p.Cat
		}
	}

	return docs
}

// benchDB is one of the two local databases of the benchmark.
type benchDB struct {
	store  *kindCounter
	col    layout.Collection
	docs   []collection.Doc
	Build  benchBuild
	Millis int64
	Sizes  benchSizes
	reused bool
}

type benchBuild struct {
	Docs    int   `json:"docs"`
	Puts    int64 `json:"puts"`
	KBWrote int64 `json:"kb_written"`
}

// benchStampFile is what stays next to a database: what it was built from
// and, left out of results.json because it varies from run to run, how long
// it took.
type benchStampFile struct {
	Hash   string
	Millis int64
	Build  benchBuild
}

type benchSizes struct {
	Idx    int64 `json:"idx"`
	DB     int64 `json:"db"`
	Tokens int64 `json:"tokens"`
	Log    int64 `json:"plog"`
	Files  int   `json:"files"`
}

// stamp is what tells that a database on disk is the one this code would build:
// the corpus, the mode and the source of everything that writes the index.
func benchStamp(t *testing.T, random bool) string {
	t.Helper()

	h := sha256.New()

	files := []string{filepath.Join(benchDir, "productos.tsv")}
	for _, pkg := range []string{"bits", "blob", "bucket", "collection", "docid", "key5", "layout", "normalize", "parallel", "plog", "postings", "tokens"} {
		sources, err := filepath.Glob(filepath.Join("..", pkg, "*.go"))
		if err != nil {
			t.Fatal(err)
		}

		for _, s := range sources {
			if !strings.HasSuffix(s, "_test.go") {
				files = append(files, s)
			}
		}
	}

	slices.Sort(files)

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}

		fmt.Fprintf(h, "%s\n%d\n", filepath.Base(f), len(data))
		h.Write(data)
	}

	fmt.Fprintf(h, "random=%v es100", random)

	return hex.EncodeToString(h.Sum(nil))
}

// openBenchDB opens bench/db/<name>, building it first when it is missing or
// was built by other code or other data.
func openBenchDB(t *testing.T, name string, docs []collection.Doc, random bool) *benchDB {
	t.Helper()

	dir := filepath.Join("../../bench/db", name)
	if random {
		dir += "-random-ids"
	}

	col, err := layout.New("genix/", "productos", "default")
	if err != nil {
		t.Fatal(err)
	}

	db := &benchDB{col: col.With(layout.GroupingES100), docs: docs}
	hash := benchStamp(t, random)

	var old benchStampFile
	if data, err := os.ReadFile(filepath.Join(dir, "stamp.json")); err == nil && json.Unmarshal(data, &old) == nil && old.Hash == hash {
		db.reused = true
		db.Build, db.Millis = old.Build, old.Millis
	}

	fsys, err := blob.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}

	if !db.reused {
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}

		if fsys, err = blob.NewFS(dir); err != nil {
			t.Fatal(err)
		}

		counting := &blob.Counting{Blob: fsys}
		start := time.Now()

		if _, err := collection.New(counting, db.col, normalize.Default, testOptions()).Init(context.Background(), docs); err != nil {
			t.Fatal(err)
		}

		db.Build = benchBuild{Docs: len(docs), Puts: counting.Writes.Load(), KBWrote: counting.BytesWritten.Load() >> 10}
		db.Millis = time.Since(start).Milliseconds()

		stamp, err := json.Marshal(benchStampFile{Hash: hash, Millis: db.Millis, Build: db.Build})
		if err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dir, "stamp.json"), stamp, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	db.store = &kindCounter{Blob: fsys}

	err = filepath.WalkDir(dir, func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() {
			return err
		}

		info, err := e.Info()
		if err != nil {
			return err
		}

		switch {
		case strings.HasSuffix(path, ".idx"):
			db.Sizes.Idx += info.Size()
		case strings.HasSuffix(path, ".db"):
			db.Sizes.DB += info.Size()
		case strings.HasSuffix(path, "/tokens"):
			db.Sizes.Tokens += info.Size()
		case strings.HasSuffix(path, "/p.log"):
			db.Sizes.Log += info.Size()
		default:
			return nil
		}

		db.Sizes.Files++

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	return db
}

// kindCounter counts the reads of a search by the kind of object: .idx, .db,
// p.log and the rest (tokens).
type kindCounter struct {
	blob.Blob
	mu   sync.Mutex
	cost benchCost
}

func (k *kindCounter) record(key string, n int) {
	k.mu.Lock()
	defer k.mu.Unlock()

	k.cost.Requests++
	k.cost.Bytes += int64(n)

	switch {
	case strings.HasSuffix(key, ".idx"):
		k.cost.IdxReq++
		k.cost.IdxBytes += int64(n)
	case strings.HasSuffix(key, ".db"):
		k.cost.DBReq++
		k.cost.DBBytes += int64(n)
	case strings.HasSuffix(key, "p.log"):
		k.cost.LogReq++
		k.cost.LogBytes += int64(n)
	default:
		k.cost.OtherReq++
		k.cost.OtherBytes += int64(n)
	}
}

func (k *kindCounter) GetRange(ctx context.Context, key string, off, n int64) ([]byte, string, error) {
	data, etag, err := k.Blob.GetRange(ctx, key, off, n)
	k.record(key, len(data))

	return data, etag, err
}

func (k *kindCounter) Get(ctx context.Context, key string) ([]byte, string, error) {
	data, etag, err := k.Blob.Get(ctx, key)
	k.record(key, len(data))

	return data, etag, err
}

// take returns what was read since the last call.
func (k *kindCounter) take() benchCost {
	k.mu.Lock()
	defer k.mu.Unlock()

	c := k.cost
	k.cost = benchCost{}

	return c
}

// benchCost is what one search read.
type benchCost struct {
	Requests   int   `json:"get"`
	Rounds     int   `json:"rounds"`
	Bytes      int64 `json:"bytes"`
	IdxReq     int   `json:"idx_get"`
	DBReq      int   `json:"db_get"`
	LogReq     int   `json:"log_get"`
	OtherReq   int   `json:"other_get"`
	IdxBytes   int64 `json:"idx_bytes"`
	DBBytes    int64 `json:"db_bytes"`
	LogBytes   int64 `json:"log_bytes"`
	OtherBytes int64 `json:"other_bytes"`
}

// metrics of one ranking of one query (PLAN_BENCH.md 6).
type benchMetrics struct {
	Top       []int   `json:"top"`
	HasTarget bool    `json:"-"`
	S1        float64 `json:"s1,omitempty"`
	S10       float64 `json:"s10,omitempty"`
	MRR       float64 `json:"mrr,omitempty"`
	Judged    bool    `json:"-"` // nDCG and P@10 count (not the negative queries)
	NDCG      float64 `json:"ndcg,omitempty"`
	P10       float64 `json:"p10,omitempty"`
	False     float64 `json:"false,omitempty"`
	Overlap   float64 `json:"overlap,omitempty"`
}

func round4(x float64) float64 { return math.Round(x*1e4) / 1e4 }

func gain(grade int) float64 { return float64(int(1)<<grade - 1) }

func discount(rank int) float64 { return 1 / math.Log2(float64(rank)+2) }

// scoreRanking judges a top 10 by the grades of its query.
func scoreRanking(q benchQuery, grades map[int]int, top, r1 []int) benchMetrics {
	m := benchMetrics{Top: top, HasTarget: q.Target != 0, Judged: q.Cat != "negativa"}

	for i, id := range top {
		if grades[id] == 3 {
			if i == 0 {
				m.S1 = 1
			}

			m.S10 = 1
			m.MRR = round4(1 / float64(i+1))

			break
		}
	}

	if q.Cat == "negativa" {
		if len(top) > 0 {
			m.False = 1
		}
	} else {
		var dcg, good, bad float64

		for i, id := range top {
			dcg += gain(grades[id]) * discount(i)

			if grades[id] >= 2 {
				good++
			}

			if grades[id] == 0 {
				bad++
			}
		}

		var ideal []int
		for _, g := range grades {
			ideal = append(ideal, g)
		}

		slices.SortFunc(ideal, func(a, b int) int { return b - a })

		var best float64
		for i, g := range ideal[:min(10, len(ideal))] {
			best += gain(g) * discount(i)
		}

		if best > 0 {
			m.NDCG = round4(dcg / best)
		}

		m.P10 = round4(good / 10)

		if len(top) > 0 {
			m.False = round4(bad / float64(len(top)))
		}
	}

	if len(r1) > 0 || len(top) > 0 {
		common := 0

		for _, id := range top {
			if slices.Contains(r1, id) {
				common++
			}
		}

		m.Overlap = round4(float64(common) / 10)
	}

	return m
}

// benchResult is one query on one base: the effectiveness of each system and
// what ours read.
type benchResult struct {
	Query  string        `json:"q"`
	Base   string        `json:"base"`
	Cat    string        `json:"cat"`
	Text   string        `json:"text"`
	Target int           `json:"target,omitempty"`
	Ours   benchMetrics  `json:"ours"`
	R1     benchMetrics  `json:"r1"`
	R2     benchMetrics  `json:"r2"`
	R3     benchMetrics  `json:"r3"`
	R3Full *benchMetrics `json:"r3_full_words,omitempty"` // base A only: no 8-letter cut
	Cold   benchCost     `json:"cold"`
	Warm   benchCost     `json:"warm"`
}

// benchRow is the average of a set of results (PLAN_BENCH.md 8).
type benchRow struct {
	N       int `json:"n"`
	Targets int `json:"targets"`

	S1, S10, MRR     float64
	NDCG, P10, False float64
	Overlap          float64

	R1, R2, R3 benchSystem

	ColdGet, ColdKB benchDist
	WarmGet, WarmKB float64
	Rounds          float64
	// By kind of object, cold average.
	IdxGet, DBGet, LogGet, OtherGet float64
	IdxKB, DBKB, LogKB, OtherKB     float64
}

type benchSystem struct {
	S1, S10, MRR, NDCG, P10, False float64
}

type benchDist struct{ Avg, P50, P95, Max float64 }

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}

	var total float64
	for _, v := range values {
		total += v
	}

	return total / float64(len(values))
}

func dist4(values []float64) benchDist {
	if len(values) == 0 {
		return benchDist{}
	}

	s := slices.Sorted(slices.Values(values))
	at := func(p float64) float64 { return round4(s[max(0, int(math.Ceil(p*float64(len(s))))-1)]) }

	return benchDist{Avg: round4(mean(values)), P50: at(0.5), P95: at(0.95), Max: round4(s[len(s)-1])}
}

func systemRow(ms []benchMetrics) benchSystem {
	var s1, s10, mrr, ndcg, p10, bad []float64

	for _, m := range ms {
		if m.HasTarget {
			s1, s10, mrr = append(s1, m.S1), append(s10, m.S10), append(mrr, m.MRR)
		}

		if m.Judged {
			ndcg, p10 = append(ndcg, m.NDCG), append(p10, m.P10)
		}

		bad = append(bad, m.False)
	}

	return benchSystem{round4(mean(s1)), round4(mean(s10)), round4(mean(mrr)), round4(mean(ndcg)), round4(mean(p10)), round4(mean(bad))}
}

func summarize(rs []benchResult) benchRow {
	row := benchRow{N: len(rs)}

	var ours, r1, r2, r3 []benchMetrics
	var overlap, coldGet, coldKB, warmGet, warmKB, rounds []float64
	var kinds [8][]float64

	for _, r := range rs {
		if r.Target != 0 {
			row.Targets++
		}

		ours, r1, r2, r3 = append(ours, r.Ours), append(r1, r.R1), append(r2, r.R2), append(r3, r.R3)
		overlap = append(overlap, r.Ours.Overlap)
		coldGet, coldKB = append(coldGet, float64(r.Cold.Requests)), append(coldKB, float64(r.Cold.Bytes)/1024)
		warmGet, warmKB = append(warmGet, float64(r.Warm.Requests)), append(warmKB, float64(r.Warm.Bytes)/1024)
		rounds = append(rounds, float64(r.Cold.Rounds))

		for i, v := range []float64{float64(r.Cold.IdxReq), float64(r.Cold.DBReq), float64(r.Cold.LogReq), float64(r.Cold.OtherReq),
			float64(r.Cold.IdxBytes) / 1024, float64(r.Cold.DBBytes) / 1024, float64(r.Cold.LogBytes) / 1024, float64(r.Cold.OtherBytes) / 1024} {
			kinds[i] = append(kinds[i], v)
		}
	}

	s := systemRow(ours)
	row.S1, row.S10, row.MRR, row.NDCG, row.P10, row.False = s.S1, s.S10, s.MRR, s.NDCG, s.P10, s.False
	row.Overlap = round4(mean(overlap))
	row.R1, row.R2, row.R3 = systemRow(r1), systemRow(r2), systemRow(r3)
	row.ColdGet, row.ColdKB = dist4(coldGet), dist4(coldKB)
	row.WarmGet, row.WarmKB, row.Rounds = round4(mean(warmGet)), round4(mean(warmKB)), round4(mean(rounds))
	row.IdxGet, row.DBGet, row.LogGet, row.OtherGet = round4(mean(kinds[0])), round4(mean(kinds[1])), round4(mean(kinds[2])), round4(mean(kinds[3]))
	row.IdxKB, row.DBKB, row.LogKB, row.OtherKB = round4(mean(kinds[4])), round4(mean(kinds[5])), round4(mean(kinds[6])), round4(mean(kinds[7]))

	return row
}

// benchOutput is results.json.
type benchOutput struct {
	IDs     string                         `json:"ids"`
	Build   map[string]benchBuild          `json:"build"`
	Sizes   map[string]benchSizes          `json:"sizes"`
	Summary map[string]map[string]benchRow `json:"summary"` // base -> category (or "todas") -> row
	Queries []benchResult                  `json:"queries"`
}

func (o benchOutput) write(path string) error {
	var b strings.Builder

	section := func(key string, v any, last bool) error {
		data, err := json.MarshalIndent(v, "  ", "  ")
		if err != nil {
			return err
		}

		fmt.Fprintf(&b, "  %q: %s", key, data)

		if !last {
			b.WriteString(",")
		}

		b.WriteString("\n")

		return nil
	}

	b.WriteString("{\n")

	for _, s := range []struct {
		key string
		v   any
	}{{"ids", o.IDs}, {"build", o.Build}, {"sizes", o.Sizes}, {"summary", o.Summary}} {
		if err := section(s.key, s.v, false); err != nil {
			return err
		}
	}

	// One query per line, so that the diff of a baseline shows what changed.
	b.WriteString("  \"queries\": [\n")

	for i, q := range o.Queries {
		data, err := json.Marshal(q)
		if err != nil {
			return err
		}

		b.WriteString("    " + string(data))

		if i < len(o.Queries)-1 {
			b.WriteString(",")
		}

		b.WriteString("\n")
	}

	b.WriteString("  ]\n}\n")

	return os.WriteFile(path, []byte(b.String()), 0o644)
}

func TestBench(t *testing.T) {
	if os.Getenv("GXS_BENCH") == "" {
		t.Skip("set GXS_BENCH=1 (PLAN_BENCH.md)")
	}

	ctx := context.Background()
	d := loadBench(t)
	ids := newBenchIDs(d, *benchRandomIDs)

	suffix := ""
	if *benchRandomIDs {
		suffix = ".random-ids"
	}

	dbs := map[string]*benchDB{
		"A": openBenchDB(t, "names", d.docs(ids, false), *benchRandomIDs),
		"B": openBenchDB(t, "groups", d.docs(ids, true), *benchRandomIDs),
	}

	rows := func(hits []Hit) []int {
		out := make([]int, len(hits))
		for i, h := range hits {
			out[i] = ids.row[h.ID]
		}

		return out
	}

	var results []benchResult

	for _, base := range []string{"A", "B"} {
		db := dbs[base]

		// R3: our rules over every document, no index and no budget; and, on the
		// names, the same with the whole words.
		whole := map[docid.Key]collection.Doc{}
		text := map[docid.Key]string{}

		for _, doc := range db.docs {
			whole[doc.Key()] = doc
			text[doc.Key()] = doc.Text
		}

		ref := newReference(normalize.Default, whole, whole, nil, 0)
		full := newFullText(normalize.Default, text)

		for _, q := range d.queries {
			if !strings.Contains(q.Bases, base) {
				continue
			}

			req := Request{Query: q.Text, Limit: 10, Fuzzy: q.fuzzy()}

			db.store.take()

			e := New(db.store, db.col, normalize.Default, DefaultOptions())

			cold, err := e.Search(ctx, req)
			if err != nil {
				t.Fatalf("%s %q: %v", q.ID, q.Text, err)
			}

			coldCost := db.store.take()
			coldCost.Rounds = cold.Stats.Rounds

			if coldCost.Requests != cold.Stats.Requests || coldCost.Bytes != cold.Stats.Bytes {
				t.Errorf("%s: the engine says %d requests, %d bytes; the store saw %d, %d", q.ID, cold.Stats.Requests, cold.Stats.Bytes, coldCost.Requests, coldCost.Bytes)
			}

			warm, err := e.Search(ctx, req)
			if err != nil {
				t.Fatalf("%s %q (warm): %v", q.ID, q.Text, err)
			}

			warmCost := db.store.take()
			warmCost.Rounds = warm.Stats.Rounds

			if !slices.Equal(rows(cold.Hits), rows(warm.Hits)) {
				t.Errorf("%s %q: the warm ranking differs from the cold", q.ID, q.Text)
			}

			grades := d.qrels[q.ID]
			r1 := d.ref["R1/"+base][q.ID]
			parsed := Parse(normalize.Default, q.Text)

			r3 := rows(ref.top(parsed, 0, 10, q.fuzzy()))

			r := benchResult{
				Query: q.ID, Base: base, Cat: q.Cat, Text: q.Text, Target: q.Target,
				Ours: scoreRanking(q, grades, rows(cold.Hits), r1),
				R1:   scoreRanking(q, grades, r1, r1),
				R2:   scoreRanking(q, grades, d.ref["R2/"+base][q.ID], r1),
				R3:   scoreRanking(q, grades, r3, r1),
				Cold: coldCost, Warm: warmCost,
			}

			if base == "A" {
				var top []int
				for _, k := range full.top(parsed, 10, len(db.docs)) {
					top = append(top, ids.row[k.ID()])
				}

				m := scoreRanking(q, grades, top, r1)
				r.R3Full = &m
			}

			results = append(results, r)
		}
	}

	out := benchOutput{
		IDs: "row numbers", Build: map[string]benchBuild{}, Sizes: map[string]benchSizes{},
		Summary: map[string]map[string]benchRow{"A": {}, "B": {}}, Queries: results,
	}

	if *benchRandomIDs {
		out.IDs = "random uint32"
	}

	for base, db := range dbs {
		out.Build[base], out.Sizes[base] = db.Build, db.Sizes

		var all []benchResult

		for _, cat := range benchCategories {
			var rs []benchResult

			for _, r := range results {
				if r.Base == base && r.Cat == cat {
					rs = append(rs, r)
				}
			}

			if len(rs) > 0 {
				out.Summary[base][cat] = summarize(rs)
				all = append(all, rs...)
			}
		}

		out.Summary[base][benchAll] = summarize(all)
	}

	report := benchReport(out, dbs)

	if err := out.write(filepath.Join(benchDir, "results"+suffix+".json")); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(benchDir, "report"+suffix+".md"), []byte(report), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Log("\n" + report)

	checkBaseline(t, out, filepath.Join(benchDir, "baseline"+suffix+".json"))
}

// checkBaseline fails when the result is worse than the accepted one
// (PLAN_BENCH.md 7): the global nDCG@10 or MRR down by more than 0.01, a
// category by more than 0.03, or the cold GETs or KB (p50) up by more than 5 %.
func checkBaseline(t *testing.T, out benchOutput, path string) {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Logf("no %s: nothing to compare with (cp results.json baseline.json accepts this result)", filepath.Base(path))

		return
	}

	var base benchOutput
	if err := json.Unmarshal(data, &base); err != nil {
		t.Fatalf("%s: %v", path, err)
	}

	const eps = 1e-9

	for _, b := range []string{"A", "B"} {
		for cat, now := range out.Summary[b] {
			was, ok := base.Summary[b][cat]
			if !ok {
				continue
			}

			limit := 0.03
			if cat == benchAll {
				limit = 0.01
			}

			if was.NDCG-now.NDCG > limit+eps {
				t.Errorf("base %s %s: nDCG@10 %.4f → %.4f (more than %.2f down)", b, cat, was.NDCG, now.NDCG, limit)
			}

			if was.Targets > 0 && was.MRR-now.MRR > limit+eps {
				t.Errorf("base %s %s: MRR %.4f → %.4f (more than %.2f down)", b, cat, was.MRR, now.MRR, limit)
			}

			if cat != benchAll {
				continue
			}

			if now.ColdGet.P50 > was.ColdGet.P50*1.05+eps {
				t.Errorf("base %s: cold GET p50 %.2f → %.2f (more than 5%% up)", b, was.ColdGet.P50, now.ColdGet.P50)
			}

			if now.ColdKB.P50 > was.ColdKB.P50*1.05+eps {
				t.Errorf("base %s: cold KB p50 %.2f → %.2f (more than 5%% up)", b, was.ColdKB.P50, now.ColdKB.P50)
			}
		}
	}

	// What changed, query by query.
	was := map[string]benchResult{}
	for _, r := range base.Queries {
		was[r.Base+r.Query] = r
	}

	var changed []string

	for _, r := range out.Queries {
		if old, ok := was[r.Base+r.Query]; ok && !slices.Equal(old.Ours.Top, r.Ours.Top) {
			changed = append(changed, fmt.Sprintf("%s/%s %q: nDCG %.2f → %.2f", r.Base, r.Query, r.Text, old.Ours.NDCG, r.Ours.NDCG))
		}
	}

	if len(changed) > 0 {
		t.Logf("%d queries changed their top 10 since the baseline:\n  %s", len(changed), strings.Join(changed, "\n  "))
	}
}

func pct(x float64) string { return fmt.Sprintf("%.2f", x) }

func (d benchDist) String() string {
	return fmt.Sprintf("%.0f · %.0f · %.0f", d.P50, d.P95, d.Max)
}

func kbDist(d benchDist) string { return fmt.Sprintf("%.1f · %.1f · %.1f", d.P50, d.P95, d.Max) }

func benchReport(out benchOutput, dbs map[string]*benchDB) string {
	var b strings.Builder

	names := map[string]string{"A": "A: nombres", "B": "B: nombres + grupos (categoría)"}

	fmt.Fprintf(&b, "# Benchmark de genix-s3 (PLAN_BENCH.md)\n\nIDs: %s. Corpus: %d productos, %d consultas. Se genera con `GXS_BENCH=1 go test ./internal/query -run Bench -v`.\n\n",
		out.IDs, len(dbs["A"].docs), len(distinctQueries(out.Queries)))

	b.WriteString("Efectividad contra los juicios de relevancia (qrels): S@1, S@10 y MRR sobre las consultas con objetivo; nDCG@10 y P@10 sobre todas salvo las negativas; " +
		"falsos es la fracción del top 10 con nota 0 (en las negativas, si devolvió algo). R1 es BM25 (FTS5), R2 BM25 de trigramas, R3 nuestras reglas sin índice. " +
		"El solapamiento con R1 es parecido, no calidad.\n\n")

	for _, base := range []string{"A", "B"} {
		fmt.Fprintf(&b, "## Base %s\n\n", names[base])
		bd := out.Build[base]
		sz := out.Sizes[base]
		fmt.Fprintf(&b, "Índice (es100): .idx %.0f KB · .db %.0f KB · tokens %.1f KB · p.log %.0f KB · %d objetos · build %d ms, %d PUTs, %d KB escritos.\n\n",
			float64(sz.Idx)/1024, float64(sz.DB)/1024, float64(sz.Tokens)/1024, float64(sz.Log)/1024, sz.Files, dbs[base].Millis, bd.Puts, bd.KBWrote)

		b.WriteString("### Efectividad\n\n| Categoría | N | S@1 · S@10 · MRR | nDCG@10 · P@10 · falsos | R1 nDCG | R2 nDCG | R3 nDCG | Solapamiento R1 |\n|---|---|---|---|---|---|---|---|\n")

		order := append(slices.Clone(benchCategories), benchAll)
		for _, cat := range order {
			r, ok := out.Summary[base][cat]
			if !ok {
				continue
			}

			targets := "-"
			if r.Targets > 0 {
				targets = fmt.Sprintf("%s · %s · %s", pct(r.S1), pct(r.S10), pct(r.MRR))
			}

			quality := fmt.Sprintf("%s · %s · %s", pct(r.NDCG), pct(r.P10), pct(r.False))
			if cat == "negativa" {
				quality = fmt.Sprintf("- · - · %s", pct(r.False))
			}

			n := strconv.Itoa(r.N)
			if cat == "nombre+grupo" && base == "A" {
				cat = "nombre+grupo (sin grupo)"
			}

			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s | %s |\n", cat, n, targets, quality, pct(r.R1.NDCG), pct(r.R2.NDCG), pct(r.R3.NDCG), pct(r.Overlap))
		}

		b.WriteString("\n### Contra las referencias (todas las consultas de la base)\n\n| Sistema | S@1 | S@10 | MRR | nDCG@10 | P@10 | falsos |\n|---|---|---|---|---|---|---|\n")

		all := out.Summary[base][benchAll]
		ours := benchSystem{all.S1, all.S10, all.MRR, all.NDCG, all.P10, all.False}

		for _, s := range []struct {
			name string
			v    benchSystem
		}{{"genix-s3", ours}, {"R1 BM25", all.R1}, {"R2 BM25 trigramas", all.R2}, {"R3 nuestras reglas sin índice", all.R3}} {
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n", s.name, pct(s.v.S1), pct(s.v.S10), pct(s.v.MRR), pct(s.v.NDCG), pct(s.v.P10), pct(s.v.False))
		}

		if base == "A" {
			var full []benchMetrics
			for _, r := range out.Queries {
				if r.Base == "A" && r.R3Full != nil {
					full = append(full, *r.R3Full)
				}
			}

			s := systemRow(full)
			fmt.Fprintf(&b, "| R3 con palabras completas (sin el corte de 8 letras) | %s | %s | %s | %s | %s | %s |\n", pct(s.S1), pct(s.S10), pct(s.MRR), pct(s.NDCG), pct(s.P10), pct(s.False))
		}

		b.WriteString("\n### Costo (solo genix-s3)\n\n| Categoría | GET frío p50 · p95 · máx | KB frío p50 · p95 · máx | GET caliente | KB caliente | Rondas frío |\n|---|---|---|---|---|---|\n")

		for _, cat := range order {
			r, ok := out.Summary[base][cat]
			if !ok {
				continue
			}

			fmt.Fprintf(&b, "| %s | %s | %s | %.2f | %.1f | %.2f |\n", cat, r.ColdGet, kbDist(r.ColdKB), r.WarmGet, r.WarmKB, r.Rounds)
		}

		all = out.Summary[base][benchAll]
		fmt.Fprintf(&b, "\nEn frío, promedio por consulta: GET .idx %.2f · .db %.2f · p.log %.2f · otros %.2f; KB .idx %.1f · .db %.1f · p.log %.1f · otros %.1f.\n\n",
			all.IdxGet, all.DBGet, all.LogGet, all.OtherGet, all.IdxKB, all.DBKB, all.LogKB, all.OtherKB)
	}

	b.WriteString(abReport(out))
	b.WriteString(lossReport(out))

	return b.String()
}

func distinctQueries(rs []benchResult) map[string]bool {
	out := map[string]bool{}
	for _, r := range rs {
		out[r.Query] = true
	}

	return out
}

// abReport is what the groups cost and give: the same queries on A and on B.
func abReport(out benchOutput) string {
	var b strings.Builder

	b.WriteString("## A → B: qué cambian los grupos\n\n| Consultas | N | Δ S@1 | Δ MRR | Δ nDCG@10 | Δ falsos | Δ GET frío (p50) | Δ KB frío (p50) | Δ KB frío (prom.) |\n|---|---|---|---|---|---|---|---|---|\n")

	pick := func(base string, keep func(benchResult) bool) benchRow {
		var rs []benchResult

		for _, r := range out.Queries {
			if r.Base == base && keep(r) {
				rs = append(rs, r)
			}
		}

		return summarize(rs)
	}

	names := func(r benchResult) bool { return r.Cat != "nombre+grupo" && r.Cat != "grupo" }
	named := func(r benchResult) bool { return r.Cat == "nombre+grupo" }

	for _, s := range []struct {
		label string
		keep  func(benchResult) bool
	}{{"consultas de nombre (sin grupo)", names}, {"nombre+grupo: sin grupo (A) → con grupo (B)", named}} {
		a, bb := pick("A", s.keep), pick("B", s.keep)
		fmt.Fprintf(&b, "| %s | %d | %+.2f | %+.2f | %+.2f | %+.2f | %+.0f | %+.1f | %+.1f |\n", s.label, a.N,
			bb.S1-a.S1, bb.MRR-a.MRR, bb.NDCG-a.NDCG, bb.False-a.False, bb.ColdGet.P50-a.ColdGet.P50, bb.ColdKB.P50-a.ColdKB.P50, bb.ColdKB.Avg-a.ColdKB.Avg)
	}

	return b.String() + "\n"
}

// lossReport lists the queries where R1 beats us by more than 0.2 of nDCG: the
// cases to look at.
func lossReport(out benchOutput) string {
	var lost []string

	for _, r := range out.Queries {
		if r.Ours.Judged && r.R1.NDCG-r.Ours.NDCG > 0.2 {
			lost = append(lost, fmt.Sprintf("- %s/%s `%s` (%s): nDCG %.2f contra R1 %.2f", r.Base, r.Query, r.Text, r.Cat, r.Ours.NDCG, r.R1.NDCG))
		}
	}

	if len(lost) == 0 {
		return "## Consultas donde perdemos contra R1 por más de 0,2 de nDCG\n\nNinguna.\n"
	}

	return fmt.Sprintf("## Consultas donde perdemos contra R1 por más de 0,2 de nDCG (%d)\n\n%s\n", len(lost), strings.Join(lost, "\n"))
}
