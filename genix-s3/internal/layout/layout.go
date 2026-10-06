// Package layout names every object of a collection in the bucket,
// and groups the 1024 slots into the objects that hold
// them.
package layout

import (
	"fmt"
	"regexp"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Collection is the key prefix of one (collection, partition) and the
// grouping its buckets are written with.
type Collection struct {
	prefix   string
	grouping *Grouping
}

// New validates the names and builds the prefix `<root><collection>/<partition>/`.
// root is normally "genix/". The grouping is GroupingNone; With changes it.
func New(root, collection, partition string) (Collection, error) {
	if !namePattern.MatchString(collection) || !namePattern.MatchString(partition) {
		return Collection{}, fmt.Errorf("layout: invalid collection %q or partition %q", collection, partition)
	}

	return Collection{prefix: root + collection + "/" + partition + "/"}, nil
}

// With returns the collection with its buckets grouped by g.
func (c Collection) With(g *Grouping) Collection {
	c.grouping = g
	return c
}

// Grouping is how the slots of the collection are grouped into objects.
func (c Collection) Grouping() *Grouping {
	if c.grouping == nil {
		return GroupingNone
	}

	return c.grouping
}

func (c Collection) Log() string    { return c.prefix + "p.log" }
func (c Collection) Tokens() string { return c.prefix + "tokens" }
func (c Collection) Lease() string  { return c.prefix + "compact.lease" }

// Idx and DB are the objects of a group: "ca.idx" and "ca.db" for a group of
// one slot, "vo-y0.idx" for the slots vo through y0. With GroupingNone the
// group is the slot.
func (c Collection) Idx(group uint16) string { return c.prefix + c.Grouping().Name(group) + ".idx" }
func (c Collection) DB(group uint16) string  { return c.prefix + c.Grouping().Name(group) + ".db" }

// unitChars names the 32 units: 0 for End, the letters, then 1–5 for the
// numbers and the extra letters.
const unitChars = "0abcdefghijklmnopqrstuvwxyz12345"

// BucketName is the two characters of a slot, its two leading units.
func BucketName(slot uint16) string {
	return string([]byte{unitChars[slot>>5&31], unitChars[slot&31]})
}

// slotOf is the slot of a bucket name.
func slotOf(name string) (uint16, bool) {
	if len(name) != 2 {
		return 0, false
	}

	var slot uint16

	for i := range 2 {
		u := -1

		for j := range len(unitChars) {
			if unitChars[j] == name[i] {
				u = j
			}
		}

		if u < 0 {
			return 0, false
		}

		slot = slot<<5 | uint16(u)
	}

	return slot, true
}
