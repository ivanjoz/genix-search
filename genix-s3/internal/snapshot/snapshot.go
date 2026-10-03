// Package snapshot is snap/<gen>.snap (PLAN_5BIT.md §5.5): the ID → tokens
// state of a generation, in plain bytes. Only the compactor reads it, to
// know what a changed or deleted document had; queries never do.
package snapshot

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"slices"
)

// Format is the layout version of the snapshot.
const Format uint16 = 1

const (
	magic      = "GXN1"
	headerSize = 36
	dirEntry   = 16 // FirstID u32 | Off u32 | Len u32 | CRC32C u32
	docHead    = 6  // ID u32 | tokLen u16
	// ChunkDocs is how many documents go in a chunk.
	ChunkDocs = 1000
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Doc is one document: its ID and its key5 token stream.
type Doc struct {
	ID     uint32
	Tokens []byte
}

// Header is what the snapshot says of itself.
type Header struct {
	NormID   uint32
	Gen      uint32
	LastSeq  uint64
	DocCount uint32
}

// Encode writes docs, which must be sorted by ID with no repeats.
func Encode(h Header, docs []Doc) ([]byte, error) {
	if !slices.IsSortedFunc(docs, func(a, b Doc) int { return cmpID(a.ID, b.ID) }) {
		return nil, errors.New("snapshot: documents not sorted by ID")
	}

	chunks := (len(docs) + ChunkDocs - 1) / ChunkDocs
	out := make([]byte, headerSize+chunks*dirEntry)

	for c := range chunks {
		part := docs[c*ChunkDocs : min((c+1)*ChunkDocs, len(docs))]
		off := len(out)

		for i, doc := range part {
			if i > 0 && doc.ID == part[i-1].ID || c > 0 && i == 0 && doc.ID == docs[c*ChunkDocs-1].ID {
				return nil, fmt.Errorf("snapshot: document %d twice", doc.ID)
			}

			if len(doc.Tokens) > math.MaxUint16 {
				return nil, fmt.Errorf("snapshot: document %d has %d bytes of tokens", doc.ID, len(doc.Tokens))
			}

			out = binary.LittleEndian.AppendUint32(out, doc.ID)
			out = binary.LittleEndian.AppendUint16(out, uint16(len(doc.Tokens)))
			out = append(out, doc.Tokens...)
		}

		if len(out) > math.MaxUint32 {
			return nil, errors.New("snapshot: over 4 GiB: partition the collection")
		}

		e := out[headerSize+c*dirEntry:]
		binary.LittleEndian.PutUint32(e, part[0].ID)
		binary.LittleEndian.PutUint32(e[4:], uint32(off))
		binary.LittleEndian.PutUint32(e[8:], uint32(len(out)-off))
		binary.LittleEndian.PutUint32(e[12:], crc32.Checksum(out[off:], castagnoli))
	}

	copy(out, magic)
	binary.LittleEndian.PutUint16(out[4:], Format)
	binary.LittleEndian.PutUint32(out[8:], h.NormID)
	binary.LittleEndian.PutUint32(out[12:], h.Gen)
	binary.LittleEndian.PutUint64(out[16:], h.LastSeq)
	binary.LittleEndian.PutUint32(out[24:], uint32(len(docs)))
	binary.LittleEndian.PutUint32(out[28:], uint32(chunks))

	crc := crc32.Update(crc32.Checksum(out[:32], castagnoli), castagnoli, out[headerSize:headerSize+chunks*dirEntry])
	binary.LittleEndian.PutUint32(out[32:], crc)

	return out, nil
}

// Decode reads a whole snapshot. The token streams alias data.
func Decode(data []byte, normID uint32) (Header, []Doc, error) {
	var h Header

	if len(data) < headerSize || string(data[:4]) != magic {
		return h, nil, errors.New("snapshot: not a snapshot")
	}

	if f := binary.LittleEndian.Uint16(data[4:]); f != Format {
		return h, nil, fmt.Errorf("snapshot: format %d, this build reads %d", f, Format)
	}

	h = Header{
		NormID:   binary.LittleEndian.Uint32(data[8:]),
		Gen:      binary.LittleEndian.Uint32(data[12:]),
		LastSeq:  binary.LittleEndian.Uint64(data[16:]),
		DocCount: binary.LittleEndian.Uint32(data[24:]),
	}

	chunks := int(binary.LittleEndian.Uint32(data[28:]))
	dirEnd := headerSize + chunks*dirEntry

	if chunks < 0 || dirEnd > len(data) {
		return h, nil, errors.New("snapshot: corrupt chunk directory")
	}

	crc := crc32.Update(crc32.Checksum(data[:32], castagnoli), castagnoli, data[headerSize:dirEnd])
	if crc != binary.LittleEndian.Uint32(data[32:]) {
		return h, nil, errors.New("snapshot: header CRC")
	}

	if h.NormID != normID {
		return h, nil, errors.New("snapshot: written with another normalizer or other extra letters")
	}

	docs := make([]Doc, 0, h.DocCount)

	for c := range chunks {
		e := data[headerSize+c*dirEntry:]
		off, n := int(binary.LittleEndian.Uint32(e[4:])), int(binary.LittleEndian.Uint32(e[8:]))

		if off < dirEnd || n > len(data) || off+n > len(data) {
			return h, nil, fmt.Errorf("snapshot: chunk %d out of range", c)
		}

		chunk := data[off : off+n]
		if crc32.Checksum(chunk, castagnoli) != binary.LittleEndian.Uint32(e[12:]) {
			return h, nil, fmt.Errorf("snapshot: chunk %d CRC", c)
		}

		for at := 0; at < len(chunk); {
			if len(chunk)-at < docHead {
				return h, nil, fmt.Errorf("snapshot: chunk %d is cut", c)
			}

			id := binary.LittleEndian.Uint32(chunk[at:])
			tokLen := int(binary.LittleEndian.Uint16(chunk[at+4:]))
			at += docHead

			if len(chunk)-at < tokLen {
				return h, nil, fmt.Errorf("snapshot: chunk %d is cut", c)
			}

			docs = append(docs, Doc{ID: id, Tokens: chunk[at : at+tokLen : at+tokLen]})
			at += tokLen
		}
	}

	if len(docs) != int(h.DocCount) {
		return h, nil, fmt.Errorf("snapshot: %d documents, the header says %d", len(docs), h.DocCount)
	}

	return h, docs, nil
}

func cmpID(a, b uint32) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}

	return 0
}
