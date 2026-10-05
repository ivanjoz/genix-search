package collection

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/parallel"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/tokens"
)

// CompactResult says what a compaction, a rebuild or a drop did.
type CompactResult struct {
	Seq     uint64 // the LastSeq of the base it wrote; 0 if it did nothing
	Epoch   uint32
	Ops     int // p.log ops folded in
	Docs    int
	Buckets int  // buckets written, tombstones included
	Full    bool // a new epoch: every bucket was written
	Indexed bool // the collection has an index after it
	// Requests are the reads, Writes the puts and deletes, Bytes the bytes
	// written.
	Requests, Writes, Bytes int64
}

// Compact folds the pending ops of p.log into the index (PLAN_BUCKETS.md §8):
//
//  1. Take the lease.
//  2. Read p.log and tokens; apply the ops to the document → tokens state.
//  3. Wait until CompactSpacing has passed since the last compaction.
//  4. Build the whole index in memory and write the buckets whose content
//     changed: <xy>.db first, then <xy>.idx with its ETag.
//  5. Write tokens, then rebase p.log with If-Match: the commit. Its base
//     says which buckets this compaction rewrote.
//
// Under MinIndexDocs there is no index: the state lives in the Docs frame of
// p.log, and the compaction only rebases p.log. Crossing the threshold either
// way, or taking over the lease of a compaction that did not finish, writes
// every bucket in a new epoch.
func (c *Collection) Compact(ctx context.Context) (res CompactResult, err error) {
	l, err := c.acquire(ctx, &res)
	if err != nil {
		return res, err
	}

	defer func() { c.release(ctx, l, err != nil, &res) }()

	st, err := c.readState(ctx, &res)
	if err != nil || st.log == nil {
		return res, err
	}

	byID, err := st.current()
	if err != nil {
		return res, err
	}

	pending := st.log.Pending()

	for id, op := range pending {
		if op.Del {
			delete(byID, id)
		} else {
			byID[id] = op.Tokens
		}
	}

	base := st.log.Base
	indexed := c.indexed(len(byID))
	leftover := base.LogOnly && st.tok != nil
	// The writers asked for another grouping: every bucket is rewritten.
	regroup := !base.LogOnly && indexed && base.Grouping != c.col.Grouping().ID()

	if len(pending) == 0 && !l.recover && indexed == !base.LogOnly && !leftover && !regroup && (st.tok == nil || len(st.tok.Garbage) == 0) {
		return res, nil
	}

	res.Ops = len(pending)
	full := l.recover || base.LogOnly || regroup || st.tok == nil || st.tok.Epoch != base.Epoch || st.tok.Grouping != c.col.Grouping().ID()

	return res, c.publish(ctx, l, st, byID, st.log.LastSeq(), full, &res)
}

// state is what a compaction starts from.
type state struct {
	log *plog.Log
	tok *tokens.State // nil when missing or unreadable
}

// current is the document → tokens state the base of the log stands for: the Docs
// frame of a LogOnly log, or tokens. A tokens object ahead of the log (a
// compaction that died after writing it) is as good: the ops are idempotent.
func (s *state) current() (map[docid.Key][]byte, error) {
	base := s.log.Base
	byID := map[docid.Key][]byte{}

	if s.tok != nil && s.tok.Epoch >= base.Epoch && s.tok.Seq >= base.LastSeq {
		for _, doc := range s.tok.Docs {
			byID[doc.ID] = doc.Tokens
		}

		return byID, nil
	}

	if !base.LogOnly {
		return nil, errors.New("collection: tokens is missing or behind p.log: rebuild the collection from its source")
	}

	for _, doc := range s.log.Docs {
		byID[doc.ID] = doc.Tokens
	}

	return byID, nil
}

// readState reads p.log and tokens in one round. A missing p.log leaves log
// nil.
func (c *Collection) readState(ctx context.Context, res *CompactResult) (*state, error) {
	var logData, tokData []byte
	var logErr, tokErr error

	_ = parallel.Run(ctx, 2, 2, func(ctx context.Context, i int) error {
		if i == 0 {
			logData, _, logErr = c.store.Get(ctx, c.col.Log())
		} else {
			tokData, _, tokErr = c.store.Get(ctx, c.col.Tokens())
		}

		return nil
	})

	res.Requests += 2

	if errors.Is(logErr, blob.ErrNotFound) {
		return &state{}, nil
	}

	if logErr != nil {
		return nil, fmt.Errorf("collection: p.log: %w", logErr)
	}

	if tokErr != nil && !errors.Is(tokErr, blob.ErrNotFound) {
		return nil, fmt.Errorf("collection: tokens: %w", tokErr)
	}

	log, err := plog.Parse(logData, c.norm.ID())
	if err != nil {
		return nil, err
	}

	st := &state{log: log}

	if tokErr == nil {
		// One of another normalizer, or corrupt, is no help: a full rewrite
		// does without it, and an incremental compaction refuses to start.
		st.tok, _ = tokens.Decode(tokData, c.norm.ID())
	}

	return st, nil
}

// publish writes the state byID as of target on top of the log of st, and
// commits it by rebasing p.log. full writes every bucket in a new epoch.
func (c *Collection) publish(ctx context.Context, l *lease, st *state, byID map[docid.Key][]byte, target uint64, full bool, res *CompactResult) error {
	base, tok := st.log.Base, st.tok

	res.Docs = len(byID)

	// The spacing keeps a query from pairing the base this writes with
	// buckets two compactions old. A LogOnly base with no tokens never had
	// buckets: its queries read p.log alone.
	if !base.LogOnly || tok != nil {
		if err := c.waitSpacing(ctx, base.UnixMs); err != nil {
			return err
		}
	}

	// An epoch no bucket was ever written in: above the base's, the tokens'
	// and any a holder of the lease that did not finish may have used.
	newEpoch := max(base.Epoch, l.epoch)
	if tok != nil {
		newEpoch = max(newEpoch, tok.Epoch)
	}

	newEpoch++

	next := plog.Base{NormID: c.norm.ID(), Epoch: base.Epoch, LastSeq: target, PrevSeq: base.LastSeq, Docs: uint32(len(byID))}

	if !c.indexed(len(byID)) {
		if !base.LogOnly {
			next.Epoch = newEpoch // the index goes: no query mixes it with this log
		}

		next.UnixMs = c.opts.Now().UnixMilli()

		start, err := c.logOnlyLog(next, byID)
		if err != nil {
			return err
		}

		if err := c.rebase(ctx, base, start, target, res); err != nil {
			return err
		}

		res.Seq, res.Epoch = target, next.Epoch

		// The buckets of an index dropped by an earlier compaction: no query
		// has read them for at least CompactSpacing.
		if base.LogOnly && tok != nil {
			c.dropIndex(ctx, tok, res)
		}

		return nil
	}

	var prev map[uint16]tokens.Bucket
	var prevExpanded map[key5.Key]bool

	if tok != nil && tok.Epoch >= base.Epoch {
		prevExpanded = map[key5.Key]bool{}
		for _, k := range tok.Expanded {
			prevExpanded[k] = true
		}
	}

	if full || tok == nil {
		full = true
		next.Epoch = newEpoch

		if err := c.claimEpoch(ctx, l, newEpoch, res); err != nil {
			return err
		}
	} else {
		prev = tok.Buckets
	}

	docs := make([]bucket.Doc, 0, len(byID))
	for _, d := range sortedDocs(byID) {
		toks, err := key5.ReadTokens(d.Tokens)
		if err != nil {
			return fmt.Errorf("collection: document %v: %w", d.ID, err)
		}

		docs = append(docs, bucket.Doc{ID: d.ID, Tokens: toks})
	}

	grouping := c.col.Grouping()
	next.Grouping = grouping.ID()

	built, err := bucket.Build(docs, prevExpanded, c.opts.Params, grouping)
	if err != nil {
		return err
	}

	// What the previous index had in this grouping: the .db to delete when a
	// bucket loses it.
	var known map[uint16]tokens.Bucket
	if tok != nil && tok.Grouping == grouping.ID() {
		known = tok.Buckets
	}

	now := map[uint16]tokens.Bucket{}
	var write []*bucket.Built

	for _, b := range built.Buckets {
		now[b.Group] = tokens.Bucket{Hash: b.Hash, HasDB: b.HasDB()}

		if old, ok := prev[b.Group]; full || !ok || old.Hash != b.Hash {
			write = append(write, b)
		}
	}

	var tombs []uint16
	var garbage []tokens.Garbage

	for group := range prev {
		if _, ok := now[group]; !ok {
			tombs = append(tombs, group)
		}
	}

	slices.Sort(tombs)

	if full && tok != nil {
		// Buckets of the index this one replaces, in an older epoch or
		// another grouping: queries on the new base take them for empty or
		// never name them; the next compaction deletes them, once no query
		// reads them, unless this index wrote over them.
		for group := range tok.Buckets {
			if _, ok := now[group]; !ok || tok.Grouping != grouping.ID() {
				garbage = append(garbage, tokens.Garbage{Grouping: tok.Grouping, Group: group})
			}
		}
	}

	// Tombstones go next time too: a query that read the base that made them
	// is done by then.
	for _, group := range tombs {
		garbage = append(garbage, tokens.Garbage{Grouping: grouping.ID(), Group: group})
	}

	var touched plog.Touched
	meta := bucket.Meta{NormID: c.norm.ID(), Epoch: next.Epoch, Seq: target, Grouping: grouping.ID()}

	l.wrote = true

	var writes, bytes atomic.Int64

	err = parallel.Run(ctx, len(write)+len(tombs), c.opts.Concurrency, func(ctx context.Context, i int) error {
		m := meta

		if i >= len(write) {
			group := tombs[i-len(write)]
			m.Group = group
			data := bucket.Tombstone(m)

			if _, err := c.store.Put(ctx, c.col.Idx(group), data, blob.PutCond{}); err != nil {
				return fmt.Errorf("collection: put %s: %w", c.col.Idx(group), err)
			}

			writes.Add(1)
			bytes.Add(int64(len(data)))

			return c.deleteDB(ctx, group, known, &writes)
		}

		b := write[i]
		m.Group = b.Group

		var etag string

		if b.HasDB() {
			data := b.EncodeDB(m)

			var err error
			if etag, err = c.store.Put(ctx, c.col.DB(b.Group), data, blob.PutCond{}); err != nil {
				return fmt.Errorf("collection: put %s: %w", c.col.DB(b.Group), err)
			}

			writes.Add(1)
			bytes.Add(int64(len(data)))
		}

		data := b.EncodeIdx(m, etag)
		if _, err := c.store.Put(ctx, c.col.Idx(b.Group), data, blob.PutCond{}); err != nil {
			return fmt.Errorf("collection: put %s: %w", c.col.Idx(b.Group), err)
		}

		writes.Add(1)
		bytes.Add(int64(len(data)))

		if !b.HasDB() {
			return c.deleteDB(ctx, b.Group, known, &writes)
		}

		return nil
	})

	res.Writes += writes.Load()
	res.Bytes += bytes.Load()

	if err != nil {
		return err
	}

	for _, b := range write {
		touched.Set(int(b.Group))
	}

	for _, group := range tombs {
		touched.Set(int(group))
	}

	res.Buckets = len(write) + len(tombs)

	// The garbage of the last compaction: no query reads it anymore. What
	// this index wrote under the same name stays.
	if tok != nil && len(tok.Garbage) > 0 {
		inUse := map[string]bool{}
		for group, b := range now {
			inUse[c.col.Idx(group)] = true
			inUse[c.col.DB(group)] = b.HasDB
		}

		var keys []string

		for _, g := range tok.Garbage {
			old, err := layout.GroupingByID(g.Grouping)
			if err != nil {
				continue
			}

			for _, key := range []string{c.col.With(old).Idx(g.Group), c.col.With(old).DB(g.Group)} {
				if !inUse[key] {
					keys = append(keys, key)
				}
			}
		}

		if err := c.store.Delete(ctx, keys...); err != nil {
			garbage = append(garbage, tok.Garbage...)
		}

		res.Writes++
	}

	var tokDocs []tokens.Doc
	for _, d := range sortedDocs(byID) {
		tokDocs = append(tokDocs, tokens.Doc{ID: d.ID, Tokens: d.Tokens})
	}

	tokData, err := tokens.Encode(&tokens.State{
		NormID: c.norm.ID(), Epoch: next.Epoch, Seq: target, Docs: tokDocs,
		Expanded: built.Expanded, Grouping: grouping.ID(), Buckets: now, Garbage: garbage,
	})
	if err != nil {
		return err
	}

	if _, err := c.store.Put(ctx, c.col.Tokens(), tokData, blob.PutCond{}); err != nil {
		return fmt.Errorf("collection: put tokens: %w", err)
	}

	res.Writes++
	res.Bytes += int64(len(tokData))

	next.Touched = touched
	next.UnixMs = c.opts.Now().UnixMilli()

	if err := c.rebase(ctx, base, plog.EncodeBase(next), target, res); err != nil {
		return err
	}

	res.Seq, res.Epoch, res.Full, res.Indexed = target, next.Epoch, full, true

	return nil
}

// deleteDB deletes the .db of a bucket that had one and has none now.
func (c *Collection) deleteDB(ctx context.Context, group uint16, known map[uint16]tokens.Bucket, writes *atomic.Int64) error {
	if old, ok := known[group]; !ok || !old.HasDB {
		return nil
	}

	writes.Add(1)

	return c.store.Delete(ctx, c.col.DB(group))
}

// dropIndex deletes every bucket of tok and tok itself, best effort.
func (c *Collection) dropIndex(ctx context.Context, tok *tokens.State, res *CompactResult) {
	keys := []string{}

	if g, err := layout.GroupingByID(tok.Grouping); err == nil {
		for group := range tok.Buckets {
			keys = append(keys, c.col.With(g).Idx(group), c.col.With(g).DB(group))
		}
	}

	for _, garbage := range tok.Garbage {
		if g, err := layout.GroupingByID(garbage.Grouping); err == nil {
			keys = append(keys, c.col.With(g).Idx(garbage.Group), c.col.With(g).DB(garbage.Group))
		}
	}

	if err := c.store.Delete(ctx, keys...); err == nil {
		_ = c.store.Delete(ctx, c.col.Tokens())
	}

	res.Writes += 2
}

var errContention = errors.New("collection: too much contention rebasing p.log")

// rebase rewrites p.log as start (the new base, and its Docs frame when
// LogOnly) plus whatever was appended after target, and only if nobody wrote
// in between (If-Match) and the log is still on the base the state was read
// from. A writer that loses against it sees an offset mismatch and re-reads.
func (c *Collection) rebase(ctx context.Context, base plog.Base, start []byte, target uint64, res *CompactResult) error {
	normID := c.norm.ID()

	for attempt := range 16 {
		started := time.Now()
		data, etag, err := c.store.Get(ctx, c.col.Log())
		res.Requests++

		if err != nil {
			return fmt.Errorf("collection: p.log: %w", err)
		}

		log, err := plog.Parse(data, normID)
		if errors.Is(err, plog.ErrNormID) && base.NormID != normID {
			// A rebuild replacing a log of another normalizer: its ops are in
			// keys this one does not read, and the source has their effect.
			b, berr := plog.ParseBase(data)
			log, err = &plog.Log{Base: b}, berr
		}

		if err != nil {
			return err
		}

		// Somebody else committed first: impossible while we hold the lease,
		// unless it expired under us.
		if b := log.Base; b.Epoch != base.Epoch || b.LastSeq != base.LastSeq || b.LogOnly != base.LogOnly || b.NormID != base.NormID {
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
			if err := plog.Backoff(ctx, attempt, time.Since(started)); err != nil {
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
