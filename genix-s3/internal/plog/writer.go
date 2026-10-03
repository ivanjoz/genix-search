package plog

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"time"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
)

// DefaultCompactBytes is GENIX_LOG_COMPACT_BYTES: once the appends since the
// last compaction pass it, an append asks for a compaction. Every query reads
// the whole log, so it is kept small.
const DefaultCompactBytes = 4096

// DefaultMinIndexDocs is GENIX_MIN_INDEX_DOCS: a collection with fewer
// documents has no index, only p.log.
const DefaultMinIndexDocs = 100

// AppendOptions are the writer's thresholds.
type AppendOptions struct {
	CompactBytes int64
	// MinIndexDocs: a LogOnly collection asks for a compaction (which builds
	// the index) as soon as it reaches this many documents.
	MinIndexDocs int
}

// maxAttempts bounds the retries when another writer or the compactor got to
// the log first.
const maxAttempts = 16

// Change is one write: the new tokens of a document, or its delete.
type Change struct {
	ID     uint32
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
		data, _, err := store.Get(ctx, key)
		result.Requests++
		result.BytesRead += int64(len(data))

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

		if (appended > opts.CompactBytes || grown) && !log.CompactPending() {
			payload = AppendCompact(payload, Compact{TargetSeq: result.LastSeq, UnixMs: time.Now().UnixMilli()})
			result.CompactRequested = true
		}

		result.Size, err = store.Append(ctx, key, payload, log.Size)
		result.Requests++

		if errors.Is(err, blob.ErrOffsetMismatch) {
			result.CompactRequested = false

			if err := Backoff(ctx, attempt); err != nil {
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
// do not collide again on the next attempt.
func Backoff(ctx context.Context, attempt int) error {
	ceiling := time.Millisecond << min(attempt, 6)
	timer := time.NewTimer(time.Duration(rand.Int64N(int64(ceiling))) + time.Millisecond)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
