package plog

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
)

// DefaultCompactBytes is the default CompactBytes: once the appends since the
// last compaction pass it, an append asks for a compaction. Every query reads
// the whole log, so it is kept small.
const DefaultCompactBytes = 4096

// DefaultMinIndexDocs is the default MinIndexDocs: a collection with fewer
// documents has no index, only p.log.
const DefaultMinIndexDocs = 100

// DefaultStaleCompact is the default StaleCompact.
const DefaultStaleCompact = time.Minute

// AppendOptions are the writer's thresholds.
type AppendOptions struct {
	CompactBytes int64
	// MinIndexDocs: a LogOnly collection asks for a compaction (which builds
	// the index) as soon as it reaches this many documents.
	MinIndexDocs int
	// Create makes an append to a missing log create it, LogOnly and empty,
	// with the changes in it. A missing log that only gets deletes is left
	// missing: there is nothing to delete.
	Create bool
	// StaleCompact: a compaction request older than this that is still
	// pending was lost (the process that should have run it died), so the
	// next append asks again. 0 never asks twice.
	StaleCompact time.Duration
	Now          func() time.Time
}

func (o AppendOptions) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}

	return time.Now()
}

// emptyLog is the log Create starts from: a LogOnly base with no documents.
func emptyLog(normID uint32, unixMs int64) []byte {
	out, _ := AppendDocs(EncodeBase(Base{NormID: normID, UnixMs: unixMs, LogOnly: true}), nil)

	return out
}

// maxAttempts bounds the retries when another writer or the compactor got to
// the log first.
const maxAttempts = 16

// Change is one write: the new tokens of a document, or its delete.
type Change struct {
	ID     docid.Key
	Del    bool
	Tokens []byte // key5.AppendTokens
}

// AppendResult is what one Append did.
type AppendResult struct {
	FirstSeq, LastSeq uint64
	Size              int64
	// CompactRequested is set when this append also wrote a Compact frame:
	// the caller should start a compaction.
	CompactRequested bool
	// Requests and Bytes count the reads and writes it took.
	Requests, BytesWritten, BytesRead int64
}

// Append writes changes at the exact offset of the log's current end. If
// another writer appended first the offset no longer matches; it re-reads
// the log and retries, which serialises writers without locks.
func Append(ctx context.Context, store blob.Blob, key string, normID uint32, changes []Change, opts AppendOptions) (AppendResult, error) {
	var result AppendResult

	if len(changes) == 0 {
		return result, errors.New("plog: nothing to append")
	}

	for attempt := range maxAttempts {
		started := time.Now()
		data, _, err := store.Get(ctx, key)
		result.Requests++
		result.BytesRead += int64(len(data))

		missing := errors.Is(err, blob.ErrNotFound) && opts.Create
		if missing {
			if !slices.ContainsFunc(changes, func(c Change) bool { return !c.Del }) {
				return result, nil
			}

			data, err = emptyLog(normID, opts.now().UnixMilli()), nil
		}

		if err != nil {
			return result, fmt.Errorf("plog: read: %w", err)
		}

		log, err := Parse(data, normID)
		if err != nil {
			return result, err
		}

		if log.TornTail {
			return result, errors.New("plog: log has a corrupt tail; refusing to append after it")
		}

		result.FirstSeq = log.LastSeq() + 1
		ops := make([]Op, len(changes))

		for i, change := range changes {
			ops[i] = Op{Seq: result.FirstSeq + uint64(i), ID: change.ID, Del: change.Del}
			if !change.Del {
				ops[i].Tokens = change.Tokens
			}
		}

		result.LastSeq = ops[len(ops)-1].Seq

		payload, err := AppendBatch(nil, ops)
		if err != nil {
			return result, err
		}

		appended := log.Size - log.BaseEnd + int64(len(payload))
		grown := false

		if log.Base.LogOnly && opts.MinIndexDocs > 0 {
			after := *log
			after.Ops = append(slices.Clone(log.Ops), ops...)
			grown = after.LiveDocs() >= opts.MinIndexDocs
		}

		now := opts.now()

		if (appended > opts.CompactBytes || grown) && !log.compactAsked(now, opts.StaleCompact) {
			payload = AppendCompact(payload, Compact{TargetSeq: result.LastSeq, UnixMs: now.UnixMilli()})
			result.CompactRequested = true
		}

		if missing {
			full := append(data, payload...)

			_, err = store.Put(ctx, key, full, blob.PutCond{IfNoneMatch: true})
			result.Size = int64(len(full))
			payload = full
		} else {
			result.Size, err = store.Append(ctx, key, payload, log.Size)
		}

		result.Requests++

		// Lost the race: another writer appended, or created the log first.
		if errors.Is(err, blob.ErrOffsetMismatch) || (missing && errors.Is(err, blob.ErrPrecondition)) {
			result.CompactRequested = false

			if err := Backoff(ctx, attempt, time.Since(started)); err != nil {
				return result, err
			}

			continue
		}

		if err != nil {
			return result, fmt.Errorf("plog: append: %w", err)
		}

		result.BytesWritten += int64(len(payload))

		return result, nil
	}

	return result, errors.New("plog: too much contention on the log")
}

// Backoff waits a jittered, doubling delay so that writers that lost a race
// do not collide again on the next attempt. The delay scales with roundTrip,
// how long the lost attempt took: two writers collide while either is between
// its read and its write, which is one round trip to the store (well under a
// millisecond on disk, ~5 ms in the zone, ~100 ms across a WAN).
func Backoff(ctx context.Context, attempt int, roundTrip time.Duration) error {
	ceiling := max(roundTrip, time.Millisecond) << min(attempt, 5)
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(ceiling))) + time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
