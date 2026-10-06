// Package docid is the identity of a document: its origin (the table it comes
// from), which a document never changes, and its ID within the origin. Both go
// in one key of 40 bits, origin<<32 | ID, and the index sorts by it: by
// origin, then by ID. The same ID in two origins is two documents. The origin
// does not take part in the score.
package docid

import (
	"encoding/binary"
	"fmt"
)

// Key is a document: origin<<32 | ID.
type Key uint64

const (
	// Bits is the width of a key.
	Bits = 40
	// Max is the largest key.
	Max Key = 1<<Bits - 1
	// Size is the bytes of a key in p.log and tokens: ID u32 | origin u8, the
	// key little-endian in 5 bytes.
	Size = 5
)

// New is the key of the document id of origin.
func New(origin uint8, id uint32) Key { return Key(origin)<<32 | Key(id) }

// Origin is the origin of the document.
func (k Key) Origin() uint8 { return uint8(k >> 32) }

// ID is the ID of the document in its origin.
func (k Key) ID() uint32 { return uint32(k) }

// String is origin:ID.
func (k Key) String() string { return fmt.Sprintf("%d:%d", k.Origin(), k.ID()) }

// Append appends the Size bytes of k.
func Append(dst []byte, k Key) []byte {
	return append(binary.LittleEndian.AppendUint32(dst, k.ID()), k.Origin())
}

// Read reads the Size bytes Append wrote; b must hold them.
func Read(b []byte) Key {
	return New(b[4], binary.LittleEndian.Uint32(b))
}
