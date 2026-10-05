package plog

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
)

func readLog(t *testing.T, store blob.Blob, key string) *Log {
	t.Helper()

	data, _, err := store.Get(context.Background(), key)
	if err != nil {
		t.Fatal(err)
	}

	log, err := Parse(data, normID)
	if err != nil {
		t.Fatal(err)
	}

	return log
}

// TestCreate: with Create, the first write of a missing log creates it,
// LogOnly and empty, with the write in it, in one put. Deletes alone leave it
// missing.
func TestCreate(t *testing.T) {
	ctx := context.Background()

	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	store := &blob.Counting{Blob: fs}
	opts := AppendOptions{CompactBytes: DefaultCompactBytes, MinIndexDocs: 3, Create: true}

	res, err := Append(ctx, store, "p.log", normID, []Change{{ID: 1, Del: true}}, opts)
	if err != nil || res.Size != 0 {
		t.Fatalf("delete on a missing log: %+v %v", res, err)
	}

	if _, _, err := fs.Get(ctx, "p.log"); !errors.Is(err, blob.ErrNotFound) {
		t.Fatalf("a delete created the log: %v", err)
	}

	store.Reset()

	changes := []Change{{ID: 1, Tokens: tokens("polo")}, {ID: 2, Tokens: tokens("casaca")}, {ID: 9, Del: true}}

	res, err = Append(ctx, store, "p.log", normID, changes, opts)
	if err != nil {
		t.Fatal(err)
	}

	if res.FirstSeq != 1 || res.LastSeq != 3 || res.CompactRequested {
		t.Errorf("create: %+v", res)
	}

	if r, w := store.Requests.Load(), store.Writes.Load(); r != 1 || w != 1 {
		t.Errorf("create took %d reads and %d writes; want 1 and 1", r, w)
	}

	log := readLog(t, fs, "p.log")
	if !log.Base.LogOnly || len(log.Docs) != 0 || len(log.Ops) != 3 || log.LiveDocs() != 2 {
		t.Errorf("created log: %+v", log)
	}

	// The third document reaches MinIndexDocs: the append asks for the index.
	res, err = Append(ctx, store, "p.log", normID, []Change{{ID: 3, Tokens: tokens("gorra")}}, opts)
	if err != nil || !res.CompactRequested {
		t.Fatalf("third document: %+v %v", res, err)
	}

	// Without Create a missing log is an error, as before.
	if _, err := Append(ctx, store, "other.log", normID, changes, AppendOptions{}); !errors.Is(err, blob.ErrNotFound) {
		t.Errorf("append to a missing log without Create: %v", err)
	}
}

// TestCreateRace: writers that find the log missing at once all land; the
// ones that lose the create append to the winner's log.
func TestCreateRace(t *testing.T) {
	ctx := context.Background()

	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	opts := AppendOptions{CompactBytes: DefaultCompactBytes, Create: true}

	const writers = 8

	var wg sync.WaitGroup

	errs := make([]error, writers)

	for i := range writers {
		wg.Go(func() {
			_, errs[i] = Append(ctx, store, "p.log", normID, []Change{{ID: docid.Key(i + 1), Tokens: tokens("polo")}}, opts)
		})
	}

	wg.Wait()

	if err := errors.Join(errs...); err != nil {
		t.Fatal(err)
	}

	if live := readLog(t, store, "p.log").LiveDocs(); live != writers {
		t.Errorf("%d documents in the log; want %d", live, writers)
	}
}

// TestStaleCompact: a pending request is not repeated while fresh; once older
// than StaleCompact it is taken as lost and the next append asks again.
func TestStaleCompact(t *testing.T) {
	ctx := context.Background()

	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	now := time.UnixMilli(1_000_000)
	opts := AppendOptions{CompactBytes: 1, Create: true, StaleCompact: time.Minute, Now: func() time.Time { return now }}

	put := func(id docid.Key) AppendResult {
		t.Helper()

		res, err := Append(ctx, store, "p.log", normID, []Change{{ID: id, Tokens: tokens("polo")}}, opts)
		if err != nil {
			t.Fatal(err)
		}

		return res
	}

	if !put(1).CompactRequested {
		t.Fatal("the first append past CompactBytes did not ask")
	}

	now = now.Add(30 * time.Second)
	if put(2).CompactRequested {
		t.Error("a fresh pending request was asked again")
	}

	now = now.Add(31 * time.Second)
	if !put(3).CompactRequested {
		t.Error("a request older than StaleCompact was not asked again")
	}

	if put(4).CompactRequested {
		t.Error("the new request was asked again right away")
	}

	opts.StaleCompact = 0
	now = now.Add(time.Hour)

	if put(5).CompactRequested {
		t.Error("StaleCompact 0 asked twice")
	}
}
