package query

import (
	"slices"
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
	// Conts are the keys of a one-letter term: its continuation of every
	// host word of the query. "m talla" looks for
	// talla›m. Nil for any other term.
	Conts []key5.Key
	// Sig is set on a virtual term: the signature of a run of words of the
	// query, which matches the documents of a group with those words.
	// It adds to the score but is not a word.
	Sig *Signature
}

// Signature is a run of words of the query read as a group.
type Signature struct {
	Key      key5.Key
	Segments int
	// Covers are the terms of the run's words.
	Covers []int
}

// letter reports a one-letter term, matched through its continuations.
func (t *Term) letter() bool { return t.Conts != nil }

// virtual reports a signature term.
func (t *Term) virtual() bool { return t.Sig != nil }

// exactKeys are the keys the term matches exactly: its own, its
// continuations, or its signature.
func (t *Term) exactKeys() []key5.Key {
	switch {
	case t.virtual():
		return []key5.Key{t.Sig.Key}
	case t.letter():
		return t.Conts
	}

	return []key5.Key{t.Token.Key}
}

// groupKey is the key the term matches as a group word: its own in that
// space. A letter has none, and neither has `foo*`, which only matches as a
// prefix.
func (t *Term) groupKey() (key5.Key, bool) {
	if t.virtual() || t.letter() || t.Explicit {
		return 0, false
	}

	return t.Token.Key.In(key5.SpaceGroupWord), true
}

// groupRanges are the ranges of group words the term matches as a prefix:
// those of its word prefix, in that space.
func (t *Term) groupRanges() []keyRange {
	if t.virtual() || t.letter() || t.Number {
		return nil
	}

	var out []keyRange
	for _, r := range t.prefixRanges() {
		out = append(out, keyRange{r.lo.In(key5.SpaceGroupWord), r.hi.In(key5.SpaceGroupWord)})
	}

	return out
}

// Parsed is a query split into terms: its words, then the virtual terms of
// its signatures.
type Parsed struct {
	Terms []Term
}

// Parse splits a query: only the first `*` term counts (later ones are
// dropped), and without one the last complete word also acts as a prefix.
//
// A one-letter word continues the host words of the query, wherever they
// are: it goes after them, so "m talla" is "talla m", and matches talla›m.
// With no host word it is dropped. It is never a prefix: "m*" is "m".
func Parse(norm *normalize.Normalizer, input string) Parsed {
	var words []string
	var explicit string

	for _, raw := range strings.Fields(input) {
		if body, ok := strings.CutSuffix(raw, "*"); ok {
			if tokens := norm.Normalize(body); len(tokens) > 0 && key5.IsLetter(tokens[0]) {
				words = append(words, norm.Tokens(body)...)
			} else if explicit == "" && len(tokens) > 0 {
				explicit = tokens[0]
			}

			continue
		}

		words = append(words, norm.Tokens(raw)...)
	}

	words = words[:min(len(words), MaxWords)]

	var hosts []key5.Key
	var letters []int
	var p Parsed

	// The term of each word, -1 for a letter without a host.
	termOf := make([]int, len(words))

	for i, w := range words {
		termOf[i] = -1

		if key5.IsLetter(w) {
			letters = append(letters, i)

			continue
		}

		// A word typed twice is one term: it counts once ("platos preparados
		// preparados"). The runs of the signatures keep both.
		if j := slices.Index(words[:i], w); j >= 0 {
			termOf[i] = termOf[j]
			p.Terms[termOf[j]].Auto = p.Terms[termOf[j]].Auto || explicit == "" && i == len(words)-1

			continue
		}

		// The last word typed is the open one; a letter typed before it
		// moves behind it ("m tal" is "tal m", tal still open).
		t := newTerm(w, len(p.Terms))
		t.Auto = explicit == "" && i == len(words)-1
		termOf[i] = len(p.Terms)
		p.Terms = append(p.Terms, t)

		if k := t.Token.Key; k.IsHost() && !slices.Contains(hosts, k) {
			hosts = append(hosts, k)
		}
	}

	if len(hosts) > 0 {
		for n, i := range letters {
			if j := slices.IndexFunc(letters[:n], func(j int) bool { return words[j] == words[i] }); j >= 0 {
				termOf[i] = termOf[letters[j]]

				continue
			}

			t := newTerm(words[i], len(p.Terms))
			// Hosts that share their first 6 letters share the continuation.
			for _, h := range hosts {
				if k := key5.Continuation(h, key5.Unit(words[i][0])); !slices.Contains(t.Conts, k) {
					t.Conts = append(t.Conts, k)
				}
			}

			termOf[i] = len(p.Terms)
			p.Terms = append(p.Terms, t)
		}
	}

	if explicit != "" {
		t := newTerm(explicit, len(p.Terms))
		t.Explicit = true
		p.Terms = append(p.Terms, t)
	}

	p.Terms = append(p.Terms, signatures(words, termOf, explicit == "")...)

	return p
}

// signatures are the virtual terms of every run of 1 to key5.Segments words
// of the query that does not start with a letter:
// the query does not know which words are a group, so it tries them all. A
// signature lives in the slot of its first word, which the query reads
// anyway. The open last word joins a run only with its segment complete:
// 3 letters, or 2 as the first word.
func signatures(words []string, termOf []int, open bool) []Term {
	var out []Term

	for from := range words {
		if key5.IsLetter(words[from]) {
			continue
		}

		for to := from + 1; to <= min(len(words), from+key5.Segments); to++ {
			last := words[to-1]
			if open && to == len(words) && (normalize.IsNumber(last) || len(last) < 3 && (to-from > 1 || len(last) < 2)) {
				break
			}

			k, n := key5.Signature(words[from:to])
			if slices.ContainsFunc(out, func(t Term) bool { return t.Sig.Key == k }) {
				continue
			}

			sig := &Signature{Key: k, Segments: n}
			for _, i := range termOf[from:to] {
				if i >= 0 && !slices.Contains(sig.Covers, i) {
					sig.Covers = append(sig.Covers, i)
				}
			}

			out = append(out, Term{Token: key5.Token{Key: k}, Position: -1, Sig: sig})
		}
	}

	return out
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

// prefixRanges are the key ranges the term covers as a prefix. A signature
// of 2 segments or more also matches the longer ones that start with it
// ("centro civico" those of "centro civico norte"); one of a single segment
// does not: 2 letters would be most of its slot.
func (t *Term) prefixRanges() []keyRange {
	if t.virtual() {
		if t.Sig.Segments < 2 {
			return nil
		}

		lo, hi := t.Sig.Key.PrefixRange(2 * t.Sig.Segments)

		return []keyRange{{lo, hi}}
	}

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
