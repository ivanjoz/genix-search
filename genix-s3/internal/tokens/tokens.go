// Package tokens is the tokens object of a partition (PLAN_BUCKETS.md §1, §8):
// the document → tokens state the index was built from, the expanded words and the
// hash of every bucket. Only the compaction reads it, to know what a changed
// document had and which buckets it has to rewrite; queries never do.
package tokens

import (
	"cmp"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// Format is the layout version.
const Format uint16 = 3

const (
	magic      = "GXT1"
	headerSize = 48
	dirEntry   = 20             // first key u64 | Off u32 | Len u32 | CRC32C u32
	slotEntry  = 12             // group u16 | flags u8 | grouping u8 (garbage) | hash u64
	docHead    = docid.Size + 2 // key (ID u32 | group u8) | tokLen u16
	// ChunkDocs is how many documents go in a chunk.
	ChunkDocs = 1000
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Doc is one document: its key and its key5 token stream.
type Doc struct {
	ID     docid.Key
	Tokens []byte
}

// State is the whole object.
type State struct {
	NormID   uint32
	Epoch    uint32
	Seq      uint64     // the ops up to Seq are in Docs
	Docs     []Doc      // sorted by key
	Expanded []key5.Key // sorted
	// Grouping is the layout.Grouping ID of Buckets.
	Grouping uint8
	// Buckets are the buckets of the index, by group.
	Buckets map[uint16]Bucket
	// Garbage are buckets no query reads anymore — tombstones, and those of
	// an older epoch or grouping — to delete once the queries that read them
	// before the change are done.
	Garbage []Garbage
}

// Garbage is a bucket to delete: a group of a grouping.
type Garbage struct {
	Grouping uint8
	Group    uint16
}

// Bucket is what the compaction remembers of a bucket it wrote.
type Bucket struct {
	Hash  uint64 // of the content, bucket.Built.Hash
	HasDB bool
}

// slot entry flags.
const (
	flagGarbage = 1
	flagDB      = 2
)

// Encode writes s.
func Encode(s *State) ([]byte, error) {
	docs := s.Docs
	if !slices.IsSortedFunc(docs, func(a, b Doc) int { return cmp.Compare(a.ID, b.ID) }) {
		return nil, errors.New("tokens: documents not sorted by key")
	}

	slots := make([]uint16, 0, len(s.Buckets))
	for slot := range s.Buckets {
		slots = append(slots, slot)
	}

	slices.Sort(slots)

	garbage := slices.Clone(s.Garbage)
	slices.SortFunc(garbage, func(a, b Garbage) int {
		return int(a.Grouping)<<16 | int(a.Group) - (int(b.Grouping)<<16 | int(b.Group))
	})
	garbage = slices.Compact(garbage)

	chunks := (len(docs) + ChunkDocs - 1) / ChunkDocs
	entries := len(slots) + len(garbage)
	tables := chunks*dirEntry + len(s.Expanded)*8 + entries*slotEntry
	out := make([]byte, headerSize+tables)

	at := headerSize + chunks*dirEntry
	for _, k := range s.Expanded {
		binary.LittleEndian.PutUint64(out[at:], uint64(k))
		at += 8
	}

	for _, slot := range slots {
		b := s.Buckets[slot]

		var flags byte
		if b.HasDB {
			flags |= flagDB
		}

		binary.LittleEndian.PutUint16(out[at:], slot)
		out[at+2] = flags
		binary.LittleEndian.PutUint64(out[at+4:], b.Hash)
		at += slotEntry
	}

	for _, g := range garbage {
		binary.LittleEndian.PutUint16(out[at:], g.Group)
		out[at+2], out[at+3] = flagGarbage, g.Grouping
		at += slotEntry
	}

	for c := range chunks {
		part := docs[c*ChunkDocs : min((c+1)*ChunkDocs, len(docs))]
		off := len(out)

		for i, doc := range part {
			if i > 0 && doc.ID == part[i-1].ID || c > 0 && i == 0 && doc.ID == docs[c*ChunkDocs-1].ID {
				return nil, fmt.Errorf("tokens: document %v twice", doc.ID)
			}

			if len(doc.Tokens) > math.MaxUint16 {
				return nil, fmt.Errorf("tokens: document %v has %d bytes of tokens", doc.ID, len(doc.Tokens))
			}

			out = docid.Append(out, doc.ID)
			out = binary.LittleEndian.AppendUint16(out, uint16(len(doc.Tokens)))
			out = append(out, doc.Tokens...)
		}

		if len(out) > math.MaxUint32 {
			return nil, errors.New("tokens: over 4 GiB: partition the collection")
		}

		e := out[headerSize+c*dirEntry:]
		binary.LittleEndian.PutUint64(e, uint64(part[0].ID))
		binary.LittleEndian.PutUint32(e[8:], uint32(off))
		binary.LittleEndian.PutUint32(e[12:], uint32(len(out)-off))
		binary.LittleEndian.PutUint32(e[16:], crc32.Checksum(out[off:], castagnoli))
	}

	copy(out, magic)
	binary.LittleEndian.PutUint16(out[4:], Format)
	binary.LittleEndian.PutUint32(out[8:], s.NormID)
	binary.LittleEndian.PutUint32(out[12:], s.Epoch)
	binary.LittleEndian.PutUint64(out[16:], s.Seq)
	binary.LittleEndian.PutUint32(out[24:], uint32(len(docs)))
	binary.LittleEndian.PutUint32(out[28:], uint32(chunks))
	binary.LittleEndian.PutUint32(out[32:], uint32(len(s.Expanded)))
	binary.LittleEndian.PutUint32(out[36:], uint32(entries))
	out[40] = s.Grouping

	crc := crc32.Update(crc32.Checksum(out[:44], castagnoli), castagnoli, out[headerSize:headerSize+tables])
	binary.LittleEndian.PutUint32(out[44:], crc)

	return out, nil
}

// Decode reads a whole object. The token streams alias data. A tokens object
// of another normalizer is an error: the caller rebuilds from the source.
func Decode(data []byte, normID uint32) (*State, error) {
	if len(data) < headerSize || string(data[:4]) != magic {
		return nil, errors.New("tokens: not a tokens object")
	}

	if f := binary.LittleEndian.Uint16(data[4:]); f != Format {
		return nil, fmt.Errorf("tokens: format %d, this build reads %d", f, Format)
	}

	s := &State{
		NormID:   binary.LittleEndian.Uint32(data[8:]),
		Epoch:    binary.LittleEndian.Uint32(data[12:]),
		Seq:      binary.LittleEndian.Uint64(data[16:]),
		Grouping: data[40],
		Buckets:  map[uint16]Bucket{},
	}

	count := int(binary.LittleEndian.Uint32(data[24:]))
	chunks := int(binary.LittleEndian.Uint32(data[28:]))
	expanded := int(binary.LittleEndian.Uint32(data[32:]))
	slots := int(binary.LittleEndian.Uint32(data[36:]))

	tables := chunks*dirEntry + expanded*8 + slots*slotEntry
	if chunks < 0 || expanded < 0 || slots < 0 || tables > len(data)-headerSize {
		return nil, errors.New("tokens: corrupt tables")
	}

	crc := crc32.Update(crc32.Checksum(data[:44], castagnoli), castagnoli, data[headerSize:headerSize+tables])
	if crc != binary.LittleEndian.Uint32(data[44:]) {
		return nil, errors.New("tokens: header CRC")
	}

	if s.NormID != normID {
		return nil, errors.New("tokens: written with another normalizer or other extra letters")
	}

	at := headerSize + chunks*dirEntry
	s.Expanded = make([]key5.Key, expanded)

	for i := range s.Expanded {
		s.Expanded[i] = key5.Key(binary.LittleEndian.Uint64(data[at:]))
		at += 8
	}

	for range slots {
		slot, flags := binary.LittleEndian.Uint16(data[at:]), data[at+2]

		if flags&flagGarbage != 0 {
			s.Garbage = append(s.Garbage, Garbage{Grouping: data[at+3], Group: slot})
		} else {
			s.Buckets[slot] = Bucket{Hash: binary.LittleEndian.Uint64(data[at+4:]), HasDB: flags&flagDB != 0}
		}

		at += slotEntry
	}

	s.Docs = make([]Doc, 0, count)

	for c := range chunks {
		e := data[headerSize+c*dirEntry:]
		off, n := int(binary.LittleEndian.Uint32(e[8:])), int(binary.LittleEndian.Uint32(e[12:]))

		if off < headerSize+tables || n > len(data) || off+n > len(data) {
			return nil, fmt.Errorf("tokens: chunk %d out of range", c)
		}

		chunk := data[off : off+n]
		if crc32.Checksum(chunk, castagnoli) != binary.LittleEndian.Uint32(e[16:]) {
			return nil, fmt.Errorf("tokens: chunk %d CRC", c)
		}

		for at := 0; at < len(chunk); {
			if len(chunk)-at < docHead {
				return nil, fmt.Errorf("tokens: chunk %d is cut", c)
			}

			id := docid.Read(chunk[at:])
			tokLen := int(binary.LittleEndian.Uint16(chunk[at+docid.Size:]))
			at += docHead

			if len(chunk)-at < tokLen {
				return nil, fmt.Errorf("tokens: chunk %d is cut", c)
			}

			s.Docs = append(s.Docs, Doc{ID: id, Tokens: chunk[at : at+tokLen : at+tokLen]})
			at += tokLen
		}
	}

	if len(s.Docs) != count {
		return nil, fmt.Errorf("tokens: %d documents, the header says %d", len(s.Docs), count)
	}

	return s, nil
}
