package collection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
)

// Source streams every document of the collection from the system of record:
// it calls yield once per document and returns the first error yield
// returns. Rebuild may call it more than once.
type Source func(ctx context.Context, yield func(Doc) error) error

// rebuildAttempts bounds the restarts when a compaction holds the lease.
const rebuildAttempts = 4

// Rebuild replaces the collection with the documents of source: the way to
// reprocess it from the system of record, also after a change of normalizer
// or of the extra letters, which keys cannot survive.
//
//  1. Take the lease, so no compaction commits meanwhile.
//  2. Read p.log and note its last seq, S0, before reading the source.
//  3. Read the source and write every bucket in a new epoch (or a LogOnly
//     p.log under MinIndexDocs).
//  4. Rebase p.log on it with If-Match, keeping the ops after S0.
//
// Writers append after their write to the source commits, so an op at or
// before S0 is already in what the source returns, and the ops after it are
// kept and applied on top; they are idempotent, so applying one the source
// already had changes nothing.
func (c *Collection) Rebuild(ctx context.Context, source Source) (CompactResult, error) {
	return c.rebuildRetrying(ctx, source, false)
}

// Drop empties the collection and deletes its index. p.log stays, as a LogOnly
// log without documents.
func (c *Collection) Drop(ctx context.Context) (CompactResult, error) {
	empty := func(context.Context, func(Doc) error) error { return nil }

	return c.rebuildRetrying(ctx, empty, true)
}

func (c *Collection) rebuildRetrying(ctx context.Context, source Source, drop bool) (CompactResult, error) {
	for attempt := range rebuildAttempts {
		res, err := c.rebuild(ctx, source, drop)
		if !errors.Is(err, ErrConflict) || attempt == rebuildAttempts-1 {
			return res, err
		}

		// A compaction is running: it takes a fraction of a second at 100k
		// documents.
		timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)

		select {
		case <-ctx.Done():
			timer.Stop()
			return res, ctx.Err()
		case <-timer.C:
		}
	}

	return CompactResult{}, ErrConflict
}

func (c *Collection) rebuild(ctx context.Context, source Source, drop bool) (res CompactResult, err error) {
	l, err := c.acquire(ctx, &res)
	if err != nil {
		return res, err
	}

	defer func() { c.release(ctx, l, err != nil, &res) }()

	st, err := c.readAnyState(ctx, &res)
	if err != nil {
		return res, err
	}

	if st.log == nil {
		// A new collection: start it empty, then rebuild it like any other.
		empty, err := c.logOnlyLog(plog.Base{NormID: c.norm.ID(), UnixMs: c.opts.Now().UnixMilli()}, nil)
		if err != nil {
			return res, err
		}

		if _, err := c.store.Put(ctx, c.col.Log(), empty, blob.PutCond{IfNoneMatch: true}); err != nil {
			if errors.Is(err, blob.ErrPrecondition) {
				return res, ErrConflict // a writer created it meanwhile
			}

			return res, fmt.Errorf("collection: create p.log: %w", err)
		}

		res.Writes++

		if st, err = c.readAnyState(ctx, &res); err != nil {
			return res, err
		}
	}

	// S0: of our own log, its last op; the ops of a log of another normalizer
	// are dropped, only its base matters.
	since := st.log.Base.LastSeq
	if st.log.Base.NormID == c.norm.ID() {
		since = st.log.LastSeq()
	}

	byID := make(map[docid.Key][]byte)

	err = source(ctx, func(doc Doc) error {
		byID[doc.Key()] = c.Tokens(doc) // a repeated document keeps its last version

		return nil
	})
	if err != nil {
		return res, fmt.Errorf("collection: rebuild source: %w", err)
	}

	if err := c.publish(ctx, l, st, byID, since, true, &res); err != nil {
		return res, err
	}

	// Drop deletes the index now, once no query reads it: a query that read
	// the old base finished, or will retry, within CompactSpacing.
	if drop && st.tok != nil {
		if err := c.waitSpacing(ctx, c.opts.Now().UnixMilli()); err != nil {
			return res, err
		}

		c.dropIndex(ctx, st.tok, &res)
	}

	return res, nil
}

// readAnyState is readState for a rebuild: a log of another normalizer is
// read for its base alone.
func (c *Collection) readAnyState(ctx context.Context, res *CompactResult) (*state, error) {
	st, err := c.readState(ctx, res)
	if !errors.Is(err, plog.ErrNormID) {
		return st, err
	}

	data, _, err := c.store.Get(ctx, c.col.Log())
	res.Requests++

	if err != nil {
		return nil, fmt.Errorf("collection: p.log: %w", err)
	}

	base, err := plog.ParseBase(data)
	if err != nil {
		return nil, err
	}

	return &state{log: &plog.Log{Base: base}}, nil
}
