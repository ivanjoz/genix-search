package query

import "github.com/ivanjoz/genix-search/genix-s3/internal/docid"

// candidates holds the matches of every document a search saw, in slabs:
// one map lookup per posting and no allocation per document.
type candidates struct {
	terms   int
	index   map[docid.Key]int32
	ids     []docid.Key
	removed []bool
	matches []termMatch // terms per document
	pos     []uint8     // the first posBuf positions of each match
}

// posBuf positions per match live in the slab; more grow their own slice.
const posBuf = 4

func newCandidates(terms, hint int) *candidates {
	return &candidates{
		terms:   terms,
		index:   make(map[docid.Key]int32, hint),
		ids:     make([]docid.Key, 0, hint),
		removed: make([]bool, 0, hint),
		matches: make([]termMatch, 0, hint*terms),
		pos:     make([]uint8, 0, hint*terms*posBuf),
	}
}

// get returns the matches of id, adding the document if it is new.
func (c *candidates) get(id docid.Key) []termMatch {
	if i, ok := c.index[id]; ok && !c.removed[i] {
		return c.matches[int(i)*c.terms : int(i+1)*c.terms]
	}

	return c.add(id)
}

func (c *candidates) add(id docid.Key) []termMatch {
	i := len(c.ids)
	c.index[id] = int32(i)
	c.ids = append(c.ids, id)
	c.removed = append(c.removed, false)

	at := len(c.pos)
	c.pos = append(c.pos, make([]uint8, c.terms*posBuf)...)

	for t := range c.terms {
		buf := c.pos[at+t*posBuf : at+t*posBuf : at+(t+1)*posBuf]
		c.matches = append(c.matches, termMatch{positions: buf})
	}

	return c.matches[i*c.terms : (i+1)*c.terms]
}

// remove drops id.
func (c *candidates) remove(id docid.Key) {
	if i, ok := c.index[id]; ok {
		c.removed[i] = true
		delete(c.index, id)
	}
}

// put replaces the matches of id.
func (c *candidates) put(id docid.Key, matches []termMatch) {
	c.remove(id)
	copy(c.add(id), matches)
}

// len is the number of documents.
func (c *candidates) len() int { return len(c.index) }

// each calls fn for every document.
func (c *candidates) each(fn func(id docid.Key, matches []termMatch)) {
	for i, id := range c.ids {
		if !c.removed[i] {
			fn(id, c.matches[i*c.terms:(i+1)*c.terms])
		}
	}
}
