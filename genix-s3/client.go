package genix

import (
	"container/list"
	"context"
	"errors"
	"os"
	"sync"

	"github.com/ivanjoz/genix-search/genix-s3/internal/collection"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/query"
)

// CompactMode says who runs the compaction a write asks for.
type CompactMode int

const (
	// CompactAuto is CompactSync on AWS Lambda, which freezes a goroutine
	// once the response is sent, and CompactAsync anywhere else.
	CompactAuto CompactMode = iota
	// CompactSync compacts inside the write that asked for it, before Apply
	// returns.
	CompactSync
	// CompactAsync compacts in a goroutine, one at a time per partition.
	CompactAsync
	// CompactCallback calls ClientOptions.OnCompact; the caller runs Compact,
	// e.g. from a queue.
	CompactCallback
)

// ClientOptions configure a Client.
type ClientOptions struct {
	Root  string  // key prefix in the bucket; default "genix/"
	Extra [2]rune // see Config.Extra: the same for every collection
	// KeepStopwords: see Config.KeepStopwords; the same for every collection.
	KeepStopwords bool
	MinIndexDocs  int      // see Config.MinIndexDocs
	Grouping      Grouping // see Config.Grouping: the same for every collection
	Search        SearchOptions
	// MaxSearchers bounds the searchers kept, one per partition searched,
	// each with the .idx it cached. Default 256.
	MaxSearchers int
	Compact      CompactMode
	// OnCompact receives the compaction requests in CompactCallback mode.
	OnCompact func(collection, partition string)
	// OnCompactError receives the errors of CompactAsync compactions, which
	// have no caller to return them to. ErrConflict is not an error: another
	// compaction is doing the work.
	OnCompactError func(collection, partition string, err error)
}

// Client writes and searches every partition of every collection in a store.
// It is safe for concurrent use; keep one for the life of the process.
type Client struct {
	store Blob
	opts  ClientOptions
	norm  *normalize.Normalizer
	mode  CompactMode

	mu        sync.Mutex
	searchers map[partKey]*list.Element
	recent    *list.List // of *searcherEntry, most recent first

	compactMu  sync.Mutex
	compacting map[partKey]*compactRun
	running    sync.WaitGroup
}

type partKey struct{ collection, partition string }

type searcherEntry struct {
	key partKey
	e   *query.Engine
}

// compactRun is the async compaction of one partition; again is set when a
// request arrives while it runs.
type compactRun struct{ again bool }

// NewClient returns the client of store.
func NewClient(store Blob, opts ClientOptions) (*Client, error) {
	norm, err := normalize.New(opts.Extra)
	if err != nil {
		return nil, err
	}

	norm = norm.WithStopwords(opts.KeepStopwords)

	if _, err := opts.Grouping.layout(); err != nil {
		return nil, err
	}

	if opts.Root == "" {
		opts.Root = "genix/"
	}

	if opts.MaxSearchers <= 0 {
		opts.MaxSearchers = 256
	}

	if opts.Search == (SearchOptions{}) {
		// A process holds up to MaxSearchers of them: a smaller .idx cache
		// each than a single collection's default.
		opts.Search = DefaultSearchOptions()
		opts.Search.CacheBytes = 8 << 20
	}

	mode := opts.Compact
	if mode == CompactAuto {
		mode = CompactAsync
		if os.Getenv("AWS_LAMBDA_FUNCTION_NAME") != "" {
			mode = CompactSync
		}
	}

	return &Client{
		store:      store,
		opts:       opts,
		norm:       norm,
		mode:       mode,
		searchers:  map[partKey]*list.Element{},
		recent:     list.New(),
		compacting: map[partKey]*compactRun{},
	}, nil
}

func (c *Client) collection(k partKey) (*collection.Collection, error) {
	col, err := c.opts.Grouping.collection(c.opts.Root, k.collection, k.partition)
	if err != nil {
		return nil, err
	}

	cfg := Config{MinIndexDocs: c.opts.MinIndexDocs}

	return collection.New(c.store, col, c.norm, cfg.collectionOptions()), nil
}

// ApplyResult is what Apply did.
type ApplyResult struct {
	AppendResult
	// Compacted is set when the write asked for a compaction and it ran
	// inside Apply (CompactSync).
	Compacted *CompactResult
	// CompactErr is the error of that compaction. The write itself landed.
	CompactErr error
}

// Apply writes puts and deletes to a partition as one batch of p.log: one
// read and one append, whatever the mix. A document in both ends up deleted.
// The first put creates the partition; deletes alone on a missing one do
// nothing.
func (c *Client) Apply(ctx context.Context, collection, partition string, puts []Doc, dels []Ref) (ApplyResult, error) {
	var res ApplyResult

	k := partKey{collection, partition}

	col, err := c.collection(k)
	if err != nil {
		return res, err
	}

	if res.AppendResult, err = col.Apply(ctx, puts, dels); err != nil || !res.CompactRequested {
		return res, err
	}

	switch c.mode {
	case CompactSync:
		compacted, err := col.Compact(ctx)
		if errors.Is(err, ErrConflict) {
			err = nil
		}

		res.Compacted, res.CompactErr = &compacted, err
	case CompactAsync:
		c.compactAsync(k)
	case CompactCallback:
		if c.opts.OnCompact != nil {
			c.opts.OnCompact(collection, partition)
		}
	}

	return res, nil
}

// compactAsync runs the compaction of k in a goroutine, unless one is running:
// then that one runs once more when it ends, so no request is lost.
func (c *Client) compactAsync(k partKey) {
	c.compactMu.Lock()
	defer c.compactMu.Unlock()

	if run, ok := c.compacting[k]; ok {
		run.again = true

		return
	}

	run := &compactRun{}
	c.compacting[k] = run

	c.running.Go(func() {
		for {
			_, err := c.Compact(context.Background(), k.collection, k.partition)
			if err != nil && !errors.Is(err, ErrConflict) && c.opts.OnCompactError != nil {
				c.opts.OnCompactError(k.collection, k.partition, err)
			}

			c.compactMu.Lock()
			if !run.again {
				delete(c.compacting, k)
				c.compactMu.Unlock()

				return
			}

			run.again = false
			c.compactMu.Unlock()
		}
	})
}

// Wait blocks until the async compactions running now end.
func (c *Client) Wait() { c.running.Wait() }

// Compact folds the pending writes of a partition into its index.
func (c *Client) Compact(ctx context.Context, collection, partition string) (CompactResult, error) {
	col, err := c.collection(partKey{collection, partition})
	if err != nil {
		return CompactResult{}, err
	}

	return col.Compact(ctx)
}

// Rebuild replaces a partition with the documents of source, keeping the
// writes that land while it runs. Writers must append after their write to
// the source commits. It is also how a partition is moved to new extra
// letters or a new normalizer.
func (c *Client) Rebuild(ctx context.Context, collection, partition string, source Source) (CompactResult, error) {
	col, err := c.collection(partKey{collection, partition})
	if err != nil {
		return CompactResult{}, err
	}

	return col.Rebuild(ctx, source)
}

// Drop empties a partition and deletes its index.
func (c *Client) Drop(ctx context.Context, collection, partition string) (CompactResult, error) {
	col, err := c.collection(partKey{collection, partition})
	if err != nil {
		return CompactResult{}, err
	}

	return col.Drop(ctx)
}

// Search runs a query on a partition. A partition that does not exist has no
// hits.
func (c *Client) Search(ctx context.Context, collection, partition string, req Request) (Response, error) {
	e, err := c.searcher(partKey{collection, partition})
	if err != nil {
		return Response{}, err
	}

	resp, err := e.Search(ctx, req)
	if errors.Is(err, query.ErrNoCollection) {
		return Response{}, nil
	}

	return resp, err
}

// searcher returns the engine of k, creating it and evicting the least
// recently used one past MaxSearchers.
func (c *Client) searcher(k partKey) (*query.Engine, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if el, ok := c.searchers[k]; ok {
		c.recent.MoveToFront(el)

		return el.Value.(*searcherEntry).e, nil
	}

	col, err := c.opts.Grouping.collection(c.opts.Root, k.collection, k.partition)
	if err != nil {
		return nil, err
	}

	entry := &searcherEntry{key: k, e: query.New(c.store, col, c.norm, c.opts.Search)}
	c.searchers[k] = c.recent.PushFront(entry)

	for c.recent.Len() > c.opts.MaxSearchers {
		oldest := c.recent.Back()
		c.recent.Remove(oldest)
		delete(c.searchers, oldest.Value.(*searcherEntry).key)
	}

	return entry.e, nil
}
