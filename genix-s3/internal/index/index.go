// Package index writes and reads k.<gen> (the dictionary) and p.<gen> (the
// postings), PLAN_5BIT.md §5.1–5.3. Both are plain bytes: a fixed header, a
// fixed-width slot directory, and bit-packed sections whose offsets are
// computed, never stored per key.
package index

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"

	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// Format is the layout version of k and p. A reader rejects any other.
const Format uint16 = 1

const (
	magic      = "GXK1"
	headerSize = 64
	dirEntries = key5.Slots + 1
	// HeadSize is the header and the slot directory: what a cold reader
	// fetches first.
	HeadSize = headerSize + dirEntries*8
	// PageSize is the unit of p's integrity check.
	PageSize = 4096
	// blockHeader is n u32 | wCount u8 | wFirst u8 | wX u8 | reserved u8 | base u32.
	blockHeader = 12
	// minX keeps Tail|Codec|X at 6 bits or more, so a list of one posting
	// holds its position (4 bits) and tail (2) there.
	minX = 4
	// posBits is the width of a position, saturated at maxPos.
	posBits = 4
	// MaxPos is the largest position stored; later tokens share it.
	MaxPos  = 1<<posBits - 1
	tailBit = 2
)

// Codec is how the gaps of a list are written.
const (
	CodecFixed  uint8 = 0 // every gap in W bits
	CodecNibble uint8 = 1 // varint of nibbles: 3 value bits + 1 continue bit
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// ErrCorrupt is a file that fails its checks.
var ErrCorrupt = errors.New("index: corrupt")

// Posting is one occurrence of a key in a document.
type Posting struct {
	ID   uint32
	Pos  uint8 // position of the token in the document, saturated at MaxPos
	Tail uint8 // key5.Tail of the word
}

// Head is the header and slot directory of k.
type Head struct {
	Gen, Keys, Docs uint32
	PSize           uint64
	PageCRCOff      uint32
	NormID          uint32
	KOff, POff      [dirEntries]uint32
}

// Block is the byte range of slot s's SlotBlock in k.
func (h *Head) Block(s uint16) (off, end uint32) {
	return h.KOff[s], h.KOff[s+1]
}

// KSize is the size of k: the page CRCs follow the slots.
func (h *Head) KSize() uint32 {
	return h.PageCRCOff + 4*uint32(Pages(h.PSize))
}

// Pages is the number of 4 KB pages of p.
func Pages(pSize uint64) int {
	return int((pSize + PageSize - 1) / PageSize)
}

// ParseHead reads the first HeadSize bytes of k.
func ParseHead(b []byte, normID uint32) (*Head, error) {
	if len(b) < HeadSize {
		return nil, fmt.Errorf("%w: head of %d bytes", ErrCorrupt, len(b))
	}

	if string(b[:4]) != magic {
		return nil, fmt.Errorf("%w: bad magic", ErrCorrupt)
	}

	if f := binary.LittleEndian.Uint16(b[4:]); f != Format {
		return nil, fmt.Errorf("index: format %d, this build reads %d", f, Format)
	}

	if crc32.Checksum(b[:60], castagnoli) != binary.LittleEndian.Uint32(b[60:]) {
		return nil, fmt.Errorf("%w: header CRC", ErrCorrupt)
	}

	if crc32.Checksum(b[headerSize:HeadSize], castagnoli) != binary.LittleEndian.Uint32(b[32:]) {
		return nil, fmt.Errorf("%w: slot directory CRC", ErrCorrupt)
	}

	h := &Head{
		Gen:        binary.LittleEndian.Uint32(b[8:]),
		Keys:       binary.LittleEndian.Uint32(b[12:]),
		Docs:       binary.LittleEndian.Uint32(b[16:]),
		PSize:      binary.LittleEndian.Uint64(b[20:]),
		PageCRCOff: binary.LittleEndian.Uint32(b[28:]),
		NormID:     binary.LittleEndian.Uint32(b[36:]),
	}

	if h.NormID != normID {
		return nil, fmt.Errorf("index: NormID %08x, this normalizer is %08x: the collection was indexed with another normalizer or other extra letters", h.NormID, normID)
	}

	for i := range dirEntries {
		h.KOff[i] = binary.LittleEndian.Uint32(b[headerSize+8*i:])
		h.POff[i] = binary.LittleEndian.Uint32(b[headerSize+8*i+4:])

		if i > 0 && (h.KOff[i] < h.KOff[i-1] || h.POff[i] < h.POff[i-1]) {
			return nil, fmt.Errorf("%w: slot directory out of order", ErrCorrupt)
		}
	}

	if h.KOff[0] != HeadSize || h.KOff[key5.Slots] != h.PageCRCOff || uint64(h.POff[key5.Slots]) != h.PSize {
		return nil, fmt.Errorf("%w: slot directory does not match the header", ErrCorrupt)
	}

	return h, nil
}

// PageCRCs reads the CRC table of p from the bytes of k at PageCRCOff.
func PageCRCs(b []byte) []uint32 {
	crcs := make([]uint32, len(b)/4)
	for i := range crcs {
		crcs[i] = binary.LittleEndian.Uint32(b[4*i:])
	}

	return crcs
}

// CheckPages verifies the pages of p held in data, which starts at byte off
// of p (a multiple of PageSize). crcs is the table of k, from page first.
func CheckPages(data []byte, off uint64, pSize uint64, crcs []uint32, first int) error {
	if off%PageSize != 0 {
		return fmt.Errorf("index: page check at %d", off)
	}

	for at := 0; at < len(data); at += PageSize {
		end := min(at+PageSize, len(data))
		page := int(off/PageSize) + at/PageSize

		if uint64(page)*PageSize+uint64(end-at) != min(uint64(page+1)*PageSize, pSize) {
			return fmt.Errorf("%w: page %d is short", ErrCorrupt, page)
		}

		if crc32.Checksum(data[at:end], castagnoli) != crcs[page-first] {
			return fmt.Errorf("%w: page %d of p", ErrCorrupt, page)
		}
	}

	return nil
}
