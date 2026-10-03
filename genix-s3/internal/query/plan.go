package query

import (
	"cmp"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// use is one term a word matches, and how.
type use struct {
	term int
	kind keyKind
	dist uint8
}

// pick is a word the query reads, with every term it serves.
type pick struct {
	slot uint16
	x    *bucket.Index
	w    *bucket.Word
	uses []use
	// full: the query reads the word's whole list. Always for a word that is
	// not expanded; for an expanded one, when its head cannot stand for it.
	full bool
}

func (p *pick) expanded() bool { return p.w.Loc == bucket.LocExpanded }

// headed reports an expanded word read through its head and combinations.
func (p *pick) headed() bool { return p.expanded() && !p.full }

// pairRead is a combination the query reads: the block of the anchor's .db
// with the lists of anchor and co over the documents with both.
type pairRead struct {
	anchor, co *pick
	combo      bucket.Combo
}

// readPlan is what a query reads, and what it knows before reading.
type readPlan struct {
	picks  []*pick
	byKey  map[key5.Key]*pick
	pairs  []pairRead
	fuzzy  []bool // per term: typos enabled (the dictionary does not have it)
	approx bool   // a prefix or a typo expansion was cut by the budget
	df     []int  // per term: documents of every key that matches it
}

// indexSource gives the decoded bucket of a slot; nil for an empty one.
type indexSource func(slot uint16) *bucket.Index

// termSlots are the buckets a term needs: its own (exact key and typos) and
// those of its prefix ranges.
func termSlots(t *Term) []uint16 {
	slots := []uint16{t.Token.Key.Slot()}

	for _, r := range t.prefixRanges() {
		for s := r.lo.Slot(); s <= r.hi.Slot(); s++ {
			slots = append(slots, s)
		}
	}

	return slots
}

// querySlots are the buckets of every term, sorted and distinct.
func querySlots(terms []Term) []uint16 {
	var slots []uint16
	for i := range terms {
		slots = append(slots, termSlots(&terms[i])...)
	}

	slices.Sort(slots)

	return slices.Compact(slots)
}

// candidate is a word a term could read.
type candidate struct {
	slot uint16
	x    *bucket.Index
	w    *bucket.Word
	dist uint8
}

// cost is the bytes of .db a word costs when read whole: lists held in the
// .idx cost nothing, and an expanded word is read through its head.
func (c candidate) cost() int64 {
	if c.w.Loc != bucket.LocDB {
		return 0
	}

	off, end := c.w.Bytes()

	return int64(end - off)
}

// planQuery selects the words of every term — its exact key, its prefix
// ranges within the budget, typos when enabled and the term has neither —
// and the combinations to read (PLAN_BUCKETS.md §6.1).
func planQuery(terms []Term, source indexSource, budget int64, fuzzy bool) *readPlan {
	p := &readPlan{byKey: map[key5.Key]*pick{}, fuzzy: make([]bool, len(terms)), df: make([]int, len(terms))}

	for i := range terms {
		t := &terms[i]
		found := false

		if !t.Explicit {
			if x := source(t.Token.Key.Slot()); x != nil {
				if w, ok := x.Find(t.Token.Key); ok {
					p.add(candidate{slot: t.Token.Key.Slot(), x: x, w: w}, use{term: i, kind: exactKey})
					p.df[i] += int(w.DF)
					found = true
				}
			}
		}

		var prefix []candidate

		if t.Auto || t.Explicit {
			if t.prefixable() {
				for _, r := range t.prefixRanges() {
					for slot := r.lo.Slot(); slot <= r.hi.Slot(); slot++ {
						x := source(slot)
						if x == nil {
							continue
						}

						in := x.Range(r.lo, r.hi)
						for j := range in {
							if w := &in[j]; w.Key != t.Token.Key || t.Explicit {
								prefix = append(prefix, candidate{slot: slot, x: x, w: w})
							}
						}
					}
				}
			} else if x := source(t.Token.Key.Slot()); x != nil && t.Explicit {
				if w, ok := x.Find(t.Token.Key); ok {
					prefix = append(prefix, candidate{slot: t.Token.Key.Slot(), x: x, w: w})
				}
			}
		}

		if len(prefix) > 0 {
			for _, c := range prefix {
				p.df[i] += int(c.w.DF)
			}

			if p.within(prefix, budget, i, prefixKey, false, !found) {
				p.approx = true
			}

			found = true
		}

		if found || !fuzzy || t.Explicit || t.Number || fuzzyBudget(len(t.Text)) == 0 {
			continue
		}

		p.fuzzy[i] = true

		// Typos only past the first two letters: the term's own bucket.
		x := source(t.Token.Key.Slot())
		if x == nil {
			continue
		}

		var near []candidate
		var scratch [key5.MaxUnits]uint8

		for j := range x.Words {
			w := &x.Words[j]
			if kind, d := t.classifyKey(w.Key, true, scratch[:]); kind == fuzzyKey {
				near = append(near, candidate{slot: t.Token.Key.Slot(), x: x, w: w, dist: d})
				p.df[i] += int(w.DF)
			}
		}

		if p.within(near, budget, i, fuzzyKey, true, true) {
			p.approx = true
		}
	}

	// A head stands for an expanded word only for one term, matched exactly
	// or as a prefix: its order is the score of that term alone.
	for _, pk := range p.picks {
		pk.full = !pk.expanded() || len(pk.uses) > 1 || pk.uses[0].kind == fuzzyKey
	}

	p.planPairs()

	return p
}

// within adds the words of an expansion while the bytes to read fit the
// budget: typos by edit distance first, then the most frequent words. Words
// that cost nothing are always taken. With force (the term has no exact
// word) the best word is taken even over the budget: the term then reads
// what an exact word would. It reports whether any word was left out.
func (p *readPlan) within(words []candidate, budget int64, term int, kind keyKind, byDist, force bool) bool {
	slices.SortStableFunc(words, func(a, b candidate) int {
		if byDist && a.dist != b.dist {
			return cmp.Compare(a.dist, b.dist)
		}

		return cmp.Compare(b.w.Shape.Count, a.w.Shape.Count)
	})

	var spent int64
	cut := false

	for _, c := range words {
		cost := c.cost()

		if cost > 0 && spent+cost > budget && !(force && spent == 0) {
			cut = true

			continue
		}

		spent += cost
		p.add(c, use{term: term, kind: kind, dist: c.dist})
	}

	return cut
}

func (p *readPlan) add(c candidate, u use) {
	pk, ok := p.byKey[c.w.Key]
	if !ok {
		pk = &pick{slot: c.slot, x: c.x, w: c.w}
		p.byKey[c.w.Key] = pk
		p.picks = append(p.picks, pk)
	}

	for _, have := range pk.uses {
		if have.term == u.term {
			return
		}
	}

	pk.uses = append(pk.uses, u)
}

// planPairs lists the combinations to read: every pair of words where one is
// read through its head. Then a document with two words or more is seen
// whole, from a list or a combination; one with only an expanded word is in
// its head or below the head's last entry.
func (p *readPlan) planPairs() {
	for i, a := range p.picks {
		for _, b := range p.picks[i+1:] {
			if !a.headed() && !b.headed() {
				continue
			}

			// Kept under the expanded word, or the smaller key if both are.
			anchor, co := a, b
			if !a.expanded() || (b.expanded() && b.w.Key < a.w.Key) {
				anchor, co = b, a
			}

			if combo, ok := anchor.w.Exp.Combo(co.w.Key); ok {
				p.pairs = append(p.pairs, pairRead{anchor: anchor, co: co, combo: combo})
			}
		}
	}
}
