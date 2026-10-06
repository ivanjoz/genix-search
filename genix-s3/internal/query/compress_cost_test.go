package query

import (
	"cmp"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/parallel"
	"github.com/ivanjoz/genix-search/genix-s3/internal/pricing"
)

// TestCompressCost prices, per cold search, what the compression study
// (bucket.TestCompressStudy) found: where the bytes a search reads come from,
// what IDs numbered 1..N would save, what reading their real IDs back would
// cost, and what better codecs would save.
//
//	GXS_COMPRESS=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv go test ./internal/query -run CompressCost -v -timeout 60m
//
// GXS_COMPRESS_SCALE (1,10), GXS_COMPRESS_IDS (random,shuffled,csv,sorted),
// GXS_COMPRESS_GROUPING (none,es100).
func TestCompressCost(t *testing.T) {
	if os.Getenv("GXS_COMPRESS") == "" {
		t.Skip("set GXS_COMPRESS=1 and GXS_EVAL_CSV")
	}

	names := readLines(t, os.Getenv("GXS_EVAL_CSV"))

	var reqs []Request
	for i, q := range sampleQueries(names, 3000) {
		reqs = append(reqs, Request{Query: q, Limit: 10})

		if i%2 == 0 {
			reqs = append(reqs, Request{Query: strings.Fields(q)[0], Limit: 10})
		}
	}

	for _, scale := range sweepInts(t, "GXS_COMPRESS_SCALE", "1,10") {
		for _, gname := range sweepList(t, "GXS_COMPRESS_GROUPING", "none,es100") {
			g, err := layout.GroupingByName(gname)
			if err != nil {
				t.Fatal(err)
			}

			var lines []string

			for _, mode := range sweepList(t, "GXS_COMPRESS_IDS", "random,shuffled,csv,sorted") {
				docs := modeDocs(names, int(scale), mode)
				lines = append(lines, compressCost(t, docs, g, reqs, int(scale), mode)...)
			}

			t.Logf("x%d %s, %d cold searches\n%s", scale, gname, len(reqs), strings.Join(lines, "\n"))
		}
	}
}

// modeDocs are scale copies of the names with IDs given as in
// bucket.TestCompressStudy.
func modeDocs(names []string, scale int, mode string) []collection.Doc {
	r := rand.New(rand.NewPCG(uint64(scale), 99))
	n := len(names) * scale
	docs := make([]collection.Doc, 0, n)

	for range scale {
		for _, name := range names {
			docs = append(docs, collection.Doc{Text: name})
		}
	}

	switch mode {
	case "random":
		used := map[uint32]bool{}

		for i := range docs {
			for {
				id := r.Uint32()
				if id != 0 && !used[id] {
					used[id], docs[i].ID = true, id

					break
				}
			}
		}
	case "shuffled":
		for i, p := range r.Perm(n) {
			docs[i].ID = uint32(p + 1)
		}
	case "sparse10", "sparse5", "del10":
		// The survivors of an autoincrement: n IDs at random in 1..10n (one
		// in ten left) or in 1..n/0.9 (one in ten deleted), in no order of
		// the names.
		for i, id := range survivors(r, n, mode) {
			docs[i].ID = id
		}
	case "csv":
		for i := range docs {
			docs[i].ID = uint32(i + 1)
		}
	case "sorted":
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}

		norm := make([]string, n)
		for i, d := range docs {
			norm[i] = strings.Join(normalize.Tokens(d.Text), " ")
		}

		size := len(names)
		slices.SortStableFunc(order, func(a, b int) int {
			return cmp.Or(cmp.Compare(a/size, b/size), strings.Compare(norm[a], norm[b]))
		})

		for rank, i := range order {
			docs[i].ID = uint32(rank + 1)
		}
	default:
		panic(mode)
	}

	return docs
}

// compactionSplit is compactionCost in two parts: the one-document puts up to
// the compaction threshold, and the compaction alone (the store's counters
// keep the compaction's).
func compactionSplit(t *testing.T, f *fixture, docs []collection.Doc) (puts, compaction float64) {
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

	puts = storeCost(f.store)
	f.store.Reset()

	if _, err := f.w.Compact(ctx); err != nil {
		t.Fatal(err)
	}

	return puts, storeCost(f.store)
}

// survivors are n distinct IDs drawn from 1..10n (sparse10), 1..5n (sparse5)
// or 1..n/0.9 (del10), in random order.
func survivors(r *rand.Rand, n int, mode string) []uint32 {
	universe := 10 * n
	switch mode {
	case "sparse5":
		universe = 5 * n
	case "del10":
		universe = int(math.Ceil(float64(n) / 0.9))
	}

	out := make([]uint32, n)
	for i, p := range r.Perm(universe)[:n] {
		out[i] = uint32(p + 1)
	}

	return out
}

// codecRatio is what the codecs of the study would leave of each part, from
// bucket.TestCompressStudy: lists with Rice gaps and a position width per
// list; fields with gamma counts; keys at their order-1 entropy (a bound).
// With IDs in name order (csv, sorted) only the positions gain: the gaps
// already are at what interpolative coding takes.
func codecRatio(scale int, mode string) (lists, fields, keys float64) {
	keys = 0.778

	switch {
	case mode == "csv" || mode == "sorted":
		return 0.95, 0.95, keys
	case scale == 1 && mode == "random":
		return 0.964, 0.927, keys
	case scale == 1 && mode == "sparse10":
		return 0.94, 0.9, keys
	case mode == "sparse10":
		return 0.886, 0.98, keys
	case scale == 1:
		return 0.937, 0.892, keys
	case scale >= 40 && mode == "random":
		return 0.914, 0.99, keys
	case scale >= 40:
		return 0.861, 0.98, keys
	case mode == "random":
		return 0.930, 0.982, keys
	default:
		return 0.877, 0.978, keys
	}
}

// readLog is what a search read, by part.
type readLog struct {
	mu                        sync.Mutex
	gets                      map[string]int
	header, keys, fields, exp int64
	inline, db, log, heads    int64
}

type recorder struct {
	blob.Blob
	r *readLog
}

func kind(key string) string {
	switch {
	case strings.HasSuffix(key, ".idx"):
		return "idx"
	case strings.HasSuffix(key, ".db"):
		return "db"
	case strings.HasSuffix(key, "p.log"):
		return "log"
	}

	return "other"
}

func (c recorder) note(key string, data []byte) {
	c.r.mu.Lock()
	defer c.r.mu.Unlock()

	k := kind(key)
	c.r.gets[k]++

	switch k {
	case "idx":
		if len(data) < bucket.IdxHeader {
			c.r.header += int64(len(data))

			return
		}

		etag := int64(binary.LittleEndian.Uint16(data[6:]))
		c.r.header += bucket.IdxHeader + etag
		c.r.keys += int64(binary.LittleEndian.Uint32(data[40:]))
		c.r.fields += int64(binary.LittleEndian.Uint32(data[44:]))
		c.r.heads += int64(binary.LittleEndian.Uint32(data[48:]))
		c.r.exp += int64(binary.LittleEndian.Uint32(data[52:]))
		c.r.inline += int64(binary.LittleEndian.Uint32(data[56:]))
	case "db":
		c.r.db += int64(len(data))
	default:
		c.r.log += int64(len(data))
	}
}

func (c recorder) GetRange(ctx context.Context, key string, off, n int64) ([]byte, string, error) {
	data, etag, err := c.Blob.GetRange(ctx, key, off, n)
	c.note(key, data)

	return data, etag, err
}

func (c recorder) Get(ctx context.Context, key string) ([]byte, string, error) {
	data, etag, err := c.Blob.Get(ctx, key)
	c.note(key, data)

	return data, etag, err
}

// compressCost builds the index of docs twice (as is, and with InlineSlot
// stretched as if the lists were smaller) and runs every request cold on
// both.
func compressCost(t *testing.T, docs []collection.Doc, g *layout.Grouping, reqs []Request, scale int, mode string) []string {
	p := pricing.S3Express
	rl, rf, rk := codecRatio(scale, mode)
	n := len(docs)

	var compaction string

	run := func(inlineSlot int) ([]*readLog, []Response) {
		opts := testOptions()
		opts.Params = bucket.DefaultParams()
		opts.Params.InlineSlot = inlineSlot

		f := newFixtureGrouped(t, normalize.Default, docs, opts, g)
		logs := make([]*readLog, len(reqs))
		resps := make([]Response, len(reqs))

		err := parallel.Run(context.Background(), len(reqs), runtime.GOMAXPROCS(0), func(ctx context.Context, i int) error {
			logs[i] = &readLog{gets: map[string]int{}}
			e := New(recorder{f.store, logs[i]}, f.col, f.norm, DefaultOptions())

			resp, err := e.Search(ctx, reqs[i])
			resps[i] = resp

			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		if compaction == "" {
			// What a compaction costs, and the part of it that is the tokens
			// object (read once, written once).
			tok, _, err := f.store.Get(context.Background(), f.col.Tokens())
			if err != nil {
				t.Fatal(err)
			}

			puts, cost := compactionSplit(t, f, docs)
			tokCost := float64(len(tok)) * (p.Retrieval + p.Upload)
			compaction = fmt.Sprintf("%-9s puts until the compaction $%.7f; compaction $%.7f (%d writes, %.0f KB written), of which tokens (%.0f KB) $%.7f = %.0f%%",
				mode, puts, cost, f.store.Writes.Load(), float64(f.store.BytesWritten.Load())/1024, float64(len(tok))/1024, tokCost, 100*tokCost/cost)
		}

		return logs, resps
	}

	now, resps := run(int(p.BytesPerGET()))
	sim, simResps := run(int(float64(p.BytesPerGET()) / rl))

	for i := range resps {
		if !slices.Equal(resps[i].Hits, simResps[i].Hits) {
			t.Errorf("%s: ranking changed with InlineSlot stretched: %q", mode, reqs[i].Query)
		}
	}

	type sum struct{ gets, getsIdx, getsDB, getsLog, header, keys, fields, exp, inline, db, log float64 }

	avg := func(logs []*readLog, codec bool) sum {
		var s sum

		for _, l := range logs {
			s.getsIdx += float64(l.gets["idx"])
			s.getsDB += float64(l.gets["db"])
			s.getsLog += float64(l.gets["log"])
			s.header += float64(l.header)
			s.exp += float64(l.exp + l.heads)
			s.log += float64(l.log)

			if codec {
				s.keys += rk * float64(l.keys)
				s.fields += rf * float64(l.fields)
				s.inline += rl * float64(l.inline)
				s.db += rl * float64(l.db)
			} else {
				s.keys += float64(l.keys)
				s.fields += float64(l.fields)
				s.inline += float64(l.inline)
				s.db += float64(l.db)
			}
		}

		k := float64(len(logs))
		s = sum{0, s.getsIdx / k, s.getsDB / k, s.getsLog / k, s.header / k, s.keys / k, s.fields / k, s.exp / k, s.inline / k, s.db / k, s.log / k}
		s.gets = s.getsIdx + s.getsDB + s.getsLog

		return s
	}

	bytesOf := func(s sum) float64 { return s.header + s.keys + s.fields + s.exp + s.inline + s.db + s.log }
	dollars := func(gets, bytes float64) float64 { return 1e6 * (gets*p.GET + bytes*p.Retrieval) }

	line := func(label string, s sum, extraGets, extraBytes float64) string {
		b := bytesOf(s) + extraBytes
		return fmt.Sprintf("%-9s %-28s | %8.4f | GET %5.2f (idx %4.2f db %4.2f log %4.2f +%4.2f) | KB %6.1f: hdr %4.1f keys %5.1f fields %5.1f exp %4.1f inline %6.1f db %6.1f log %4.1f +%5.1f",
			mode, label, dollars(s.gets+extraGets, b), s.gets+extraGets, s.getsIdx, s.getsDB, s.getsLog, extraGets,
			b/1024, s.header/1024, s.keys/1024, s.fields/1024, s.exp/1024, s.inline/1024, s.db/1024, s.log/1024, extraBytes/1024)
	}

	out := []string{
		line("now", avg(now, false), 0, 0),
		line("codecs (rice, pos, keys)", avg(sim, true), 0, 0),
		compaction,
	}

	if mode != "random" {
		// The IDs are ordinals: the real IDs of the hits come from a table
		// in an object of its own, in one more round.
		for _, tab := range []struct {
			name  string
			entry int64 // bytes per entry, entries per chunk
			chunk int64
		}{
			{"+ID table u32", 4, 1},
			{"+ID table EF/64", int64(math.Ceil(64*(math.Floor(math.Log2(float64(uint64(1)<<32)/float64(n)))+2)/8)) + 8, 64},
		} {
			var gets, bytes float64

			for _, resp := range resps {
				g, b := tableCost(resp.Hits, tab.entry, tab.chunk, p.BytesPerGET())
				gets += g
				bytes += b
			}

			k := float64(len(resps))
			out = append(out, line(tab.name, avg(now, false), gets/k, bytes/k))
			out = append(out, line("codecs"+tab.name, avg(sim, true), gets/k, bytes/k))
		}
	}

	return out
}

// tableCost is the requests and bytes to read the entries of the hits from a
// table of fixed chunks (entry bytes per chunk of chunk IDs), merging ranges
// closer than gap.
func tableCost(hits []Hit, entry, chunk, gap int64) (float64, float64) {
	if len(hits) == 0 {
		return 0, 0
	}

	var offs []int64
	for _, h := range hits {
		offs = append(offs, (int64(h.ID)-1)/chunk*entry)
	}

	slices.Sort(offs)
	offs = slices.Compact(offs)

	gets, bytes := 1.0, float64(entry)
	for i := 1; i < len(offs); i++ {
		if hole := offs[i] - offs[i-1] - entry; hole < gap {
			bytes += float64(hole + entry)
		} else {
			gets++
			bytes += float64(entry)
		}
	}

	return gets, bytes
}
