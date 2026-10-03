package query

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/index"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/manifest"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/parallel"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
)

// Options are the query tunables.
type Options struct {
	// PrefixBudget is GENIX_PREFIX_BUDGET_KB: the bytes of p a prefix (or a
	// typo expansion) may read; the most frequent keys go first.
	PrefixBudget int64
	// CoalesceGap is the largest hole merged into one range read: S3 has no
	// multi-range GET, so two lists close together cost one request.
	CoalesceGap int64
	// MaxRequest is the largest merged range read.
	MaxRequest int64
	// Concurrency is the simultaneous requests of one round.
	Concurrency int
	// VerifyPages checks every read of p against the page CRCs of k, which
	// rounds every read to 4 KB pages.
	VerifyPages bool
	// Preload reads the whole k the first time a generation is used, instead
	// of the head and then the slots each query needs.
	Preload bool
	// CacheBytes bounds the decoded slots kept per generation.
	CacheBytes int64
}

// DefaultOptions are the defaults.
func DefaultOptions() Options {
	return Options{
		PrefixBudget: 32 << 10,
		CoalesceGap:  4 << 10,
		MaxRequest:   1 << 20,
		Concurrency:  16,
		VerifyPages:  false,
		Preload:      false,
		CacheBytes:   64 << 20,
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
	Rounds     int
	Requests   int
	Bytes      int64 // every byte read, p.log included
	LogBytes   int64 // of which p.log
	Candidates int
	LogEntries int
	// Approx is set when a prefix or typo expansion was cut by the budget.
	Approx bool
	// LogOnly is set when the collection has no index (under MinIndexDocs)
	// and the answer came from p.log alone.
	LogOnly bool
	Elapsed time.Duration
}

// Response is the answer to a Request.
type Response struct {
	Gen   uint32
	Hits  []Hit
	Stats Stats
}

// genState is what the engine holds of one generation.
type genState struct {
	gen      uint32
	manifest *manifest.Manifest
	head     *index.Head
	pageCRCs []uint32 // nil until loaded

	mu    sync.Mutex
	slots map[uint16]*index.Slot
	bytes int64
}

func (g *genState) crcs() []uint32 {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.pageCRCs
}

func (g *genState) slot(s uint16) *index.Slot {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.slots[s]
}

func (g *genState) putSlots(slots []*index.Slot, limit int64) {
	g.mu.Lock()
	defer g.mu.Unlock()

	for _, s := range slots {
		if g.bytes > limit {
			// A simple bound: start over rather than track recency.
			g.slots, g.bytes = map[uint16]*index.Slot{}, 0
		}

		if _, ok := g.slots[s.ID]; !ok {
			g.slots[s.ID] = s
			g.bytes += int64(s.Size) + int64(len(s.Entries))*48
		}
	}
}

// Engine serves the searches of one collection. It is safe for concurrent
// use; a container holds one per collection for its whole life.
type Engine struct {
	store blob.Blob
	col   layout.Collection
	norm  *normalize.Normalizer
	opts  Options

	mu      sync.Mutex
	current *genState
}

// New returns an engine with empty caches. norm must be the normalizer the
// collection was indexed with: every file's NormID is checked against it.
func New(store blob.Blob, col layout.Collection, norm *normalize.Normalizer, opts Options) *Engine {
	return &Engine{store: store, col: col, norm: norm, opts: opts}
}

// ErrNoCollection is returned for a collection without p.log.
var ErrNoCollection = errors.New("query: the collection does not exist")

// counters are shared by the requests of one search.
type counters struct {
	requests, bytes, logBytes atomic.Int64
}

const maxAttempts = 4

// Search runs one query. With the generation's slots cached it is one round:
// p.log and the lists of p, in parallel.
func (e *Engine) Search(ctx context.Context, req Request) (Response, error) {
	start := time.Now()
	q := Parse(e.norm, req.Query)

	var resp Response
	var c counters

	if q.Empty() {
		return resp, nil
	}

	var log *plog.Log

	for range maxAttempts {
		g := e.state()

		if g == nil {
			if log == nil {
				var err error
				if log, err = e.fetchLog(ctx, &c); err != nil {
					return resp, err
				}

				resp.Stats.Rounds++
			}

			if log.Base.LogOnly {
				return e.searchLog(q, req, log, resp, &c, start)
			}

			var err error
			if g, err = e.loadGen(ctx, log.Base.ManifestGen, &c); err != nil {
				if stale(err) {
					log = nil

					continue
				}

				return resp, err
			}

			resp.Stats.Rounds++
		}

		hits, err := e.run(ctx, g, q, req, &log, &c, &resp.Stats)
		if err != nil {
			if stale(err) {
				e.drop(g)
				log = nil

				continue
			}

			return resp, err
		}

		if log.Base.ManifestGen != g.gen || log.Base.LogOnly {
			// p.log moved to another generation (or dropped the index) while
			// this one was read: the lists belong to the old one. Read again
			// from what p.log says now.
			e.drop(g)

			continue
		}

		resp.Gen = g.gen
		resp.Hits = hits
		resp.Stats.Requests = int(c.requests.Load())
		resp.Stats.Bytes = c.bytes.Load()
		resp.Stats.LogBytes = c.logBytes.Load()
		resp.Stats.Elapsed = time.Since(start)

		return resp, nil
	}

	return resp, errors.New("query: the collection kept changing generation during the search")
}

// searchLog answers from p.log alone, for a collection without index (under
// MinIndexDocs): every document of the Docs frame and the pending ops is
// matched by the same rules the index path uses.
func (e *Engine) searchLog(q Parsed, req Request, log *plog.Log, resp Response, c *counters, start time.Time) (Response, error) {
	pending := log.Pending()

	docs := make([][]key5.Token, 0, len(log.Docs))
	ids := make([]uint32, 0, len(log.Docs))

	for _, doc := range log.Docs {
		if _, changed := pending[doc.ID]; changed {
			continue
		}

		tokens, err := key5.ReadTokens(doc.Tokens)
		if err != nil {
			return resp, fmt.Errorf("query: p.log document %d: %w", doc.ID, err)
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
			return resp, fmt.Errorf("query: p.log op %d: %w", op.Seq, err)
		}

		docs = append(docs, tokens)
		ids = append(ids, id)
	}

	// Typos only for the terms no document has, exactly or as a prefix: the
	// same rule as the index, whose dictionary is every key of the collection.
	fuzzy := make([]bool, len(q.Terms))
	var scratch [key5.MaxUnits]uint8

	if req.Fuzzy {
		for i := range q.Terms {
			t := &q.Terms[i]
			if t.Explicit || t.Number || fuzzyBudget(len(t.Text)) == 0 {
				continue
			}

			fuzzy[i] = !slices.ContainsFunc(docs, func(tokens []key5.Token) bool {
				return slices.ContainsFunc(tokens, func(tok key5.Token) bool {
					kind, _ := t.classifyKey(tok.Key, false, scratch[:])

					return kind != noKey
				})
			})
		}
	}

	cands := newCandidates(len(q.Terms), len(docs))

	for i, tokens := range docs {
		if matches, any := matchTokens(q.Terms, fuzzy, tokens, scratch[:]); any {
			cands.put(ids[i], matches)
		}
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	results := rankAll(q.Terms, cands, len(docs), nil, max(req.Offset, 0)+limit)
	from := min(max(req.Offset, 0), len(results))
	to := min(from+limit, len(results))

	resp.Gen = 0
	resp.Hits = make([]Hit, 0, to-from)

	for _, r := range results[from:to] {
		resp.Hits = append(resp.Hits, Hit{ID: r.id, Coverage: r.score.coverage, Score: r.score.score})
	}

	resp.Stats.LogOnly = true
	resp.Stats.Candidates = cands.len()
	resp.Stats.LogEntries = len(pending)
	resp.Stats.Requests = int(c.requests.Load())
	resp.Stats.Bytes = c.bytes.Load()
	resp.Stats.LogBytes = c.logBytes.Load()
	resp.Stats.Elapsed = time.Since(start)

	return resp, nil
}

// stale is an object that is gone: the generation was collected.
func stale(err error) bool { return errors.Is(err, blob.ErrNotFound) }

func (e *Engine) state() *genState {
	e.mu.Lock()
	defer e.mu.Unlock()

	return e.current
}

func (e *Engine) drop(g *genState) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.current == g {
		e.current = nil
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

func (e *Engine) get(ctx context.Context, key string, off, n int64, c *counters) ([]byte, error) {
	data, err := e.store.GetRange(ctx, key, off, n)
	c.requests.Add(1)
	c.bytes.Add(int64(len(data)))

	if err != nil {
		return nil, fmt.Errorf("query: %s: %w", key, err)
	}

	if n >= 0 && int64(len(data)) != n {
		return nil, fmt.Errorf("query: %s: short read at %d", key, off)
	}

	return data, nil
}

// loadGen reads the manifest and the head of k (or all of k with Preload),
// in one round.
func (e *Engine) loadGen(ctx context.Context, gen uint32, c *counters) (*genState, error) {
	g := &genState{gen: gen, slots: map[uint16]*index.Slot{}}

	var kData []byte

	err := parallel.Run(ctx, 2, 2, func(ctx context.Context, i int) error {
		if i == 0 {
			data, _, err := e.store.Get(ctx, e.col.Manifest(gen))
			c.requests.Add(1)
			c.bytes.Add(int64(len(data)))

			if err != nil {
				return fmt.Errorf("query: manifest %d: %w", gen, err)
			}

			g.manifest, err = manifest.Decode(data)

			return err
		}

		n := int64(index.HeadSize)
		if e.opts.Preload {
			n = -1
		}

		var err error
		kData, err = e.get(ctx, e.col.K(gen), 0, n, c)

		return err
	})
	if err != nil {
		return nil, err
	}

	if g.manifest.NormID != e.norm.ID() {
		return nil, fmt.Errorf("query: the collection uses normalizer %08x (extra letters %q %q), this engine %08x",
			g.manifest.NormID, rune(g.manifest.Extra1), rune(g.manifest.Extra2), e.norm.ID())
	}

	if g.head, err = index.ParseHead(kData, e.norm.ID()); err != nil {
		return nil, err
	}

	if g.head.Gen != gen {
		return nil, fmt.Errorf("query: k.%d says generation %d", gen, g.head.Gen)
	}

	if e.opts.Preload {
		if int64(len(kData)) != int64(g.head.KSize()) {
			return nil, fmt.Errorf("query: k.%d has %d bytes, the header says %d", gen, len(kData), g.head.KSize())
		}

		slots := make([]*index.Slot, 0, key5.Slots)

		for s := range uint16(key5.Slots) {
			off, end := g.head.Block(s)

			slot, err := index.ParseSlot(g.head, s, kData[off:end])
			if err != nil {
				return nil, err
			}

			slots = append(slots, slot)
		}

		g.putSlots(slots, 1<<62)
		g.pageCRCs = index.PageCRCs(kData[g.head.PageCRCOff:])
	}

	e.mu.Lock()
	e.current = g
	e.mu.Unlock()

	return g, nil
}

// loadSlots pins the slots of want for this search: from the cache, or read
// in one round with the page CRCs if needed and p.log if it was not read yet.
// Pinned slots stay put even if another search resets the cache meanwhile.
func (e *Engine) loadSlots(ctx context.Context, g *genState, want []uint16, pinned map[uint16]*index.Slot, log **plog.Log, c *counters) error {
	var missing []uint16

	for _, s := range want {
		if pinned[s] != nil {
			continue
		}

		if slot := g.slot(s); slot != nil {
			pinned[s] = slot
		} else if !slices.Contains(missing, s) {
			missing = append(missing, s)
		}
	}

	slices.Sort(missing)

	needCRCs := e.opts.VerifyPages && g.crcs() == nil

	if len(missing) == 0 && !needCRCs {
		return nil
	}

	// Coalesce the blocks into range reads.
	type span struct {
		off, end uint32
		slots    []uint16
	}

	var spans []span

	for _, s := range missing {
		off, end := g.head.Block(s)
		if n := len(spans); n > 0 && int64(off)-int64(spans[n-1].end) <= e.opts.CoalesceGap && int64(end-spans[n-1].off) <= e.opts.MaxRequest {
			spans[n-1].end = max(spans[n-1].end, end)
			spans[n-1].slots = append(spans[n-1].slots, s)

			continue
		}

		spans = append(spans, span{off: off, end: end, slots: []uint16{s}})
	}

	// Empty slots need no read.
	spans = slices.DeleteFunc(spans, func(sp span) bool { return sp.end == sp.off })

	data := make([][]byte, len(spans))
	jobs := len(spans)
	logJob, crcJob := -1, -1

	if *log == nil {
		logJob = jobs
		jobs++
	}

	if needCRCs {
		crcJob = jobs
		jobs++
	}

	var crcData []byte

	err := parallel.Run(ctx, jobs, e.opts.Concurrency, func(ctx context.Context, i int) error {
		var err error

		switch i {
		case logJob:
			*log, err = e.fetchLog(ctx, c)
		case crcJob:
			off := int64(g.head.PageCRCOff)
			crcData, err = e.get(ctx, e.col.K(g.gen), off, int64(g.head.KSize())-off, c)
		default:
			data[i], err = e.get(ctx, e.col.K(g.gen), int64(spans[i].off), int64(spans[i].end-spans[i].off), c)
		}

		return err
	})
	if err != nil {
		return err
	}

	slots := make([]*index.Slot, 0, len(missing))

	for _, s := range missing {
		off, end := g.head.Block(s)
		if off == end {
			slots = append(slots, &index.Slot{ID: s})

			continue
		}

		i := slices.IndexFunc(spans, func(sp span) bool { return off >= sp.off && end <= sp.end })

		slot, err := index.ParseSlot(g.head, s, data[i][off-spans[i].off:end-spans[i].off])
		if err != nil {
			return err
		}

		slots = append(slots, slot)
	}

	for _, slot := range slots {
		pinned[slot.ID] = slot
	}

	g.putSlots(slots, e.opts.CacheBytes)

	if needCRCs {
		g.mu.Lock()
		g.pageCRCs = index.PageCRCs(crcData)
		g.mu.Unlock()
	}

	return nil
}

// run plans and reads one query on generation g.
func (e *Engine) run(ctx context.Context, g *genState, q Parsed, req Request, log **plog.Log, c *counters, stats *Stats) ([]Hit, error) {
	var want []uint16
	for i := range q.Terms {
		want = append(want, termSlots(&q.Terms[i])...)
	}

	// round counts a round when the loads since the last one made requests.
	before := c.requests.Load()
	round := func() {
		if now := c.requests.Load(); now > before {
			stats.Rounds++
			before = now
		}
	}

	pinned := map[uint16]*index.Slot{}

	if err := e.loadSlots(ctx, g, want, pinned, log, c); err != nil {
		return nil, err
	}

	round()

	// The slots of typo candidates are pinned from the cache when there;
	// planTerms lists the others.
	source := func(s uint16) *index.Slot {
		if slot, ok := pinned[s]; ok {
			return slot
		}

		slot := g.slot(s)
		if slot != nil {
			pinned[s] = slot
		}

		return slot
	}

	p, need := planTerms(q.Terms, source, e.opts.PrefixBudget, req.Fuzzy)

	if len(need) > 0 {
		if err := e.loadSlots(ctx, g, need, pinned, log, c); err != nil {
			return nil, err
		}

		round()

		p, _ = planTerms(q.Terms, source, e.opts.PrefixBudget, req.Fuzzy)
	}

	postings, err := e.readLists(ctx, g, p, log, c)
	if err != nil {
		return nil, err
	}

	round()
	stats.Approx = p.approx

	candidates := gather(q.Terms, p, postings)

	pending := (*log).Pending()
	stats.LogEntries = len(pending)

	if err := overlay(q.Terms, p.fuzzy, candidates, pending); err != nil {
		return nil, err
	}

	stats.Candidates = candidates.len()

	var dfFloor []int
	if p.approx {
		dfFloor = p.dfFloor
	}

	limit := req.Limit
	if limit <= 0 {
		limit = 10
	}

	results := rankAll(q.Terms, candidates, int(g.head.Docs), dfFloor, max(req.Offset, 0)+limit)

	from := min(max(req.Offset, 0), len(results))
	to := min(from+limit, len(results))
	hits := make([]Hit, 0, to-from)

	for _, r := range results[from:to] {
		hits = append(hits, Hit{ID: r.id, Coverage: r.score.coverage, Score: r.score.score})
	}

	return hits, nil
}

// readLists reads the lists of the plan from p (and p.log if not read yet)
// in one round, and decodes them.
func (e *Engine) readLists(ctx context.Context, g *genState, p *readPlan, log **plog.Log, c *counters) ([][]index.Posting, error) {
	type span struct{ off, end uint64 }

	var spans []span
	var ranges []span

	for _, s := range p.entries {
		if s.entry.Bits == 0 {
			continue
		}

		off, end := s.entry.Bytes()
		if e.opts.VerifyPages {
			off = off / index.PageSize * index.PageSize
			end = min((end+index.PageSize-1)/index.PageSize*index.PageSize, g.head.PSize)
		}

		ranges = append(ranges, span{off, end})
	}

	slices.SortFunc(ranges, func(a, b span) int {
		switch {
		case a.off < b.off:
			return -1
		case a.off > b.off:
			return 1
		}

		return 0
	})

	for _, r := range ranges {
		if n := len(spans); n > 0 && int64(r.off)-int64(spans[n-1].end) <= e.opts.CoalesceGap && int64(max(r.end, spans[n-1].end)-spans[n-1].off) <= e.opts.MaxRequest {
			spans[n-1].end = max(spans[n-1].end, r.end)

			continue
		}

		spans = append(spans, r)
	}

	data := make([][]byte, len(spans))
	jobs := len(spans)

	if *log == nil {
		jobs++
	}

	err := parallel.Run(ctx, jobs, e.opts.Concurrency, func(ctx context.Context, i int) error {
		if i == len(spans) {
			var err error
			*log, err = e.fetchLog(ctx, c)

			return err
		}

		var err error
		data[i], err = e.get(ctx, e.col.P(g.gen), int64(spans[i].off), int64(spans[i].end-spans[i].off), c)

		return err
	})
	if err != nil {
		return nil, err
	}

	if e.opts.VerifyPages {
		for i, sp := range spans {
			if err := index.CheckPages(data[i], sp.off, g.head.PSize, g.crcs(), 0); err != nil {
				return nil, err
			}
		}
	}

	out := make([][]index.Posting, len(p.entries))

	for j, s := range p.entries {
		var err error

		if s.entry.Bits == 0 {
			out[j], err = index.Decode(s.entry, nil, 0, nil)
		} else {
			off, end := s.entry.Bytes()
			i := slices.IndexFunc(spans, func(sp span) bool { return off >= sp.off && end <= sp.end })
			out[j], err = index.Decode(s.entry, data[i], spans[i].off, make([]index.Posting, 0, s.entry.Count))
		}

		if err != nil {
			return nil, err
		}
	}

	return out, nil
}

// gather turns the postings read into per-document matches.
func gather(terms []Term, p *readPlan, postings [][]index.Posting) *candidates {
	n := 0
	for _, list := range postings {
		n += len(list)
	}

	cands := newCandidates(len(terms), n)

	for j, s := range p.entries {
		for _, posting := range postings[j] {
			matches := cands.get(posting.ID)

			for _, u := range s.uses {
				matches[u.term].add(terms[u.term].resolve(u.kind, posting.Tail), u.dist, posting.Pos)
			}
		}
	}

	return cands
}

// overlay applies p.log: a document with a pending op is replaced by its new
// version, matched against the terms by the same rules, or removed.
func overlay(terms []Term, fuzzy []bool, cands *candidates, pending map[uint32]plog.Op) error {
	var scratch [key5.MaxUnits]uint8

	for id, op := range pending {
		cands.remove(id)

		if op.Del {
			continue
		}

		tokens, err := key5.ReadTokens(op.Tokens)
		if err != nil {
			return fmt.Errorf("query: p.log op %d: %w", op.Seq, err)
		}

		matches, any := matchTokens(terms, fuzzy, tokens, scratch[:])
		if any {
			cands.put(id, matches)
		}
	}

	return nil
}

// matchTokens matches a whole document against the terms.
func matchTokens(terms []Term, fuzzy []bool, tokens []key5.Token, scratch []uint8) ([]termMatch, bool) {
	matches := make([]termMatch, len(terms))
	any := false

	for pos, tok := range tokens {
		p := uint8(min(pos, index.MaxPos))

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
