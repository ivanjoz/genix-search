// Package blobtest is the contract every blob.Blob must meet: the semantics
// the writer and the compaction rely on. It runs against the
// local store in unit tests and against a real bucket in the S3 integration
// test, so both are held to the same rules.
package blobtest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
)

// Run checks store against the contract. Every key it writes is under prefix
// and is deleted at the end, so a shared bucket can be used.
func Run(t *testing.T, store blob.Blob, prefix string) {
	ctx := context.Background()
	key := func(name string) string { return prefix + name }

	t.Cleanup(func() {
		names := []string{"cond", "range", "missing", "delete-a", "delete-b", "race-append", "race-create", "if-match-missing"}
		keys := make([]string, len(names))

		for i, name := range names {
			keys[i] = key(name)
		}

		if err := store.Delete(context.Background(), keys...); err != nil {
			t.Errorf("cleanup: %v", err)
		}
	})

	t.Run("ConditionalPutAndAppend", func(t *testing.T) {
		k := key("cond")

		tag, err := store.Put(ctx, k, []byte("abc"), blob.PutCond{IfNoneMatch: true})
		if err != nil {
			t.Fatal(err)
		}

		if _, err := store.Put(ctx, k, []byte("x"), blob.PutCond{IfNoneMatch: true}); !errors.Is(err, blob.ErrPrecondition) {
			t.Errorf("second create: %v", err)
		}

		if _, err := store.Append(ctx, k, []byte("de"), 2); !errors.Is(err, blob.ErrOffsetMismatch) {
			t.Errorf("stale append: %v", err)
		}

		size, err := store.Append(ctx, k, []byte("de"), 3)
		if err != nil || size != 5 {
			t.Fatalf("append: %d %v", size, err)
		}

		if _, err := store.Put(ctx, k, []byte("new"), blob.PutCond{IfMatch: tag}); !errors.Is(err, blob.ErrPrecondition) {
			t.Errorf("If-Match with the etag from before the append: %v", err)
		}

		data, current, err := store.Get(ctx, k)
		if err != nil || string(data) != "abcde" {
			t.Fatalf("get: %q %v", data, err)
		}

		if _, err := store.Put(ctx, k, []byte("new"), blob.PutCond{IfMatch: current}); err != nil {
			t.Errorf("If-Match with the current etag: %v", err)
		}

		if data, _, err := store.Get(ctx, k); err != nil || string(data) != "new" {
			t.Errorf("after If-Match put: %q %v", data, err)
		}
	})

	t.Run("IfMatchOnMissing", func(t *testing.T) {
		if _, err := store.Put(ctx, key("if-match-missing"), []byte("x"), blob.PutCond{IfMatch: `"0"`}); !errors.Is(err, blob.ErrPrecondition) {
			t.Errorf("If-Match on a missing key: %v", err)
		}
	})

	t.Run("Range", func(t *testing.T) {
		k := key("range")

		etag, err := store.Put(ctx, k, []byte("0123456789"), blob.PutCond{})
		if err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			off, n int64
			want   string
		}{
			{0, 3, "012"},
			{4, 2, "45"},
			{7, 10, "789"}, // past the end: clamped
			{6, -1, "6789"},
			{10, -1, ""}, // at the end: empty, not an error
			{3, 0, ""},
		}

		for _, c := range cases {
			got, tag, err := store.GetRange(ctx, k, c.off, c.n)
			if err != nil || string(got) != c.want || tag != etag {
				t.Errorf("GetRange(%d, %d) = %q, %q, %v; want %q, %q", c.off, c.n, got, tag, err, c.want, etag)
			}
		}

		if _, _, err := store.GetRange(ctx, k, 11, -1); err == nil || errors.Is(err, blob.ErrNotFound) {
			t.Errorf("range past the end: %v", err)
		}

		// After a rewrite, a range carries the new ETag.
		etag2, err := store.Put(ctx, k, []byte("abcdefghij"), blob.PutCond{})
		if err != nil {
			t.Fatal(err)
		}

		if got, tag, err := store.GetRange(ctx, k, 1, 2); err != nil || string(got) != "bc" || tag != etag2 || tag == etag {
			t.Errorf("GetRange after a rewrite = %q, %q, %v; want \"bc\", %q", got, tag, err, etag2)
		}
	})

	t.Run("Missing", func(t *testing.T) {
		k := key("missing")

		if _, _, err := store.Get(ctx, k); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("Get: %v", err)
		}

		if _, _, err := store.GetRange(ctx, k, 0, 4); !errors.Is(err, blob.ErrNotFound) {
			t.Errorf("GetRange: %v", err)
		}

		// plog never appends to a log it has not read, so either answer makes
		// it re-read and stop on ErrNotFound.
		if _, err := store.Append(ctx, k, []byte("x"), 4); !errors.Is(err, blob.ErrNotFound) && !errors.Is(err, blob.ErrOffsetMismatch) {
			t.Errorf("Append: %v", err)
		}

		if err := store.Delete(ctx, k); err != nil {
			t.Errorf("Delete: %v", err)
		}
	})

	t.Run("Delete", func(t *testing.T) {
		a, b := key("delete-a"), key("delete-b")

		for _, k := range []string{a, b} {
			if _, err := store.Put(ctx, k, []byte("x"), blob.PutCond{}); err != nil {
				t.Fatal(err)
			}
		}

		if err := store.Delete(ctx, a, b, key("missing")); err != nil {
			t.Fatal(err)
		}

		for _, k := range []string{a, b} {
			if _, _, err := store.Get(ctx, k); !errors.Is(err, blob.ErrNotFound) {
				t.Errorf("%s after delete: %v", k, err)
			}
		}
	})

	// Concurrent appends at the same offset are what serialises the writers of
	// p.log: exactly one may land.
	t.Run("AppendRace", func(t *testing.T) {
		k := key("race-append")

		if _, err := store.Put(ctx, k, []byte("base"), blob.PutCond{}); err != nil {
			t.Fatal(err)
		}

		won, err := race(func(i int) error {
			_, err := store.Append(ctx, k, []byte(fmt.Sprintf("<%d>", i)), 4)
			if errors.Is(err, blob.ErrOffsetMismatch) {
				return errLost
			}

			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		if won != 1 {
			t.Errorf("%d appends at the same offset landed; want 1", won)
		}

		if data, _, err := store.Get(ctx, k); err != nil || len(data) != 7 {
			t.Errorf("after the race: %q %v", data, err)
		}
	})

	// Concurrent creates are how a compaction claims a generation.
	t.Run("CreateRace", func(t *testing.T) {
		k := key("race-create")

		won, err := race(func(i int) error {
			_, err := store.Put(ctx, k, []byte(fmt.Sprint(i)), blob.PutCond{IfNoneMatch: true})
			if errors.Is(err, blob.ErrPrecondition) {
				return errLost
			}

			return err
		})
		if err != nil {
			t.Fatal(err)
		}

		if won != 1 {
			t.Errorf("%d creates of the same key succeeded; want 1", won)
		}
	})
}

var errLost = errors.New("lost the race")

// race runs attempt concurrently and counts the winners; an attempt reports a
// loss with errLost and anything else is a failure.
func race(attempt func(i int) error) (won int, err error) {
	const writers = 8

	errs := make([]error, writers)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Go(func() { errs[i] = attempt(i) })
	}

	wg.Wait()

	for _, e := range errs {
		switch {
		case e == nil:
			won++
		case !errors.Is(e, errLost):
			return won, e
		}
	}

	return won, nil
}
