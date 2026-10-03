// Package layout names every object of a collection in the bucket
// (PLAN_BUCKETS.md §1).
package layout

import (
	"fmt"
	"regexp"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Collection is the key prefix of one (collection, partition).
type Collection struct {
	prefix string
}

// New validates the names and builds the prefix `<root><collection>/<partition>/`.
// root is normally "genix/".
func New(root, collection, partition string) (Collection, error) {
	if !namePattern.MatchString(collection) || !namePattern.MatchString(partition) {
		return Collection{}, fmt.Errorf("layout: invalid collection %q or partition %q", collection, partition)
	}

	return Collection{prefix: root + collection + "/" + partition + "/"}, nil
}

func (c Collection) Log() string    { return c.prefix + "p.log" }
func (c Collection) Tokens() string { return c.prefix + "tokens" }
func (c Collection) Lease() string  { return c.prefix + "compact.lease" }

// Idx and DB are the objects of the bucket of a slot: "ca.idx", "ca.db".
func (c Collection) Idx(slot uint16) string { return c.prefix + BucketName(slot) + ".idx" }
func (c Collection) DB(slot uint16) string  { return c.prefix + BucketName(slot) + ".db" }

// unitChars names the 32 units: 0 for End, the letters, then 1–5 for the
// numbers and the extra letters.
const unitChars = "0abcdefghijklmnopqrstuvwxyz12345"

// BucketName is the two characters of a slot, its two leading units.
func BucketName(slot uint16) string {
	return string([]byte{unitChars[slot>>5&31], unitChars[slot&31]})
}
