// Package collection is the write side of a collection: the first build,
// appends to p.log and the compaction that folds p.log into a new generation
// of k, p and the snapshot (PLAN_5BIT.md §8).
package collection

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/index"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/manifest"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/snapshot"
)

// Options are the write-side tunables.
type Options struct {
	// CompactBytes is the size of the appends since the last compaction that
	// triggers a compaction request.
	CompactBytes int64
	// MinIndexDocs: a collection with fewer documents has no index (no k, p,
	// snapshot or manifest); its documents live in p.log and a query reads
	// only p.log. A compaction builds the index once it has this many, and
	// drops it if deletes take it below. 1 always indexes.
	MinIndexDocs int
	// ClaimTimeout is how long a claimed generation that never got published
	// blocks the next compaction.
	ClaimTimeout time.Duration
	// StaleCompact is how old a pending compaction request gets before the
	// next write asks again (plog.AppendOptions.StaleCompact).
	StaleCompact time.Duration
	Now          func() time.Time
}

// DefaultOptions are the defaults.
func DefaultOptions() Options {
	return Options{
		CompactBytes: plog.DefaultCompactBytes,
		MinIndexDocs: plog.DefaultMinIndexDocs,
		ClaimTimeout: 5 * time.Minute,
		StaleCompact: plog.DefaultStaleCompact,
		Now:          time.Now,
	}
}

// appendOptions: a write to a collection that does not exist yet creates it.
func (c *Collection) appendOptions() plog.AppendOptions {
	return plog.AppendOptions{
		CompactBytes: c.opts.CompactBytes,
		MinIndexDocs: c.opts.MinIndexDocs,
		Create:       true,
		StaleCompact: c.opts.StaleCompact,
		Now:          c.opts.Now,
	}
}

// indexed reports whether a collection of n documents gets an index.
func (c *Collection) indexed(n int) bool {
	return n >= c.opts.MinIndexDocs
}

// logOnlyBase is the start of the p.log of a collection without index: the
// Base, flagged LogOnly, and the Docs frame with every document.
func (c *Collection) logOnlyBase(lastGen uint32, lastSeq uint64, byID map[uint32][]byte) ([]byte, error) {
	docs := make([]plog.Doc, 0, len(byID))
	for id, tokens := range byID {
		docs = append(docs, plog.Doc{ID: id, Tokens: tokens})
	}

	slices.SortFunc(docs, func(a, b plog.Doc) int { return cmp.Compare(a.ID, b.ID) })

	base := plog.EncodeBase(plog.Base{NormID: c.norm.ID(), ManifestGen: lastGen, LastSeq: lastSeq, UnixMs: c.opts.Now().UnixMilli(), LogOnly: true})

	return plog.AppendDocs(base, docs)
}

// Collection writes one collection.
type Collection struct {
	store blob.Blob
	col   layout.Collection
	norm  *normalize.Normalizer
	opts  Options
}

// New returns the writer of a collection.
func New(store blob.Blob, col layout.Collection, norm *normalize.Normalizer, opts Options) *Collection {
	return &Collection{store: store, col: col, norm: norm, opts: opts}
}

// Doc is a document as the caller sends it. The text is normalized on
// arrival; only its 5-bit tokens are stored.
type Doc struct {
	ID   uint32
	Text string
}

// Tokens is the key5 token stream of a text.
func (c *Collection) Tokens(text string) []byte {
	return key5.AppendTokens(nil, key5.FromTokens(c.norm.Tokens(text)))
}

// ErrExists is returned by Init for a collection that already has a p.log.
var ErrExists = errors.New("collection: already exists")

// Init creates the collection: generation 1 and p.log on it, or, under
// MinIndexDocs, only p.log with the documents in it (the manifest is nil).
func (c *Collection) Init(ctx context.Context, docs []Doc) (*manifest.Manifest, error) {
	if _, _, err := c.store.Get(ctx, c.col.Log()); err == nil {
		return nil, ErrExists
	} else if !errors.Is(err, blob.ErrNotFound) {
		return nil, err
	}

	byID := make(map[uint32][]byte, len(docs))
	for _, doc := range docs {
		byID[doc.ID] = c.Tokens(doc.Text) // a repeated ID keeps its last version
	}

	if !c.indexed(len(byID)) {
		data, err := c.logOnlyBase(0, 0, byID)
		if err != nil {
			return nil, err
		}

		if _, err := c.store.Put(ctx, c.col.Log(), data, blob.PutCond{IfNoneMatch: true}); err != nil {
			return nil, fmt.Errorf("collection: create p.log: %w", err)
		}

		return nil, nil
	}

	gen, err := c.encode(1, 0, byID)
	if err != nil {
		return nil, err
	}

	if _, err := c.store.Put(ctx, c.col.Manifest(1), gen.manifestBytes, blob.PutCond{IfNoneMatch: true}); err != nil {
		return nil, fmt.Errorf("collection: claim generation 1: %w", err)
	}

	if err := c.putObjects(ctx, gen); err != nil {
		return nil, err
	}

	base := plog.EncodeBase(plog.Base{NormID: c.norm.ID(), ManifestGen: 1, UnixMs: c.opts.Now().UnixMilli()})
	if _, err := c.store.Put(ctx, c.col.Log(), base, blob.PutCond{IfNoneMatch: true}); err != nil {
		return nil, fmt.Errorf("collection: create p.log: %w", err)
	}

	return gen.manifest, nil
}

// Put appends new versions of documents to p.log.
func (c *Collection) Put(ctx context.Context, docs []Doc) (plog.AppendResult, error) {
	return c.Apply(ctx, docs, nil)
}

// Delete appends deletes to p.log.
func (c *Collection) Delete(ctx context.Context, ids ...uint32) (plog.AppendResult, error) {
	return c.Apply(ctx, nil, ids)
}

// Apply appends new versions of puts and the deletes of dels to p.log as one
// batch: one read and one append, whatever the mix. The deletes go last, so
// an ID in both ends up deleted. A collection that does not exist yet is
// created by its first put; deletes alone leave it missing.
func (c *Collection) Apply(ctx context.Context, puts []Doc, dels []uint32) (plog.AppendResult, error) {
	changes := make([]plog.Change, 0, len(puts)+len(dels))

	for _, doc := range puts {
		changes = append(changes, plog.Change{ID: doc.ID, Tokens: c.Tokens(doc.Text)})
	}

	for _, id := range dels {
		changes = append(changes, plog.Change{ID: id, Del: true})
	}

	if len(changes) == 0 {
		return plog.AppendResult{}, nil
	}

	return plog.Append(ctx, c.store, c.col.Log(), c.norm.ID(), changes, c.appendOptions())
}

// generation is a generation encoded in memory, before it is written.
type generation struct {
	manifest      *manifest.Manifest
	manifestBytes []byte
	k, p, snap    []byte
}

// encode builds k, p and the snapshot of a generation from the ID → tokens
// state.
func (c *Collection) encode(gen uint32, lastSeq uint64, byID map[uint32][]byte) (*generation, error) {
	ids := make([]uint32, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}

	slices.Sort(ids)

	docs := make([]snapshot.Doc, len(ids))
	postings := make(map[key5.Key][]index.Posting)

	for i, id := range ids {
		docs[i] = snapshot.Doc{ID: id, Tokens: byID[id]}

		tokens, err := key5.ReadTokens(byID[id])
		if err != nil {
			return nil, fmt.Errorf("collection: document %d: %w", id, err)
		}

		// Documents go in ID order and tokens in position order, so every
		// list comes out sorted by (ID, Pos).
		for pos, t := range tokens {
			postings[t.Key] = append(postings[t.Key], index.Posting{ID: id, Pos: uint8(min(pos, index.MaxPos)), Tail: t.Tail})
		}
	}

	lists := make([]index.List, 0, len(postings))
	for k, list := range postings {
		lists = append(lists, index.List{Key: k, Postings: list})
	}

	slices.SortFunc(lists, func(a, b index.List) int {
		switch {
		case a.Key < b.Key:
			return -1
		case a.Key > b.Key:
			return 1
		}

		return 0
	})

	normID := c.norm.ID()

	k, p, err := index.Build(index.Meta{Gen: gen, Docs: uint32(len(ids)), NormID: normID}, lists)
	if err != nil {
		return nil, err
	}

	snap, err := snapshot.Encode(snapshot.Header{NormID: normID, Gen: gen, LastSeq: lastSeq}, docs)
	if err != nil {
		return nil, err
	}

	extra := c.norm.Extra()
	m := &manifest.Manifest{
		Gen:               gen,
		LastSeq:           lastSeq,
		NormalizerVersion: normalize.Version,
		Extra1:            extra[0],
		Extra2:            extra[1],
		NormID:            normID,
		Docs:              uint32(len(ids)),
		Keys:              uint32(len(lists)),
		KSize:             uint64(len(k)),
		PSize:             uint64(len(p)),
		SnapSize:          uint64(len(snap)),
		CreatedUnixMs:     c.opts.Now().UnixMilli(),
	}

	mb, err := manifest.Encode(m)
	if err != nil {
		return nil, err
	}

	return &generation{manifest: m, manifestBytes: mb, k: k, p: p, snap: snap}, nil
}

// putObjects writes k, p and the snapshot of a claimed generation.
func (c *Collection) putObjects(ctx context.Context, g *generation) error {
	gen := g.manifest.Gen
	objects := map[string][]byte{c.col.K(gen): g.k, c.col.P(gen): g.p, c.col.Snapshot(gen): g.snap}

	for key, data := range objects {
		if _, err := c.store.Put(ctx, key, data, blob.PutCond{}); err != nil {
			return fmt.Errorf("collection: put %s: %w", key, err)
		}
	}

	return nil
}
