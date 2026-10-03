package collection

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/tokens"
)

// liveState is the live ID → tokens of a collection: its base (tokens or Docs
// frame) with the pending ops of p.log applied.
func liveState(t *testing.T, store blob.Blob, col layout.Collection, norm *normalize.Normalizer) (map[uint32][]byte, plog.Base) {
	t.Helper()

	ctx := context.Background()

	data, _, err := store.Get(ctx, col.Log())
	if err != nil {
		t.Fatal(err)
	}

	log, err := plog.Parse(data, norm.ID())
	if err != nil {
		t.Fatal(err)
	}

	byID := map[uint32][]byte{}

	if log.Base.LogOnly {
		for _, d := range log.Docs {
			byID[d.ID] = d.Tokens
		}
	} else {
		data, _, err := store.Get(ctx, col.Tokens())
		if err != nil {
			t.Fatal(err)
		}

		tok, err := tokens.Decode(data, norm.ID())
		if err != nil {
			t.Fatal(err)
		}

		if tok.Epoch != log.Base.Epoch || tok.Seq != log.Base.LastSeq || len(tok.Docs) != int(log.Base.Docs) {
			t.Fatalf("tokens at epoch %d seq %d with %d docs, the base is %+v", tok.Epoch, tok.Seq, len(tok.Docs), log.Base)
		}

		for _, d := range tok.Docs {
			byID[d.ID] = d.Tokens
		}
	}

	for id, op := range log.Pending() {
		if op.Del {
			delete(byID, id)
		} else {
			byID[id] = op.Tokens
		}
	}

	return byID, log.Base
}

func ids(byID map[uint32][]byte) []uint32 {
	return slices.Sorted(maps.Keys(byID))
}

func sliceSource(docs []Doc) Source {
	return func(_ context.Context, yield func(Doc) error) error {
		for _, d := range docs {
			if err := yield(d); err != nil {
				return err
			}
		}

		return nil
	}
}

func numbered(from, to int) []Doc {
	var docs []Doc
	for i := from; i < to; i++ {
		docs = append(docs, Doc{ID: uint32(i), Text: fmt.Sprintf("producto %d", i)})
	}

	return docs
}

// TestRebuildNew: a rebuild creates a missing collection, with or without
// index.
func TestRebuildNew(t *testing.T) {
	ctx := context.Background()

	for _, n := range []int{3, 10} {
		fs, col, c := newTestCollection(t, 5)

		res, err := c.Rebuild(ctx, sliceSource(numbered(0, n)))
		if err != nil {
			t.Fatal(err)
		}

		byID, base := liveState(t, fs, col, normalize.Default)
		if len(byID) != n || base.LogOnly != (n < 5) || res.Docs != n || res.Indexed != (n >= 5) {
			t.Errorf("%d docs: %d live, base %+v, result %+v", n, len(byID), base, res)
		}
	}
}

// TestRebuildReplaces: what the source does not have goes away, and the
// writes that land while the source is read are kept.
func TestRebuildReplaces(t *testing.T) {
	ctx := context.Background()

	for _, minIndex := range []int{1, 1000} {
		fs, col, c := newTestCollection(t, minIndex)

		if _, err := c.Init(ctx, numbered(0, 20)); err != nil {
			t.Fatal(err)
		}

		if _, err := c.Put(ctx, []Doc{{ID: 100, Text: "pendiente"}}); err != nil {
			t.Fatal(err)
		}

		// The source has 10..30; while it is read, a writer adds 500 and
		// deletes 12.
		source := func(ctx context.Context, yield func(Doc) error) error {
			for i, d := range numbered(10, 30) {
				if i == 5 {
					if _, err := c.Apply(ctx, []Doc{{ID: 500, Text: "durante"}}, []uint32{12}); err != nil {
						return err
					}
				}

				if err := yield(d); err != nil {
					return err
				}
			}

			return nil
		}

		if _, err := c.Rebuild(ctx, source); err != nil {
			t.Fatal(err)
		}

		want := append(slices.DeleteFunc(slices.Collect(func(yield func(uint32) bool) {
			for i := uint32(10); i < 30; i++ {
				yield(i)
			}
		}), func(id uint32) bool { return id == 12 }), 500)

		byID, base := liveState(t, fs, col, normalize.Default)
		if got := ids(byID); !slices.Equal(got, want) {
			t.Errorf("min %d: live %v\nwant %v", minIndex, got, want)
		}

		// The concurrent write stays pending, on top of the rebuilt base.
		data, _, _ := fs.Get(ctx, col.Log())
		log, _ := plog.Parse(data, normalize.Default.ID())

		if len(log.Pending()) != 2 || base.LogOnly != (minIndex > 20) {
			t.Errorf("min %d: pending %v, base %+v", minIndex, log.Pending(), base)
		}

		// A compaction afterwards folds it in.
		if _, err := c.Compact(ctx); err != nil {
			t.Fatal(err)
		}

		if after, _ := liveState(t, fs, col, normalize.Default); !slices.Equal(ids(after), want) {
			t.Errorf("min %d: after compaction %v", minIndex, ids(after))
		}
	}
}

// TestRebuildBlocksCompaction: the lease taken before the source is read
// keeps any compaction, with or without index, from committing meanwhile.
func TestRebuildBlocksCompaction(t *testing.T) {
	ctx := context.Background()

	for _, minIndex := range []int{1, 1000} {
		fs, col, c := newTestCollection(t, minIndex)

		if _, err := c.Init(ctx, numbered(0, 10)); err != nil {
			t.Fatal(err)
		}

		var compactErr error

		source := func(ctx context.Context, yield func(Doc) error) error {
			if _, err := c.Put(ctx, []Doc{{ID: 99, Text: "x"}}); err != nil {
				return err
			}

			_, compactErr = c.Compact(ctx)

			return sliceSource(numbered(0, 10))(ctx, yield)
		}

		if _, err := c.Rebuild(ctx, source); err != nil {
			t.Fatal(err)
		}

		if !errors.Is(compactErr, ErrConflict) {
			t.Errorf("min %d: a compaction during the rebuild: %v, want ErrConflict", minIndex, compactErr)
		}

		// The write landed during the rebuild stays pending.
		if byID, _ := liveState(t, fs, col, normalize.Default); len(byID) != 11 {
			t.Errorf("min %d: %d live documents; want 11", minIndex, len(byID))
		}
	}
}

// TestRebuildWaitsForLease: a fresh lease of another compaction makes the
// rebuild wait and retry, not fail at once.
func TestRebuildWaitsForLease(t *testing.T) {
	ctx := context.Background()

	fs, col, c := newTestCollection(t, 1)

	if _, err := c.Init(ctx, numbered(0, 10)); err != nil {
		t.Fatal(err)
	}

	held := &lease{owner: 1, expiry: time.Now().Add(time.Minute).UnixMilli()}
	if _, err := fs.Put(ctx, col.Lease(), held.encode(), blob.PutCond{IfNoneMatch: true}); err != nil {
		t.Fatal(err)
	}

	time.AfterFunc(100*time.Millisecond, func() { fs.Delete(context.Background(), col.Lease()) })

	res, err := c.Rebuild(ctx, sliceSource(numbered(0, 5)))
	if err != nil || res.Docs != 5 || !res.Full {
		t.Fatalf("rebuild after the lease was released: %+v %v", res, err)
	}
}

// TestRebuildNewNormalizer: a rebuild with other extra letters replaces a log
// written with the old ones, which no write can append to anymore.
func TestRebuildNewNormalizer(t *testing.T) {
	ctx := context.Background()

	for _, minIndex := range []int{1, 1000} {
		fs, col, old := newTestCollection(t, minIndex)

		if _, err := old.Init(ctx, []Doc{{ID: 1, Text: "piña"}, {ID: 2, Text: "pina"}}); err != nil {
			t.Fatal(err)
		}

		if _, err := old.Put(ctx, []Doc{{ID: 3, Text: "niño"}}); err != nil {
			t.Fatal(err)
		}

		norm, err := normalize.New([2]rune{'ñ', 0})
		if err != nil {
			t.Fatal(err)
		}

		c := New(fs, col, norm, testOptions(minIndex))

		if _, err := c.Put(ctx, []Doc{{ID: 4, Text: "x"}}); !errors.Is(err, plog.ErrNormID) {
			t.Fatalf("append with the new normalizer before the rebuild: %v", err)
		}

		if _, err := c.Rebuild(ctx, sliceSource([]Doc{{ID: 1, Text: "piña"}, {ID: 2, Text: "pina"}, {ID: 3, Text: "niño"}})); err != nil {
			t.Fatal(err)
		}

		byID, _ := liveState(t, fs, col, norm)
		if len(byID) != 3 || slices.Equal(byID[1], byID[2]) {
			t.Errorf("min %d: %d docs; piña and pina must differ with ñ as a letter", minIndex, len(byID))
		}

		if _, err := c.Put(ctx, []Doc{{ID: 4, Text: "x"}}); err != nil {
			t.Errorf("min %d: append after the rebuild: %v", minIndex, err)
		}
	}
}

// TestDrop: the index goes away, p.log stays empty and LogOnly, and the
// index comes back in a new epoch.
func TestDrop(t *testing.T) {
	ctx := context.Background()

	fs, col, c := newTestCollection(t, 1)

	if _, err := c.Init(ctx, numbered(0, 10)); err != nil {
		t.Fatal(err)
	}

	if _, err := c.Put(ctx, numbered(10, 12)); err != nil {
		t.Fatal(err)
	}

	first, err := c.Compact(ctx)
	if err != nil || first.Seq != 2 {
		t.Fatalf("compact: %+v %v", first, err)
	}

	if _, err := c.Drop(ctx); err != nil {
		t.Fatal(err)
	}

	byID, base := liveState(t, fs, col, normalize.Default)
	if len(byID) != 0 || !base.LogOnly || base.Epoch <= first.Epoch {
		t.Errorf("after drop: %d docs, base %+v", len(byID), base)
	}

	for _, key := range []string{col.Tokens(), col.Idx(key5.FromToken("producto").Key.Slot()), col.DB(key5.FromToken("producto").Key.Slot())} {
		if _, _, err := fs.Get(ctx, key); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("%s survived the drop: %v", key, err)
		}
	}

	if _, err := c.Put(ctx, numbered(0, 3)); err != nil {
		t.Fatal(err)
	}

	if res, err := c.Compact(ctx); err != nil || !res.Indexed || res.Epoch <= base.Epoch {
		t.Errorf("first compaction after the drop: %+v %v; want a new epoch", res, err)
	}
}
