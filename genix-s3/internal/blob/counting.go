package blob

import (
	"context"
	"sync/atomic"
)

// Counting wraps a Blob and counts requests and bytes, read and written:
// how the tests and benchmarks measure what a query or a write costs.
type Counting struct {
	Blob
	Requests     atomic.Int64 // reads: GetRange and Get
	BytesRead    atomic.Int64
	Writes       atomic.Int64 // Put, Append and Delete
	BytesWritten atomic.Int64
}

func (c *Counting) GetRange(ctx context.Context, key string, off, n int64) ([]byte, error) {
	data, err := c.Blob.GetRange(ctx, key, off, n)
	c.Requests.Add(1)
	c.BytesRead.Add(int64(len(data)))

	return data, err
}

func (c *Counting) Get(ctx context.Context, key string) ([]byte, string, error) {
	data, etag, err := c.Blob.Get(ctx, key)
	c.Requests.Add(1)
	c.BytesRead.Add(int64(len(data)))

	return data, etag, err
}

func (c *Counting) Put(ctx context.Context, key string, data []byte, cond PutCond) (string, error) {
	c.Writes.Add(1)
	c.BytesWritten.Add(int64(len(data)))

	return c.Blob.Put(ctx, key, data, cond)
}

func (c *Counting) Append(ctx context.Context, key string, data []byte, offset int64) (int64, error) {
	c.Writes.Add(1)
	c.BytesWritten.Add(int64(len(data)))

	return c.Blob.Append(ctx, key, data, offset)
}

func (c *Counting) Delete(ctx context.Context, keys ...string) error {
	c.Writes.Add(1)

	return c.Blob.Delete(ctx, keys...)
}

// Reset zeroes the counters.
func (c *Counting) Reset() {
	c.Requests.Store(0)
	c.BytesRead.Store(0)
	c.Writes.Store(0)
	c.BytesWritten.Store(0)
}
