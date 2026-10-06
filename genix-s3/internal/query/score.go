package query

import (
	"cmp"
	"math"
	"math/bits"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// Ranking tunables (PLAN_BUCKETS.md §17.5). Results are ordered by score
// alone, then by document key. Everything is computed from keys, tails and
// positions: the index holds no text.
const (
	// Per term matched by the text: its letters. A term earns LetterFirst
	// for its first letter and LetterNext for each other one that matches,
	// up to the 8 of a key (a number counts digits), and WordFull more for
	// the whole word: same key and same tail. "juan" is 21, "carlos" 27.
	LetterFirst = 5
	LetterNext  = 3
	WordFull    = 7

	// A word that differs from the term only by a last letter or a last "es"
	// ("bolsas" and "bolsa", "pan" and "panes": a near word) earns the letters
	// the two share, less LetterExtra for each letter one has and the other
	// does not: what a letter is worth. "bolsas" against "bolsa" is
	// 17 − 3 = 14, as the prefix "bols"; the exact word, 27.
	LetterExtra = LetterNext
	// NearExtra is the most letters a near word may add or lack, and
	// NearMinLetters the fewest the shorter of the two must have.
	NearExtra      = 2
	NearMinLetters = 3

	// The group: a signature earns GroupSegment per segment of the query and
	// GroupFull more when it is the document's whole signature; a term
	// matched only by a group word earns GroupWord. The group adds the best
	// of the two.
	GroupSegment = 4
	GroupFull    = 2
	GroupWord    = 4

	// Per pair of terms matched by the text, by how close they sit in the
	// document and whether they keep the query's order.
	PairAdjacent         = 6
	PairAdjacentReversed = 3
	PairNear             = 2

	// Per term matched by the text, when it is the document's first or
	// second word.
	DocFirstWord  = 2
	DocSecondWord = 1

	// Per term matched by the text, when it is the first word the user typed:
	// that one usually leads the intent ("cafe organico" looks for coffee).
	QueryFirstWord = 2

	// A term matched by the text also earns its rarity:
	// round(IDFWeight · ln(N/df)), at most IDFMax: any word in under 1.8 % of
	// the documents is rare enough (PLAN_BENCH.md 11: 8 ranks better than 15).
	IDFWeight = 2
	IDFMax    = 8
)

type matchKind uint8

// Better kinds are smaller.
const (
	noMatch matchKind = iota
	exactMatch
	stemMatch
	prefixMatch
	longerMatch  // a near word with letters the term lacks: "panes" for "pan"
	shorterMatch // a near word that lacks letters of the term: "bolsa" for "bolsas"
	fuzzyMatch
	groupWordMatch // only through a group word
)

// near reports a match of a near word, whose dist is the letters of difference.
func (k matchKind) near() bool { return k == longerMatch || k == shorterMatch }

// keyKind is how a key matches a term, before the tail is known: exactKey
// becomes exactMatch or stemMatch by the tail of each posting.
type keyKind uint8

const (
	noKey keyKind = iota
	exactKey
	prefixKey
	longerKey
	shorterKey
	fuzzyKey
	groupKey
)

func (k keyKind) near() bool { return k == longerKey || k == shorterKey }

// classifyKey says how a dictionary key matches term t. fuzzy enables the
// typo match for this term (the engine enables it only for terms the
// dictionary does not have). A typo is never in the first two letters: the
// key must be in the term's bucket (PLAN_BUCKETS.md §6.1).
//
// A one-letter term matches only its continuations, and no other term
// matches a continuation: talla›m is neither a prefix nor a typo of talla. A
// signature term matches only signatures, and a word only words and group
// words (§17).
func (t *Term) classifyKey(k key5.Key, fuzzy bool, scratch []uint8) (keyKind, uint8) {
	switch {
	case t.virtual():
		if k == t.Sig.Key {
			return exactKey, 0
		}

		for _, r := range t.prefixRanges() {
			if k >= r.lo && k <= r.hi {
				return prefixKey, 0
			}
		}

		return noKey, 0
	case k.Space() == key5.SpaceSignature:
		return noKey, 0
	case k.Space() == key5.SpaceGroupWord:
		if g, ok := t.groupKey(); ok && k == g {
			return groupKey, 0
		}

		for _, r := range t.groupRanges() {
			if k >= r.lo && k <= r.hi {
				return groupKey, 0
			}
		}

		return noKey, 0
	}

	if t.letter() || k.IsContinuation() {
		if t.letter() && slices.Contains(t.Conts, k) {
			return exactKey, 0
		}

		return noKey, 0
	}

	if !t.Explicit && k == t.Token.Key {
		return exactKey, 0
	}

	if t.Auto || t.Explicit {
		if t.prefixable() {
			for _, r := range t.prefixRanges() {
				if k >= r.lo && k <= r.hi {
					return prefixKey, 0
				}
			}
		} else if k == t.Token.Key {
			return prefixKey, 0
		}
	}

	if kind, d := t.nearKey(k); kind != noKey {
		return kind, d
	}

	if fuzzy && !t.Explicit && !t.Number && !k.IsNumber() && k.Slot() == t.Token.Key.Slot() {
		budget := fuzzyBudget(len(t.Text))
		if budget == 0 {
			return noKey, 0
		}

		if d, ok := key5.KeyDistance(k.Units(scratch[:0]), t.Units, budget); ok {
			return fuzzyKey, uint8(d)
		}
	}

	return noKey, 0
}

// nearKey reports a near word (PLAN_BUCKETS.md §19): one whose letters are
// those of the term with 1 more or fewer at the end ("bolsas", "bolsa"), or
// "es" more or fewer ("pan", "panes"), the shorter of the two of
// NearMinLetters or more. It
// is enabled for every word, typos or not, and found or not: the plural of
// a word is in the word's own bucket, which the query reads anyway. The key
// has the word's first 8 letters, so a shorter word must have fewer than 8
// to be whole; a longer one of 8 can be longer still, which its tail tells
// (resolve).
func (t *Term) nearKey(k key5.Key) (keyKind, uint8) {
	if t.Explicit || t.Number || k.IsNumber() || k.Space() != key5.SpaceWord {
		return noKey, 0
	}

	n, m := len(t.Units), k.Len()
	kind := longerKey

	if m < n {
		kind = shorterKey
		if m >= key5.MaxUnits {
			return noKey, 0
		}
	}

	d := max(n, m) - min(n, m)
	if m == n || min(n, m) < NearMinLetters || d > NearExtra {
		return noKey, 0
	}

	for i := range min(n, m) {
		if k.Unit(i) != t.Units[i] {
			return noKey, 0
		}
	}

	// Two letters only as the plural "-es": "pan" and "panes", not "bot" and
	// "boton" or "smile" and "smileat".
	if d == 2 {
		long := t.Units
		if kind == longerKey {
			long = k.Units(make([]uint8, 0, key5.MaxUnits))
		}

		if long[len(long)-2] != key5.Unit('e') || long[len(long)-1] != key5.Unit('s') {
			return noKey, 0
		}
	}

	return kind, uint8(d)
}

// resolve turns a key match into a match of one posting.
func (t *Term) resolve(kind keyKind, tail uint8) matchKind {
	switch kind {
	case exactKey:
		if tail == t.Token.Tail {
			return exactMatch
		}

		return stemMatch
	case prefixKey:
		return prefixMatch
	case longerKey:
		// A tail: the word goes on past its 8 letters, too far from the term.
		if tail != 0 {
			return noMatch
		}

		return longerMatch
	case shorterKey:
		return shorterMatch
	case fuzzyKey:
		return fuzzyMatch
	case groupKey:
		return groupWordMatch
	}

	return noMatch
}

// termMatch is how a document matched one term: the best kind, and every
// position it matched at with that kind (one for a typo: the closest).
type termMatch struct {
	kind      matchKind
	dist      uint8
	positions []uint8
}

func (m *termMatch) add(kind matchKind, dist, pos uint8) {
	switch {
	case kind == noMatch:
	case m.kind == noMatch || kind < m.kind:
		m.kind, m.dist, m.positions = kind, dist, append(m.positions[:0], pos)
	case kind > m.kind:
	case kind == fuzzyMatch || kind.near():
		if dist < m.dist || (dist == m.dist && pos < m.positions[0]) {
			m.dist, m.positions[0] = dist, pos
		}
	default:
		m.positions = append(m.positions, pos)
	}
}

// byText reports a match of the text: not a group word.
func (m *termMatch) byText() bool { return m.kind != noMatch && m.kind != groupWordMatch }

// docScore ranks a document. coverage, the terms it covers by its text or
// its group, is reported but does not order.
type docScore struct {
	coverage uint32
	score    uint32
}

// scoreDoc scores a document from its matches, one per term, with idf the
// rarity bonus of every term. ok is false when a required explicit prefix is
// missing.
//
// When the query has words besides those the document's group matched (the
// run of its best signature), it is a name and a group, and the words of the
// run count once: by the text or by the group, whichever earns more
// (PLAN_BUCKETS.md §17.5). For "hidratante cuidado corporal", "Crema corporal
// @ cuidado corporal" does not earn "corporal" twice, and "Crema hidratante @
// cuidado corporal" ranks first. When the group matched the whole query, the
// text and the group both count: a member whose name says it too ranks first.
// The group never lowers a score.
func scoreDoc(terms []Term, matches []termMatch, idf []uint32) (docScore, bool) {
	var s docScore
	var covered, bySig, byText, words uint64
	var groupWords, signature uint32
	var points [64]uint32 // of each term matched by the text

	for i := range terms {
		t, m := &terms[i], &matches[i]
		if !t.virtual() {
			words |= 1 << i
		}

		switch {
		case m.kind == noMatch:
			if t.Explicit {
				return docScore{}, false
			}
		case t.virtual():
			var run uint64
			for _, c := range t.Sig.Covers {
				run |= 1 << c
			}

			covered |= run

			// The best signature; at the same points, the longest run.
			if p := signaturePoints(t, m.kind == exactMatch); p > signature || p == signature && bits.OnesCount64(run) > bits.OnesCount64(bySig) {
				signature, bySig = p, run
			}
		case m.kind == groupWordMatch:
			groupWords += GroupWord
			covered |= 1 << i
		default:
			points[i] = termPoints(t, m) + docPositionBonus(slices.Min(m.positions)) + queryPositionBonus(t) + idf[i]
			covered |= 1 << i
			byText |= 1 << i
		}
	}

	// alone counts every word by the text and ignores the group; grouped, the
	// group instead of the words of its run.
	var alone, grouped uint32

	for i := range terms {
		if byText&(1<<i) == 0 {
			continue
		}

		alone += points[i]
		if bySig&(1<<i) == 0 {
			grouped += points[i]
		}

		for j := i + 1; j < len(terms); j++ {
			if byText&(1<<j) == 0 {
				continue
			}

			distance, ordered := closest(matches[i].positions, matches[j].positions)
			bonus := pairBonus(distance, ordered)

			alone += bonus
			if bySig&(1<<i|1<<j) == 0 {
				grouped += bonus
			}
		}
	}

	if words&^bySig == 0 {
		s.score = alone + max(signature, groupWords)
	} else {
		s.score = max(alone, grouped+max(signature, groupWords))
	}

	s.coverage = uint32(bits.OnesCount64(covered))

	return s, true
}

// letters is how many letters of the term a match can count: its letters up
// to the 8 of a key, or its digits.
func (t *Term) letters() int {
	return max(1, min(len(t.Text), key5.MaxUnits))
}

// letterPoints is the score of n matching letters.
func letterPoints(n int) uint32 {
	return LetterFirst + LetterNext*uint32(max(n, 1)-1)
}

// termPoints is what a match of the text earns for its letters.
func termPoints(t *Term, m *termMatch) uint32 {
	switch m.kind {
	case exactMatch:
		return letterPoints(t.letters()) + WordFull
	case stemMatch, prefixMatch:
		return letterPoints(t.letters())
	case longerMatch, shorterMatch:
		return nearPoints(t, m.kind, m.dist)
	case fuzzyMatch:
		return letterPoints(t.letters() - int(m.dist))
	}

	return 0
}

// nearPoints is what a near word earns: the letters it shares with the term,
// less LetterExtra per letter of difference.
func nearPoints(t *Term, kind matchKind, dist uint8) uint32 {
	shared := len(t.Text)
	if kind == shorterMatch {
		shared -= int(dist)
	}

	return letterPoints(min(shared, key5.MaxUnits)) - LetterExtra*uint32(dist)
}

// signaturePoints is what a signature term earns: per segment of the query,
// and more when it is the document's whole signature.
func signaturePoints(t *Term, whole bool) uint32 {
	points := GroupSegment * uint32(t.Sig.Segments)
	if whole {
		points += GroupFull
	}

	return points
}

// closest finds the nearest pair of occurrences of two terms; at the same
// distance it prefers the one in query order.
func closest(first, second []uint8) (distance int, ordered bool) {
	distance = -1

	for _, a := range first {
		for _, b := range second {
			d := int(b) - int(a)
			inOrder := d > 0

			if d < 0 {
				d = -d
			}

			if d == 0 {
				continue
			}

			if distance < 0 || d < distance || (d == distance && inOrder && !ordered) {
				distance, ordered = d, inOrder
			}
		}
	}

	if distance < 0 {
		// Both terms matched the same token (e.g. a prefix of the other).
		return 1 << 30, false
	}

	return distance, ordered
}

// compareScores is the result order: score descending, then the document key
// (origin, then ID), which only breaks ties.
func compareScores(a, b docScore, aID, bID docid.Key) int {
	return cmp.Or(
		cmp.Compare(b.score, a.score),
		cmp.Compare(aID, bID),
	)
}

func idfBonus(df, docs int) uint32 {
	if df <= 0 {
		df = 1
	}

	if docs <= df {
		return 0
	}

	return uint32(min(IDFMax, math.Round(IDFWeight*math.Log(float64(docs)/float64(df)))))
}

func docPositionBonus(pos uint8) uint32 {
	switch pos {
	case 0:
		return DocFirstWord
	case 1:
		return DocSecondWord
	}

	return 0
}

// queryPositionBonus is what a term matched by the text earns for its place
// in the query: the first word typed leads.
func queryPositionBonus(t *Term) uint32 {
	if t.Position == 0 {
		return QueryFirstWord
	}

	return 0
}

func pairBonus(distance int, ordered bool) uint32 {
	switch {
	case distance == 1 && ordered:
		return PairAdjacent
	case distance == 1:
		return PairAdjacentReversed
	case distance == 2:
		return PairNear
	}

	return 0
}

// Ranked is a scored document.
type ranked struct {
	id    docid.Key
	score docScore
}

// idfs is the rarity bonus of every term. df of a term is the sum, over the
// keys of the dictionary that match it, of the documents with the key: known
// from the dictionary alone, without reading a list (PLAN_BUCKETS.md §6.2).
// docs is the documents the dictionary describes.
func idfs(df []int, docs int) []uint32 {
	idf := make([]uint32, len(df))
	for i := range idf {
		idf[i] = idfBonus(df[i], docs)
	}

	return idf
}

func orderRanked(a, b ranked) int { return compareScores(a.score, b.score, a.id, b.id) }

// rankAll scores every candidate, adds the rarity bonus and returns the
// first top in order.
func rankAll(terms []Term, cands *candidates, idf []uint32, top int) []ranked {
	results := make([]ranked, 0, cands.len())

	cands.each(func(id docid.Key, matches []termMatch) {
		s, ok := scoreDoc(terms, matches, idf)
		if !ok || s.score == 0 {
			return
		}

		results = append(results, ranked{id, s})
	})

	if top < len(results) {
		results = selectTop(results, top, orderRanked)
	}

	slices.SortFunc(results, orderRanked)

	return results
}

// selectTop keeps the first n of results in order (unsorted), with a heap
// whose root is the worst kept: O(len · log n) instead of a full sort.
func selectTop(results []ranked, n int, order func(a, b ranked) int) []ranked {
	if n <= 0 {
		return results[:0]
	}

	heap := results[:n:n]

	down := func(i int) {
		for {
			worst, l, r := i, 2*i+1, 2*i+2
			if l < n && order(heap[l], heap[worst]) > 0 {
				worst = l
			}

			if r < n && order(heap[r], heap[worst]) > 0 {
				worst = r
			}

			if worst == i {
				return
			}

			heap[i], heap[worst] = heap[worst], heap[i]
			i = worst
		}
	}

	for i := n/2 - 1; i >= 0; i-- {
		down(i)
	}

	for _, r := range results[n:] {
		if order(r, heap[0]) < 0 {
			heap[0] = r
			down(0)
		}
	}

	return heap
}
