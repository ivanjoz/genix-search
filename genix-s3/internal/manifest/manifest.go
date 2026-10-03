// Package manifest is the metadata of a generation (PLAN_5BIT.md §5.6). It
// is metadata, so it is colbin; the index files it points to are not.
package manifest

import (
	"fmt"

	"github.com/ivanjoz/colbin"
)

// Format is the version of the manifest struct.
const Format uint16 = 1

// Manifest describes one generation of a collection.
type Manifest struct {
	Format            uint16 `cb:"1"`
	Gen               uint32 `cb:"2"`
	LastSeq           uint64 `cb:"3"` // the last p.log seq the generation includes
	NormalizerVersion uint16 `cb:"4"`
	Extra1            int32  `cb:"5"` // the configured letters of codes 30 and 31 (0: unused)
	Extra2            int32  `cb:"6"`
	NormID            uint32 `cb:"7"`
	Docs              uint32 `cb:"8"`
	Keys              uint32 `cb:"9"`
	KSize             uint64 `cb:"10"`
	PSize             uint64 `cb:"11"`
	SnapSize          uint64 `cb:"12"`
	CreatedUnixMs     int64  `cb:"13"`
}

// Encode writes m.
func Encode(m *Manifest) ([]byte, error) {
	m.Format = Format

	return colbin.Marshal(m)
}

// Decode reads a manifest.
func Decode(data []byte) (*Manifest, error) {
	var m Manifest
	if err := colbin.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}

	if m.Format != Format {
		return nil, fmt.Errorf("manifest: format %d, this build reads %d", m.Format, Format)
	}

	return &m, nil
}
