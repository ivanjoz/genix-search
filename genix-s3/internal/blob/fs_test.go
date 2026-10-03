package blob

import (
	"context"
	"errors"
	"testing"
)

func TestFSConditionalPutAndAppend(t *testing.T) {
	ctx := context.Background()
	store, err := NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	tag, err := store.Put(ctx, "a/p.log", []byte("abc"), PutCond{IfNoneMatch: true})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Put(ctx, "a/p.log", []byte("x"), PutCond{IfNoneMatch: true}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("second create: %v", err)
	}

	if _, err := store.Append(ctx, "a/p.log", []byte("de"), 2); !errors.Is(err, ErrOffsetMismatch) {
		t.Errorf("stale append: %v", err)
	}

	size, err := store.Append(ctx, "a/p.log", []byte("de"), 3)
	if err != nil || size != 5 {
		t.Fatalf("append: %d %v", size, err)
	}

	if _, err := store.Put(ctx, "a/p.log", []byte("new"), PutCond{IfMatch: tag}); !errors.Is(err, ErrPrecondition) {
		t.Errorf("If-Match with stale etag: %v", err)
	}

	data, current, err := store.Get(ctx, "a/p.log")
	if err != nil || string(data) != "abcde" {
		t.Fatalf("get: %q %v", data, err)
	}

	if _, err := store.Put(ctx, "a/p.log", []byte("new"), PutCond{IfMatch: current}); err != nil {
		t.Errorf("If-Match with current etag: %v", err)
	}

	part, err := store.GetRange(ctx, "a/p.log", 1, 10)
	if err != nil || string(part) != "ew" {
		t.Errorf("range: %q %v", part, err)
	}

	if _, _, err := store.Get(ctx, "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}
