package tokens

import (
	"bufio"
	"bytes"
	"cmp"
	"compress/flate"
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

	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

// TestCompressStudy measures other layouts of the documents of the tokens
// object, with and without flate (the standard library's deflate). It is a
// measurement, not a check.
//
//	GXS_COMPRESS=1 GXS_EVAL_CSV=$PWD/../scripts/perf/data/productos.csv go test ./internal/tokens -run CompressStudy -v
//
// GXS_COMPRESS_SCALE (1,10) and GXS_COMPRESS_IDS (random,sparse10,del10,
// shuffled) as in bucket.TestCompressStudy; GXS_COMPRESS_OUT writes every
// layout there, for external compressors.
func TestCompressStudy(t *testing.T) {
	if os.Getenv("GXS_COMPRESS") == "" {
		t.Skip("set GXS_COMPRESS=1 and GXS_EVAL_CSV")
	}

	names := studyLines(t, os.Getenv("GXS_EVAL_CSV"))

	for _, scale := range studyList("GXS_COMPRESS_SCALE", "1,10") {
		n, _ := strconv.Atoi(scale)

		for _, mode := range studyList("GXS_COMPRESS_IDS", "random,sparse10,del10,shuffled") {
			t.Run(fmt.Sprintf("x%d/%s", n, mode), func(t *testing.T) {
				docs := studyDocs(names, n, mode)
				layouts := studyLayouts(docs)

				var rep strings.Builder
				fmt.Fprintf(&rep, "%d docs, %d tokens\n%-34s %10s %10s %10s\n", len(docs), countTokens(docs), "layout", "raw KB", "flate KB", "flate %")

				base := float64(len(layouts[0].data))
				for _, l := range layouts {
					z := deflate(l.data)
					fmt.Fprintf(&rep, "%-34s %10.1f %10.1f %9.0f%%\n", l.name, float64(len(l.data))/1024, float64(len(z))/1024, 100*(float64(len(z))/base-1))
				}

				fmt.Fprintf(&rep, "%s", entropies(docs))
				t.Log("\n" + rep.String())

				if dir := os.Getenv("GXS_COMPRESS_OUT"); dir != "" {
					dir = filepath.Join(dir, fmt.Sprintf("tokens-x%d-%s", n, mode))
					if err := os.MkdirAll(dir, 0o755); err != nil {
						t.Fatal(err)
					}

					for i, l := range layouts {
						if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.bin", i)), l.data, 0o644); err != nil {
							t.Fatal(err)
						}
					}
				}
			})
		}
	}
}

type layout struct {
	name string
	data []byte
}

// studyLayouts are the documents in several layouts, the first being the one
// of today.
func studyLayouts(docs []studyDoc) []layout {
	var now, delta []byte

	for i, d := range docs {
		tok := key5.AppendTokens(nil, d.tokens)

		now = docid.Append(now, d.id)
		now = binary.LittleEndian.AppendUint16(now, uint16(len(tok)))
		now = append(now, tok...)

		delta = binary.AppendUvarint(delta, gap(docs, i))
		delta = binary.AppendUvarint(delta, uint64(len(tok)))
		delta = append(delta, tok...)
	}

	// The vocabulary: every distinct token (key and tail) once, most frequent
	// first, as one token stream; a document is its tokens' numbers.
	freq := map[key5.Token]int{}
	for _, d := range docs {
		for _, tk := range d.tokens {
			freq[tk]++
		}
	}

	vocab := make([]key5.Token, 0, len(freq))
	for tk := range freq {
		vocab = append(vocab, tk)
	}

	slices.SortFunc(vocab, func(a, b key5.Token) int {
		return cmp.Or(cmp.Compare(freq[b], freq[a]), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Tail, b.Tail))
	})

	number := make(map[key5.Token]uint64, len(vocab))
	for i, tk := range vocab {
		number[tk] = uint64(i)
	}

	vocabBytes := key5.AppendTokens(nil, vocab)

	// Row by row: gap, count, numbers. And in columns: every gap, then every
	// count, then every number.
	rows := slices.Clone(vocabBytes)
	var gaps, counts, numbers []byte
	var numbers16 []byte

	for i, d := range docs {
		rows = binary.AppendUvarint(rows, gap(docs, i))
		rows = binary.AppendUvarint(rows, uint64(len(d.tokens)))
		gaps = binary.AppendUvarint(gaps, gap(docs, i))
		counts = append(counts, byte(len(d.tokens)))

		for _, tk := range d.tokens {
			rows = binary.AppendUvarint(rows, number[tk])
			numbers = binary.AppendUvarint(numbers, number[tk])
			numbers16 = binary.LittleEndian.AppendUint16(numbers16, uint16(number[tk]))
		}
	}

	columns := slices.Concat(vocabBytes, gaps, counts, numbers)
	columns16 := slices.Concat(vocabBytes, gaps, counts, numbers16)

	return []layout{
		{"now: key 5 B | len u16 | packed", now},
		{"gap uvarint | len uvarint | packed", delta},
		{"vocabulary + rows of uvarints", rows},
		{"vocabulary + columns, uvarint", columns},
		{"vocabulary + columns, u16 numbers", columns16},
	}
}

func gap(docs []studyDoc, i int) uint64 {
	if i == 0 {
		return uint64(docs[0].id)
	}

	return uint64(docs[i].id - docs[i-1].id)
}

func countTokens(docs []studyDoc) int {
	n := 0
	for _, d := range docs {
		n += len(d.tokens)
	}

	return n
}

// entropies are the order-0 entropies of the parts of the vocabulary layout:
// what an entropy coder (deflate's Huffman, or better) can get near.
func entropies(docs []studyDoc) string {
	gaps, counts, toks := map[uint64]float64{}, map[uint64]float64{}, map[key5.Token]float64{}
	maxGap := uint64(0)

	for i, d := range docs {
		g := gap(docs, i)
		if i > 0 {
			maxGap = max(maxGap, g)
		}

		gaps[g]++
		counts[uint64(len(d.tokens))]++

		for _, tk := range d.tokens {
			toks[tk]++
		}
	}

	n := float64(len(docs))
	hg, hc, ht := entropy(gaps), entropy(counts), entropy(toks)
	nt := float64(countTokens(docs))

	return fmt.Sprintf("order-0 entropy: gap %.2f bits (max gap %d), count %.2f bits, token %.2f bits (%d distinct) → %.1f KB + vocabulary\n",
		hg, maxGap, hc, ht, len(toks), (n*(hg+hc)+nt*ht)/8/1024)
}

func entropy[K comparable](freq map[K]float64) float64 {
	var all, h float64
	for _, v := range freq {
		all += v
	}

	for _, v := range freq {
		h -= v / all * math.Log2(v/all)
	}

	return h
}

func deflate(data []byte) []byte {
	var buf bytes.Buffer

	w, _ := flate.NewWriter(&buf, flate.BestCompression)
	w.Write(data)
	w.Close()

	return buf.Bytes()
}

type studyDoc struct {
	id     docid.Key
	tokens []key5.Token
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

// studyDocs are as in bucket.TestCompressStudy, sorted by key.
func studyDocs(names []string, scale int, mode string) []studyDoc {
	r := rand.New(rand.NewPCG(uint64(scale), 99))
	n := len(names) * scale
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
	case "sparse10", "del10":
		universe := 10 * n
		if mode == "del10" {
			universe = int(math.Ceil(float64(n) / 0.9))
		}

		for i, p := range r.Perm(universe)[:n] {
			ids[i] = uint32(p + 1)
		}
	default:
		panic(mode)
	}

	docs := make([]studyDoc, 0, n)

	for c := range scale {
		for i, name := range names {
			docs = append(docs, studyDoc{id: docid.New(0, ids[c*len(names)+i]), tokens: key5.FromTokens(normalize.Default.Tokens(name))})
		}
	}

	slices.SortFunc(docs, func(a, b studyDoc) int { return cmp.Compare(a.id, b.id) })

	return docs
}
