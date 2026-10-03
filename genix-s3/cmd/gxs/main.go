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
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/index"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/manifest"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/query"
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
  compact                         fold p.log into a new generation
  log                             print p.log
  inspect [WORD]                  print the manifest, or the slot of WORD

common flags: -root DIR (default data/gxs) -collection NAME -bucket NAME -extra CHARS
              -min-index-docs N (default 100): with fewer documents there is no
              index, only p.log; build, put, del and compact apply it`

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
	bucket := flags.String("bucket", "default", "bucket")
	extra := flags.String("extra", "", "up to two extra letters (codes 30 and 31), e.g. ñ")
	csv := flags.String("csv", "", "catalogue file")
	limit := flags.Int("limit", 10, "results")
	fuzzy := flags.Bool("fuzzy", false, "tolerate typos")
	minIndex := flags.Int("min-index-docs", plog.DefaultMinIndexDocs, "documents below which there is no index, only p.log")

	if err := flags.Parse(args); err != nil {
		fail(err)
	}

	ctx := context.Background()

	e, err := open(*root, *name, *bucket, *extra)
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

func open(root, name, bucket, extra string) (env, error) {
	store, err := blob.NewFS(root)
	if err != nil {
		return env{}, err
	}

	col, err := layout.New("genix/", name, bucket)
	if err != nil {
		return env{}, err
	}

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

	m, err := e.writer().Init(ctx, docs)
	if err != nil {
		return err
	}

	if m == nil {
		fmt.Printf("%d docs, under -min-index-docs %d: no index, only p.log · %v\n", len(docs), e.minIndex, time.Since(start).Round(time.Millisecond))

		return nil
	}

	fmt.Printf("generation %d: %d docs, %d keys · k %s · p %s · snapshot %s · %v\n",
		m.Gen, m.Docs, m.Keys, kb(m.KSize), kb(m.PSize), kb(m.SnapSize), time.Since(start).Round(time.Millisecond))

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

	fmt.Printf("generation %d · %d rounds · %d requests · %s read (p.log %s) · %d candidates · %d pending · approx %v · %v\n",
		resp.Gen, s.Rounds, s.Requests, kb(uint64(s.Bytes)), kb(uint64(s.LogBytes)), s.Candidates, s.LogEntries, s.Approx, s.Elapsed.Round(time.Microsecond))

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

	if res.Ops == 0 {
		fmt.Println("nothing to compact")

		return nil
	}

	if res.Gen == 0 {
		fmt.Printf("no index (%d docs, under -min-index-docs %d): %d ops folded into p.log · %d reads, %d writes, %s written · %v\n",
			res.Docs, e.minIndex, res.Ops, res.Requests, res.Writes, kb(uint64(res.Bytes)), time.Since(start).Round(time.Millisecond))

		return nil
	}

	fmt.Printf("generation %d: %d ops folded, %d docs · %d reads, %d writes, %s written · %v\n",
		res.Gen, res.Ops, res.Docs, res.Requests, res.Writes, kb(uint64(res.Bytes)), time.Since(start).Round(time.Millisecond))

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
		fmt.Printf("base: no index, %d docs (last generation %d), last seq %d · %s (%s since the base) · torn tail %v\n",
			len(log.Docs), log.Base.ManifestGen, log.Base.LastSeq, kb(uint64(log.Size)), kb(uint64(log.Size-log.BaseEnd)), log.TornTail)

		for _, doc := range log.Docs {
			words, err := keys(doc.Tokens)
			if err != nil {
				return err
			}

			fmt.Printf("  doc %d  %s\n", doc.ID, words)
		}
	} else {
		fmt.Printf("base: generation %d, last seq %d · %s · torn tail %v\n", log.Base.ManifestGen, log.Base.LastSeq, kb(uint64(log.Size)), log.TornTail)
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

	gen := log.Base.ManifestGen

	mdata, _, err := e.store.Get(ctx, e.col.Manifest(gen))
	if err != nil {
		return err
	}

	m, err := manifest.Decode(mdata)
	if err != nil {
		return err
	}

	k, _, err := e.store.Get(ctx, e.col.K(gen))
	if err != nil {
		return err
	}

	h, err := index.ParseHead(k, e.norm.ID())
	if err != nil {
		return err
	}

	if len(args) == 0 {
		used := 0
		for s := range uint16(key5.Slots) {
			if off, end := h.Block(s); end > off {
				used++
			}
		}

		fmt.Printf("generation %d · last seq %d · %d docs · %d keys in %d slots · k %s · p %s · snapshot %s · NormID %08x (normalizer %d, extra %q %q)\n",
			m.Gen, m.LastSeq, m.Docs, m.Keys, used, kb(m.KSize), kb(m.PSize), kb(m.SnapSize), m.NormID, m.NormalizerVersion, rune(m.Extra1), rune(m.Extra2))

		return nil
	}

	word := key5.FromToken(strings.Join(e.norm.Tokens(strings.Join(args, " ")), ""))
	s := word.Key.Slot()
	off, end := h.Block(s)

	slot, err := index.ParseSlot(h, s, k[off:end])
	if err != nil {
		return err
	}

	fmt.Printf("slot %d (%s): %d keys, block %d B, lists [%d, %d) of p\n", s, word.Key.String()[:min(2, len(word.Key.String()))], len(slot.Entries), end-off, h.POff[s], h.POff[s+1])

	for _, entry := range slot.Entries {
		mark := " "
		if entry.Key == word.Key {
			mark = "*"
		}

		from, to := entry.Bytes()
		fmt.Printf("%s %-10s count %5d  codec %d  x %2d  tail %-5v  first %10d  p [%d, %d)\n",
			mark, entry.Key, entry.Count, entry.Codec, entry.X, entry.HasTail, entry.First, from, to)
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
