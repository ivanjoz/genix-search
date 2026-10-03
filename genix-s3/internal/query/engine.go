// Package query answers a search from p.log and the buckets of the index
// (PLAN_BUCKETS.md §6–7, §10).
package query

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/bucket"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/parallel"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
	"github.com/ivanjoz/genix-search/genix-s3/internal/postings"
)

// Options are the query tunables.
type Options struct {
	// PrefixBudget is the bytes of .db a prefix (or a typo expansion) may
	// read; the most frequent words go first.
	PrefixBudget int64
	// CoalesceGap is the largest hole merged into one range read: S3 has no
	// multi-range GET, so two lists close together cost one request.
	CoalesceGap int64
	// MaxRequest is the largest merged range read.
	MaxRequest int64
	// Concurrency is the simultaneous requests of one round.
	Concurrency int
	// CacheBytes bounds the decoded .idx kept between searches.
	CacheBytes int64
	// RoundLimit: a cold first round, p.log and the .idx read together, that
	// takes longer is repeated. It must stay below the writers'
	// CompactSpacing (PLAN_BUCKETS.md §7).
	RoundLimit time.Duration
}

// DefaultOptions are the defaults.
func DefaultOptions() Options {
	return Options{
		PrefixBudget: 32 << 10,
		CoalesceGap:  4 << 10,
		MaxRequest:   1 << 20,
		Concurrency:  16,
		CacheBytes:   64 << 20,
		RoundLimit:   500 * time.Millisecond,
	}
}

// Request is one search.
type Request struct {
	Query  string
	Limit  int
	Offset int
	Fuzzy  bool
}

// Hit is one result: the ID, which the caller resolves in its own database,
// and how it ranked.
type Hit struct {
	ID       uint32
	Coverage uint32
	Score    uint32
}

// Stats says what a search cost.
type Stats struct {
	Rounds   int
	Requests int
	Bytes    int64 // every byte read, p.log included
	LogBytes int64 // of which p.log
	IdxBytes int64 // of which .idx
	// Candidates is the documents scored; LogEntries the pending ops of p.log.
	Candidates int
	LogEntries int
	// Approx is set when a prefix or typo expansion was cut by the budget.
	Approx bool
	// LogOnly is set when the collection has no index (under MinIndexDocs)
	// and the answer came from p.log alone.
	LogOnly bool
	// Warm is set when the .idx it needed were cached.
	Warm bool
	// Heads is the expanded words read through their head; Pairs the
	// combinations read; Deep the whole lists read after the heads fell
	// short.
	Heads, Pairs, Deep int
	// Retries counts the attempts started over: a compaction or a rebuild
	// between the reads.
	Retries int
	Elapsed time.Duration
}

// Response is the answer to a Request.
type Response struct {
	Seq   uint64 // the LastSeq of the index the answer comes from
	Hits  []Hit
	Stats Stats
}

// Engine serves the searches of one collection. It is safe for concurrent
// use; a container holds one per collection for its whole life.
type Engine struct {
	store blob.Blob
	col   layout.Collection
	norm  *normalize.Normalizer
	opts  Options

	// The cache: decoded buckets by group, valid as of (epoch, seq). A nil
	// Index is a bucket known to be empty. grouping is how the index groups
	// its slots: the collection's, until a p.log base says otherwise.
	mu         sync.Mutex
	grouping   *layout.Grouping
	epoch      uint32
	seq        uint64
	cache      map[uint16]*bucket.Index
	cacheBytes int64
}

// New returns an engine with an empty cache. norm must be the normalizer the
// collection was indexed with: every file's NormID is checked against it.
func New(store blob.Blob, col layout.Collection, norm *normalize.Normalizer, opts Options) *Engine {
	def := DefaultOptions()

	if opts.RoundLimit <= 0 {
		opts.RoundLimit = def.RoundLimit
	}

	if opts.Concurrency <= 0 {
		opts.Concurrency = def.Concurrency
	}

	if opts.MaxRequest <= 0 {
		opts.MaxRequest = def.MaxRequest
	}

	return &Engine{store: store, col: col, norm: norm, opts: opts, grouping: col.Grouping(), cache: map[uint16]*bucket.Index{}}
}

// ErrNoCollection is returned for a collection without p.log.
var ErrNoCollection = errors.New("query: the collection does not exist")

// errStale is a bucket of another version than the p.log read with it: a
// compaction or a rebuild ran in between. The search starts over.
var errStale = errors.New("query: the index changed during the search")

// errRegroup: the index is grouped otherwise than the engine thought; the
// search starts over with its grouping.
var errRegroup = fmt.Errorf("%w: regrouped", errStale)

// counters are shared by the requests of one search.
type counters struct {
	requests, bytes, logBytes, idxBytes atomic.Int64
}

// A search started over waits a random time under staleBackoff << attempt
// (capped at attempt 5): about 3 s over maxAttempts at most, enough for a
// rebuild, whose new-epoch buckets are stale until it rebases p.log, to
// finish writing them.
const (
	maxAttempts  = 8
	staleBackoff = 25 * time.Millisecond
)

// Search runs one query: p.log and the .idx of its buckets in one round, the
// ranges of .db it needs in a second. With the .idx cached, one round.
func (e *Engine) Search(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	q := Parse(e.norm, req.Query)

	var resp Response
	var c counters

	if q.Empty() {
		return resp, nil
	}

	var last error

	for attempt := range maxAttempts {
		if attempt > 0 {
			resp.Stats.Retries++

			// Following the grouping of the index has nothing to wait for.
			if last != errRegroup {
				if err := plog.Backoff(ctx, attempt, staleBackoff); err != nil {
					return resp, err
				}
			}
		}

		resp.Stats.Rounds = 0

		err := e.attempt(ctx, q, req, &resp, &c)
		if errors.Is(err, errStale) {
			last = err

			continue
		}

		resp.Stats.Requests = int(c.requests.Load())
		resp.Stats.Bytes = c.bytes.Load()
		resp.Stats.LogBytes = c.logBytes.Load()
		resp.Stats.IdxBytes = c.idxBytes.Load()
		resp.Stats.Elapsed = time.Since(start)

		return resp, err
	}

	return resp, errors.New("query: the index kept changing during the search")
}

// fetched is an .idx read in this search.
type fetched struct {
	group uint16
	data  []byte
	found bool
}

// attempt runs the search once.
func (e *Engine) attempt(ctx context.Context, q Parsed, req Request, resp *Response, c *counters) error {
	// Round 1: p.log, the .idx not cached, and — when every .idx is cached —
	// the .db ranges they say, speculatively.
	grouping, pinned, missing := e.lookup(querySlots(q.Terms))
	col := e.col.With(grouping)
	source := func(s uint16) *bucket.Index { return pinned[grouping.Of(s)] }

	var spec *readPlan
	var specData *dbData

	if len(missing) == 0 {
		spec = planQuery(q.Terms, source, e.opts.PrefixBudget, req.Fuzzy)
		specData = &dbData{col: col}
	}

	var log *plog.Log
	got := make([]fetched, len(missing))
	started := time.Now()

	var specJobs []dbRange
	if spec != nil {
		specJobs = e.ranges(spec.picks, spec.pairs)
	}

	err := parallel.Run(ctx, 1+len(missing)+len(specJobs), e.opts.Concurrency, func(ctx context.Context, i int) error {
		switch {
		case i == 0:
			var err error
			log, err = e.fetchLog(ctx, c)

			return err
		case i <= len(missing):
			got[i-1].group = missing[i-1]
			got[i-1].data, got[i-1].found, _ = e.fetchIdx(ctx, col, missing[i-1], c)

			return nil
		}

		return e.fetchRange(ctx, specJobs[i-1-len(missing)], specData, c)
	})

	resp.Stats.Rounds++

	if err != nil {
		return err
	}

	if len(missing) > 0 && time.Since(started) > e.opts.RoundLimit {
		return errStale // too slow to tell an old bucket from a current one
	}

	base := log.Base
	resp.Seq = base.LastSeq
	resp.Stats.LogEntries = len(log.Pending())

	if base.LogOnly {
		return e.searchLog(q, req, log, resp)
	}

	// The index is grouped otherwise than this engine thought: follow it.
	if base.Grouping != grouping.ID() {
		return e.regroup(base.Grouping)
	}

	// Validate what was cached against this base, and what was read.
	usable := e.sync(base)

	var refetch []uint16

	for group, x := range pinned {
		if !usable || !e.still(group, x) {
			delete(pinned, group)
			refetch = append(refetch, group)
		}
	}

	for _, f := range got {
		x, err := e.validate(f, base, grouping)
		if err != nil {
			return err
		}

		pinned[f.group] = x
	}

	if len(refetch) > 0 {
		// Read after p.log: never older than the base it says.
		again := make([]fetched, len(refetch))

		err := parallel.Run(ctx, len(refetch), e.opts.Concurrency, func(ctx context.Context, i int) error {
			again[i].group = refetch[i]
			again[i].data, again[i].found, _ = e.fetchIdx(ctx, col, refetch[i], c)

			return nil
		})

		resp.Stats.Rounds++

		if err != nil {
			return err
		}

		for _, f := range again {
			x, err := e.validate(f, base, grouping)
			if err != nil {
				return err
			}

			pinned[f.group] = x
		}

		got = append(got, again...)
		spec = nil
	}

	if usable {
		e.remember(grouping, got, pinned)
	}

	resp.Stats.Warm = len(missing) == 0 && spec != nil

	p := spec
	data := specData

	if p == nil {
		p = planQuery(q.Terms, source, e.opts.PrefixBudget, req.Fuzzy)
		data = &dbData{col: col}

		if n, err := e.read(ctx, p.picks, p.pairs, data, c); err != nil {
			return err
		} else if n > 0 {
			resp.Stats.Rounds++
		}
	}

	if err := data.check(p); err != nil {
		return err
	}

	resp.Stats.Approx = p.approx

	return e.answer(ctx, q, req, p, data, log, int(base.Docs), resp, c)
}

// answer assembles the candidates, ranks them and checks the ranking is
// exact; when a head left room for a document it did not show, it reads the
// whole list of that word and ranks again (PLAN_BUCKETS.md §6.1, step 6).
func (e *Engine) answer(ctx context.Context, q Parsed, req Request, p *readPlan, data *dbData, log *plog.Log, docs int, resp *Response, c *counters) error {
	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	offset := max(req.Offset, 0)
	need := offset + limit
	idf := idfs(p.df, docs)
	pending := log.Pending()

	resp.Stats.Pairs = len(p.pairs)
	for _, pk := range p.picks {
		if pk.headed() {
			resp.Stats.Heads++
		}
	}

	for {
		cands, err := assemble(q.Terms, p, data, pending)
		if err != nil {
			return err
		}

		results := rankAll(q.Terms, cands, idf, need)
		short := shortHeads(q.Terms, p, idf, results, need)

		if len(short) == 0 {
			resp.Stats.Candidates = cands.len()
			from, to := min(offset, len(results)), min(offset+limit, len(results))
			resp.Hits = make([]Hit, 0, to-from)

			for _, r := range results[from:to] {
				resp.Hits = append(resp.Hits, Hit{ID: r.id, Coverage: r.score.coverage, Score: r.score.score})
			}

			return nil
		}

		for _, pk := range short {
			pk.full = true
		}

		resp.Stats.Deep += len(short)

		if _, err := e.read(ctx, short, nil, data, c); err != nil {
			return err
		}

		resp.Stats.Rounds++

		if err := data.check(p); err != nil {
			return err
		}
	}
}

func (e *Engine) fetchLog(ctx context.Context, c *counters) (*plog.Log, error) {
	data, _, err := e.store.Get(ctx, e.col.Log())
	c.requests.Add(1)
	c.bytes.Add(int64(len(data)))
	c.logBytes.Add(int64(len(data)))

	if errors.Is(err, blob.ErrNotFound) {
		return nil, ErrNoCollection
	}

	if err != nil {
		return nil, fmt.Errorf("query: p.log: %w", err)
	}

	return plog.Parse(data, e.norm.ID())
}

// fetchIdx reads the .idx of a group. Errors other than a missing object
// come back as found with no data, which validate reports.
func (e *Engine) fetchIdx(ctx context.Context, col layout.Collection, group uint16, c *counters) ([]byte, bool, error) {
	data, _, err := e.store.Get(ctx, col.Idx(group))
	c.requests.Add(1)
	c.bytes.Add(int64(len(data)))
	c.idxBytes.Add(int64(len(data)))

	if errors.Is(err, blob.ErrNotFound) {
		return nil, false, nil
	}

	if err != nil {
		return nil, true, err
	}

	return data, true, nil
}

// validate decides whether an .idx read belongs to the p.log base read with
// it (PLAN_BUCKETS.md §7). It returns the decoded index, nil for an empty
// bucket, or errStale.
func (e *Engine) validate(f fetched, base plog.Base, grouping *layout.Grouping) (*bucket.Index, error) {
	touched := base.Touched.Has(int(f.group))
	name := e.col.With(grouping).Idx(f.group)

	if !f.found {
		// Missing: never written, or written by the last compaction after
		// this read.
		if touched {
			return nil, errStale
		}

		return nil, nil
	}

	if f.data == nil {
		return nil, fmt.Errorf("query: %s: read failed", name)
	}

	m, err := bucket.ReadMeta(f.data)
	if err != nil {
		return nil, err
	}

	switch {
	case m.Epoch > base.Epoch:
		return nil, errStale // a rebuild in progress
	case m.Epoch < base.Epoch:
		if touched {
			return nil, errStale
		}

		return nil, nil // a bucket of a replaced index or grouping, garbage
	case m.Seq < base.LastSeq && touched:
		return nil, errStale
	}

	x, err := bucket.Decode(f.data, e.norm.ID())
	if err != nil {
		return nil, err
	}

	if x.Group != f.group || x.Grouping != grouping.ID() {
		return nil, fmt.Errorf("query: %s holds group %d of grouping %d", name, x.Group, x.Grouping)
	}

	if len(x.Words) == 0 {
		return nil, nil // a tombstone
	}

	return x, nil
}

// lookup returns the grouping, the cached buckets of the groups of slots and
// the groups not cached.
func (e *Engine) lookup(slots []uint16) (*layout.Grouping, map[uint16]*bucket.Index, []uint16) {
	e.mu.Lock()
	defer e.mu.Unlock()

	pinned := make(map[uint16]*bucket.Index, len(slots))
	var missing []uint16

	for _, s := range slots {
		group := e.grouping.Of(s)

		if _, done := pinned[group]; done || slices.Contains(missing, group) {
			continue
		}

		if x, ok := e.cache[group]; ok {
			pinned[group] = x
		} else {
			missing = append(missing, group)
		}
	}

	return e.grouping, pinned, missing
}

// regroup switches the engine to the grouping a p.log base says, and has
// the search start over with it.
func (e *Engine) regroup(id uint8) error {
	g, err := layout.GroupingByID(id)
	if err != nil {
		return err
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.grouping != g {
		e.grouping = g
		e.cache, e.cacheBytes = map[uint16]*bucket.Index{}, 0
		e.epoch, e.seq = 0, 0
	}

	return errRegroup
}

// sync moves the cache to base: the buckets the compaction that wrote base
// rewrote are dropped, everything if more than one compaction ran. It
// reports false for a base older than the cache, which is then neither used
// nor filled.
func (e *Engine) sync(base plog.Base) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	switch {
	case e.epoch == base.Epoch && e.seq == base.LastSeq:
	case e.epoch == base.Epoch && e.seq == base.PrevSeq:
		for s := range e.cache {
			if base.Touched.Has(int(s)) {
				e.forget(s)
			}
		}

		e.seq = base.LastSeq
	case e.epoch == base.Epoch && base.LastSeq < e.seq:
		return false
	default:
		e.cache, e.cacheBytes = map[uint16]*bucket.Index{}, 0
		e.epoch, e.seq = base.Epoch, base.LastSeq
	}

	return true
}

func (e *Engine) forget(s uint16) {
	if x := e.cache[s]; x != nil {
		e.cacheBytes -= int64(x.Size)
	}

	delete(e.cache, s)
}

// still reports whether the cache still holds x for slot s.
func (e *Engine) still(s uint16, x *bucket.Index) bool {
	e.mu.Lock()
	defer e.mu.Unlock()

	have, ok := e.cache[s]

	return ok && have == x
}

// remember caches the buckets read, unless the engine moved to another
// grouping meanwhile.
func (e *Engine) remember(grouping *layout.Grouping, got []fetched, pinned map[uint16]*bucket.Index) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.grouping != grouping {
		return
	}

	for _, f := range got {
		x := pinned[f.group]
		if _, ok := e.cache[f.group]; ok {
			continue
		}

		size := int64(64)
		if x != nil {
			size += int64(x.Size) + int64(len(x.Words))*64
		}

		if e.cacheBytes+size > e.opts.CacheBytes {
			// A simple bound: drop a random half rather than track recency.
			for s := range e.cache {
				if rand.IntN(2) == 0 {
					e.forget(s)
				}
			}
		}

		e.cache[f.group] = x
		e.cacheBytes += size
	}
}

// dbRange is a byte range of the .db of a group.
type dbRange struct {
	group    uint16
	off, end uint64
	etag     string // the .idx's DBETag: what the read must come back with
}

// dbData holds the ranges read, per group, from the objects of col.
type dbData struct {
	col    layout.Collection
	mu     sync.Mutex
	chunks map[uint16][]chunk
	bad    bool // a range came back from another version of its .db
}

type chunk struct {
	off  uint64
	data []byte
}

// get returns the data covering [off, end) of a group and the offset it
// starts at.
func (d *dbData) get(group uint16, off, end uint64) ([]byte, uint64, bool) {
	for _, ch := range d.chunks[group] {
		if off >= ch.off && end <= ch.off+uint64(len(ch.data)) {
			return ch.data, ch.off, true
		}
	}

	return nil, 0, false
}

// check fails with errStale when a range was of another .db than its .idx says.
func (d *dbData) check(p *readPlan) error {
	if d.bad {
		return errStale
	}

	return nil
}

// ranges are the reads of the .db that the whole lists of picks and the
// combinations of pairs need, merged when close.
func (e *Engine) ranges(picks []*pick, pairs []pairRead) []dbRange {
	var want []dbRange

	for _, pk := range picks {
		if pk.full && (pk.w.Loc == bucket.LocDB || pk.w.Loc == bucket.LocExpanded) {
			off, end := pk.w.Bytes()
			want = append(want, dbRange{group: pk.x.Group, off: off, end: end, etag: pk.x.DBETag})
		}
	}

	for _, pr := range pairs {
		off := uint64(pr.combo.Off)
		want = append(want, dbRange{group: pr.anchor.x.Group, off: off, end: off + uint64(pr.combo.Len), etag: pr.anchor.x.DBETag})
	}

	slices.SortFunc(want, func(a, b dbRange) int {
		if a.group != b.group {
			return int(a.group) - int(b.group)
		}

		if a.off != b.off {
			if a.off < b.off {
				return -1
			}

			return 1
		}

		return 0
	})

	var out []dbRange

	for _, r := range want {
		if n := len(out); n > 0 && out[n-1].group == r.group && int64(r.off)-int64(out[n-1].end) <= e.opts.CoalesceGap &&
			int64(max(r.end, out[n-1].end)-out[n-1].off) <= e.opts.MaxRequest {
			out[n-1].end = max(out[n-1].end, r.end)

			continue
		}

		out = append(out, r)
	}

	return out
}

// read reads, in one round, the .db ranges of picks and pairs. It returns
// the requests made.
func (e *Engine) read(ctx context.Context, picks []*pick, pairs []pairRead, data *dbData, c *counters) (int, error) {
	jobs := e.ranges(picks, pairs)

	return len(jobs), parallel.Run(ctx, len(jobs), e.opts.Concurrency, func(ctx context.Context, i int) error {
		return e.fetchRange(ctx, jobs[i], data, c)
	})
}

func (e *Engine) fetchRange(ctx context.Context, r dbRange, data *dbData, c *counters) error {
	b, etag, err := e.store.GetRange(ctx, data.col.DB(r.group), int64(r.off), int64(r.end-r.off))
	c.requests.Add(1)
	c.bytes.Add(int64(len(b)))

	data.mu.Lock()
	defer data.mu.Unlock()

	if data.chunks == nil {
		data.chunks = map[uint16][]chunk{}
	}

	switch {
	case errors.Is(err, blob.ErrNotFound):
		data.bad = true // rewritten without a .db, or dropped
	case err != nil:
		return fmt.Errorf("query: %s: %w", data.col.DB(r.group), err)
	case etag != r.etag || uint64(len(b)) != r.end-r.off:
		data.bad = true
	default:
		data.chunks[r.group] = append(data.chunks[r.group], chunk{off: r.off, data: b})
	}

	return nil
}

// searchLog answers from p.log alone, for a collection without index (under
// MinIndexDocs): every document of the Docs frame and the pending ops is
// matched by the same rules the index path uses, the live documents being
// the dictionary.
func (e *Engine) searchLog(q Parsed, req Request, log *plog.Log, resp *Response) error {
	pending := log.Pending()

	docs := make([][]key5.Token, 0, len(log.Docs))
	ids := make([]uint32, 0, len(log.Docs))

	for _, doc := range log.Docs {
		if _, changed := pending[doc.ID]; changed {
			continue
		}

		tokens, err := key5.ReadTokens(doc.Tokens)
		if err != nil {
			return fmt.Errorf("query: p.log document %d: %w", doc.ID, err)
		}

		docs = append(docs, tokens)
		ids = append(ids, doc.ID)
	}

	for id, op := range pending {
		if op.Del {
			continue
		}

		tokens, err := key5.ReadTokens(op.Tokens)
		if err != nil {
			return fmt.Errorf("query: p.log op %d: %w", op.Seq, err)
		}

		docs = append(docs, tokens)
		ids = append(ids, id)
	}

	fuzzy, df := dictMatch(q.Terms, docs, req.Fuzzy)
	cands := newCandidates(len(q.Terms), len(docs))
	var scratch [key5.MaxUnits]uint8

	for i, tokens := range docs {
		if matches, any := matchTokens(q.Terms, fuzzy, tokens, scratch[:]); any {
			cands.put(ids[i], matches)
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	results := rankAll(q.Terms, cands, idfs(df, len(docs)), max(req.Offset, 0)+limit)
	from := min(max(req.Offset, 0), len(results))
	to := min(from+limit, len(results))

	resp.Hits = make([]Hit, 0, to-from)

	for _, r := range results[from:to] {
		resp.Hits = append(resp.Hits, Hit{ID: r.id, Coverage: r.score.coverage, Score: r.score.score})
	}

	resp.Stats.LogOnly = true
	resp.Stats.Candidates = cands.len()

	return nil
}

// dictMatch is what the dictionary of docs says of every term: whether typos
// are enabled for it (it has no exact or prefix key), and its df, the sum of
// the documents of every key that matches it.
func dictMatch(terms []Term, docs [][]key5.Token, fuzzy bool) ([]bool, []int) {
	dfByKey := map[key5.Key]int{}

	for _, tokens := range docs {
		seen := map[key5.Key]bool{}

		for _, tok := range tokens {
			if !seen[tok.Key] {
				seen[tok.Key] = true
				dfByKey[tok.Key]++
			}
		}
	}

	enabled := make([]bool, len(terms))
	df := make([]int, len(terms))
	var scratch [key5.MaxUnits]uint8

	for i := range terms {
		t := &terms[i]

		for k, n := range dfByKey {
			if kind, _ := t.classifyKey(k, false, scratch[:]); kind != noKey {
				df[i] += n
			}
		}

		if df[i] > 0 || !fuzzy || t.Explicit || t.Number || fuzzyBudget(len(t.Text)) == 0 {
			continue
		}

		enabled[i] = true

		for k, n := range dfByKey {
			if kind, _ := t.classifyKey(k, true, scratch[:]); kind != noKey {
				df[i] += n
			}
		}
	}

	return enabled, df
}

// matchTokens matches a whole document against the terms.
func matchTokens(terms []Term, fuzzy []bool, tokens []key5.Token, scratch []uint8) ([]termMatch, bool) {
	matches := make([]termMatch, len(terms))
	any := false

	for pos, tok := range tokens {
		p := uint8(min(pos, postings.MaxPos))

		for i := range terms {
			kind, d := terms[i].classifyKey(tok.Key, fuzzy[i], scratch)
			if kind == noKey {
				continue
			}

			matches[i].add(terms[i].resolve(kind, tok.Tail), d, p)
			any = true
		}
	}

	return matches, any
}
