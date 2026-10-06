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

// pick is a word the query reads, with every term it serves. x.Group is the
// bucket, the objects it is read from.
type pick struct {
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

// indexSource gives the decoded bucket that holds a slot; nil for an empty
// one. Several slots may share a bucket.
type indexSource func(slot uint16) *bucket.Index

// termSlots are the buckets a term needs: its own (exact key and typos), or
// those of its host words for a one-letter term, and those of its prefix
// ranges.
func termSlots(t *Term) []uint16 {
	var slots []uint16
	for _, k := range t.exactKeys() {
		slots = append(slots, k.Slot())
	}

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
// and the combinations to read.
func planQuery(terms []Term, source indexSource, budget int64, fuzzy bool) *readPlan {
	p := &readPlan{byKey: map[key5.Key]*pick{}, fuzzy: make([]bool, len(terms)), df: make([]int, len(terms))}

	for i := range terms {
		t := &terms[i]
		found := false

		if t.virtual() {
			p.addSignature(t, i, source, budget)

			continue
		}

		p.addGroupWords(t, i, source, budget)

		if !t.Explicit {
			for _, k := range t.exactKeys() {
				if x := source(k.Slot()); x != nil {
					if w, ok := x.Find(k); ok {
						p.add(candidate{x: x, w: w}, use{term: i, kind: exactKey})
						p.df[i] += int(w.DF)
						found = true
					}
				}
			}
		}

		var prefix []candidate

		if t.Auto || t.Explicit {
			if t.prefixable() {
				for _, r := range t.prefixRanges() {
					// The slots of a range may share a bucket: each is read once.
					var seen []*bucket.Index

					for slot := r.lo.Slot(); slot <= r.hi.Slot(); slot++ {
						x := source(slot)
						if x == nil || slices.Contains(seen, x) {
							continue
						}

						seen = append(seen, x)

						in := x.Range(r.lo, r.hi)
						for j := range in {
							if w := &in[j]; (w.Key != t.Token.Key || t.Explicit) && !w.Key.IsContinuation() {
								prefix = append(prefix, candidate{x: x, w: w})
							}
						}
					}
				}
			} else if x := source(t.Token.Key.Slot()); x != nil && t.Explicit {
				if w, ok := x.Find(t.Token.Key); ok {
					prefix = append(prefix, candidate{x: x, w: w})
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

		if p.addNear(t, i, source, budget, !found) {
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
				near = append(near, candidate{x: x, w: w, dist: d})
				p.df[i] += int(w.DF)
			}
		}

		if p.within(near, budget, i, fuzzyKey, true, true) {
			p.approx = true
		}
	}

	// A head stands for an expanded word only for one term, matched exactly
	// or as a prefix: its order is the score of that term alone.
	// So does it for a near word of under 8 letters: its postings all match
	// alike, as an exact word's do.
	for _, pk := range p.picks {
		u := pk.uses[0]
		pk.full = !pk.expanded() || len(pk.uses) > 1 || u.kind == fuzzyKey || (u.kind.near() && pk.w.Key.Len() >= key5.MaxUnits)
	}

	p.planPairs()

	return p
}

// addSignature adds the signatures of a virtual term: its own, and with 2
// segments or more the longer ones that start with it, within the budget.
// They are in the slot of the run's first word.
func (p *readPlan) addSignature(t *Term, term int, source indexSource, budget int64) {
	x := source(t.Sig.Key.Slot())
	if x == nil {
		return
	}

	if w, ok := x.Find(t.Sig.Key); ok {
		p.add(candidate{x: x, w: w}, use{term: term, kind: exactKey})
	}

	var longer []candidate

	for _, r := range t.prefixRanges() {
		in := x.Range(r.lo, r.hi)
		for j := range in {
			if w := &in[j]; w.Key != t.Sig.Key {
				longer = append(longer, candidate{x: x, w: w})
			}
		}
	}

	if p.within(longer, budget, term, prefixKey, false, false) {
		p.approx = true
	}
}

// addNear adds the near words of a term, those its
// exact key and its prefix ranges do not already match, within the budget,
// closest first. They are in the term's own bucket, read anyway: they cost
// no request. It reports whether there was any.
func (p *readPlan) addNear(t *Term, term int, source indexSource, budget int64, force bool) bool {
	if t.virtual() || t.letter() {
		return false
	}

	x := source(t.Token.Key.Slot())
	if x == nil {
		return false
	}

	var longer, shorter []candidate
	var scratch [key5.MaxUnits]uint8

	for j := range x.Words {
		w := &x.Words[j]

		switch kind, d := t.classifyKey(w.Key, false, scratch[:]); kind {
		case longerKey:
			longer = append(longer, candidate{x: x, w: w, dist: d})
		case shorterKey:
			shorter = append(shorter, candidate{x: x, w: w, dist: d})
		default:
			continue
		}

		p.df[term] += int(w.DF)
	}

	if len(longer) > 0 && p.within(longer, budget, term, longerKey, true, force) {
		p.approx = true
	}

	if len(shorter) > 0 && p.within(shorter, budget, term, shorterKey, true, force) {
		p.approx = true
	}

	return len(longer)+len(shorter) > 0
}

// addGroupWords adds the group words a term matches: its own, and as a
// prefix those of its prefix ranges, within the budget. They do not count
// in its df: a group word is not a word of the text.
func (p *readPlan) addGroupWords(t *Term, term int, source indexSource, budget int64) {
	if k, ok := t.groupKey(); ok {
		if x := source(k.Slot()); x != nil {
			if w, ok := x.Find(k); ok {
				p.add(candidate{x: x, w: w}, use{term: term, kind: groupKey})
			}
		}
	}

	var prefix []candidate

	for _, r := range t.groupRanges() {
		var seen []*bucket.Index

		for slot := r.lo.Slot(); slot <= r.hi.Slot(); slot++ {
			x := source(slot)
			if x == nil || slices.Contains(seen, x) {
				continue
			}

			seen = append(seen, x)

			in := x.Range(r.lo, r.hi)
			for j := range in {
				if w := &in[j]; t.Explicit || w.Key != t.Token.Key.In(key5.SpaceGroupWord) {
					prefix = append(prefix, candidate{x: x, w: w})
				}
			}
		}
	}

	if p.within(prefix, budget, term, groupKey, false, false) {
		p.approx = true
	}
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
		pk = &pick{x: c.x, w: c.w}
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
