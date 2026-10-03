package collection

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/manifest"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
)

// Source streams every document of the collection from the system of record:
// it calls yield once per document and returns the first error yield
// returns. Rebuild may call it more than once.
type Source func(ctx context.Context, yield func(Doc) error) error

// rebuildAttempts bounds the restarts when a compaction commits during a
// rebuild.
const rebuildAttempts = 4

// Rebuild replaces the collection with the documents of source: the way to
// reprocess it from the system of record, also after a change of normalizer
// or of the extra letters, which keys cannot survive (PLAN_5BIT.md §5.4).
//
//  1. Read p.log and note its last seq, S0, before reading the source.
//  2. Claim the next generation, as a compaction does, so no compaction
//     commits while the source is read.
//  3. Read the source and build a generation (or a LogOnly p.log) from it.
//  4. Rebase p.log on it with If-Match, keeping the ops after S0.
//
// Writers append after their write to the source commits, so an op at or
// before S0 is already in what the source returns, and the ops after it are
// kept and applied on top; they are idempotent, so applying one the source
// already had changes nothing. A compaction that commits anyway (a LogOnly
// one claims nothing) makes the rebuild start over.
func (c *Collection) Rebuild(ctx context.Context, source Source) (CompactResult, error) {
	return c.rebuildRetrying(ctx, source, false)
}

// Drop empties the collection and deletes its index. p.log stays, as a LogOnly
// log without documents, so the generation numbers go on from where they were;
// a query in flight on the deleted generation finds it gone and reads p.log
// again.
func (c *Collection) Drop(ctx context.Context) (CompactResult, error) {
	empty := func(context.Context, func(Doc) error) error { return nil }

	return c.rebuildRetrying(ctx, empty, true)
}

func (c *Collection) rebuildRetrying(ctx context.Context, source Source, drop bool) (CompactResult, error) {
	var res CompactResult

	for attempt := range rebuildAttempts {
		err := c.rebuild(ctx, source, drop, &res)
		if !errors.Is(err, ErrConflict) {
			return res, err
		}

		// A compaction is running: it takes a fraction of a second at 100k
		// documents (PLAN_5BIT.md §13.4).
		timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)

		select {
		case <-ctx.Done():
			timer.Stop()
			return res, ctx.Err()
		case <-timer.C:
		}
	}

	return res, ErrConflict
}

func (c *Collection) rebuild(ctx context.Context, source Source, drop bool, res *CompactResult) error {
	normID := c.norm.ID()

	data, _, err := c.store.Get(ctx, c.col.Log())
	res.Requests++

	exists := !errors.Is(err, blob.ErrNotFound)
	if err != nil && exists {
		return fmt.Errorf("collection: p.log: %w", err)
	}

	var (
		base  plog.Base
		since uint64
	)

	if exists {
		if base, err = plog.ParseBase(data); err != nil {
			return err
		}

		since = base.LastSeq

		// The ops of a log of another normalizer are dropped: only its base
		// matters. Of our own log, S0 is its last op.
		if base.NormID == normID {
			log, err := plog.Parse(data, normID)
			if err != nil {
				return err
			}

			since = log.LastSeq()
		}
	}

	// Claim the generation the rebuild may publish, before the source is read.
	// Without a p.log there is nothing to compact, and nothing to claim yet.
	var gen uint32

	if exists {
		if gen, err = c.freeGeneration(ctx, base.ManifestGen, res); err != nil {
			return err
		}

		claim, err := manifest.Encode(&manifest.Manifest{Gen: gen, NormID: normID, CreatedUnixMs: c.opts.Now().UnixMilli()})
		if err != nil {
			return err
		}

		if _, err := c.store.Put(ctx, c.col.Manifest(gen), claim, blob.PutCond{IfNoneMatch: true}); err != nil {
			if errors.Is(err, blob.ErrPrecondition) {
				return ErrConflict
			}

			return fmt.Errorf("collection: claim %d: %w", gen, err)
		}

		res.Writes++
	}

	release := func(cause error) error {
		if gen == 0 {
			return cause
		}

		keys := append(c.col.Objects(gen), c.col.Manifest(gen))

		return errors.Join(cause, c.store.Delete(context.WithoutCancel(ctx), keys...))
	}

	byID := make(map[uint32][]byte)

	err = source(ctx, func(doc Doc) error {
		byID[doc.ID] = c.Tokens(doc.Text) // a repeated ID keeps its last version

		return nil
	})
	if err != nil {
		return release(fmt.Errorf("collection: rebuild source: %w", err))
	}

	res.Docs = len(byID)

	var start []byte

	if c.indexed(len(byID)) {
		if gen == 0 {
			gen = 1 // a new collection: Init's generation
		}

		g, err := c.encode(gen, since, byID)
		if err != nil {
			return release(err)
		}

		// Our claim is replaced by the real manifest. For a new collection the
		// put is a claim of its own.
		cond := blob.PutCond{IfNoneMatch: !exists}
		if _, err := c.store.Put(ctx, c.col.Manifest(gen), g.manifestBytes, cond); err != nil {
			if errors.Is(err, blob.ErrPrecondition) {
				return ErrConflict
			}

			return release(err)
		}

		if err := c.putObjects(ctx, g); err != nil {
			return release(err)
		}

		res.Writes += 4
		res.Bytes += int64(len(g.manifestBytes) + len(g.k) + len(g.p) + len(g.snap))
		res.Gen = gen
		start = plog.EncodeBase(plog.Base{NormID: normID, ManifestGen: gen, LastSeq: since, UnixMs: c.opts.Now().UnixMilli()})
	} else {
		// No index: the claim is not needed, and the log keeps the last
		// generation the collection had.
		if err := release(nil); err != nil {
			return err
		}

		gen = 0

		if start, err = c.logOnlyBase(base.ManifestGen, since, byID); err != nil {
			return err
		}
	}

	if !exists {
		if _, err := c.store.Put(ctx, c.col.Log(), start, blob.PutCond{IfNoneMatch: true}); err != nil {
			if errors.Is(err, blob.ErrPrecondition) {
				// A writer created the collection meanwhile.
				return release(ErrConflict)
			}

			return release(fmt.Errorf("collection: create p.log: %w", err))
		}

		res.Writes++
		res.Bytes += int64(len(start))

		return nil
	}

	if err := c.rebase(ctx, base, start, since, res); err != nil {
		if errors.Is(err, errContention) {
			err = ErrConflict
		}

		return release(err)
	}

	c.collect(ctx, base, res)

	if drop && !base.LogOnly && base.ManifestGen > 0 {
		if err := c.store.Delete(ctx, append(c.col.Objects(base.ManifestGen), c.col.Manifest(base.ManifestGen))...); err == nil {
			res.Writes++
		}
	}

	return nil
}
