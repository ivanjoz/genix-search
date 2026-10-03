// Package layout names every object of a collection in the bucket
// (PLAN_5BIT.md §5.0).
package layout

import (
	"fmt"
	"regexp"
)

var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)

// Collection is the key prefix of one (collection, bucket).
type Collection struct {
	prefix string
}

// New validates the names and builds the prefix `<root><collection>/<bucket>/`.
// root is normally "genix/".
func New(root, collection, bucket string) (Collection, error) {
	if !namePattern.MatchString(collection) || !namePattern.MatchString(bucket) {
		return Collection{}, fmt.Errorf("layout: invalid collection %q or bucket %q", collection, bucket)
	}

	return Collection{prefix: root + collection + "/" + bucket + "/"}, nil
}

func (c Collection) Log() string                { return c.prefix + "p.log" }
func (c Collection) Manifest(gen uint32) string { return fmt.Sprintf("%sm/%08d.cbm", c.prefix, gen) }
func (c Collection) Snapshot(gen uint32) string {
	return fmt.Sprintf("%ssnap/%08d.snap", c.prefix, gen)
}
func (c Collection) K(gen uint32) string { return fmt.Sprintf("%sk.%08d", c.prefix, gen) }
func (c Collection) P(gen uint32) string { return fmt.Sprintf("%sp.%08d", c.prefix, gen) }

// Objects are the data objects of a generation, besides its manifest.
func (c Collection) Objects(gen uint32) []string {
	return []string{c.K(gen), c.P(gen), c.Snapshot(gen)}
}
