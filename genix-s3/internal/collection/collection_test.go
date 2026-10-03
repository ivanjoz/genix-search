package collection

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
)

// hookStore runs hook right before the n-th read of key, and fails the puts
// fail says to.
type hookStore struct {
	blob.Blob
	key   string
	n     int
	reads int
	hook  func()
	fail  func(key string) bool
}

func (h *hookStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if key == h.key {
		h.reads++
		if h.reads == h.n && h.hook != nil {
			h.hook()
		}
	}

	return h.Blob.Get(ctx, key)
}

func (h *hookStore) Put(ctx context.Context, key string, data []byte, cond blob.PutCond) (string, error) {
	if h.fail != nil && h.fail(key) {
		return "", errors.New("injected failure")
	}

	return h.Blob.Put(ctx, key, data, cond)
}

// TestLease: a compaction that finds another holding the lease gives up with
// ErrConflict, and does not touch anything.
func TestLease(t *testing.T) {
	ctx := context.Background()
	fs, col, other := newTestCollection(t, 1)

	if _, err := other.Init(ctx, numbered(0, 5)); err != nil {
		t.Fatal(err)
	}

	if _, err := other.Put(ctx, []Doc{{ID: 7, Text: "polo azul"}}); err != nil {
		t.Fatal(err)
	}

	// While the slow compaction reads tokens, another one tries.
	var inner error

	hooked := &hookStore{Blob: fs, key: col.Tokens(), n: 1}
	hooked.hook = func() {
		_, inner = other.Compact(ctx)
	}

	slow := New(hooked, col, normalize.Default, testOptions(1))

	res, err := slow.Compact(ctx)
	if err != nil || res.Seq != 1 {
		t.Fatalf("slow compaction: %+v %v", res, err)
	}

	if !errors.Is(inner, ErrConflict) {
		t.Errorf("a compaction while another holds the lease: %v", inner)
	}

	if _, _, err := fs.Get(ctx, col.Lease()); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("the lease was not released: %v", err)
	}
}

// TestRecovery: a compaction that fails after writing buckets keeps the
// lease, expired; the next one takes it over and rewrites every bucket in a
// new epoch, so no bucket of the failed one survives with the old epoch.
func TestRecovery(t *testing.T) {
	ctx := context.Background()
	fs, col, c := newTestCollection(t, 1)

	if _, err := c.Init(ctx, numbered(0, 20)); err != nil {
		t.Fatal(err)
	}

	_, before := liveState(t, fs, col, normalize.Default)

	if _, err := c.Put(ctx, []Doc{{ID: 3, Text: "zapato rojo"}, {ID: 50, Text: "gorra"}}); err != nil {
		t.Fatal(err)
	}

	var puts atomic.Int32

	hooked := &hookStore{Blob: fs, fail: func(key string) bool {
		return strings.HasSuffix(key, ".idx") && puts.Add(1) == 2
	}}

	broken := New(hooked, col, normalize.Default, testOptions(1))
	if _, err := broken.Compact(ctx); err == nil {
		t.Fatal("the injected failure did not fail the compaction")
	}

	if _, _, err := fs.Get(ctx, col.Lease()); err != nil {
		t.Fatalf("the lease of a failed compaction is gone: %v", err)
	}

	res, err := c.Compact(ctx)
	if err != nil || !res.Full || res.Epoch <= before.Epoch {
		t.Fatalf("recovery: %+v %v (epoch before %d)", res, err, before.Epoch)
	}

	byID, base := liveState(t, fs, col, normalize.Default)
	if len(byID) != 21 || base.Epoch != res.Epoch {
		t.Errorf("after recovery: %d docs, base %+v", len(byID), base)
	}

	for slot := range uint16(key5.Slots) {
		data, _, err := fs.Get(ctx, col.Idx(slot))
		if err != nil {
			continue
		}

		m, _ := bucket.ReadMeta(data)
		if m.Epoch != res.Epoch && base.Touched.Has(int(slot)) {
			t.Errorf("%s: epoch %d, touched by the recovery in %d", col.Idx(slot), m.Epoch, res.Epoch)
		}
	}
}

func TestMinIndexDocs(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		min, docs int
		indexed   bool
	}{
		{100, 99, false}, {100, 100, true}, {1, 0, false}, {1, 1, true}, {5, 4, false}, {0, 0, true},
	} {
		fs, col, c := newTestCollection(t, tc.min)

		var docs []Doc
		for i := range tc.docs {
			docs = append(docs, Doc{ID: uint32(i), Text: "polo"})
		}

		res, err := c.Init(ctx, docs)
		if err != nil {
			t.Fatal(err)
		}

		_, _, tokErr := fs.Get(ctx, col.Tokens())
		if res.Indexed != tc.indexed || (tokErr == nil) != tc.indexed {
			t.Errorf("min %d, %d docs: indexed %v, tokens %v, want indexed %v", tc.min, tc.docs, res.Indexed, tokErr, tc.indexed)
		}
	}
}

// TestIncremental: a compaction rewrites only the buckets of the words that
// changed, says which in the base, and stamps them with its seq.
func TestIncremental(t *testing.T) {
	ctx := context.Background()
	fs, col, c := newTestCollection(t, 1)

	docs := []Doc{
		{ID: 1, Text: "polo rojo"}, {ID: 2, Text: "camisa azul"}, {ID: 3, Text: "zapato negro"},
		{ID: 4, Text: "gorra verde"}, {ID: 5, Text: "bolso marron"},
	}

	if _, err := c.Init(ctx, docs); err != nil {
		t.Fatal(err)
	}

	_, first := liveState(t, fs, col, normalize.Default)

	// 2 changes from "camisa azul" to "camisa celeste"; 4 goes.
	if _, err := c.Apply(ctx, []Doc{{ID: 2, Text: "camisa celeste"}}, []uint32{4}); err != nil {
		t.Fatal(err)
	}

	res, err := c.Compact(ctx)
	if err != nil {
		t.Fatal(err)
	}

	byID, base := liveState(t, fs, col, normalize.Default)
	if len(byID) != 4 || res.Full || base.Epoch != first.Epoch || base.PrevSeq != first.LastSeq || base.LastSeq != 2 {
		t.Fatalf("compaction %+v, base %+v", res, base)
	}

	// Not "ca": camisa is still at the same place of the same document, so
	// its bucket did not change.
	slot := func(w string) int { return int(key5.FromToken(w).Key.Slot()) }
	want := []int{slot("azul"), slot("celeste"), slot("gorra"), slot("verde")}
	slices.Sort(want)
	want = slices.Compact(want)

	var got []int
	for s := range plog.Slots {
		if base.Touched.Has(s) {
			got = append(got, s)
		}
	}

	if !slices.Equal(got, want) || res.Buckets != len(want) {
		t.Errorf("touched %v, want %v (%d rewritten)", got, want, res.Buckets)
	}

	for _, w := range []string{"camisa", "polo", "celeste", "gorra"} {
		data, _, err := fs.Get(ctx, col.Idx(uint16(slot(w))))
		if err != nil {
			t.Fatal(err)
		}

		m, _ := bucket.ReadMeta(data)
		if wantSeq := map[bool]uint64{true: 2, false: 0}[w == "celeste" || w == "gorra"]; m.Seq != wantSeq {
			t.Errorf("%s: seq %d, want %d", col.Idx(uint16(slot(w))), m.Seq, wantSeq)
		}
	}

	// gorra is a tombstone now.
	data, _, _ := fs.Get(ctx, col.Idx(uint16(slot("gorra"))))
	if x, err := bucket.Decode(data, normalize.Default.ID()); err != nil || len(x.Words) != 0 {
		t.Errorf("go.idx: %v, %+v", err, x)
	}
}

// TestCompactionNothing: a compaction with nothing to fold does nothing.
func TestCompactionNothing(t *testing.T) {
	ctx := context.Background()
	fs, _, c := newTestCollection(t, 1)

	if _, err := c.Init(ctx, numbered(0, 5)); err != nil {
		t.Fatal(err)
	}

	counting := &blob.Counting{Blob: fs}
	c.store = counting

	res, err := c.Compact(ctx)
	if err != nil || res.Seq != 0 || counting.BytesWritten.Load() > 64 {
		t.Errorf("compaction of nothing: %+v %v, %d B written", res, err, counting.BytesWritten.Load())
	}
}

func newTestCollection(t *testing.T, minIndex int) (*blob.FS, layout.Collection, *Collection) {
	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	col, _ := layout.New("genix/", "c", "p1")

	return fs, col, New(fs, col, normalize.Default, testOptions(minIndex))
}

func testOptions(minIndex int) Options {
	opts := DefaultOptions()
	opts.MinIndexDocs = minIndex
	opts.CompactSpacing = 0

	return opts
}
