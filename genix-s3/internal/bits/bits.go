// Package bits reads and writes LSB-first bit streams: the first value sits in
// the lowest bits of the first byte, like packed5. Every bit-packed section of
// the index (5-bit keys, slot fields, posting lists) goes through it.
package bits

import (
	"errors"
	mbits "math/bits"
)

// ErrOverrun is a read past the end of the buffer: a corrupt or short range.
var ErrOverrun = errors.New("bits: read past the end")

// Writer appends values to a byte slice.
type Writer struct {
	buf []byte
	acc uint64 // pending bits, LSB-first
	n   uint   // how many bits of acc are pending (< 8 between calls)
}

// NewWriter appends to buf.
func NewWriter(buf []byte) *Writer {
	return &Writer{buf: buf}
}

// Write appends the low n bits of v, n ≤ 56.
func (w *Writer) Write(v uint64, n uint) {
	if n == 0 {
		return
	}

	w.acc |= (v & (1<<n - 1)) << w.n
	w.n += n

	for w.n >= 8 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc >>= 8
		w.n -= 8
	}
}

// Align pads with zeros to the next byte.
func (w *Writer) Align() {
	if w.n > 0 {
		w.buf = append(w.buf, byte(w.acc))
		w.acc, w.n = 0, 0
	}
}

// BitLen is the number of bits written so far, counting the buffer it started with.
func (w *Writer) BitLen() uint64 {
	return uint64(len(w.buf))*8 + uint64(w.n)
}

// Bytes aligns and returns the buffer.
func (w *Writer) Bytes() []byte {
	w.Align()

	return w.buf
}

// Reader reads values from a byte slice, from any bit.
type Reader struct {
	buf []byte
	pos uint64 // next bit
	err error
}

// NewReader reads buf from bit start.
func NewReader(buf []byte, start uint64) *Reader {
	return &Reader{buf: buf, pos: start}
}

// Read returns the next n bits, n ≤ 56. Past the end it returns 0 and Err
// reports ErrOverrun.
func (r *Reader) Read(n uint) uint64 {
	if n == 0 {
		return 0
	}

	end := r.pos + uint64(n)
	if end > uint64(len(r.buf))*8 {
		r.err = ErrOverrun
		r.pos = end

		return 0
	}

	byteAt := r.pos / 8
	shift := r.pos % 8

	// Gather up to 8 bytes; n + shift ≤ 63, so one word always holds them.
	var word uint64
	for i := uint64(0); i < 8 && byteAt+i < uint64(len(r.buf)); i++ {
		word |= uint64(r.buf[byteAt+i]) << (8 * i)
	}

	r.pos = end

	return (word >> shift) & (1<<n - 1)
}

// Pos is the next bit to read.
func (r *Reader) Pos() uint64 { return r.pos }

// Err is ErrOverrun once any read went past the end.
func (r *Reader) Err() error { return r.err }

// Len is the number of bits needed to write v (0 for 0).
func Len(v uint64) uint {
	return uint(mbits.Len64(v))
}
