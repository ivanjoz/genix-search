// Command gxs drives a collection stored on local disk with the same code
// that runs against S3: build it from a file, query it, write to its log,
// and look inside it.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/query"
	"github.com/ivanjoz/genix-search/genix-s3/internal/tokens"
)

const usage = `usage: gxs <command> [flags] [args]

commands:
  build   -csv FILE               create the collection from FILE: one name per
                                  line (ID = line number), or "id<TAB>text" lines
  query   [-limit N] [-fuzzy] [-csv FILE] TERMS...
                                  print the ranked IDs; -csv looks the names up in
                                  FILE, the way the application uses its database
  put     ID TEXT...              write one document through p.log
  del     ID                      delete one document through p.log
  compact                         fold p.log into the buckets of the index
  log                             print p.log
  inspect [WORD]                  print the buckets, or the bucket of WORD

common flags: -root DIR (default data/gxs) -collection NAME -partition NAME -extra CHARS
              -min-index-docs N (default 100): with fewer documents there is no
              index, only p.log; build, put, del and compact apply it
              -grouping es100|none (default es100): how build and compact group
              the buckets into objects; another than the index's rewrites
              every bucket. query, log and inspect follow the index`

type env struct {
	store    blob.Blob
	col      layout.Collection
	norm     *normalize.Normalizer
	minIndex int
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	command, args := os.Args[1], os.Args[2:]
	flags := flag.NewFlagSet(command, flag.ExitOnError)
	root := flags.String("root", "data/gxs", "directory that stands in for the bucket")
	name := flags.String("collection", "productos", "collection")
	partition := flags.String("partition", "default", "partition of the collection")
	extra := flags.String("extra", "", "up to two extra letters (codes 30 and 31), e.g. ñ")
	csv := flags.String("csv", "", "catalogue file")
	limit := flags.Int("limit", 10, "results")
	fuzzy := flags.Bool("fuzzy", false, "tolerate typos")
	minIndex := flags.Int("min-index-docs", plog.DefaultMinIndexDocs, "documents below which there is no index, only p.log")
	grouping := flags.String("grouping", "es100", "how the writers group the buckets: es100 or none")

	if err := flags.Parse(args); err != nil {
		fail(err)
	}

	ctx := context.Background()

	e, err := open(*root, *name, *partition, *extra, *grouping)
	if err != nil {
		fail(err)
	}

	e.minIndex = *minIndex

	switch command {
	case "build":
		err = cmdBuild(ctx, e, *csv)
	case "query":
		err = cmdQuery(ctx, e, *csv, *limit, *fuzzy, flags.Args())
	case "put":
		err = cmdPut(ctx, e, flags.Args())
	case "del":
		err = cmdDel(ctx, e, flags.Args())
	case "compact":
		err = cmdCompact(ctx, e)
	case "log":
		err = cmdLog(ctx, e)
	case "inspect":
		err = cmdInspect(ctx, e, flags.Args())
	default:
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	if err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "gxs:", err)
	os.Exit(1)
}

func open(root, name, partition, extra, grouping string) (env, error) {
	store, err := blob.NewFS(root)
	if err != nil {
		return env{}, err
	}

	g, err := layout.GroupingByName(grouping)
	if err != nil {
		return env{}, err
	}

	col, err := layout.New("genix/", name, partition)
	if err != nil {
		return env{}, err
	}

	col = col.With(g)

	var letters [2]rune
	for i, r := range []rune(extra) {
		if i >= 2 {
			return env{}, fmt.Errorf("at most 2 extra letters, got %q", extra)
		}

		letters[i] = r
	}

	norm, err := normalize.New(letters)

	return env{store: store, col: col, norm: norm}, err
}

func (e env) writer() *collection.Collection {
	opts := collection.DefaultOptions()
	opts.MinIndexDocs = e.minIndex

	return collection.New(e.store, e.col, e.norm, opts)
}

func readDocs(path string) ([]collection.Doc, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var docs []collection.Doc

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)

	for line := 1; scanner.Scan(); line++ {
		text := scanner.Text()
		id := uint32(line)

		if before, after, ok := strings.Cut(text, "\t"); ok {
			n, err := strconv.ParseUint(before, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("%s:%d: id %q", path, line, before)
			}

			id, text = uint32(n), after
		}

		docs = append(docs, collection.Doc{ID: id, Text: text})
	}

	return docs, scanner.Err()
}

func cmdBuild(ctx context.Context, e env, path string) error {
	if path == "" {
		return fmt.Errorf("build needs -csv FILE")
	}

	docs, err := readDocs(path)
	if err != nil {
		return err
	}

	start := time.Now()

	res, err := e.writer().Init(ctx, docs)
	if err != nil {
		return err
	}

	if !res.Indexed {
		fmt.Printf("%d docs, under -min-index-docs %d: no index, only p.log · %v\n", len(docs), e.minIndex, time.Since(start).Round(time.Millisecond))

		return nil
	}

	fmt.Printf("epoch %d: %d docs in %d buckets · %d writes, %s written · %v\n",
		res.Epoch, res.Docs, res.Buckets, res.Writes, kb(uint64(res.Bytes)), time.Since(start).Round(time.Millisecond))

	return nil
}

func cmdQuery(ctx context.Context, e env, csv string, limit int, fuzzy bool, terms []string) error {
	names := map[uint32]string{}

	if csv != "" {
		docs, err := readDocs(csv)
		if err != nil {
			return err
		}

		for _, d := range docs {
			names[d.ID] = d.Text
		}
	}

	engine := query.New(e.store, e.col, e.norm, query.DefaultOptions())

	resp, err := engine.Search(ctx, query.Request{Query: strings.Join(terms, " "), Limit: limit, Fuzzy: fuzzy})
	if err != nil {
		return err
	}

	for i, h := range resp.Hits {
		fmt.Printf("%2d. %10d  cov %d  score %3d  %s\n", i+1, h.ID, h.Coverage, h.Score, names[h.ID])
	}

	s := resp.Stats
	if s.LogOnly {
		fmt.Print("no index, p.log only · ")
	}

	fmt.Printf("seq %d · %d rounds · %d requests · %s read (p.log %s, .idx %s) · %d candidates · %d pending · approx %v · %v\n",
		resp.Seq, s.Rounds, s.Requests, kb(uint64(s.Bytes)), kb(uint64(s.LogBytes)), kb(uint64(s.IdxBytes)), s.Candidates, s.LogEntries, s.Approx, s.Elapsed.Round(time.Microsecond))

	return nil
}

func cmdPut(ctx context.Context, e env, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("put ID TEXT...")
	}

	id, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return err
	}

	res, err := e.writer().Put(ctx, []collection.Doc{{ID: uint32(id), Text: strings.Join(args[1:], " ")}})

	return report(res, err)
}

func cmdDel(ctx context.Context, e env, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("del ID")
	}

	id, err := strconv.ParseUint(args[0], 10, 32)
	if err != nil {
		return err
	}

	res, err := e.writer().Delete(ctx, uint32(id))

	return report(res, err)
}

func report(res plog.AppendResult, err error) error {
	if err != nil {
		return err
	}

	fmt.Printf("seq %d · p.log %s · compaction requested %v\n", res.LastSeq, kb(uint64(res.Size)), res.CompactRequested)

	return nil
}

func cmdCompact(ctx context.Context, e env) error {
	start := time.Now()

	res, err := e.writer().Compact(ctx)
	if err != nil {
		return err
	}

	// Any compaction rewrites p.log; seq 0 is also that of a collection never
	// written to since Init.
	if res.Bytes == 0 {
		fmt.Println("nothing to compact")

		return nil
	}

	if !res.Indexed {
		fmt.Printf("no index (%d docs, under -min-index-docs %d): %d ops folded into p.log · %d reads, %d writes, %s written · %v\n",
			res.Docs, e.minIndex, res.Ops, res.Requests, res.Writes, kb(uint64(res.Bytes)), time.Since(start).Round(time.Millisecond))

		return nil
	}

	fmt.Printf("seq %d, epoch %d (full %v): %d ops folded, %d docs · %d buckets rewritten · %d reads, %d writes, %s written · %v\n",
		res.Seq, res.Epoch, res.Full, res.Ops, res.Docs, res.Buckets, res.Requests, res.Writes, kb(uint64(res.Bytes)), time.Since(start).Round(time.Millisecond))

	return nil
}

func cmdLog(ctx context.Context, e env) error {
	data, _, err := e.store.Get(ctx, e.col.Log())
	if err != nil {
		return err
	}

	log, err := plog.Parse(data, e.norm.ID())
	if err != nil {
		return err
	}

	if log.Base.LogOnly {
		fmt.Printf("base: no index, %d docs (epoch %d), last seq %d · %s (%s since the base) · torn tail %v\n",
			len(log.Docs), log.Base.Epoch, log.Base.LastSeq, kb(uint64(log.Size)), kb(uint64(log.Size-log.BaseEnd)), log.TornTail)

		for _, doc := range log.Docs {
			words, err := keys(doc.Tokens)
			if err != nil {
				return err
			}

			fmt.Printf("  doc %d  %s\n", doc.ID, words)
		}
	} else {
		touched := 0
		for s := range plog.Slots {
			if log.Base.Touched.Has(s) {
				touched++
			}
		}

		grouping := fmt.Sprintf("%d", log.Base.Grouping)
		if g, err := layout.GroupingByID(log.Base.Grouping); err == nil {
			grouping = g.String()
		}

		fmt.Printf("base: epoch %d, last seq %d (previous %d), %d docs, grouping %s, %d buckets rewritten by its compaction · %s · torn tail %v\n",
			log.Base.Epoch, log.Base.LastSeq, log.Base.PrevSeq, log.Base.Docs, grouping, touched, kb(uint64(log.Size)), log.TornTail)
	}

	for _, op := range log.Ops {
		if op.Del {
			fmt.Printf("%6d  del %d\n", op.Seq, op.ID)

			continue
		}

		words, err := keys(op.Tokens)
		if err != nil {
			return err
		}

		fmt.Printf("%6d  put %d  %s\n", op.Seq, op.ID, words)
	}

	for _, c := range log.Compacts {
		fmt.Printf("compaction requested up to seq %d\n", c.TargetSeq)
	}

	return nil
}

func cmdInspect(ctx context.Context, e env, args []string) error {
	data, _, err := e.store.Get(ctx, e.col.Log())
	if err != nil {
		return err
	}

	log, err := plog.Parse(data, e.norm.ID())
	if err != nil {
		return err
	}

	if log.Base.LogOnly {
		fmt.Printf("no index: %d docs in p.log (%s), %d pending ops · NormID %08x\n",
			log.LiveDocs(), kb(uint64(log.Size)), len(log.Pending()), log.Base.NormID)

		return nil
	}

	// The index's grouping, whatever -grouping says.
	g, err := layout.GroupingByID(log.Base.Grouping)
	if err != nil {
		return err
	}

	col := e.col.With(g)

	if len(args) == 0 {
		tdata, _, err := e.store.Get(ctx, col.Tokens())
		if err != nil {
			return err
		}

		tok, err := tokens.Decode(tdata, e.norm.ID())
		if err != nil {
			return err
		}

		var idxBytes, dbBytes, dbs int

		for group := range tok.Buckets {
			x, _, err := e.store.Get(ctx, col.Idx(group))
			if err != nil {
				return err
			}

			idxBytes += len(x)

			if d, _, err := e.store.Get(ctx, col.DB(group)); err == nil {
				dbBytes += len(d)
				dbs++
			}
		}

		fmt.Printf("epoch %d · last seq %d · %d docs · grouping %s · %d buckets (.idx %s), %d with a .db (%s) · %d expanded words · tokens %s · NormID %08x\n",
			log.Base.Epoch, log.Base.LastSeq, log.Base.Docs, g, len(tok.Buckets), kb(uint64(idxBytes)), dbs, kb(uint64(dbBytes)),
			len(tok.Expanded), kb(uint64(len(tdata))), log.Base.NormID)

		return nil
	}

	word := key5.FromToken(strings.Join(e.norm.Tokens(strings.Join(args, " ")), ""))
	group := g.Of(word.Key.Slot())

	data, _, err = e.store.Get(ctx, col.Idx(group))
	if err != nil {
		return err
	}

	x, err := bucket.Decode(data, e.norm.ID())
	if err != nil {
		return err
	}

	fmt.Printf("%s.idx: %d keys, %s · epoch %d, seq %d · inline %s · .db %s, combinations from %d\n",
		g.Name(group), len(x.Words), kb(uint64(len(data))), x.Epoch, x.Seq, kb(uint64(len(x.Inline))), kb(uint64(x.DBSize)), x.DBCombos)

	locs := []string{"field", "inline", ".db", "expanded"}

	for _, w := range x.Words {
		mark := " "
		if w.Key == word.Key {
			mark = "*"
		}

		fmt.Printf("%s %-10s count %5d  df %5d  %-8s %6d B", mark, w.Key, w.Shape.Count, w.DF, locs[w.Loc], (w.Shape.Bits()+7)/8)

		if w.Exp != nil {
			var blocks uint32
			for _, c := range w.Exp.Combos {
				blocks += c.Len
			}

			fmt.Printf("  · %d heads (%d entries of %d docs) · %d combination blocks by %d letters, %s",
				len(w.Exp.Groups), len(w.Exp.Groups[0].Entries), w.Exp.Groups[0].Total, len(w.Exp.Combos), w.Exp.P, kb(uint64(blocks)))
		}

		fmt.Println()
	}

	return nil
}

// keys prints a token stream as its keys.
func keys(stream []byte) (string, error) {
	tokens, err := key5.ReadTokens(stream)
	if err != nil {
		return "", err
	}

	words := make([]string, len(tokens))
	for i, t := range tokens {
		words[i] = t.Key.String()
	}

	return strings.Join(words, " "), nil
}

func kb(n uint64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}

	if n < 1<<20 {
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	}

	return fmt.Sprintf("%.2f MB", float64(n)/(1<<20))
}
