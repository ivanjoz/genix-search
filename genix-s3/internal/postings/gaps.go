package postings

import (
	"fmt"
	"math"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
)

// A list is sorted by document key, origin<<32 | ID, so it is a run per
// origin, and a gap is the difference of the keys. The gap to the next run is
// large; three ways to write the gaps, the smallest wins:
//
//   - CodecFixed, plain: every gap in W bits, W that of the largest gap.
//   - CodecFixed with escapes: W is that of the largest gap inside a run,
//     plus one; its all-ones value is an escape, followed by the origins
//     skipped (Δorigin − 1) and the ID in the new origin, both nibble varints.
//     So a change of origin does not widen every gap of the list.
//   - CodecNibble: every gap a varint of nibbles.
//
// A list of one origin has no escapes and costs what it did before origins.

// gaps is how the gaps of a sorted list of keys are written.
type gaps struct {
	fixed bool
	esc   bool // fixed with escapes
	w     uint // the width of a fixed gap
	// escNibbles are the nibbles after the escapes; nibbles those of every
	// gap as a varint (CodecNibble).
	escNibbles, nibbles uint64
}

// planGaps picks the codec that writes the gaps between the n keys of key, in
// order, in fewer bits.
func planGaps(n int, key func(i int) docid.Key) gaps {
	var nibbles, escNibbles, maxGap, maxRun uint64

	for i := 1; i < n; i++ {
		prev, next := key(i-1), key(i)
		gap := uint64(next - prev)
		nibbles += uint64(NibbleCount(gap))
		maxGap = max(maxGap, gap)

		if next.Origin() == prev.Origin() {
			maxRun = max(maxRun, gap)
		} else {
			escNibbles += uint64(escapeNibbles(prev, next))
		}
	}

	gapCount := uint64(max(n-1, 0))

	best := gaps{fixed: true, w: bits.Len(maxGap)}
	size := gapCount * uint64(best.w)

	if escNibbles > 0 {
		esc := gaps{fixed: true, esc: true, w: bits.Len(maxRun + 1), escNibbles: escNibbles}
		if n := gapCount*uint64(esc.w) + 4*escNibbles; n < size {
			best, size = esc, n
		}
	}

	if 4*nibbles < size {
		best = gaps{nibbles: nibbles}
	}

	return best
}

func escapeNibbles(prev, next docid.Key) uint {
	return NibbleCount(uint64(next.Origin()-prev.Origin()-1)) + NibbleCount(uint64(next.ID()))
}

// write writes the gap from prev to next.
func (g gaps) write(w *bits.Writer, prev, next docid.Key) {
	switch {
	case !g.fixed:
		WriteNibbles(w, uint64(next-prev))
	case g.esc && next.Origin() != prev.Origin():
		w.Write(1<<g.w-1, g.w)
		WriteNibbles(w, uint64(next.Origin()-prev.Origin()-1))
		WriteNibbles(w, uint64(next.ID()))
	default:
		w.Write(uint64(next-prev), g.w)
	}
}

// read reads the gap after prev and returns the next key.
func (g gaps) read(r *bits.Reader, prev docid.Key) (docid.Key, error) {
	if !g.fixed {
		gap, err := ReadNibbles(r)
		if err != nil || gap > uint64(docid.Max-prev) {
			return 0, fmt.Errorf("%w: key gap", ErrCorrupt)
		}

		return prev + docid.Key(gap), nil
	}

	gap := r.Read(g.w)
	if !g.esc || gap != 1<<g.w-1 {
		next := prev + docid.Key(gap)
		if gap > uint64(docid.Max-prev) || g.esc && next.Origin() != prev.Origin() {
			return 0, fmt.Errorf("%w: ID gap out of the origin", ErrCorrupt)
		}

		return next, r.Err()
	}

	skip, err := ReadNibbles(r)
	if err != nil {
		return 0, err
	}

	id, err := ReadNibbles(r)
	if err != nil || int(prev.Origin())+1+int(min(skip, math.MaxUint8)) > math.MaxUint8 || id > math.MaxUint32 {
		return 0, fmt.Errorf("%w: origin escape", ErrCorrupt)
	}

	return docid.New(prev.Origin()+1+uint8(skip), uint32(id)), nil
}

// Split writes keys that are not in order, of any origin, as their origin −
// Origin in WO bits and their ID − ID in WID bits: the IDs of every origin
// share one range, so a key costs what it would in one origin plus the bits
// of the origins. In one origin WO is 0.
type Split struct {
	Origin  uint8
	ID      uint32
	WO, WID uint
}

// SplitOf is the Split of keys.
func SplitOf(keys ...docid.Key) Split {
	if len(keys) == 0 {
		return Split{}
	}

	loG, hiG := keys[0].Origin(), keys[0].Origin()
	loID, hiID := keys[0].ID(), keys[0].ID()

	for _, k := range keys[1:] {
		loG, hiG = min(loG, k.Origin()), max(hiG, k.Origin())
		loID, hiID = min(loID, k.ID()), max(hiID, k.ID())
	}

	return Split{Origin: loG, ID: loID, WO: bits.Len(uint64(hiG - loG)), WID: bits.Len(uint64(hiID - loID))}
}

// Write writes k, which must be in the ranges of s.
func (s Split) Write(w *bits.Writer, k docid.Key) {
	w.Write(uint64(k.Origin()-s.Origin), s.WO)
	w.Write(uint64(k.ID()-s.ID), s.WID)
}

// Read reads a key Write wrote.
func (s Split) Read(r *bits.Reader) (docid.Key, error) {
	g := uint64(s.Origin) + r.Read(s.WO)
	id := uint64(s.ID) + r.Read(s.WID)

	if g > math.MaxUint8 || id > math.MaxUint32 {
		return 0, fmt.Errorf("%w: split key", ErrCorrupt)
	}

	return docid.New(uint8(g), uint32(id)), r.Err()
}

// Valid reports widths a reader accepts.
func (s Split) Valid() bool { return s.WO <= 8 && s.WID <= 32 }
