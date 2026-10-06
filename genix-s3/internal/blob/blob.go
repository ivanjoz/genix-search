// Package blob is the object store under the engine: S3 Express One Zone in
// production, a directory on disk for tests and the CLI.
//
// The interface is exactly the subset of S3 the design relies on:
// range reads, conditional puts, and the native append of
// directory buckets, whose write offset is what serialises concurrent writers.
package blob

import (
	"context"
	"errors"
)

var (
	// ErrNotFound is returned for a key that does not exist.
	ErrNotFound = errors.New("blob: not found")
	// ErrPrecondition is S3's 412: an If-Match or If-None-Match did not hold.
	ErrPrecondition = errors.New("blob: precondition failed")
	// ErrOffsetMismatch is an append whose offset is not the object's size:
	// somebody else wrote first.
	ErrOffsetMismatch = errors.New("blob: append offset mismatch")
)

// PutCond is the condition of a Put. The zero value is unconditional.
type PutCond struct {
	IfNoneMatch bool   // create only
	IfMatch     string // replace only if the current ETag is this
}

// Blob is an object store.
type Blob interface {
	// GetRange reads n bytes at off; n < 0 reads to the end. The ETag is the
	// whole object's, so a reader can tell the range came from the version it
	// expected.
	GetRange(ctx context.Context, key string, off, n int64) (data []byte, etag string, err error)
	Get(ctx context.Context, key string) (data []byte, etag string, err error)
	Put(ctx context.Context, key string, data []byte, cond PutCond) (etag string, err error)
	// Append writes data at offset, which must equal the object's size.
	Append(ctx context.Context, key string, data []byte, offset int64) (newSize int64, err error)
	Delete(ctx context.Context, keys ...string) error
}
