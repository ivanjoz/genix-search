package collection

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
)

// ErrConflict is another compaction or rebuild holding the lease.
var ErrConflict = errors.New("collection: another compaction is in progress")

// The lease is compact.lease: magic "GXC1" | owner u64 | expiry ms i64 |
// epoch u32. Only its holder writes buckets. A compaction that ends,
// successfully or not, before writing any bucket deletes it; one that dies or
// fails after leaves it, and whoever takes it over once it expires rewrites
// every bucket in an epoch above the one it names.
const (
	leaseMagic = "GXC1"
	leaseSize  = 24
)

type lease struct {
	owner  uint64
	expiry int64
	// epoch is the highest epoch the holder may have written buckets in.
	epoch uint32
	etag  string
	// recover is set when the lease was taken over from a holder that did
	// not finish: the buckets may not match tokens.
	recover bool
	// wrote is set once a bucket was written.
	wrote bool
}

func (l *lease) encode() []byte {
	out := []byte(leaseMagic)
	out = binary.LittleEndian.AppendUint64(out, l.owner)
	out = binary.LittleEndian.AppendUint64(out, uint64(l.expiry))

	return binary.LittleEndian.AppendUint32(out, l.epoch)
}

func decodeLease(data []byte) (*lease, error) {
	if len(data) != leaseSize || string(data[:4]) != leaseMagic {
		return nil, errors.New("collection: corrupt compact.lease")
	}

	return &lease{
		owner:  binary.LittleEndian.Uint64(data[4:]),
		expiry: int64(binary.LittleEndian.Uint64(data[12:])),
		epoch:  binary.LittleEndian.Uint32(data[20:]),
	}, nil
}

// acquire takes the lease: creates it, or takes over an expired one.
func (c *Collection) acquire(ctx context.Context, res *CompactResult) (*lease, error) {
	l := &lease{owner: rand.Uint64(), expiry: c.opts.Now().Add(c.opts.LeaseTimeout).UnixMilli()}

	for range 4 {
		etag, err := c.store.Put(ctx, c.col.Lease(), l.encode(), blob.PutCond{IfNoneMatch: true})
		res.Writes++

		if err == nil {
			l.etag = etag

			return l, nil
		}

		if !errors.Is(err, blob.ErrPrecondition) {
			return nil, fmt.Errorf("collection: lease: %w", err)
		}

		data, etag, err := c.store.Get(ctx, c.col.Lease())
		res.Requests++

		if errors.Is(err, blob.ErrNotFound) {
			continue // released meanwhile
		}

		if err != nil {
			return nil, fmt.Errorf("collection: lease: %w", err)
		}

		held, err := decodeLease(data)
		if err != nil {
			return nil, err
		}

		if c.opts.Now().UnixMilli() < held.expiry {
			return nil, ErrConflict
		}

		l.epoch, l.recover = held.epoch, true

		etag, err = c.store.Put(ctx, c.col.Lease(), l.encode(), blob.PutCond{IfMatch: etag})
		res.Writes++

		if errors.Is(err, blob.ErrPrecondition) {
			return nil, ErrConflict // somebody else took it over first
		}

		if err != nil {
			return nil, fmt.Errorf("collection: lease: %w", err)
		}

		l.etag = etag

		return l, nil
	}

	return nil, ErrConflict
}

// claimEpoch records in the lease the epoch the holder is about to write
// buckets in, before it writes any.
func (c *Collection) claimEpoch(ctx context.Context, l *lease, epoch uint32, res *CompactResult) error {
	if epoch <= l.epoch {
		return nil
	}

	next := *l
	next.epoch = epoch

	etag, err := c.store.Put(ctx, c.col.Lease(), next.encode(), blob.PutCond{IfMatch: l.etag})
	res.Writes++

	if errors.Is(err, blob.ErrPrecondition) {
		return ErrConflict // our lease expired and was taken over
	}

	if err != nil {
		return fmt.Errorf("collection: lease: %w", err)
	}

	l.epoch, l.etag = epoch, etag

	return nil
}

// release ends the lease. After a failure that left buckets written it is
// kept, expired, so the next compaction takes it over and rewrites them all.
func (c *Collection) release(ctx context.Context, l *lease, failed bool, res *CompactResult) {
	ctx = context.WithoutCancel(ctx)

	if failed && l.wrote {
		l.expiry = 0
		if etag, err := c.store.Put(ctx, c.col.Lease(), l.encode(), blob.PutCond{IfMatch: l.etag}); err == nil {
			l.etag = etag
		}

		res.Writes++

		return
	}

	// Delete it only while it is still ours.
	if _, etag, err := c.store.Get(ctx, c.col.Lease()); err == nil && etag == l.etag {
		_ = c.store.Delete(ctx, c.col.Lease())
		res.Writes++
	}

	res.Requests++
}

// waitSpacing sleeps until CompactSpacing has passed since unixMs, when the
// last compaction wrote its base.
func (c *Collection) waitSpacing(ctx context.Context, unixMs int64) error {
	wait := time.UnixMilli(unixMs).Add(c.opts.CompactSpacing).Sub(c.opts.Now())
	if wait <= 0 {
		return nil
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
