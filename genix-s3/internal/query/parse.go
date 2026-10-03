// Package query answers a search from k, p and p.log (PLAN_5BIT.md §7).
package query

import (
	"strings"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
)

// MaxWords bounds the complete words of a query; later ones are dropped.
const MaxWords = 16

// Term is one thing a document can cover: a word or number of the query, or
// the explicit `foo*` prefix.
type Term struct {
	Text     string // normalized
	Token    key5.Token
	Units    []uint8 // every letter of the word, not cut at 8: what fuzzy compares
	Number   bool
	Position int  // in the query, for the query-position bonus
	Auto     bool // the last word: also matches as a prefix
	Explicit bool // `foo*`: matches only as a prefix, and is required
}

// Parsed is a query split into terms.
type Parsed struct {
	Terms []Term
}

// Parse splits a query: only the first `*` term counts (later ones are
// dropped), and without one the last complete word also acts as a prefix.
func Parse(norm *normalize.Normalizer, input string) Parsed {
	var words []string
	var explicit string

	for _, raw := range strings.Fields(input) {
		if body, ok := strings.CutSuffix(raw, "*"); ok {
			if explicit == "" {
				if tokens := norm.Normalize(body); len(tokens) > 0 {
					explicit = tokens[0]
				}
			}

			continue
		}

		words = append(words, norm.Tokens(raw)...)
	}

	words = words[:min(len(words), MaxWords)]

	var p Parsed

	for i, w := range words {
		t := newTerm(w, i)
		t.Auto = explicit == "" && i == len(words)-1
		p.Terms = append(p.Terms, t)
	}

	if explicit != "" {
		t := newTerm(explicit, len(words))
		t.Explicit = true
		p.Terms = append(p.Terms, t)
	}

	return p
}

func newTerm(text string, position int) Term {
	t := Term{Text: text, Token: key5.FromToken(text), Number: normalize.IsNumber(text), Position: position}

	if !t.Number {
		t.Units = make([]uint8, len(text))
		for i := range len(text) {
			t.Units[i] = key5.Unit(text[i])
		}
	}

	return t
}

// Empty reports a query with nothing to match.
func (p Parsed) Empty() bool { return len(p.Terms) == 0 }

// prefixable reports whether the term is expanded as a prefix: an open term
// of two characters or more. A one-character prefix would be most of the
// index, so it only matches itself.
func (t *Term) prefixable() bool {
	return (t.Auto || t.Explicit) && len(t.Text) >= 2
}

// keyRange is a range of keys lo ≤ key ≤ hi.
type keyRange struct{ lo, hi key5.Key }

// prefixRanges are the key ranges the term covers as a prefix.
func (t *Term) prefixRanges() []keyRange {
	if !t.prefixable() {
		return nil
	}

	if t.Number {
		var out []keyRange
		for _, r := range key5.NumberPrefixRanges(t.Text) {
			out = append(out, keyRange{key5.Number(r[0]), key5.Number(r[1])})
		}

		return out
	}

	lo, hi := t.Token.Key.PrefixRange(min(len(t.Text), key5.MaxUnits))

	return []keyRange{{lo, hi}}
}

// fuzzyBudget is the edit budget for a word of this length: none under four
// letters, where one edit is a different word.
func fuzzyBudget(length int) int {
	switch {
	case length < 4:
		return 0
	case length < 8:
		return 1
	}

	return 2
}
