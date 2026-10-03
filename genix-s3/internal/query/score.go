package query

import (
	"cmp"
	"math"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// Ranking tunables. Results are ordered by coverage first — how many of the
// query's terms the document holds — and by score within the same coverage.
// Everything is computed from keys, tails and positions: the index holds no
// text.
const (
	// Per term, by where it sits in the query and in the document.
	QueryFirstWordBonus  = 3
	QuerySecondWordBonus = 1
	DocFirstWordBonus    = 4
	DocSecondWordBonus   = 2

	// Per matched term: an exact word earns BASE + len, clamped.
	WordBaseScoreBase  = 6
	WordBaseScoreFloor = 8
	WordBaseScoreCeil  = 12

	// A word with the same key (the same first 8 letters) and another tail:
	// most likely another ending of the same word ("chocolatera" for
	// "chocolate"). It covers the term, below the exact word.
	StemPenalty = 2

	// A term matched as a prefix (the open last word, or `foo*`).
	PrefixScore = 5

	// A term matched with a typo. Must stay below WordBaseScoreFloor, so a
	// typo never outranks the exact word.
	FuzzyScoreMax = 6

	// Per combination of two matched terms, by how close they sit in the
	// document and whether they keep the query's order.
	PairAdjacent         = 5
	PairAdjacentReversed = 3
	PairNear             = 2
	PairNearReversed     = 1
	PairFar              = 1

	// Every term matched exactly, consecutive and in the query's order.
	PhraseBonus = 5

	// A matched term also earns its rarity: round(IDFWeight · ln(N/df)),
	// at most IDFMax.
	IDFWeight = 1.5
	IDFMax    = 15
)

type matchKind uint8

// Better kinds are smaller.
const (
	noMatch matchKind = iota
	exactMatch
	stemMatch
	prefixMatch
	fuzzyMatch
)

// keyKind is how a key matches a term, before the tail is known: exactKey
// becomes exactMatch or stemMatch by the tail of each posting.
type keyKind uint8

const (
	noKey keyKind = iota
	exactKey
	prefixKey
	fuzzyKey
)

// classifyKey says how a dictionary key matches term t. fuzzy enables the
// typo match for this term (the engine enables it only for terms the
// dictionary does not have).
func (t *Term) classifyKey(k key5.Key, fuzzy bool, scratch []uint8) (keyKind, uint8) {
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

	if fuzzy && !t.Explicit && !t.Number && !k.IsNumber() {
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
	case fuzzyKey:
		return fuzzyMatch
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
	case kind == fuzzyMatch:
		if dist < m.dist || (dist == m.dist && pos < m.positions[0]) {
			m.dist, m.positions[0] = dist, pos
		}
	default:
		m.positions = append(m.positions, pos)
	}
}

// docScore ranks a document: coverage first, then score.
type docScore struct {
	coverage uint32
	score    uint32
	matched  uint32 // bit i: term i matched
}

// scoreDoc scores a document from its matches, one per term. ok is false
// when a required explicit prefix is missing. The rarity bonus is added
// afterwards, once the query knows how many documents matched each term.
func scoreDoc(terms []Term, matches []termMatch) (docScore, bool) {
	var s docScore

	for i := range terms {
		m := &matches[i]
		if m.kind == noMatch {
			if terms[i].Explicit {
				return docScore{}, false
			}

			continue
		}

		s.coverage++
		s.score += termScore(&terms[i], m)
		s.matched |= 1 << i
	}

	for i := range terms {
		for j := i + 1; j < len(terms); j++ {
			if matches[i].kind == noMatch || matches[j].kind == noMatch {
				continue
			}

			distance, ordered := closest(matches[i].positions, matches[j].positions)
			anyFuzzy := matches[i].kind == fuzzyMatch || matches[j].kind == fuzzyMatch
			s.score += pairBonus(distance, ordered, anyFuzzy)
		}
	}

	if len(terms) > 1 && isPhrase(matches) {
		s.score += PhraseBonus
	}

	return s, true
}

func termScore(t *Term, m *termMatch) uint32 {
	first := slices.Min(m.positions)

	switch m.kind {
	case exactMatch:
		return wordBaseScore(t.Text) + queryPositionBonus(t.Position) + docPositionBonus(first)
	case stemMatch:
		return wordBaseScore(t.Text) - StemPenalty + queryPositionBonus(t.Position) + docPositionBonus(first)
	case prefixMatch:
		return PrefixScore + queryPositionBonus(t.Position) + docPositionBonus(first)
	case fuzzyMatch:
		return fuzzyScore(len(t.Text), int(m.dist))
	}

	return 0
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

// isPhrase reports whether every term matched without a typo, at
// consecutive positions in the query's order.
func isPhrase(matches []termMatch) bool {
	for _, m := range matches {
		if m.kind == noMatch || m.kind == fuzzyMatch {
			return false
		}
	}

	for _, start := range matches[0].positions {
		consecutive := true

		for k := 1; k < len(matches) && consecutive; k++ {
			consecutive = slices.Contains(matches[k].positions, start+uint8(k))
		}

		if consecutive {
			return true
		}
	}

	return false
}

func (s docScore) withIDF(idf []uint32) docScore {
	for i, bonus := range idf {
		if s.matched&(1<<i) != 0 {
			s.score += bonus
		}
	}

	return s
}

// compareScores is the result order: coverage and score descending, then ID.
func compareScores(a, b docScore, aID, bID uint32) int {
	return cmp.Or(
		cmp.Compare(b.coverage, a.coverage),
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

func wordBaseScore(token string) uint32 {
	return min(max(WordBaseScoreBase+uint32(len(token)), WordBaseScoreFloor), WordBaseScoreCeil)
}

func queryPositionBonus(position int) uint32 {
	switch position {
	case 0:
		return QueryFirstWordBonus
	case 1:
		return QuerySecondWordBonus
	}

	return 0
}

func docPositionBonus(pos uint8) uint32 {
	switch pos {
	case 0:
		return DocFirstWordBonus
	case 1:
		return DocSecondWordBonus
	}

	return 0
}

func pairBonus(distance int, ordered, fuzzy bool) uint32 {
	var bonus uint32

	switch {
	case distance == 1 && ordered:
		bonus = PairAdjacent
	case distance == 1:
		bonus = PairAdjacentReversed
	case distance == 2 && ordered:
		bonus = PairNear
	case distance == 2:
		bonus = PairNearReversed
	default:
		bonus = PairFar
	}

	if fuzzy {
		bonus /= 2
	}

	return bonus
}

func fuzzyScore(length, distance int) uint32 {
	return uint32(FuzzyScoreMax * (length - distance) / length)
}

// Ranked is a scored document.
type ranked struct {
	id    uint32
	score docScore
}

// rankAll scores every candidate, adds the rarity bonus and returns the
// first top in order. df of a term is the number of candidates that matched
// it, unless dfFloor says the read was cut and the dictionary knows more.
func rankAll(terms []Term, cands *candidates, docs int, dfFloor []int, top int) []ranked {
	results := make([]ranked, 0, cands.len())
	df := make([]int, len(terms))

	cands.each(func(id uint32, matches []termMatch) {
		s, ok := scoreDoc(terms, matches)
		if !ok || s.coverage == 0 {
			return
		}

		results = append(results, ranked{id, s})

		for i := range df {
			if s.matched&(1<<i) != 0 {
				df[i]++
			}
		}
	})

	idf := make([]uint32, len(terms))
	for i := range idf {
		if dfFloor != nil {
			df[i] = max(df[i], dfFloor[i])
		}

		idf[i] = idfBonus(df[i], docs)
	}

	for i := range results {
		results[i].score = results[i].score.withIDF(idf)
	}

	order := func(a, b ranked) int { return compareScores(a.score, b.score, a.id, b.id) }

	if top < len(results) {
		results = selectTop(results, top, order)
	}

	slices.SortFunc(results, order)

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
