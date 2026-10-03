package layout

import (
	"bufio"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

func TestGroupings(t *testing.T) {
	for s := range uint16(slots) {
		if back, ok := slotOf(BucketName(s)); !ok || back != s {
			t.Fatalf("slot %d: name %q reads back as %d", s, BucketName(s), back)
		}
	}

	for _, g := range []*Grouping{GroupingNone, GroupingES100} {
		if byID, err := GroupingByID(g.ID()); err != nil || byID != g {
			t.Errorf("%s: by ID: %v", g, err)
		}

		names := map[string]bool{}
		next := uint16(0)

		for group := range uint16(g.Groups()) {
			lo, hi := g.Slots(group)
			if lo != next || hi < lo {
				t.Fatalf("%s: group %d is %d..%d after %d", g, group, lo, hi, next)
			}

			for s := lo; s <= hi; s++ {
				if g.Of(s) != group {
					t.Fatalf("%s: slot %s in group %d, Of says %d", g, BucketName(s), group, g.Of(s))
				}
			}

			if names[g.Name(group)] {
				t.Fatalf("%s: name %s twice", g, g.Name(group))
			}

			names[g.Name(group)] = true
			next = hi + 1
		}

		if next != slots {
			t.Errorf("%s: the groups end at slot %d", g, next)
		}
	}

	if GroupingES100.Groups() != 100 {
		t.Errorf("es100 has %d groups", GroupingES100.Groups())
	}

	if g := GroupingES100; g.Name(g.Of(key5.FromToken("camisa").Key.Slot())) != "ca" || g.Name(g.Of(key5.FromToken("xilofono").Key.Slot())) != "vo-y0" {
		t.Errorf("es100: camisa in %s, xilofono in %s", g.Name(g.Of(key5.FromToken("camisa").Key.Slot())), g.Name(g.Of(key5.FromToken("xilofono").Key.Slot())))
	}

	if _, err := GroupingByID(200); err == nil {
		t.Error("an unknown grouping was accepted")
	}
}

// TestES100 rebuilds the es100 table from a Spanish word frequency list,
// "word count" per line, most frequent first:
//
//	curl -LO https://raw.githubusercontent.com/hermitdave/FrequencyWords/master/content/2018/es/es_50k.txt
//	GXS_ES_FREQ=es_50k.txt go test ./internal/layout -run ES100 -v
func TestES100(t *testing.T) {
	path := os.Getenv("GXS_ES_FREQ")
	if path == "" {
		t.Skip("set GXS_ES_FREQ to a Spanish word frequency list")
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// Letters: half the share of distinct keys, half the share of
	// occurrences.
	lex := make([]float64, slots)
	freq := make([]float64, slots)
	seen := map[key5.Key]bool{}

	sc := bufio.NewScanner(file)
	for sc.Scan() {
		word, count, _ := strings.Cut(sc.Text(), " ")
		n, _ := strconv.ParseFloat(count, 64)

		for _, tok := range normalize.Tokens(word) {
			k := key5.FromToken(tok).Key
			freq[k.Slot()] += n

			if !seen[k] {
				seen[k] = true
				lex[k.Slot()]++
			}
		}
	}

	// Numbers: no word list has them; a value n is as frequent as 1/(n+1).
	num := make([]float64, slots)
	for n := range key5.MaxNumber + 1 {
		num[key5.Number(uint32(n)).Slot()] += 1 / float64(n+1)
	}

	lex, freq, num = share(lex), share(freq), share(num)
	letters := make([]float64, slots)

	for s := range letters {
		if num[s] == 0 {
			letters[s] = (lex[s] + freq[s]) / 2
		}
	}

	letters = share(letters)
	weights := make([]float64, slots)

	for s := range weights {
		weights[s] = 0.8*letters[s] + 0.2*num[s]
	}

	var firsts []string

	groups := partition(weights, 100)
	for s, g := range groups {
		if s == 0 || g != groups[s-1] {
			firsts = append(firsts, BucketName(uint16(s)))
		}
	}

	if got := strings.Join(firsts, " "); got != es100 {
		t.Errorf("es100 rebuilt:\n%s\nin the code:\n%s", got, es100)
	}
}

func share(v []float64) []float64 {
	total := 0.0
	for _, x := range v {
		total += x
	}

	out := slices.Clone(v)
	for i := range out {
		out[i] /= total
	}

	return out
}

// partition cuts the slots, in order, into at most g runs, making the
// heaviest as light as it can; a slot heavier than that stands alone.
func partition(w []float64, g int) []int {
	assign := func(c float64) ([]int, int) {
		out := make([]int, len(w))
		group, sum := 0, 0.0

		for s, v := range w {
			if sum+v > c && sum > 0 {
				group++
				sum = 0
			}

			sum += v
			out[s] = group
		}

		return out, group + 1
	}

	lo, hi := 0.0, 1.0
	for range 100 {
		if _, n := assign((lo + hi) / 2); n <= g {
			hi = (lo + hi) / 2
		} else {
			lo = (lo + hi) / 2
		}
	}

	out, _ := assign(hi)

	return out
}
