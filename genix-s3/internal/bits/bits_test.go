package bits

import (
	"math/rand/v2"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))

	type item struct {
		v uint64
		n uint
	}

	var items []item

	w := NewWriter([]byte{0xAA}) // writes after existing bytes

	for range 10000 {
		n := uint(r.IntN(57))
		v := r.Uint64() & (1<<n - 1)
		items = append(items, item{v, n})
		w.Write(v|^(1<<n-1), n) // bits above n must be ignored
	}

	data := w.Bytes()
	rd := NewReader(data, 8)

	for i, it := range items {
		if got := rd.Read(it.n); got != it.v {
			t.Fatalf("item %d: got %x, want %x (%d bits)", i, got, it.v, it.n)
		}
	}

	if rd.Err() != nil {
		t.Fatal(rd.Err())
	}

	rd.Read(16)
	if rd.Err() != ErrOverrun {
		t.Fatal("reading past the end did not fail")
	}
}

func TestLSBFirst(t *testing.T) {
	w := NewWriter(nil)
	w.Write(0b101, 3)
	w.Write(0b11, 2)

	if got := w.Bytes(); len(got) != 1 || got[0] != 0b11101 {
		t.Fatalf("got %08b", got)
	}
}
