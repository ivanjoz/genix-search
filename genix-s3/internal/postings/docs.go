package postings

import (
	"fmt"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
)

// Columns are the lists of a combination: one per word, all over the same
// documents, each sorted by (ID, Pos). It is written front to back and
// decoded the same way, so it carries what it needs to delimit itself
// (PLAN_BUCKETS.md §4):
//
//	docs        nibbles        documents
//	first       nibbles        first ID − base
//	codec       1 bit          then W (6 bits) and W bits per gap, or a nibble varint per gap
//	per column  dup 1 bit | tail 1 bit
//	            with dup: per document, its postings − 1 (nibbles)
//	            per posting: pos (4 bits), then per posting: tail (2 bits) with tail
type Columns [][]Posting

// MaxRepeats bounds the postings of one word in one document a reader
// accepts: a document has at most plog.MaxTokens bytes of tokens.
const MaxRepeats = 1 << 17

// docIDs is the distinct IDs of a list sorted by ID.
func docIDs(list []Posting) []uint32 {
	var ids []uint32

	for i, p := range list {
		if i == 0 || p.ID != list[i-1].ID {
			ids = append(ids, p.ID)
		}
	}

	return ids
}

// WriteColumns writes cols, whose lists must have the same documents.
func WriteColumns(w *bits.Writer, base uint32, cols Columns) error {
	ids := docIDs(cols[0])
	if len(ids) == 0 || ids[0] < base {
		return fmt.Errorf("postings: columns without documents or below the base")
	}

	for _, col := range cols[1:] {
		if !slices.Equal(docIDs(col), ids) {
			return fmt.Errorf("postings: columns over different documents")
		}
	}

	WriteNibbles(w, uint64(len(ids)))
	WriteNibbles(w, uint64(ids[0]-base))

	var maxGap uint32
	var nibbles uint64

	for i := 1; i < len(ids); i++ {
		gap := ids[i] - ids[i-1]
		maxGap = max(maxGap, gap)
		nibbles += uint64(NibbleCount(uint64(gap)))
	}

	width := bits.Len(uint64(maxGap))
	fixed := uint64(len(ids)-1)*uint64(width) <= 4*nibbles

	if fixed {
		w.Write(0, 1)
		w.Write(uint64(width), 6)
	} else {
		w.Write(1, 1)
	}

	for i := 1; i < len(ids); i++ {
		if fixed {
			w.Write(uint64(ids[i]-ids[i-1]), width)
		} else {
			WriteNibbles(w, uint64(ids[i]-ids[i-1]))
		}
	}

	for _, col := range cols {
		writeColumn(w, col, len(ids))
	}

	return nil
}

func writeColumn(w *bits.Writer, col []Posting, docs int) {
	dup := len(col) != docs
	tail := slices.ContainsFunc(col, func(p Posting) bool { return p.Tail != 0 })

	w.Write(b2u(dup), 1)
	w.Write(b2u(tail), 1)

	if dup {
		for i := 0; i < len(col); {
			j := i + 1
			for j < len(col) && col[j].ID == col[i].ID {
				j++
			}

			WriteNibbles(w, uint64(j-i-1))
			i = j
		}
	}

	for _, p := range col {
		w.Write(uint64(min(p.Pos, MaxPos)), PosBits)
	}

	if tail {
		for _, p := range col {
			w.Write(uint64(p.Tail), TailBits)
		}
	}
}

// ReadColumns decodes n columns written by WriteColumns.
func ReadColumns(r *bits.Reader, base uint32, n int) (Columns, error) {
	docs, err := ReadNibbles(r)
	if err != nil {
		return nil, err
	}

	first, err := ReadNibbles(r)
	if err != nil {
		return nil, err
	}

	if docs == 0 || docs > 1<<32 || uint64(base)+first > 1<<32-1 {
		return nil, fmt.Errorf("%w: columns header", ErrCorrupt)
	}

	ids := make([]uint32, docs)
	ids[0] = base + uint32(first)

	fixed := r.Read(1) == 0
	var width uint

	if fixed {
		width = uint(r.Read(6))
	}

	for i := 1; i < len(ids); i++ {
		var gap uint64

		if fixed {
			gap = r.Read(width)
		} else if gap, err = ReadNibbles(r); err != nil {
			return nil, err
		}

		next := uint64(ids[i-1]) + gap
		if next > 1<<32-1 || r.Err() != nil {
			return nil, fmt.Errorf("%w: columns IDs", ErrCorrupt)
		}

		ids[i] = uint32(next)
	}

	cols := make(Columns, n)
	for c := range cols {
		if cols[c], err = readColumn(r, ids); err != nil {
			return nil, err
		}
	}

	return cols, nil
}

func readColumn(r *bits.Reader, ids []uint32) ([]Posting, error) {
	dup, tail := r.Read(1) == 1, r.Read(1) == 1

	var col []Posting

	if dup {
		for _, id := range ids {
			extra, err := ReadNibbles(r)
			if err != nil || extra >= MaxRepeats {
				return nil, fmt.Errorf("%w: column counts", ErrCorrupt)
			}

			for range extra + 1 {
				col = append(col, Posting{ID: id})
			}
		}
	} else {
		col = make([]Posting, len(ids))
		for i, id := range ids {
			col[i].ID = id
		}
	}

	for i := range col {
		col[i].Pos = uint8(r.Read(PosBits))
	}

	if tail {
		for i := range col {
			col[i].Tail = uint8(r.Read(TailBits))
		}
	}

	if r.Err() != nil {
		return nil, fmt.Errorf("%w: column is cut", ErrCorrupt)
	}

	return col, nil
}

func b2u(b bool) uint64 {
	if b {
		return 1
	}

	return 0
}

// Entry is a document of a head with every posting of the word in it.
type Entry struct {
	ID       uint32
	Postings []Posting // sorted by Pos
}

// WriteEntries writes the entries of a head, in their order (not by ID):
// per entry the ID − base in wID bits, its postings − 1 (nibbles), then a
// position per posting and, with tail, a tail per posting.
func WriteEntries(w *bits.Writer, base uint32, wID uint, tail bool, entries []Entry) {
	for _, e := range entries {
		w.Write(uint64(e.ID-base), wID)
		WriteNibbles(w, uint64(len(e.Postings)-1))

		for _, p := range e.Postings {
			w.Write(uint64(min(p.Pos, MaxPos)), PosBits)
		}

		if tail {
			for _, p := range e.Postings {
				w.Write(uint64(p.Tail), TailBits)
			}
		}
	}
}

// ReadEntries reads n entries written by WriteEntries.
func ReadEntries(r *bits.Reader, base uint32, wID uint, tail bool, n int) ([]Entry, error) {
	entries := make([]Entry, n)

	for i := range entries {
		id := uint64(base) + r.Read(wID)

		extra, err := ReadNibbles(r)
		if err != nil || extra >= MaxRepeats || id > 1<<32-1 {
			return nil, fmt.Errorf("%w: head entry", ErrCorrupt)
		}

		e := &entries[i]
		e.ID = uint32(id)
		e.Postings = make([]Posting, extra+1)

		for j := range e.Postings {
			e.Postings[j] = Posting{ID: e.ID, Pos: uint8(r.Read(PosBits))}
		}

		if tail {
			for j := range e.Postings {
				e.Postings[j].Tail = uint8(r.Read(TailBits))
			}
		}
	}

	if r.Err() != nil {
		return nil, fmt.Errorf("%w: head is cut", ErrCorrupt)
	}

	return entries, nil
}
