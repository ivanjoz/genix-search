// Package genix is a search index on S3 Express One Zone that returns ranked
// IDs (PLAN_5BIT.md). Words are their first 8 letters in 5-bit units and
// numbers are their binary value; the index stores IDs and those keys, never
// text.
//
//	store, _ := genix.NewFSStore("/tmp/genix")
//	cfg := genix.Config{Collection: "productos", Extra: [2]rune{'ñ', 0}}
//	ix, _ := genix.NewIndexer(store, cfg)
//	ix.Init(ctx, docs)
//	s, _ := genix.NewSearcher(store, cfg, genix.DefaultSearchOptions())
//	resp, _ := s.Search(ctx, genix.Request{Query: "polo spiderman rojo"})
package genix

import (
	"context"
	"fmt"

	"github.com/ivanjoz/colbin"
	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/query"
)

type (
	// Blob is the object store: S3 Express One Zone, or a directory.
	Blob = blob.Blob
	// Doc is a document to index: its ID and its text.
	Doc = collection.Doc
	// Request, Response and Hit are a search and its answer.
	Request  = query.Request
	Response = query.Response
	Hit      = query.Hit
	// SearchOptions are the query tunables.
	SearchOptions = query.Options
	// AppendResult is what a write did.
	AppendResult = plog.AppendResult
	// CompactResult is what a compaction, a rebuild or a drop did.
	CompactResult = collection.CompactResult
	// Source streams every document of a collection from the system of
	// record, for Rebuild. It may be called more than once.
	Source = collection.Source
)

// ErrConflict is another compaction or rebuild in progress on the same
// collection.
var ErrConflict = collection.ErrConflict

// NewFSStore is a store in a local directory, for tests and the CLI.
func NewFSStore(dir string) (Blob, error) { return blob.NewFS(dir) }

// DefaultSearchOptions are the default query tunables.
func DefaultSearchOptions() SearchOptions { return query.DefaultOptions() }

// Config names a collection and fixes its alphabet.
type Config struct {
	Root       string // key prefix in the bucket; default "genix/"
	Collection string
	// Partition is an independent index of the collection (one per company,
	// in a multi-tenant system): its own p.log and generations. Default
	// "default".
	Partition string
	// Extra are the letters of codes 30 and 31 of the 5-bit table: characters
	// the normalizer keeps as letters of their own instead of folding them
	// (ñ) or treating them as separators. 0 is unused. Changing them for an
	// existing collection means reprocessing it from the source system.
	Extra [2]rune
	// MinIndexDocs: with fewer documents the collection has no index; its
	// documents live in p.log and a search is one request. 0 means the
	// default, 100; 1 always builds the index.
	MinIndexDocs int
}

func (c Config) open() (layout.Collection, *normalize.Normalizer, error) {
	root, partition := c.Root, c.Partition
	if root == "" {
		root = "genix/"
	}

	if partition == "" {
		partition = "default"
	}

	col, err := layout.New(root, c.Collection, partition)
	if err != nil {
		return col, nil, err
	}

	norm, err := normalize.New(c.Extra)

	return col, norm, err
}

// Indexer writes a collection.
type Indexer struct {
	c *collection.Collection
}

// NewIndexer returns the writer of the collection of cfg.
func NewIndexer(store Blob, cfg Config) (*Indexer, error) {
	col, norm, err := cfg.open()
	if err != nil {
		return nil, err
	}

	return &Indexer{c: collection.New(store, col, norm, cfg.collectionOptions())}, nil
}

func (c Config) collectionOptions() collection.Options {
	opts := collection.DefaultOptions()
	if c.MinIndexDocs > 0 {
		opts.MinIndexDocs = c.MinIndexDocs
	}

	return opts
}

// Init creates a collection: its first generation, or only p.log when it has
// fewer than MinIndexDocs documents.
func (ix *Indexer) Init(ctx context.Context, docs []Doc) error {
	_, err := ix.c.Init(ctx, docs)

	return err
}

// Put writes new versions of documents (a new ID is a new document). The
// first put of a collection that does not exist creates it.
func (ix *Indexer) Put(ctx context.Context, docs ...Doc) (AppendResult, error) {
	return ix.c.Put(ctx, docs)
}

// Delete removes documents.
func (ix *Indexer) Delete(ctx context.Context, ids ...uint32) (AppendResult, error) {
	return ix.c.Delete(ctx, ids...)
}

// Apply writes puts and deletes as one batch of p.log (one read and one
// append). An ID in both ends up deleted.
func (ix *Indexer) Apply(ctx context.Context, puts []Doc, dels []uint32) (AppendResult, error) {
	return ix.c.Apply(ctx, puts, dels)
}

// Rebuild replaces the collection with the documents of source, keeping the
// writes that land while it runs. Writers must append after their write to
// the source commits.
func (ix *Indexer) Rebuild(ctx context.Context, source Source) (CompactResult, error) {
	return ix.c.Rebuild(ctx, source)
}

// Drop empties the collection and deletes its index.
func (ix *Indexer) Drop(ctx context.Context) (CompactResult, error) {
	return ix.c.Drop(ctx)
}

// Compact folds the pending writes into a new generation (or into p.log,
// under MinIndexDocs). Call it when a write reports CompactRequested: that
// includes reaching MinIndexDocs, so the index gets built.
func (ix *Indexer) Compact(ctx context.Context) (CompactResult, error) {
	return ix.c.Compact(ctx)
}

// Searcher answers queries on a collection. It caches the dictionary of the
// current generation; keep one per collection for the life of the process.
type Searcher struct {
	e *query.Engine
}

// NewSearcher returns the searcher of the collection of cfg. cfg.Extra must
// be the collection's: it is checked against the files.
func NewSearcher(store Blob, cfg Config, opts SearchOptions) (*Searcher, error) {
	col, norm, err := cfg.open()
	if err != nil {
		return nil, err
	}

	return &Searcher{e: query.New(store, col, norm, opts)}, nil
}

// Search runs one query.
func (s *Searcher) Search(ctx context.Context, req Request) (Response, error) {
	return s.e.Search(ctx, req)
}

// QueryRequest is a search on the wire (colbin).
type QueryRequest struct {
	Query  string `cb:"1"`
	Limit  uint32 `cb:"2"`
	Offset uint32 `cb:"3"`
	Fuzzy  bool   `cb:"4"`
}

// QueryHit is one result on the wire.
type QueryHit struct {
	ID       uint32 `cb:"1"`
	Coverage uint32 `cb:"2"`
	Score    uint32 `cb:"3"`
}

// QueryResponse is the answer on the wire.
type QueryResponse struct {
	Gen    uint32     `cb:"1"`
	Hits   []QueryHit `cb:"2"`
	Approx bool       `cb:"3"`
}

// Serve answers a colbin QueryRequest with a colbin QueryResponse.
func (s *Searcher) Serve(ctx context.Context, body []byte) ([]byte, error) {
	var req QueryRequest
	if err := colbin.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("genix: request: %w", err)
	}

	resp, err := s.Search(ctx, Request{Query: req.Query, Limit: int(req.Limit), Offset: int(req.Offset), Fuzzy: req.Fuzzy})
	if err != nil {
		return nil, err
	}

	out := QueryResponse{Gen: resp.Gen, Approx: resp.Stats.Approx, Hits: make([]QueryHit, len(resp.Hits))}
	for i, h := range resp.Hits {
		out.Hits[i] = QueryHit(h)
	}

	return colbin.Marshal(&out)
}
