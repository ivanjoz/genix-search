package bucket

import (
	"bufio"
	"cmp"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/postings"
)

// TestCompressStudy measures where the bits of the index go and what other
// codecs and other orders of the document IDs would take, without changing
// the format. It is a measurement, not a check.
//
//	GXS_COMPRESS=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv go test ./internal/bucket -run CompressStudy -v
//
// GXS_COMPRESS_SCALE (1,10) are copies of the catalogue, GXS_COMPRESS_IDS
// (random,shuffled,csv,sorted) how IDs are given: random uint32; dense in a
// random order; dense in the order of the CSV; dense in the order of the
// normalized names. GXS_COMPRESS_OUT writes the objects there, for external
// compressors.
func TestCompressStudy(t *testing.T) {
	if os.Getenv("GXS_COMPRESS") == "" {
		t.Skip("set GXS_COMPRESS=1 and GXS_EVAL_CSV")
	}

	names := studyLines(t, os.Getenv("GXS_EVAL_CSV"))

	for _, scale := range studyList("GXS_COMPRESS_SCALE", "1,10") {
		n, _ := strconv.Atoi(scale)

		for _, mode := range studyList("GXS_COMPRESS_IDS", "random,shuffled,csv,sorted") {
			t.Run(fmt.Sprintf("x%d/%s", n, mode), func(t *testing.T) {
				docs := studyDocs(names, n, mode)
				out := ""

				if dir := os.Getenv("GXS_COMPRESS_OUT"); dir != "" {
					out = filepath.Join(dir, fmt.Sprintf("x%d-%s", n, mode))
				}

				studyRun(t, docs, out)
			})
		}
	}
}

func studyList(env, def string) []string {
	v := os.Getenv(env)
	if v == "" {
		v = def
	}

	return strings.Split(v, ",")
}

func studyLines(t *testing.T, path string) []string {
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	var out []string

	s := bufio.NewScanner(f)
	for s.Scan() {
		if line := strings.Trim(strings.TrimSpace(s.Text()), `"`); line != "" {
			out = append(out, line)
		}
	}

	return out
}

// studyDocs are scale copies of the names, with IDs by mode, sorted by key.
func studyDocs(names []string, scale int, mode string) []Doc {
	r := rand.New(rand.NewPCG(uint64(scale), 99))
	n := len(names) * scale

	type item struct {
		text string
		toks []key5.Token
		copy int
	}

	items := make([]item, 0, n)

	for c := range scale {
		for _, name := range names {
			items = append(items, item{text: name, toks: key5.FromTokens(normalize.Default.Tokens(name)), copy: c})
		}
	}

	ids := make([]uint32, n)

	switch mode {
	case "random":
		used := map[uint32]bool{}

		for i := range ids {
			for {
				id := r.Uint32()
				if !used[id] {
					used[id], ids[i] = true, id

					break
				}
			}
		}
	case "shuffled":
		for i, p := range r.Perm(n) {
			ids[i] = uint32(p + 1)
		}
	case "sparse10", "sparse5", "del10":
		universe := 10 * n
		switch mode {
		case "sparse5":
			universe = 5 * n
		case "del10":
			universe = int(math.Ceil(float64(n) / 0.9))
		}

		for i, p := range r.Perm(universe)[:n] {
			ids[i] = uint32(p + 1)
		}
	case "csv":
		for i := range ids {
			ids[i] = uint32(i + 1)
		}
	case "sorted":
		// Within each copy, by the normalized name: what the engine could do
		// if it gave the documents numbers of its own.
		order := make([]int, n)
		for i := range order {
			order[i] = i
		}

		slices.SortStableFunc(order, func(a, b int) int {
			return cmp.Or(cmp.Compare(items[a].copy, items[b].copy),
				strings.Compare(strings.Join(normalize.Default.Tokens(items[a].text), " "), strings.Join(normalize.Default.Tokens(items[b].text), " ")))
		})

		for rank, i := range order {
			ids[i] = uint32(rank + 1)
		}
	default:
		panic(mode)
	}

	docs := make([]Doc, n)
	for i, it := range items {
		docs[i] = Doc{ID: docid.New(0, ids[i]), Tokens: it.toks}
	}

	slices.SortFunc(docs, func(a, b Doc) int { return cmp.Compare(a.ID, b.ID) })

	return docs
}

// tally is bits by component.
type tally struct {
	names []string
	bits  map[string]float64
}

func (t *tally) add(name string, b float64) {
	if t.bits == nil {
		t.bits = map[string]float64{}
	}

	if _, ok := t.bits[name]; !ok {
		t.names = append(t.names, name)
	}

	t.bits[name] += b
}

func (t *tally) kb(name string) float64 { return t.bits[name] / 8 / 1024 }

func studyRun(t *testing.T, docs []Doc, out string) {
	p := DefaultParams()
	g := layout.GroupingNone

	res, err := Build(docs, nil, p, g)
	if err != nil {
		t.Fatal(err)
	}

	var rep strings.Builder
	fmt.Fprintf(&rep, "%d docs, %d buckets\n", len(docs), len(res.Buckets))

	// 1. Sizes of the objects, by section.
	var obj tally
	dbs := 0

	for _, b := range res.Buckets {
		obj.add("idx header", 8*IdxHeader)
		obj.add("idx keys", float64(8*len(b.sections[0])))
		obj.add("idx fields", float64(8*len(b.sections[1])))
		obj.add("idx exp (heads+dirs)", float64(8*len(b.sections[3])))
		obj.add("idx inline lists", float64(8*len(b.sections[4])))

		if b.HasDB() {
			dbs++
			words := int(b.dbCombos) - DBHeader
			obj.add("db header", 8*DBHeader)
			obj.add("db words zone", float64(8*words))
			obj.add("db combos zone", float64(8*(len(b.db)-words)))
		}
	}

	idxTotal, dbTotal := 0.0, 0.0
	for _, n := range obj.names {
		if strings.HasPrefix(n, "idx") {
			idxTotal += obj.kb(n)
		} else {
			dbTotal += obj.kb(n)
		}
	}

	fmt.Fprintf(&rep, "\n== objects: .idx %.1f KB, .db %.1f KB (%d with .db)\n", idxTotal, dbTotal, dbs)
	for _, n := range obj.names {
		fmt.Fprintf(&rep, "  %-22s %9.1f KB\n", n, obj.kb(n))
	}

	// 2. The words, as Build makes them.
	words := map[key5.Key]*word{}
	for _, d := range CapGroupWords(docs, p.GroupWordDocs) {
		for pos, tk := range d.Tokens {
			w := words[tk.Key]
			if w == nil {
				w = &word{key: tk.Key}
				words[tk.Key] = w
			}

			w.list = append(w.list, postings.Posting{ID: d.ID, Pos: uint8(min(pos, postings.MaxPos)), Tail: tk.Tail})
		}
	}

	keys := make([]key5.Key, 0, len(words))
	slotBits := map[uint16]uint64{}
	expanded := map[key5.Key]bool{}

	for _, k := range res.Expanded {
		expanded[k] = true
	}

	for k, w := range words {
		keys = append(keys, k)

		if w.shape, err = postings.ShapeOf(w.list); err != nil {
			t.Fatal(err)
		}

		w.df = uint32(len(docIDs(w.list)))
		w.expand = expanded[k]

		if w.shape.Count > 1 && !w.expand {
			slotBits[k.Slot()] += w.shape.Bits()
		}
	}

	slices.Sort(keys)

	for _, w := range words {
		switch {
		case w.shape.Count == 1:
			w.loc = LocField
		case w.expand:
			w.loc = LocExpanded
		case (slotBits[w.key.Slot()]+7)/8 <= uint64(p.InlineSlot):
			w.loc = LocInline
		default:
			w.loc = LocDB
		}
	}

	// 3. Lists: gaps, positions, tails, by where they live; and other codecs.
	type listTally struct {
		lists, postings, docs                    int
		gaps, pos, tails                         float64
		rice, ef, bic, bound                     float64
		posWidth, posGamma, posH0, tailH0, dupBi float64
	}

	posFreq := map[uint8]float64{}
	tailFreq := map[uint8]float64{}
	totalPostings := 0.0

	for _, w := range words {
		if w.shape.Count > 1 {
			for _, ps := range w.list {
				posFreq[ps.Pos]++
				totalPostings++

				if w.shape.HasTail {
					tailFreq[ps.Tail]++
				}
			}
		}
	}

	posH := entropy(posFreq)
	tailH := entropy(tailFreq)

	byLoc := map[Loc]*listTally{LocInline: {}, LocDB: {}, LocExpanded: {}}

	for _, w := range words {
		if w.shape.Count <= 1 {
			continue
		}

		lt := byLoc[w.loc]
		n := float64(w.shape.Count)
		lt.lists++
		lt.postings += int(w.shape.Count)
		lt.docs += int(w.df)

		gapBits := float64(w.shape.Bits()) - postings.PosBits*n
		if w.shape.HasTail {
			gapBits -= postings.TailBits * n
			lt.tails += postings.TailBits * n
			lt.tailH0 += tailH * n
		}

		lt.gaps += gapBits
		lt.pos += postings.PosBits * n

		ids := make([]uint64, len(w.list))
		maxPos := uint8(0)

		for i, ps := range w.list {
			ids[i] = uint64(ps.ID)
			maxPos = max(maxPos, ps.Pos)
			lt.posGamma += float64(2*bits.Len(uint64(ps.Pos)+1) - 1)
		}

		lt.rice += riceBits(ids)
		lt.ef += efBits(ids)
		lt.bic += bicBits(ids)
		lt.bound += boundBits(ids)
		lt.posWidth += 2 + float64(bits.Len(uint64(maxPos)))*n
		lt.posH0 += posH * n
	}

	fmt.Fprintf(&rep, "\n== lists (KB): where | lists postings docs | gaps pos tails = now | gaps: rice ef bic bound(C(u,n)) | pos: width/list gamma H0 | tails H0\n")

	for _, loc := range []Loc{LocInline, LocDB, LocExpanded} {
		lt := byLoc[loc]
		k := func(b float64) float64 { return b / 8 / 1024 }
		fmt.Fprintf(&rep, "  %-8s | %6d %8d %8d | %8.1f %7.1f %6.1f = %8.1f | %8.1f %8.1f %8.1f %8.1f | %7.1f %7.1f %7.1f | %6.1f\n",
			[]string{"field", "inline", "db", "expanded"}[loc], lt.lists, lt.postings, lt.docs,
			k(lt.gaps), k(lt.pos), k(lt.tails), k(lt.gaps+lt.pos+lt.tails),
			k(lt.rice), k(lt.ef), k(lt.bic), k(lt.bound), k(lt.posWidth), k(lt.posGamma), k(lt.posH0), k(lt.tailH0))
	}

	fmt.Fprintf(&rep, "  positions: H0 %.2f bits (of 4); ≥8: %.2f%%, ≥7: %.2f%%; distribution %s\n", posH,
		100*share(posFreq, 8), 100*share(posFreq, 7), dist(posFreq))
	fmt.Fprintf(&rep, "  tails of HasTail lists: H0 %.2f bits (of 2)\n", tailH)

	// 4. Fields, per bucket as encodeFields writes them.
	var fields tally
	gammaCount, gammaDup := 0.0, 0.0
	hapax, words1 := 0, 0

	for from := 0; from < len(keys); {
		group := g.Of(keys[from].Slot())
		to := from + 1

		for to < len(keys) && g.Of(keys[to].Slot()) == group {
			to++
		}

		ws := make([]*word, 0, to-from)
		for _, k := range keys[from:to] {
			ws = append(ws, words[k])
		}

		var maxCount, maxDup, maxX uint32
		firsts := make([]docid.Key, len(ws))

		for i, w := range ws {
			maxCount = max(maxCount, w.shape.Count)
			maxDup = max(maxDup, w.shape.Count-w.df)
			firsts[i] = w.shape.First

			if w.shape.Count > 1 {
				maxX = max(maxX, w.shape.X)
			}

			gammaCount += float64(2*bits.Len(uint64(w.shape.Count)) - 1)
			gammaDup += float64(2*bits.Len(uint64(w.shape.Count-w.df)+1) - 1)

			if w.df == 1 {
				hapax++
			}

			words1++
		}

		split := postings.SplitOf(firsts...)
		wx := max(bits.Len(uint64(maxX)), postings.MinX)
		n := float64(len(ws))

		fields.add("header", 96)
		fields.add("count", n*float64(bits.Len(uint64(maxCount))))
		fields.add("dup", n*float64(bits.Len(uint64(maxDup))))
		fields.add("first key", n*float64(split.WO+split.WID))
		fields.add("V", n*float64(2+wx))
		fields.add("loc", 2*n)

		from = to
	}

	fmt.Fprintf(&rep, "\n== fields (KB), %d keys, %d with df=1 (%.0f%%)\n", words1, hapax, 100*float64(hapax)/float64(words1))
	for _, n := range fields.names {
		fmt.Fprintf(&rep, "  %-10s %8.1f\n", n, fields.kb(n))
	}
	fmt.Fprintf(&rep, "  count as gamma %.1f KB, dup as gamma %.1f KB\n", gammaCount/8/1024, gammaDup/8/1024)

	// 5. Keys: units and their entropy.
	studyKeys(&rep, res, keys)

	// 6. Heads: split keys against gaps within the rank runs.
	studyHeads(&rep, p, words, docs)

	// 7. A local ID table per object: the distinct documents of the object
	// once (Elias-Fano), and every list as positions in that table.
	for _, lg := range []*layout.Grouping{layout.GroupingNone, layout.GroupingES100} {
		studyLocal(&rep, lg, keys, words)
	}

	// 8. A table of the IDs (for numbering documents): Elias-Fano of the sorted keys.
	n := float64(len(docs))
	l := math.Floor(math.Log2(float64(uint64(1)<<32) / n))
	fmt.Fprintf(&rep, "\n== ID table (ordinal → key): Elias-Fano %.1f KB; plain 4 B %.1f KB\n", (n*(l+2))/8/1024, 4*n/1024)

	t.Log("\n" + rep.String())

	if out != "" {
		studyWrite(t, out, res, docs)
	}
}

func share(freq map[uint8]float64, from uint8) float64 {
	var in, all float64

	for k, v := range freq {
		all += v
		if k >= from {
			in += v
		}
	}

	return in / all
}

func dist(freq map[uint8]float64) string {
	var all float64
	for _, v := range freq {
		all += v
	}

	var parts []string
	for k := range uint8(16) {
		parts = append(parts, fmt.Sprintf("%d:%.1f", k, 100*freq[k]/all))
	}

	return strings.Join(parts, " ")
}

func entropy[K comparable](freq map[K]float64) float64 {
	var all, h float64
	for _, v := range freq {
		all += v
	}

	for _, v := range freq {
		if v > 0 {
			h -= v / all * math.Log2(v/all)
		}
	}

	return h
}

// riceBits is the gaps of a sorted list as Rice codes with the best k, plus 6
// bits for k.
func riceBits(ids []uint64) float64 {
	best := math.Inf(1)

	for k := range 41 {
		total := 6.0
		for i := 1; i < len(ids); i++ {
			total += float64((ids[i]-ids[i-1])>>k) + 1 + float64(k)
		}

		best = min(best, total)
	}

	return best
}

// efBits is Elias-Fano of the values after the first, from it, plus 6 bits for
// the low width.
func efBits(ids []uint64) float64 {
	m := float64(len(ids) - 1)
	u := float64(ids[len(ids)-1] - ids[0])
	l := 0.0

	if u/m >= 2 {
		l = math.Floor(math.Log2(u / m))
	}

	return 6 + m*l + m + math.Floor(u/math.Exp2(l)) + 1
}

// bicBits is binary interpolative coding of the list after the first: the last
// in plain bits, then the middle ones recursively with truncated binary codes
// (non-strict bounds: a document may repeat).
func bicBits(ids []uint64) float64 {
	total := float64(bits.Len(ids[len(ids)-1] - ids[0]))

	var rec func(lo, hi int) // ids[lo] and ids[hi] are known
	rec = func(lo, hi int) {
		if hi-lo < 2 {
			return
		}

		mid := (lo + hi) / 2
		total += truncBits(ids[hi]-ids[lo]+1, ids[mid]-ids[lo])
		rec(lo, mid)
		rec(mid, hi)
	}

	rec(0, len(ids)-1)

	return total
}

func truncBits(r, v uint64) float64 {
	if r <= 1 {
		return 0
	}

	k := uint64(bits.Len(r) - 1) // 2^k ≤ r
	u := uint64(1)<<(k+1) - r

	if v < u {
		return float64(k)
	}

	return float64(k + 1)
}

// boundBits is log2 C(u, d) for the distinct documents after the first: no
// codec of the documents alone does better on average.
func boundBits(ids []uint64) float64 {
	d := 0
	for i := 1; i < len(ids); i++ {
		if ids[i] != ids[i-1] {
			d++
		}
	}

	u := float64(ids[len(ids)-1] - ids[0])
	if d == 0 || u <= float64(d) {
		return 0
	}

	lg, _ := math.Lgamma(u + 1)
	ld, _ := math.Lgamma(float64(d) + 1)
	lr, _ := math.Lgamma(u - float64(d) + 1)

	return (lg - ld - lr) / math.Ln2
}

// studyLocal compares, per object of grouping g, the document keys as they
// are now (first keys in the fields, gaps in the lists) against a local table
// of the object's distinct documents plus lists of positions in it.
func studyLocal(rep *strings.Builder, g *layout.Grouping, keys []key5.Key, words map[key5.Key]*word) {
	sorted := slices.Clone(keys)
	slices.SortFunc(sorted, func(a, b key5.Key) int { return cmp.Or(cmp.Compare(g.Of(a.Slot()), g.Of(b.Slot())), cmp.Compare(a, b)) })

	var nowFirst, nowGaps, table, localFirst, localGaps, refs, distinct float64

	for from := 0; from < len(sorted); {
		group := g.Of(sorted[from].Slot())
		to := from + 1

		for to < len(sorted) && g.Of(sorted[to].Slot()) == group {
			to++
		}

		ws := make([]*word, 0, to-from)
		set := map[docid.Key]bool{}
		firsts := make([]docid.Key, 0, to-from)

		for _, k := range sorted[from:to] {
			w := words[k]
			ws = append(ws, w)
			firsts = append(firsts, w.shape.First)

			for _, p := range w.list {
				set[p.ID] = true
			}

			refs += float64(w.df)
		}

		docs := slices.Sorted(func(yield func(docid.Key) bool) {
			for k := range set {
				if !yield(k) {
					return
				}
			}
		})

		ord := make(map[docid.Key]uint64, len(docs))
		for i, d := range docs {
			ord[d] = uint64(i)
		}

		n := float64(len(docs))
		distinct += n
		split := postings.SplitOf(firsts...)
		nowFirst += float64(len(ws)) * float64(split.WO+split.WID)
		localFirst += float64(len(ws)) * float64(bits.Len(uint64(len(docs)-1)))
		// The table: the first key in full, the others Elias-Fano over the
		// object's own range of keys.
		table += 8 * docid.Size
		if len(docs) > 1 {
			keys := make([]uint64, len(docs))
			for i, d := range docs {
				keys[i] = uint64(d)
			}

			table += efBits(keys)
		}

		for _, w := range ws {
			if w.shape.Count <= 1 {
				continue
			}

			gapBits := float64(w.shape.Bits()) - postings.PosBits*float64(w.shape.Count)
			if w.shape.HasTail {
				gapBits -= postings.TailBits * float64(w.shape.Count)
			}

			nowGaps += gapBits

			local := make([]uint64, len(w.list))
			maxGap := uint64(0)

			for i, p := range w.list {
				local[i] = ord[p.ID]
				if i > 0 {
					maxGap = max(maxGap, local[i]-local[i-1])
				}
			}

			fixed := float64(len(local)-1) * float64(bits.Len(maxGap))
			localGaps += min(fixed, riceBits(local), efBits(local))
		}

		from = to
	}

	kb := func(b float64) float64 { return b / 8 / 1024 }
	fmt.Fprintf(rep, "\n== local ID table, %s: %.2f lists per document of an object; now first %.1f + gaps %.1f = %.1f KB; local: table %.1f + first %.1f + gaps %.1f = %.1f KB\n",
		g, refs/distinct, kb(nowFirst), kb(nowGaps), kb(nowFirst+nowGaps), kb(table), kb(localFirst), kb(localGaps), kb(table+localFirst+localGaps))
}

func studyKeys(rep *strings.Builder, res *Result, keys []key5.Key) {
	var sectionBits, dirBits float64
	for _, b := range res.Buckets {
		sectionBits += float64(8 * len(b.sections[0]))
	}

	headers := map[uint8]float64{}
	units0 := map[uint8]float64{}
	units1 := map[[2]uint8]float64{}
	var nUnits, nKeys float64
	runs := 0

	var prev key5.Key
	for i, k := range keys {
		newRun := i == 0 || k.Slot() != prev.Slot() || k.Space() != prev.Space()
		if newRun {
			runs++
		}

		l := max(k.Len()-2, 0)
		s := 0

		if !newRun {
			pl := max(prev.Len()-2, 0)
			for s < l && s < pl && k.Unit(2+s) == prev.Unit(2+s) {
				s++
			}
		}

		headers[uint8(s*8+l)]++
		nKeys++

		for j := 2 + s; j < 2+l; j++ {
			units0[k.Unit(j)]++
			units1[[2]uint8{k.Unit(j - 1), k.Unit(j)}]++
			nUnits++
		}

		prev = k
	}

	dirBits = float64(runs*6*8 + len(res.Buckets)*16)

	// Conditional entropy H(u | previous unit).
	byPrev := map[uint8]map[uint8]float64{}
	for pair, v := range units1 {
		if byPrev[pair[0]] == nil {
			byPrev[pair[0]] = map[uint8]float64{}
		}

		byPrev[pair[0]][pair[1]] = v
	}

	h1 := 0.0
	for _, m := range byPrev {
		var all float64
		for _, v := range m {
			all += v
		}

		h1 += all / nUnits * entropy(m)
	}

	hh, h0 := entropy(headers), entropy(units0)
	kb := func(b float64) float64 { return b / 8 / 1024 }

	fmt.Fprintf(rep, "\n== keys: section %.1f KB = slot directory %.1f + %0.f headers (5 bits) %.1f + %.0f units (5 bits) %.1f\n",
		kb(sectionBits), kb(dirBits), nKeys, kb(5*nKeys), nUnits, kb(5*nUnits))
	fmt.Fprintf(rep, "  entropy: header %.2f bits → %.1f KB; unit order-0 %.2f bits → %.1f KB; order-1 %.2f bits → %.1f KB\n",
		hh, kb(hh*nKeys), h0, kb(h0*nUnits), h1, kb(h1*nUnits))
}

func studyHeads(rep *strings.Builder, p Params, words map[key5.Key]*word, docs []Doc) {
	b := &builder{p: p, words: words}

	var entries, groups int
	var splitBits, gapBits, restBits float64

	for _, w := range words {
		if !w.expand {
			continue
		}

		for _, g := range b.heads(w, groupByDoc(w.list)) {
			groups++
			keys := make([]docid.Key, len(g.Entries))

			for i, e := range g.Entries {
				keys[i] = e.ID
				restBits += float64(4*postings.NibbleCount(uint64(len(e.Postings)-1))) + 4*float64(len(e.Postings))
			}

			s := postings.SplitOf(keys...)
			splitBits += float64(len(keys)) * float64(s.WO+s.WID)
			entries += len(keys)

			// The same keys as runs sorted by key (a head is by rank, then
			// key): the first of each run in full, the rest as Rice gaps.
			var run []uint64
			flush := func() {
				if len(run) > 0 {
					gapBits += float64(s.WO+s.WID) + 2 // the first, and the run length (≈)
					if len(run) > 1 {
						gapBits += riceBits(run)
					}
				}

				run = run[:0]
			}

			for i, k := range keys {
				if i > 0 && k < keys[i-1] {
					flush()
				}

				run = append(run, uint64(k))
			}

			flush()
		}
	}

	kb := func(b float64) float64 { return b / 8 / 1024 }
	fmt.Fprintf(rep, "\n== heads: %d groups, %d entries; keys as split %.1f KB, as gaps in their rank runs %.1f KB; counts+positions %.1f KB\n",
		groups, entries, kb(splitBits), kb(gapBits), kb(restBits))
}

// studyWrite writes the objects and the token streams to dir: one file per
// .idx and .db, and the tokens in their packed form and as text.
func studyWrite(t *testing.T, dir string, res *Result, docs []Doc) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	g := layout.GroupingNone

	for _, b := range res.Buckets {
		name := g.Name(b.Group)
		if err := os.WriteFile(filepath.Join(dir, name+".idx"), b.EncodeIdx(Meta{Group: b.Group}, ""), 0o644); err != nil {
			t.Fatal(err)
		}

		if b.HasDB() {
			if err := os.WriteFile(filepath.Join(dir, name+".db"), b.EncodeDB(Meta{Group: b.Group}), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}

	var packed []byte
	var text strings.Builder

	for _, d := range docs {
		tok := key5.AppendTokens(nil, d.Tokens)
		packed = docid.Append(packed, d.ID)
		packed = binary.LittleEndian.AppendUint16(packed, uint16(len(tok)))
		packed = append(packed, tok...)

		fmt.Fprintf(&text, "%d", d.ID.ID())
		for _, tk := range d.Tokens {
			fmt.Fprintf(&text, " %s", tk.Key)
		}

		text.WriteByte('\n')
	}

	if err := os.WriteFile(filepath.Join(dir, "tokens.packed"), packed, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "tokens.txt"), []byte(text.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}
