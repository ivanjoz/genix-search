package query

import (
	"cmp"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/index"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// use is one term an entry matches, and how.
type use struct {
	term int
	kind keyKind
	dist uint8
}

// selected is a dictionary entry the query reads, with every term it serves.
type selected struct {
	entry *index.Entry
	uses  []use
}

// readPlan is what a query reads from p, and what it knows about it.
type readPlan struct {
	entries []*selected
	byKey   map[key5.Key]*selected
	fuzzy   []bool // per term: typos enabled (the dictionary does not have it)
	approx  bool   // a prefix or a typo expansion was cut by the budget
	dfFloor []int  // per term: postings in the dictionary for the selected entries
}

func newPlan(terms int) *readPlan {
	return &readPlan{byKey: map[key5.Key]*selected{}, fuzzy: make([]bool, terms), dfFloor: make([]int, terms)}
}

func (p *readPlan) add(e *index.Entry, u use) {
	s, ok := p.byKey[e.Key]
	if !ok {
		s = &selected{entry: e}
		p.byKey[e.Key] = s
		p.entries = append(p.entries, s)
	}

	for _, have := range s.uses {
		if have.term == u.term {
			return
		}
	}

	s.uses = append(s.uses, u)
}

// slotSource gives the decoded slots of the generation.
type slotSource func(s uint16) *index.Slot

// termSlots are the slots a term needs for its exact key and its prefix.
func termSlots(t *Term) []uint16 {
	slots := []uint16{t.Token.Key.Slot()}

	for _, r := range t.prefixRanges() {
		for s := r.lo.Slot(); s <= r.hi.Slot(); s++ {
			slots = append(slots, s)
		}
	}

	return slots
}

// fuzzySlots are the slots of the words one edit away from the term in its
// first two letters: transposition, deletion, insertion and substitution
// (PLAN_5BIT.md §7.4). The term's own slot is included.
func fuzzySlots(t *Term) []uint16 {
	u := t.Units
	if t.Number || len(u) < 2 {
		return nil
	}

	at := func(i int) uint8 {
		if i < len(u) {
			return u[i]
		}

		return key5.End
	}

	pair := func(a, b uint8) uint16 { return uint16(a)<<5 | uint16(b) }

	set := map[uint16]bool{
		pair(u[0], u[1]):  true,
		pair(u[1], u[0]):  true, // transposition of 1 and 2
		pair(u[1], at(2)): true, // first letter deleted
		pair(u[0], at(2)): true, // second letter deleted, or 2 and 3 transposed
	}

	for c := uint8(1); c < 32; c++ {
		if c >= key5.Num1 && c <= key5.Num3 {
			continue
		}

		set[pair(c, u[1])] = true // first letter substituted
		set[pair(u[0], c)] = true // second letter substituted, or one inserted there
		set[pair(c, u[0])] = true // a letter inserted before the first
	}

	slots := make([]uint16, 0, len(set))
	for s := range set {
		slots = append(slots, s)
	}

	slices.Sort(slots)

	return slots
}

// planTerms selects the entries of every term: its exact key, its prefix
// range within the budget, and typos when enabled and the term is missing.
// The second result lists the slots fuzzy matching still needs; the caller
// loads them and plans again.
func planTerms(terms []Term, slot slotSource, budget int64, fuzzy bool) (*readPlan, []uint16) {
	p := newPlan(len(terms))
	var need []uint16

	for i := range terms {
		t := &terms[i]
		found := false

		if !t.Explicit {
			if s := slot(t.Token.Key.Slot()); s != nil {
				if e, ok := s.Find(t.Token.Key); ok {
					p.add(e, use{term: i, kind: exactKey})
					p.dfFloor[i] += int(e.Count)
					found = true
				}
			}
		}

		var prefix []*index.Entry

		if t.Auto || t.Explicit {
			if t.prefixable() {
				for _, r := range t.prefixRanges() {
					for sid := r.lo.Slot(); sid <= r.hi.Slot(); sid++ {
						s := slot(sid)
						if s == nil {
							continue
						}

						in := s.Range(r.lo, r.hi)
						for j := range in {
							if e := &in[j]; e.Key != t.Token.Key || t.Explicit {
								prefix = append(prefix, e)
							}
						}
					}
				}
			} else if s := slot(t.Token.Key.Slot()); s != nil && t.Explicit {
				if e, ok := s.Find(t.Token.Key); ok {
					prefix = append(prefix, e)
				}
			}
		}

		if len(prefix) > 0 {
			if p.within(prefix, budget, i, prefixKey, nil, !found) {
				p.approx = true
			}

			found = true
		}

		if found || !fuzzy || t.Explicit || t.Number || fuzzyBudget(len(t.Text)) == 0 {
			continue
		}

		p.fuzzy[i] = true

		var near []*index.Entry
		var dists []uint8
		var scratch [key5.MaxUnits]uint8

		for _, sid := range fuzzySlots(t) {
			s := slot(sid)
			if s == nil {
				need = append(need, sid)

				continue
			}

			for j := range s.Entries {
				e := &s.Entries[j]
				if kind, d := t.classifyKey(e.Key, true, scratch[:]); kind == fuzzyKey {
					near = append(near, e)
					dists = append(dists, d)
				}
			}
		}

		if p.within(near, budget, i, fuzzyKey, dists, true) {
			p.approx = true
		}
	}

	return p, need
}

// within adds the entries of an expansion while the bytes to read fit the
// budget: typos by edit distance first, then the most frequent keys. Entries
// held in the dictionary (Count 1) cost nothing and are always taken. With
// force (the term has no exact key) the best entry is taken even over the
// budget: the term then reads what an exact word would. It reports whether
// any entry was left out.
func (p *readPlan) within(entries []*index.Entry, budget int64, term int, kind keyKind, dists []uint8, force bool) bool {
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}

	slices.SortStableFunc(order, func(a, b int) int {
		if dists != nil && dists[a] != dists[b] {
			return int(dists[a]) - int(dists[b])
		}

		return cmp.Compare(entries[b].Count, entries[a].Count)
	})

	var spent int64
	cut := false

	for _, i := range order {
		e := entries[i]
		cost := int64((e.Bits + 7) / 8)

		if cost > 0 && spent+cost > budget && !(force && spent == 0) {
			cut = true

			continue
		}

		spent += cost

		var d uint8
		if dists != nil {
			d = dists[i]
		}

		p.add(e, use{term: term, kind: kind, dist: d})
		p.dfFloor[term] += int(e.Count)
	}

	return cut
}
