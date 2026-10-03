package index

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"

	"github.com/ivanjoz/genix-search/genix-s3/internal/bits"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// List is the postings of one key, sorted by (ID, Pos).
type List struct {
	Key      key5.Key
	Postings []Posting
}

// Meta is what the header records besides the lists.
type Meta struct {
	Gen, Docs, NormID uint32
}

// shape is how one list is written.
type shape struct {
	count   uint32
	hasTail bool
	codec   uint8
	x       uint32 // W for CodecFixed, the number of nibbles for CodecNibble
	first   uint32
}

// Build writes k and p from lists sorted by key.
func Build(meta Meta, lists []List) (k, p []byte, err error) {
	k = make([]byte, HeadSize, HeadSize+len(lists)*8)

	var dir [dirEntries][2]uint32

	at := 0
	for s := range key5.Slots {
		dir[s] = [2]uint32{uint32(len(k)), uint32(len(p))}

		end := at
		for end < len(lists) && lists[end].Key.Slot() == uint16(s) {
			end++
		}

		if end > at {
			if k, p, err = appendSlot(k, p, lists[at:end]); err != nil {
				return nil, nil, err
			}
		}

		at = end
	}

	if at != len(lists) {
		return nil, nil, fmt.Errorf("index: lists not sorted by key at %s", lists[at].Key)
	}

	if len(k) > math.MaxUint32 || len(p) > math.MaxUint32 {
		return nil, nil, fmt.Errorf("index: k (%d B) or p (%d B) over 4 GiB: partition the collection", len(k), len(p))
	}

	dir[key5.Slots] = [2]uint32{uint32(len(k)), uint32(len(p))}

	for i, e := range dir {
		binary.LittleEndian.PutUint32(k[headerSize+8*i:], e[0])
		binary.LittleEndian.PutUint32(k[headerSize+8*i+4:], e[1])
	}

	pageCRCOff := uint32(len(k))

	for off := 0; off < len(p); off += PageSize {
		k = binary.LittleEndian.AppendUint32(k, crc32.Checksum(p[off:min(off+PageSize, len(p))], castagnoli))
	}

	h := k[:headerSize]
	copy(h, magic)
	binary.LittleEndian.PutUint16(h[4:], Format)
	binary.LittleEndian.PutUint32(h[8:], meta.Gen)
	binary.LittleEndian.PutUint32(h[12:], uint32(len(lists)))
	binary.LittleEndian.PutUint32(h[16:], meta.Docs)
	binary.LittleEndian.PutUint64(h[20:], uint64(len(p)))
	binary.LittleEndian.PutUint32(h[28:], pageCRCOff)
	binary.LittleEndian.PutUint32(h[32:], crc32.Checksum(k[headerSize:HeadSize], castagnoli))
	binary.LittleEndian.PutUint32(h[36:], meta.NormID)
	binary.LittleEndian.PutUint32(h[60:], crc32.Checksum(h[:60], castagnoli))

	return k, p, nil
}

// appendSlot writes the SlotBlock of one slot to k and its lists to p.
func appendSlot(k, p []byte, lists []List) ([]byte, []byte, error) {
	shapes := make([]shape, len(lists))
	keys := make([]key5.Key, len(lists))

	var maxCount, maxX uint32
	base, maxFirst := uint32(math.MaxUint32), uint32(0)

	for i, l := range lists {
		if len(l.Postings) == 0 {
			return nil, nil, fmt.Errorf("index: key %s has no postings", l.Key)
		}

		if len(l.Postings) > math.MaxUint32 {
			return nil, nil, fmt.Errorf("index: key %s has too many postings", l.Key)
		}

		sh, err := shapeOf(l.Postings)
		if err != nil {
			return nil, nil, fmt.Errorf("index: key %s: %w", l.Key, err)
		}

		shapes[i], keys[i] = sh, l.Key
		maxCount = max(maxCount, sh.count)
		base, maxFirst = min(base, sh.first), max(maxFirst, sh.first)

		if sh.count > 1 {
			maxX = max(maxX, sh.x)
		}
	}

	wCount := bits.Len(uint64(maxCount))
	wFirst := bits.Len(uint64(maxFirst - base))
	wX := max(bits.Len(uint64(maxX)), minX)

	if wX > 32 || wCount > 32 {
		return nil, nil, fmt.Errorf("index: slot %d: field too wide", keys[0].Slot())
	}

	start := len(k)
	k = binary.LittleEndian.AppendUint32(k, uint32(len(lists)))
	k = append(k, byte(wCount), byte(wFirst), byte(wX), 0)
	k = binary.LittleEndian.AppendUint32(k, base)

	// The restart offsets go before the keys, but are only known once the
	// keys are written: reserve them, then fill them in.
	restartsAt := len(k)
	k = append(k, make([]byte, 4*((len(lists)-1)/key5.RestartEvery))...)

	w := bits.NewWriter(k)

	restarts, err := key5.AppendSlot(w, keys)
	if err != nil {
		return nil, nil, err
	}

	k = w.Bytes()

	for i, off := range restarts {
		binary.LittleEndian.PutUint32(k[restartsAt+4*i:], off)
	}

	w = bits.NewWriter(k)
	for i, sh := range shapes {
		w.Write(uint64(sh.count), wCount)
		w.Write(packV(sh, lists[i].Postings), 2+wX)
		w.Write(uint64(sh.first-base), wFirst)
	}

	k = w.Bytes()
	k = binary.LittleEndian.AppendUint32(k, crc32.Checksum(k[start:], castagnoli))

	w = bits.NewWriter(p)
	for i, sh := range shapes {
		if sh.count > 1 {
			writeList(w, sh, lists[i].Postings)
		}
	}

	return k, w.Bytes(), nil
}

// packV is the Tail|Codec|X field: for a list of one posting it holds the
// posting's position and tail instead.
func packV(sh shape, postings []Posting) uint64 {
	if sh.count == 1 {
		return uint64(postings[0].Pos) | uint64(postings[0].Tail)<<posBits
	}

	v := uint64(sh.codec)<<1 | uint64(sh.x)<<2
	if sh.hasTail {
		v |= 1
	}

	return v
}

// shapeOf checks a list and picks the codec that writes its gaps in fewer bits.
func shapeOf(postings []Posting) (shape, error) {
	sh := shape{count: uint32(len(postings)), first: postings[0].ID}

	var maxGap uint32
	var nibbles uint64

	for i, posting := range postings {
		if posting.Pos > MaxPos || posting.Tail > 3 {
			return sh, fmt.Errorf("position %d or tail %d out of range", posting.Pos, posting.Tail)
		}

		sh.hasTail = sh.hasTail || posting.Tail != 0

		if i == 0 {
			continue
		}

		prev := postings[i-1]
		if posting.ID < prev.ID || (posting.ID == prev.ID && posting.Pos < prev.Pos) {
			return sh, fmt.Errorf("postings not sorted by (ID, Pos)")
		}

		gap := posting.ID - prev.ID
		maxGap = max(maxGap, gap)
		nibbles += uint64(nibbleCount(gap))
	}

	w := bits.Len(uint64(maxGap))
	fixedBits := uint64(sh.count-1) * uint64(w)

	if fixedBits <= 4*nibbles {
		sh.codec, sh.x = CodecFixed, uint32(w)
	} else {
		sh.codec, sh.x = CodecNibble, uint32(nibbles)
	}

	return sh, nil
}

func nibbleCount(v uint32) uint {
	return max(1, (bits.Len(uint64(v))+2)/3)
}

// listBits is the size of a list in p.
func listBits(count uint32, hasTail bool, codec uint8, x uint32) uint64 {
	if count <= 1 {
		return 0
	}

	gapBits := uint64(count-1) * uint64(x)
	if codec == CodecNibble {
		gapBits = 4 * uint64(x)
	}

	n := gapBits + posBits*uint64(count)
	if hasTail {
		n += tailBit * uint64(count)
	}

	return n
}

func writeList(w *bits.Writer, sh shape, postings []Posting) {
	for i := 1; i < len(postings); i++ {
		gap := postings[i].ID - postings[i-1].ID

		if sh.codec == CodecFixed {
			w.Write(uint64(gap), uint(sh.x))

			continue
		}

		for {
			nibble := uint64(gap & 7)
			gap >>= 3

			if gap == 0 {
				w.Write(nibble, 4)

				break
			}

			w.Write(nibble|8, 4)
		}
	}

	for _, posting := range postings {
		w.Write(uint64(posting.Pos), posBits)
	}

	if sh.hasTail {
		for _, posting := range postings {
			w.Write(uint64(posting.Tail), tailBit)
		}
	}
}
