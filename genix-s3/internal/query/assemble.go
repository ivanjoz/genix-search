package query

import (
	"fmt"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/postings"
)

// docWords is what a search knows of one document: per word it has, its
// postings.
type docWords struct {
	picks []int
	lists [][]postings.Posting
}

func (d *docWords) has(pick int) bool {
	for _, p := range d.picks {
		if p == pick {
			return true
		}
	}

	return false
}

func (d *docWords) set(pick int, list []postings.Posting) {
	if !d.has(pick) {
		d.picks = append(d.picks, pick)
		d.lists = append(d.lists, list)
	}
}

type docSet map[uint32]*docWords

func (s docSet) doc(id uint32) *docWords {
	d := s[id]
	if d == nil {
		d = &docWords{}
		s[id] = d
	}

	return d
}

// addList adds a list, sorted by ID, as the postings of pick.
func (s docSet) addList(pick int, list []postings.Posting) {
	for i := 0; i < len(list); {
		j := i + 1
		for j < len(list) && list[j].ID == list[i].ID {
			j++
		}

		s.doc(list[i].ID).set(pick, list[i:j:j])
		i = j
	}
}

// assemble turns what was read into candidates: every document of a whole
// list, a combination or a head, with the postings of each word it has; then
// p.log on top: a document with a pending op is replaced by its new version,
// matched by the same rules, or removed.
func assemble(terms []Term, p *readPlan, data *dbData, pending map[uint32]plog.Op) (*candidates, error) {
	index := make(map[*pick]int, len(p.picks))
	for i, pk := range p.picks {
		index[pk] = i
	}

	docs := docSet{}

	for i, pk := range p.picks {
		if !pk.full {
			continue
		}

		var raw []byte
		var off uint64

		if pk.w.Loc == bucket.LocDB || pk.w.Loc == bucket.LocExpanded {
			start, end := pk.w.Bytes()

			var ok bool
			if raw, off, ok = data.get(pk.x.Group, start, end); !ok {
				return nil, fmt.Errorf("query: list of %s was not read", pk.w.Key)
			}
		}

		list, err := pk.x.Postings(pk.w, raw, off, make([]postings.Posting, 0, pk.w.Shape.Count))
		if err != nil {
			return nil, err
		}

		docs.addList(i, list)
	}

	type blockKey struct {
		group uint16
		off   uint32
	}

	blocks := map[blockKey]bucket.Block{}

	for _, pr := range p.pairs {
		k := blockKey{pr.anchor.x.Group, pr.combo.Off}

		block, ok := blocks[k]
		if !ok {
			start, end := uint64(pr.combo.Off), uint64(pr.combo.Off)+uint64(pr.combo.Len)

			raw, off, ok := data.get(pr.anchor.x.Group, start, end)
			if !ok {
				return nil, fmt.Errorf("query: combinations of %s were not read", pr.anchor.w.Key)
			}

			var err error
			if block, err = bucket.DecodeBlock(raw[start-off : end-off]); err != nil {
				return nil, err
			}

			blocks[k] = block
		}

		if cols, ok := block[pr.co.w.Key]; ok {
			docs.addList(index[pr.anchor], cols[0])
			docs.addList(index[pr.co], cols[1])
		}
	}

	// A document in a head and nowhere else has only that word of the query:
	// with another, a list or a combination would have shown it.
	for i, pk := range p.picks {
		if !pk.headed() {
			continue
		}

		for _, g := range pk.w.Exp.Groups {
			for _, e := range g.Entries {
				docs.doc(e.ID).set(i, e.Postings)
			}
		}
	}

	for id := range pending {
		delete(docs, id)
	}

	cands := newCandidates(len(terms), len(docs)+len(pending))

	for id, d := range docs {
		matches := cands.get(id)

		for j, pi := range d.picks {
			for _, u := range p.picks[pi].uses {
				for _, posting := range d.lists[j] {
					matches[u.term].add(terms[u.term].resolve(u.kind, posting.Tail), u.dist, posting.Pos)
				}
			}
		}
	}

	var scratch [key5.MaxUnits]uint8

	for id, op := range pending {
		if op.Del {
			continue
		}

		tokens, err := key5.ReadTokens(op.Tokens)
		if err != nil {
			return nil, fmt.Errorf("query: p.log op %d: %w", op.Seq, err)
		}

		if matches, any := matchTokens(terms, p.fuzzy, tokens, scratch[:]); any {
			cands.put(id, matches)
		}
	}

	return cands, nil
}

// shortHeads returns the words read through their head whose head may hide a
// document that belongs in the first need results: the head's last entry,
// scored as a document with only that word, does not rank below the result
// number need (PLAN_BUCKETS.md §6.1, step 6).
func shortHeads(terms []Term, p *readPlan, idf []uint32, results []ranked, need int) []*pick {
	explicit := -1

	for i := range terms {
		if terms[i].Explicit {
			explicit = i
		}
	}

	var short []*pick

	for _, pk := range p.picks {
		if !pk.headed() {
			continue
		}

		u := pk.uses[0]

		// A document with only this word lacks the required term.
		if explicit >= 0 && u.term != explicit {
			continue
		}

		for _, b := range headBounds(&terms[u.term], u, pk.w.Exp, idf[u.term]) {
			if len(results) < need || orderRanked(results[need-1], b) >= 0 {
				short = append(short, pk)

				break
			}
		}
	}

	return short
}

// headBounds are, per head the word's term reads, the rank of its last
// entry: a document of the word that the head does not show ranks after it.
// A complete head has none.
func headBounds(t *Term, u use, exp *bucket.Expansion, idf uint32) []ranked {
	qpos := queryPositionBonus(t.Position)
	exact := wordBaseScore(t.Text) + qpos

	var out []ranked

	bound := func(g *bucket.Group, base uint32) {
		if g == nil || g.Complete() || len(g.Entries) == 0 {
			return
		}

		last := &g.Entries[len(g.Entries)-1]
		pos, _ := bucket.FirstPos(last, g.Tail)

		out = append(out, ranked{id: last.ID, score: docScore{coverage: 1, score: base + docPositionBonus(pos) + idf}})
	}

	all := exp.Group(bucket.AllTails)

	switch {
	case u.kind == prefixKey:
		bound(all, PrefixScore+qpos)
	case len(exp.Groups) > 1:
		// Documents with the query's tail rank by it; the others, stems, by
		// their first position.
		bound(exp.Group(t.Token.Tail), exact)
		bound(all, exact-StemPenalty)
	case len(all.Entries) > 0 && all.Entries[0].Postings[0].Tail == t.Token.Tail:
		bound(all, exact)
	default:
		bound(all, exact-StemPenalty)
	}

	return out
}
