// Package collection is the write side of a collection: appends to p.log and
// the compaction that folds p.log into the buckets of the index, rewriting
// only those that changed (PLAN_BUCKETS.md §8–9).
package collection

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
)

// Options are the write-side tunables.
type Options struct {
	// CompactBytes is the size of the appends since the last compaction that
	// triggers a compaction request.
	CompactBytes int64
	// MinIndexDocs: a collection with fewer documents has no index; its
	// documents live in p.log and a query reads only p.log. A compaction
	// builds the index once it has this many, and drops it if deletes take it
	// below. 1 always indexes.
	MinIndexDocs int
	// LeaseTimeout is how long the lease of a compaction that never released
	// it blocks the next one.
	LeaseTimeout time.Duration
	// CompactSpacing is the least time between two compactions: what lets a
	// query trust a bucket the last compaction did not rewrite
	// (PLAN_BUCKETS.md §7). It must exceed the query's RoundLimit.
	CompactSpacing time.Duration
	// StaleCompact is how old a pending compaction request gets before the
	// next write asks again (plog.AppendOptions.StaleCompact).
	StaleCompact time.Duration
	// Params tune the index.
	Params bucket.Params
	// Concurrency is the buckets written at a time.
	Concurrency int
	Now         func() time.Time
}

// DefaultOptions are the defaults.
func DefaultOptions() Options {
	return Options{
		CompactBytes:   plog.DefaultCompactBytes,
		MinIndexDocs:   plog.DefaultMinIndexDocs,
		LeaseTimeout:   2 * time.Minute,
		CompactSpacing: time.Second,
		StaleCompact:   plog.DefaultStaleCompact,
		Params:         bucket.DefaultParams(),
		Concurrency:    32,
		Now:            time.Now,
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

// logOnlyLog is the start of the p.log of a collection without index: the
// Base, flagged LogOnly, and the Docs frame with every document.
func (c *Collection) logOnlyLog(base plog.Base, byID map[docid.Key][]byte) ([]byte, error) {
	base.LogOnly = true
	base.Docs = uint32(len(byID))
	base.Touched = plog.Touched{}

	return plog.AppendDocs(plog.EncodeBase(base), sortedDocs(byID))
}

func sortedDocs(byID map[docid.Key][]byte) []plog.Doc {
	docs := make([]plog.Doc, 0, len(byID))
	for id, tokens := range byID {
		docs = append(docs, plog.Doc{ID: id, Tokens: tokens})
	}

	slices.SortFunc(docs, func(a, b plog.Doc) int { return cmp.Compare(a.ID, b.ID) })

	return docs
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
	if opts.Now == nil {
		opts.Now = time.Now
	}

	if opts.Params == (bucket.Params{}) {
		opts.Params = bucket.DefaultParams()
	}

	return &Collection{store: store, col: col, norm: norm, opts: opts}
}

// Doc is a document as the caller sends it. The text is normalized on
// arrival; only its 5-bit tokens are stored.
type Doc struct {
	// OriginID and ID are the document: an ID is unique within its origin
	// (the table it comes from), and a document never changes origin. The
	// origin is returned with every hit and takes no part in the score.
	OriginID uint8
	ID       uint32
	Text     string
	// Group is the text of the document's group, if it has one: a parent
	// many documents share ("Juan Carlos" lives in "centro civico"). It only
	// adds to the score of a search that names it, below the text
	// (PLAN_BUCKETS.md §17).
	Group string
}

// Ref names a document: its origin and its ID in it.
type Ref struct {
	OriginID uint8
	ID       uint32
}

// Key is the index key of the document.
func (d Doc) Key() docid.Key { return docid.New(d.OriginID, d.ID) }

// Key is the index key of the document.
func (r Ref) Key() docid.Key { return docid.New(r.OriginID, r.ID) }

// Tokens is the key5 token stream of a document: the tokens of its text,
// then those of its group (PLAN_BUCKETS.md §17.1).
func (c *Collection) Tokens(doc Doc) []byte {
	tokens := key5.FromTokens(c.norm.Tokens(doc.Text))
	if doc.Group != "" {
		tokens = append(tokens, key5.GroupTokens(c.norm.Tokens(doc.Group))...)
	}

	return key5.AppendTokens(nil, tokens)
}

// ErrExists is returned by Init for a collection that already has a p.log.
var ErrExists = errors.New("collection: already exists")

// Init creates the collection: p.log with every document in it, then, from
// MinIndexDocs on, the compaction that builds the index from it. No bucket is
// written before p.log exists, so a failed Init leaves nothing a query reads.
func (c *Collection) Init(ctx context.Context, docs []Doc) (CompactResult, error) {
	byID := make(map[docid.Key][]byte, len(docs))
	for _, doc := range docs {
		byID[doc.Key()] = c.Tokens(doc) // a repeated document keeps its last version
	}

	data, err := c.logOnlyLog(plog.Base{NormID: c.norm.ID(), UnixMs: c.opts.Now().UnixMilli()}, byID)
	if err != nil {
		return CompactResult{}, err
	}

	if _, err := c.store.Put(ctx, c.col.Log(), data, blob.PutCond{IfNoneMatch: true}); err != nil {
		if errors.Is(err, blob.ErrPrecondition) {
			return CompactResult{}, ErrExists
		}

		return CompactResult{}, fmt.Errorf("collection: create p.log: %w", err)
	}

	if !c.indexed(len(byID)) {
		return CompactResult{Docs: len(byID), Writes: 1, Bytes: int64(len(data))}, nil
	}

	return c.Compact(ctx)
}

// Put appends new versions of documents to p.log.
func (c *Collection) Put(ctx context.Context, docs []Doc) (plog.AppendResult, error) {
	return c.Apply(ctx, docs, nil)
}

// Delete appends deletes to p.log.
func (c *Collection) Delete(ctx context.Context, refs ...Ref) (plog.AppendResult, error) {
	return c.Apply(ctx, nil, refs)
}

// Apply appends new versions of puts and the deletes of dels to p.log as one
// batch: one read and one append, whatever the mix. The deletes go last, so
// a document in both ends up deleted. A collection that does not exist yet is
// created by its first put; deletes alone leave it missing.
func (c *Collection) Apply(ctx context.Context, puts []Doc, dels []Ref) (plog.AppendResult, error) {
	changes := make([]plog.Change, 0, len(puts)+len(dels))

	for _, doc := range puts {
		changes = append(changes, plog.Change{ID: doc.Key(), Tokens: c.Tokens(doc)})
	}

	for _, ref := range dels {
		changes = append(changes, plog.Change{ID: ref.Key(), Del: true})
	}

	if len(changes) == 0 {
		return plog.AppendResult{}, nil
	}

	return plog.Append(ctx, c.store, c.col.Log(), c.norm.ID(), changes, c.appendOptions())
}
