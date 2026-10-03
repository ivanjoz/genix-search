// Package plog is p.log, the change log every query reads (PLAN_5BIT.md §5.4).
// It is plain bytes: a fixed Base, then one frame per append. It holds no
// text: each op carries the document's ID and its 5-bit tokens.
//
// A collection under the index threshold (PLAN_5BIT.md §5.8) has no index:
// its Base is flagged LogOnly and is followed by one Docs frame with every
// document as of Base.LastSeq.
package plog

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
)

// Format is the layout version of p.log.
const Format uint16 = 1

const (
	magic      = "GXL1"
	BaseSize   = 36
	frameHead  = 9 // tag u8 | len u32 | crc32c u32
	opHead     = 7 // ID u32 | flags u8 | tokLen u16
	flagDel    = 1
	tagBatch   = 1
	tagCompact = 2
	tagDocs    = 3
	// baseLogOnly is bit 0 of the Base flags (offset 6).
	baseLogOnly = 1
	docHead     = 6 // ID u32 | tokLen u16
	// MaxTokens is the largest token stream of one document.
	MaxTokens = math.MaxUint16
	maxBatch  = math.MaxUint16
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Base opens the log: the manifest it applies on top of, and the last seq
// that manifest already includes.
type Base struct {
	NormID uint32
	// ManifestGen is the generation of the index. With LogOnly it is the last
	// generation the collection had (0 if never indexed), which no query reads.
	ManifestGen uint32
	LastSeq     uint64
	UnixMs      int64
	// LogOnly: the collection has no index; the Docs frame after the Base
	// holds every document as of LastSeq.
	LogOnly bool
}

// Doc is a document of the Docs frame: its ID and its key5 token stream.
type Doc struct {
	ID     uint32
	Tokens []byte
}

// Op is one change. Tokens is the key5 token stream of the new version of
// the document (key5.AppendTokens); empty for a delete.
type Op struct {
	Seq    uint64
	ID     uint32
	Del    bool
	Tokens []byte
}

// Compact asks for a compaction up to TargetSeq.
type Compact struct {
	TargetSeq uint64
	UnixMs    int64
}

// Log is a parsed p.log.
type Log struct {
	Base     Base
	Docs     []Doc // LogOnly: every document as of Base.LastSeq, sorted by ID
	Ops      []Op
	Compacts []Compact
	// Size is the number of valid bytes, which is the offset of the next append.
	Size int64
	// BaseEnd is where the appends start: after the Base and the Docs frame.
	// Size − BaseEnd is what was appended since the last compaction.
	BaseEnd int64
	// TornTail is set when bytes past Size did not form a valid frame.
	TornTail bool
}

// EncodeBase starts a log.
func EncodeBase(b Base) []byte {
	out := make([]byte, BaseSize)
	copy(out, magic)
	binary.LittleEndian.PutUint16(out[4:], Format)

	if b.LogOnly {
		binary.LittleEndian.PutUint16(out[6:], baseLogOnly)
	}

	binary.LittleEndian.PutUint32(out[8:], b.NormID)
	binary.LittleEndian.PutUint32(out[12:], b.ManifestGen)
	binary.LittleEndian.PutUint64(out[16:], b.LastSeq)
	binary.LittleEndian.PutUint64(out[24:], uint64(b.UnixMs))
	binary.LittleEndian.PutUint32(out[32:], crc32.Checksum(out[:32], castagnoli))

	return out
}

// AppendBatch appends ops, whose seqs must be consecutive, as as many Batch
// frames as they need.
func AppendBatch(dst []byte, ops []Op) ([]byte, error) {
	for len(ops) > 0 {
		n := min(len(ops), maxBatch)
		body := binary.LittleEndian.AppendUint64(nil, ops[0].Seq)
		body = binary.LittleEndian.AppendUint16(body, uint16(n))

		for i, op := range ops[:n] {
			if op.Seq != ops[0].Seq+uint64(i) {
				return nil, errors.New("plog: batch seqs are not consecutive")
			}

			if len(op.Tokens) > MaxTokens {
				return nil, fmt.Errorf("plog: document %d has %d bytes of tokens, at most %d", op.ID, len(op.Tokens), MaxTokens)
			}

			var flags byte
			if op.Del {
				flags |= flagDel
			}

			body = binary.LittleEndian.AppendUint32(body, op.ID)
			body = append(body, flags)
			body = binary.LittleEndian.AppendUint16(body, uint16(len(op.Tokens)))
			body = append(body, op.Tokens...)
		}

		dst = appendFrame(dst, tagBatch, body)
		ops = ops[n:]
	}

	return dst, nil
}

// AppendDocs appends the Docs frame of a LogOnly base: n u32, then per
// document ID u32 | tokLen u16 | tokens. docs must be sorted by ID.
func AppendDocs(dst []byte, docs []Doc) ([]byte, error) {
	body := binary.LittleEndian.AppendUint32(nil, uint32(len(docs)))

	for i, doc := range docs {
		if i > 0 && doc.ID <= docs[i-1].ID {
			return nil, errors.New("plog: documents not sorted by ID")
		}

		if len(doc.Tokens) > MaxTokens {
			return nil, fmt.Errorf("plog: document %d has %d bytes of tokens, at most %d", doc.ID, len(doc.Tokens), MaxTokens)
		}

		body = binary.LittleEndian.AppendUint32(body, doc.ID)
		body = binary.LittleEndian.AppendUint16(body, uint16(len(doc.Tokens)))
		body = append(body, doc.Tokens...)
	}

	return appendFrame(dst, tagDocs, body), nil
}

func parseDocs(body []byte) ([]Doc, error) {
	if len(body) < 4 {
		return nil, errors.New("short docs")
	}

	n := int(binary.LittleEndian.Uint32(body))
	if n > len(body)/docHead {
		return nil, errors.New("bad docs count")
	}

	docs := make([]Doc, n)
	at := 4

	for i := range docs {
		if len(body)-at < docHead {
			return nil, errors.New("short doc")
		}

		docs[i].ID = binary.LittleEndian.Uint32(body[at:])
		tokLen := int(binary.LittleEndian.Uint16(body[at+4:]))
		at += docHead

		if len(body)-at < tokLen {
			return nil, errors.New("short tokens")
		}

		docs[i].Tokens = body[at : at+tokLen : at+tokLen]
		at += tokLen
	}

	if at != len(body) {
		return nil, errors.New("trailing bytes")
	}

	return docs, nil
}

// AppendCompact appends a compaction request.
func AppendCompact(dst []byte, c Compact) []byte {
	body := binary.LittleEndian.AppendUint64(nil, c.TargetSeq)
	body = binary.LittleEndian.AppendUint64(body, uint64(c.UnixMs))

	return appendFrame(dst, tagCompact, body)
}

func appendFrame(dst []byte, tag byte, body []byte) []byte {
	dst = append(dst, tag)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(body)))
	dst = binary.LittleEndian.AppendUint32(dst, crc32.Checksum(body, castagnoli))

	return append(dst, body...)
}

// ErrNormID is a log written with another normalizer.
var ErrNormID = errors.New("plog: the log was written with another normalizer or other extra letters")

// Parse reads a whole log. normID is the reader's normalizer.
func Parse(data []byte, normID uint32) (*Log, error) {
	if len(data) < BaseSize || string(data[:4]) != magic {
		return nil, errors.New("plog: not a p.log")
	}

	if f := binary.LittleEndian.Uint16(data[4:]); f != Format {
		return nil, fmt.Errorf("plog: format %d, this build reads %d", f, Format)
	}

	if crc32.Checksum(data[:32], castagnoli) != binary.LittleEndian.Uint32(data[32:]) {
		return nil, errors.New("plog: base CRC")
	}

	log := &Log{Base: Base{
		NormID:      binary.LittleEndian.Uint32(data[8:]),
		ManifestGen: binary.LittleEndian.Uint32(data[12:]),
		LastSeq:     binary.LittleEndian.Uint64(data[16:]),
		UnixMs:      int64(binary.LittleEndian.Uint64(data[24:])),
		LogOnly:     binary.LittleEndian.Uint16(data[6:])&baseLogOnly != 0,
	}}

	if log.Base.NormID != normID {
		return nil, ErrNormID
	}

	at := BaseSize
	log.BaseEnd = BaseSize

	if log.Base.LogOnly {
		// The Docs frame is part of the base: it was written with it, in one
		// put, so a torn one is corruption, not a torn append.
		if len(data)-at < frameHead || data[at] != tagDocs {
			return nil, errors.New("plog: a LogOnly base without its Docs frame")
		}

		n := int(binary.LittleEndian.Uint32(data[at+1:]))
		end := at + frameHead + n

		if n > len(data) || end > len(data) || crc32.Checksum(data[at+frameHead:end], castagnoli) != binary.LittleEndian.Uint32(data[at+5:]) {
			return nil, errors.New("plog: corrupt Docs frame")
		}

		docs, err := parseDocs(data[at+frameHead : end])
		if err != nil {
			return nil, fmt.Errorf("plog: Docs frame: %w", err)
		}

		log.Docs = docs
		at = end
		log.BaseEnd = int64(end)
	}

	for at < len(data) {
		if len(data)-at < frameHead {
			log.TornTail = true

			break
		}

		tag := data[at]
		n := int(binary.LittleEndian.Uint32(data[at+1:]))
		end := at + frameHead + n

		if n > len(data) || end > len(data) || crc32.Checksum(data[at+frameHead:end], castagnoli) != binary.LittleEndian.Uint32(data[at+5:]) {
			log.TornTail = true

			break
		}

		body := data[at+frameHead : end]

		switch tag {
		case tagBatch:
			ops, err := parseBatch(body)
			if err != nil {
				return nil, fmt.Errorf("plog: batch at %d: %w", at, err)
			}

			log.Ops = append(log.Ops, ops...)
		case tagCompact:
			if len(body) != 16 {
				return nil, fmt.Errorf("plog: compact at %d: %d bytes", at, len(body))
			}

			log.Compacts = append(log.Compacts, Compact{
				TargetSeq: binary.LittleEndian.Uint64(body),
				UnixMs:    int64(binary.LittleEndian.Uint64(body[8:])),
			})
		default:
			// Includes a Docs frame anywhere but right after a LogOnly base.
			return nil, fmt.Errorf("plog: unexpected tag %d at %d", tag, at)
		}

		at = end
	}

	log.Size = int64(at)

	return log, nil
}

func parseBatch(body []byte) ([]Op, error) {
	if len(body) < 10 {
		return nil, errors.New("short batch")
	}

	first := binary.LittleEndian.Uint64(body)
	n := int(binary.LittleEndian.Uint16(body[8:]))
	ops := make([]Op, n)
	at := 10

	for i := range ops {
		if len(body)-at < opHead {
			return nil, errors.New("short op")
		}

		op := &ops[i]
		op.Seq = first + uint64(i)
		op.ID = binary.LittleEndian.Uint32(body[at:])
		op.Del = body[at+4]&flagDel != 0
		tokLen := int(binary.LittleEndian.Uint16(body[at+5:]))
		at += opHead

		if len(body)-at < tokLen {
			return nil, errors.New("short tokens")
		}

		op.Tokens = body[at : at+tokLen : at+tokLen]
		at += tokLen
	}

	if at != len(body) {
		return nil, errors.New("trailing bytes")
	}

	return ops, nil
}

// LastSeq is the highest seq in the log, or the base's when it has no ops.
func (l *Log) LastSeq() uint64 {
	last := l.Base.LastSeq
	for _, op := range l.Ops {
		last = max(last, op.Seq)
	}

	return last
}

// Pending returns, per ID, the last op the base does not include yet: the
// overlay a query applies on top of k and p.
func (l *Log) Pending() map[uint32]Op {
	pending := make(map[uint32]Op)

	for _, op := range l.Ops {
		if op.Seq <= l.Base.LastSeq {
			continue
		}

		if current, ok := pending[op.ID]; !ok || op.Seq > current.Seq {
			pending[op.ID] = op
		}
	}

	return pending
}

// LiveDocs is the number of documents once the pending ops apply, for a
// LogOnly log (Docs plus Pending).
func (l *Log) LiveDocs() int {
	live := make(map[uint32]bool, len(l.Docs))
	for _, doc := range l.Docs {
		live[doc.ID] = true
	}

	for id, op := range l.Pending() {
		live[id] = !op.Del
	}

	n := 0
	for _, ok := range live {
		if ok {
			n++
		}
	}

	return n
}

// CompactPending reports a compaction that was asked for and is not done yet.
func (l *Log) CompactPending() bool {
	for _, c := range l.Compacts {
		if c.TargetSeq > l.Base.LastSeq {
			return true
		}
	}

	return false
}
