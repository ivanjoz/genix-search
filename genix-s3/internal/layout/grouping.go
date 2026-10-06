package layout

import (
	"fmt"
	"strings"
)

// slots is the number of slots, key5.Slots: every pair of leading units.
const slots = 1024

// Grouping maps every slot to its group: the .idx/.db pair that holds it.
// A group is a run of consecutive slots. It is fixed
// in the code and never learned from a collection: a query names the objects
// it reads from the query alone.
type Grouping struct {
	id   uint8
	name string
	// first is the first slot of every group, ascending, starting at 0; nil
	// for one group per slot.
	first []uint16
	of    [slots]uint16
}

var (
	// GroupingNone is one group per slot: up to 1024 .idx, as small as they
	// get, so a cold query reads the fewest bytes.
	GroupingNone = &Grouping{id: 0, name: "none"}
	// GroupingES100 is 100 groups, cut so that each weighs about the same in
	// Spanish: the frequent initial pairs (ca, co, de, pa, pr…) alone, the
	// rare ones together (vo-y0 holds w and x, ya-z5 y and z), and a fifth of
	// the weight for numbers. Up to 200 objects instead of 2048; a cold query
	// reads about twice the .idx bytes.
	GroupingES100 = mustGrouping(1, "es100", es100)
)

// es100 is the first slot of each group of GroupingES100. It was cut from
// the word frequencies of a Spanish corpus (OpenSubtitles 2018, the 50 000
// most frequent words: half the share of distinct keys of each slot, half
// the share of occurrences) for the letters, and from 1/(n+1) for the
// numbers, with a fifth of the weight. TestES100 rebuilds it.
const es100 = "00 ad ai am ap ar as au ba bh br ca cb ci co cp cu da de df di dj ed en eo es et fa fl ga gr " +
	"ha hb ho ig in io ju la li lo ma mb mi mo mu na no oc os pa pb pe pf po pr ps qu ra re rf sa sb se sf so st " +
	"ta te tf to tr tu un uo ve vi vo ya 10 1a 1b 1d 1g 1m 1x 2a 2b 2c 2e 2i 2p 23 3a 3b 3c 3e 3i 3p 33"

func mustGrouping(id uint8, name, firsts string) *Grouping {
	g := &Grouping{id: id, name: name}

	for _, f := range strings.Fields(firsts) {
		slot, ok := slotOf(f)
		if !ok || (len(g.first) == 0) != (slot == 0) || len(g.first) > 0 && slot <= g.first[len(g.first)-1] {
			panic(fmt.Sprintf("layout: grouping %s: bad first slot %q", name, f))
		}

		g.first = append(g.first, slot)
	}

	for i, lo := range g.first {
		hi := uint16(slots)
		if i+1 < len(g.first) {
			hi = g.first[i+1]
		}

		for s := lo; s < hi; s++ {
			g.of[s] = uint16(i)
		}
	}

	return g
}

// GroupingByID is the grouping an object says it was written with.
func GroupingByID(id uint8) (*Grouping, error) {
	for _, g := range []*Grouping{GroupingNone, GroupingES100} {
		if g.id == id {
			return g, nil
		}
	}

	return nil, fmt.Errorf("layout: unknown grouping %d: written by a newer genix-s3", id)
}

// GroupingByName is the grouping called name: "none" or "es100".
func GroupingByName(name string) (*Grouping, error) {
	for _, g := range []*Grouping{GroupingNone, GroupingES100} {
		if g.name == name {
			return g, nil
		}
	}

	return nil, fmt.Errorf("layout: unknown grouping %q: none or es100", name)
}

// ID is what the objects record.
func (g *Grouping) ID() uint8 { return g.id }

func (g *Grouping) String() string { return g.name }

// Groups is the number of groups.
func (g *Grouping) Groups() int {
	if g.first == nil {
		return slots
	}

	return len(g.first)
}

// Of is the group of a slot.
func (g *Grouping) Of(slot uint16) uint16 {
	if g.first == nil {
		return slot
	}

	return g.of[slot&(slots-1)]
}

// Slots is the first and the last slot of a group.
func (g *Grouping) Slots(group uint16) (lo, hi uint16) {
	if g.first == nil {
		return group, group
	}

	lo, hi = g.first[group], slots-1
	if int(group)+1 < len(g.first) {
		hi = g.first[group+1] - 1
	}

	return lo, hi
}

// Name is the name of a group's objects: its slot, or its first and last.
func (g *Grouping) Name(group uint16) string {
	lo, hi := g.Slots(group)
	if lo == hi {
		return BucketName(lo)
	}

	return BucketName(lo) + "-" + BucketName(hi)
}
