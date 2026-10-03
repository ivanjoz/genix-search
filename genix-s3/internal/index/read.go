package index

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"sort"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// Entry is one key of a slot, with what the dictionary says of its list.
type Entry struct {
	Key     key5.Key
	Count   uint32
	HasTail bool
	Codec   uint8
	X       uint32
	First   uint32 // ID of the first posting
	// Pos and Tail of the only posting, when Count == 1.
	Pos, Tail uint8
	// BitOff and Bits locate the list in p; Bits is 0 when Count == 1.
	BitOff, Bits uint64
}

// Bytes is the byte range of the list in p.
func (e *Entry) Bytes() (off, end uint64) {
	return e.BitOff / 8, (e.BitOff + e.Bits + 7) / 8
}

// Slot is a decoded SlotBlock.
type Slot struct {
	ID      uint16
	Entries []Entry // sorted by key
	Size    int     // bytes of the block, for cache accounting
}

// ParseSlot decodes the SlotBlock of slot s, block being exactly its bytes.
func ParseSlot(h *Head, s uint16, block []byte) (*Slot, error) {
	slot := &Slot{ID: s, Size: len(block)}
	if len(block) == 0 {
		return slot, nil
	}

	if len(block) < blockHeader+4 {
		return nil, fmt.Errorf("%w: slot %d too short", ErrCorrupt, s)
	}

	body := block[:len(block)-4]
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(block[len(body):]) {
		return nil, fmt.Errorf("%w: slot %d CRC", ErrCorrupt, s)
	}

	n := int(binary.LittleEndian.Uint32(body))
	wCount, wFirst, wX := uint(body[4]), uint(body[5]), uint(body[6])
	base := binary.LittleEndian.Uint32(body[8:])

	keysAt := blockHeader + 4*((n-1)/key5.RestartEvery)
	if n == 0 || keysAt > len(body) || wX < minX || wX > 32 || wCount > 32 || wFirst > 32 {
		return nil, fmt.Errorf("%w: slot %d header", ErrCorrupt, s)
	}

	r := bits.NewReader(body[keysAt:], 0)

	keys, err := key5.ReadSlot(r, s, n)
	if err != nil {
		return nil, fmt.Errorf("slot %d: %w", s, err)
	}

	fieldsAt := keysAt + int((r.Pos()+7)/8)
	r = bits.NewReader(body[min(fieldsAt, len(body)):], 0)

	slot.Entries = make([]Entry, n)
	bitOff := 8 * uint64(h.POff[s])

	for i := range slot.Entries {
		e := &slot.Entries[i]
		e.Key = keys[i]
		e.Count = uint32(r.Read(wCount))
		v := r.Read(2 + wX)
		e.First = base + uint32(r.Read(wFirst))

		if e.Count == 0 {
			return nil, fmt.Errorf("%w: slot %d key %s has no postings", ErrCorrupt, s, e.Key)
		}

		if e.Count == 1 {
			e.Pos, e.Tail = uint8(v&MaxPos), uint8(v>>posBits&3)
		} else {
			e.HasTail, e.Codec, e.X = v&1 != 0, uint8(v>>1&1), uint32(v>>2)
		}

		e.BitOff = bitOff
		e.Bits = listBits(e.Count, e.HasTail, e.Codec, e.X)
		bitOff += e.Bits
	}

	if r.Err() != nil {
		return nil, fmt.Errorf("%w: slot %d fields", ErrCorrupt, s)
	}

	if (bitOff+7)/8 != uint64(h.POff[s+1]) {
		return nil, fmt.Errorf("%w: slot %d lists end at bit %d, the directory says byte %d", ErrCorrupt, s, bitOff, h.POff[s+1])
	}

	return slot, nil
}

// Find returns the entry of key k.
func (s *Slot) Find(k key5.Key) (*Entry, bool) {
	i := sort.Search(len(s.Entries), func(i int) bool { return s.Entries[i].Key >= k })
	if i < len(s.Entries) && s.Entries[i].Key == k {
		return &s.Entries[i], true
	}

	return nil, false
}

// Range returns the entries with lo ≤ key ≤ hi.
func (s *Slot) Range(lo, hi key5.Key) []Entry {
	from := sort.Search(len(s.Entries), func(i int) bool { return s.Entries[i].Key >= lo })
	to := sort.Search(len(s.Entries), func(i int) bool { return s.Entries[i].Key > hi })

	return s.Entries[from:max(from, to)]
}

// Decode returns the postings of e. data holds bytes of p starting at byte
// dataOff and must cover e.Bytes(); a list of one posting needs no data.
func Decode(e *Entry, data []byte, dataOff uint64, dst []Posting) ([]Posting, error) {
	if e.Count == 1 {
		return append(dst, Posting{ID: e.First, Pos: e.Pos, Tail: e.Tail}), nil
	}

	if e.BitOff < 8*dataOff {
		return nil, fmt.Errorf("index: list at bit %d before the data at byte %d", e.BitOff, dataOff)
	}

	r := bits.NewReader(data, e.BitOff-8*dataOff)
	start := len(dst)
	id := e.First

	dst = append(dst, Posting{ID: id})

	for range e.Count - 1 {
		var gap uint64

		if e.Codec == CodecFixed {
			gap = r.Read(uint(e.X))
		} else {
			for shift := uint(0); ; shift += 3 {
				nibble := r.Read(4)
				gap |= (nibble & 7) << shift

				if nibble&8 == 0 || r.Err() != nil {
					break
				}

				if shift > 33 {
					return nil, fmt.Errorf("%w: gap too long in %s", ErrCorrupt, e.Key)
				}
			}
		}

		next := uint64(id) + gap
		if next > 1<<32-1 {
			return nil, fmt.Errorf("%w: ID overflow in %s", ErrCorrupt, e.Key)
		}

		id = uint32(next)
		dst = append(dst, Posting{ID: id})
	}

	list := dst[start:]
	for i := range list {
		list[i].Pos = uint8(r.Read(posBits))
	}

	if e.HasTail {
		for i := range list {
			list[i].Tail = uint8(r.Read(tailBit))
		}
	}

	if r.Err() != nil {
		return nil, fmt.Errorf("%w: list of %s is cut", ErrCorrupt, e.Key)
	}

	if e.BitOff+e.Bits != 8*dataOff+r.Pos() {
		return nil, fmt.Errorf("%w: list of %s has %d bits, the dictionary says %d", ErrCorrupt, e.Key, 8*dataOff+r.Pos()-e.BitOff, e.Bits)
	}

	return dst, nil
}
