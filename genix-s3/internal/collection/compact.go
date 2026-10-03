package collection

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/manifest"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/snapshot"
)

// ErrConflict is another compaction that owns the next generation.
var ErrConflict = errors.New("collection: another compaction is in progress")

// CompactResult says what a compaction did.
type CompactResult struct {
	Gen      uint32 // the new generation; 0 if there was nothing to fold
	Ops      int    // p.log ops folded in
	Docs     int
	Requests int64 // reads
	Writes   int64 // puts and deletes
	Bytes    int64 // written
}

// Compact folds the pending ops of p.log into a new generation:
//
//  1. Read p.log, the base manifest and its snapshot.
//  2. Apply the ops to the snapshot's ID → tokens state, build k, p and the
//     snapshot of the next generation.
//  3. Claim the generation: put its manifest with If-None-Match, so only one
//     compaction writes objects with that number.
//  4. Write k, p and the snapshot.
//  5. Rebase p.log on the new generation with If-Match: the commit. Ops
//     appended meanwhile are carried over.
//  6. Delete the generations no query can be reading anymore.
//
// Under MinIndexDocs there is no index: the state lives in the Docs frame of
// p.log, and the compaction only rebases p.log with the new state (steps 1, 2
// and 5). Crossing the threshold either way switches the mode.
func (c *Collection) Compact(ctx context.Context) (CompactResult, error) {
	var res CompactResult

	normID := c.norm.ID()

	data, _, err := c.store.Get(ctx, c.col.Log())
	res.Requests++

	if err != nil {
		return res, fmt.Errorf("collection: p.log: %w", err)
	}

	log, err := plog.Parse(data, normID)
	if err != nil {
		return res, err
	}

	pending := log.Pending()
	if len(pending) == 0 {
		return res, nil
	}

	base, target := log.Base, log.LastSeq()
	byID := make(map[uint32][]byte)

	if base.LogOnly {
		for _, doc := range log.Docs {
			byID[doc.ID] = doc.Tokens
		}
	} else {
		snapData, _, err := c.store.Get(ctx, c.col.Snapshot(base.ManifestGen))
		res.Requests++

		if err != nil {
			return res, fmt.Errorf("collection: snapshot %d: %w", base.ManifestGen, err)
		}

		_, docs, err := snapshot.Decode(snapData, normID)
		if err != nil {
			return res, err
		}

		for _, doc := range docs {
			byID[doc.ID] = doc.Tokens
		}
	}

	for id, op := range pending {
		if op.Del {
			delete(byID, id)
		} else {
			byID[id] = op.Tokens
		}
	}

	res.Ops, res.Docs = len(pending), len(byID)

	if !c.indexed(len(byID)) {
		start, err := c.logOnlyBase(base.ManifestGen, target, byID)
		if err != nil {
			return res, err
		}

		if err := c.rebase(ctx, base, start, target, &res); err != nil {
			return res, err
		}

		c.collect(ctx, base, &res)

		return res, nil
	}

	gen, err := c.freeGeneration(ctx, base.ManifestGen, &res)
	if err != nil {
		return res, err
	}

	g, err := c.encode(gen, target, byID)
	if err != nil {
		return res, err
	}

	if _, err := c.store.Put(ctx, c.col.Manifest(gen), g.manifestBytes, blob.PutCond{IfNoneMatch: true}); err != nil {
		if errors.Is(err, blob.ErrPrecondition) {
			return res, ErrConflict
		}

		return res, fmt.Errorf("collection: claim %d: %w", gen, err)
	}

	res.Writes++
	res.Bytes += int64(len(g.manifestBytes))

	release := func(cause error) error {
		keys := append(c.col.Objects(gen), c.col.Manifest(gen))

		return errors.Join(cause, c.store.Delete(context.WithoutCancel(ctx), keys...))
	}

	if err := c.putObjects(ctx, g); err != nil {
		return res, release(err)
	}

	res.Writes += 3
	res.Bytes += int64(len(g.k) + len(g.p) + len(g.snap))

	start := plog.EncodeBase(plog.Base{NormID: normID, ManifestGen: gen, LastSeq: target, UnixMs: c.opts.Now().UnixMilli()})

	if err := c.rebase(ctx, base, start, target, &res); err != nil {
		if errors.Is(err, ErrConflict) || errors.Is(err, errContention) {
			return res, release(err)
		}

		return res, err
	}

	c.collect(ctx, base, &res)
	res.Gen = gen

	return res, nil
}

// collect deletes, best effort, the generations no query reads once p.log
// left base: the one before an indexed base (queries in flight may still
// read the base itself), or the last one of a LogOnly base, which nothing
// has read since the compaction that dropped the index.
func (c *Collection) collect(ctx context.Context, base plog.Base, res *CompactResult) {
	var gens []uint32

	if base.LogOnly {
		gens = append(gens, base.ManifestGen)
	}

	if base.ManifestGen > 1 {
		gens = append(gens, base.ManifestGen-1)
	}

	for _, gen := range gens {
		if gen == 0 {
			continue
		}

		if err := c.store.Delete(ctx, append(c.col.Objects(gen), c.col.Manifest(gen))...); err == nil {
			res.Writes++
		}
	}
}

// freeGeneration finds the number to claim: the one after the base, or past
// a claim on it that was abandoned.
func (c *Collection) freeGeneration(ctx context.Context, baseGen uint32, res *CompactResult) (uint32, error) {
	for gen := baseGen + 1; ; gen++ {
		data, _, err := c.store.Get(ctx, c.col.Manifest(gen))
		res.Requests++

		if errors.Is(err, blob.ErrNotFound) {
			return gen, nil
		}

		if err != nil {
			return 0, fmt.Errorf("collection: manifest %d: %w", gen, err)
		}

		claim, err := manifest.Decode(data)
		if err != nil {
			return 0, err
		}

		if c.opts.Now().Sub(time.UnixMilli(claim.CreatedUnixMs)) < c.opts.ClaimTimeout {
			return 0, ErrConflict
		}
	}
}

var errContention = errors.New("collection: too much contention rebasing p.log")

// rebase rewrites p.log as start (the new base, and its Docs frame when
// LogOnly) plus whatever was appended after target, and only if nobody wrote
// in between (If-Match) and the log is still on the base the state was read
// from. A writer that loses against it sees an offset mismatch and re-reads.
func (c *Collection) rebase(ctx context.Context, base plog.Base, start []byte, target uint64, res *CompactResult) error {
	normID := c.norm.ID()

	for attempt := range 16 {
		data, etag, err := c.store.Get(ctx, c.col.Log())
		res.Requests++

		if err != nil {
			return fmt.Errorf("collection: p.log: %w", err)
		}

		log, err := plog.Parse(data, normID)
		if err != nil {
			return err
		}

		// Another compaction committed first (our claim timed out, or two
		// compactions of a LogOnly log, which claim nothing): the state we
		// built may miss what it folded.
		if b := log.Base; b.ManifestGen != base.ManifestGen || b.LastSeq != base.LastSeq || b.LogOnly != base.LogOnly {
			return ErrConflict
		}

		if log.TornTail {
			return errors.New("collection: p.log has a corrupt tail")
		}

		out := slices.Clone(start)

		var rest []plog.Op

		for _, op := range log.Ops {
			if op.Seq > target {
				rest = append(rest, op)
			}
		}

		if out, err = plog.AppendBatch(out, rest); err != nil {
			return err
		}

		for _, request := range log.Compacts {
			if request.TargetSeq > target {
				out = plog.AppendCompact(out, request)
			}
		}

		_, err = c.store.Put(ctx, c.col.Log(), out, blob.PutCond{IfMatch: etag})
		res.Writes++

		if errors.Is(err, blob.ErrPrecondition) {
			if err := plog.Backoff(ctx, attempt); err != nil {
				return err
			}

			continue
		}

		if err != nil {
			return fmt.Errorf("collection: rebase p.log: %w", err)
		}

		res.Bytes += int64(len(out))

		return nil
	}

	return errContention
}
