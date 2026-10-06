// Package tokens is the tokens object of a partition:
// the document → tokens state the index was built from, the expanded words and the
// hash of every bucket. Only the compaction reads it, to know what a changed
// document had and which buckets it has to rewrite; queries never do.
//
// The documents are stored through a vocabulary: every distinct token once,
// the most frequent first, and each document as the numbers of its tokens,
// all compressed with DEFLATE. The compaction reads and writes the object
// whole, so nothing in it needs random access.
package tokens

import (
	"bytes"
	"cmp"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"slices"

	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

// Format is the layout version.
const Format uint16 = 4

const (
	magic      = "GXT1"
	headerSize = 48
	slotEntry  = 12 // group u16 | flags u8 | grouping u8 (garbage) | hash u64
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

// Doc is one document: its key and its key5 token stream.
type Doc struct {
	ID     docid.Key
	Tokens []byte
}

// State is the whole object.
type State struct {
	NormID   uint32
	Epoch    uint32
	Seq      uint64     // the ops up to Seq are in Docs
	Docs     []Doc      // sorted by key
	Expanded []key5.Key // sorted
	// Grouping is the layout.Grouping ID of Buckets.
	Grouping uint8
	// Buckets are the buckets of the index, by group.
	Buckets map[uint16]Bucket
	// Garbage are buckets no query reads anymore — tombstones, and those of
	// an older epoch or grouping — to delete once the queries that read them
	// before the change are done.
	Garbage []Garbage
}

// Garbage is a bucket to delete: a group of a grouping.
type Garbage struct {
	Grouping uint8
	Group    uint16
}

// Bucket is what the compaction remembers of a bucket it wrote.
type Bucket struct {
	Hash  uint64 // of the content, bucket.Built.Hash
	HasDB bool
}

// slot entry flags.
const (
	flagGarbage = 1
	flagDB      = 2
)

// Encode writes s. Every document's tokens must be a valid key5 token stream.
func Encode(s *State) ([]byte, error) {
	docs := s.Docs
	if !slices.IsSortedFunc(docs, func(a, b Doc) int { return cmp.Compare(a.ID, b.ID) }) {
		return nil, errors.New("tokens: documents not sorted by key")
	}

	slots := make([]uint16, 0, len(s.Buckets))
	for slot := range s.Buckets {
		slots = append(slots, slot)
	}

	slices.Sort(slots)

	garbage := slices.Clone(s.Garbage)
	slices.SortFunc(garbage, func(a, b Garbage) int {
		return int(a.Grouping)<<16 | int(a.Group) - (int(b.Grouping)<<16 | int(b.Group))
	})
	garbage = slices.Compact(garbage)

	entries := len(slots) + len(garbage)
	tables := len(s.Expanded)*8 + entries*slotEntry
	out := make([]byte, headerSize+tables)

	at := headerSize
	for _, k := range s.Expanded {
		binary.LittleEndian.PutUint64(out[at:], uint64(k))
		at += 8
	}

	for _, slot := range slots {
		b := s.Buckets[slot]

		var flags byte
		if b.HasDB {
			flags |= flagDB
		}

		binary.LittleEndian.PutUint16(out[at:], slot)
		out[at+2] = flags
		binary.LittleEndian.PutUint64(out[at+4:], b.Hash)
		at += slotEntry
	}

	for _, g := range garbage {
		binary.LittleEndian.PutUint16(out[at:], g.Group)
		out[at+2], out[at+3] = flagGarbage, g.Grouping
		at += slotEntry
	}

	body, err := encodeDocs(docs)
	if err != nil {
		return nil, err
	}

	out = append(out, body...)

	copy(out, magic)
	binary.LittleEndian.PutUint16(out[4:], Format)
	binary.LittleEndian.PutUint32(out[8:], s.NormID)
	binary.LittleEndian.PutUint32(out[12:], s.Epoch)
	binary.LittleEndian.PutUint64(out[16:], s.Seq)
	binary.LittleEndian.PutUint32(out[24:], uint32(len(docs)))
	binary.LittleEndian.PutUint32(out[28:], crc32.Checksum(body, castagnoli))
	binary.LittleEndian.PutUint32(out[32:], uint32(len(s.Expanded)))
	binary.LittleEndian.PutUint32(out[36:], uint32(entries))
	out[40] = s.Grouping

	crc := crc32.Update(crc32.Checksum(out[:44], castagnoli), castagnoli, out[headerSize:headerSize+tables])
	binary.LittleEndian.PutUint32(out[44:], crc)

	return out, nil
}

// encodeDocs writes the body: DEFLATE of the vocabulary (its length and a
// token stream of every distinct token, the most frequent first), then the
// key gaps, then the token counts, then the token numbers of every document,
// all uvarints.
func encodeDocs(docs []Doc) ([]byte, error) {
	toks := make([][]key5.Token, len(docs))
	freq := map[key5.Token]int{}

	for i, doc := range docs {
		if i > 0 && doc.ID == docs[i-1].ID {
			return nil, fmt.Errorf("tokens: document %v twice", doc.ID)
		}

		t, err := key5.ReadTokens(doc.Tokens)
		if err != nil {
			return nil, fmt.Errorf("tokens: document %v: %w", doc.ID, err)
		}

		toks[i] = t
		for _, tk := range t {
			freq[tk]++
		}
	}

	vocab := make([]key5.Token, 0, len(freq))
	for tk := range freq {
		vocab = append(vocab, tk)
	}

	// The most frequent first, so most numbers take one byte; ties by key,
	// so the object depends only on its documents.
	slices.SortFunc(vocab, func(a, b key5.Token) int {
		return cmp.Or(cmp.Compare(freq[b], freq[a]), cmp.Compare(a.Key, b.Key), cmp.Compare(a.Tail, b.Tail))
	})

	number := make(map[key5.Token]uint64, len(vocab))
	for i, tk := range vocab {
		number[tk] = uint64(i)
	}

	stream := key5.AppendTokens(nil, vocab)
	raw := binary.AppendUvarint(nil, uint64(len(stream)))
	raw = append(raw, stream...)

	prev := docid.Key(0)
	for _, doc := range docs {
		raw = binary.AppendUvarint(raw, uint64(doc.ID-prev))
		prev = doc.ID
	}

	for _, t := range toks {
		raw = binary.AppendUvarint(raw, uint64(len(t)))
	}

	for _, t := range toks {
		for _, tk := range t {
			raw = binary.AppendUvarint(raw, number[tk])
		}
	}

	var buf bytes.Buffer

	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return nil, err
	}

	if _, err := w.Write(raw); err != nil {
		return nil, err
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	return buf.Bytes(), nil
}

// Decode reads a whole object. A tokens object of another normalizer is an
// error: the caller rebuilds from the source.
func Decode(data []byte, normID uint32) (*State, error) {
	if len(data) < headerSize || string(data[:4]) != magic {
		return nil, errors.New("tokens: not a tokens object")
	}

	if f := binary.LittleEndian.Uint16(data[4:]); f != Format {
		return nil, fmt.Errorf("tokens: format %d, this build reads %d", f, Format)
	}

	s := &State{
		NormID:   binary.LittleEndian.Uint32(data[8:]),
		Epoch:    binary.LittleEndian.Uint32(data[12:]),
		Seq:      binary.LittleEndian.Uint64(data[16:]),
		Grouping: data[40],
		Buckets:  map[uint16]Bucket{},
	}

	count := int(binary.LittleEndian.Uint32(data[24:]))
	expanded := int(binary.LittleEndian.Uint32(data[32:]))
	slots := int(binary.LittleEndian.Uint32(data[36:]))

	tables := expanded*8 + slots*slotEntry
	if expanded < 0 || slots < 0 || tables < 0 || tables > len(data)-headerSize {
		return nil, errors.New("tokens: corrupt tables")
	}

	crc := crc32.Update(crc32.Checksum(data[:44], castagnoli), castagnoli, data[headerSize:headerSize+tables])
	if crc != binary.LittleEndian.Uint32(data[44:]) {
		return nil, errors.New("tokens: header CRC")
	}

	if s.NormID != normID {
		return nil, errors.New("tokens: written with another normalizer or other extra letters")
	}

	at := headerSize
	s.Expanded = make([]key5.Key, expanded)

	for i := range s.Expanded {
		s.Expanded[i] = key5.Key(binary.LittleEndian.Uint64(data[at:]))
		at += 8
	}

	for range slots {
		slot, flags := binary.LittleEndian.Uint16(data[at:]), data[at+2]

		if flags&flagGarbage != 0 {
			s.Garbage = append(s.Garbage, Garbage{Grouping: data[at+3], Group: slot})
		} else {
			s.Buckets[slot] = Bucket{Hash: binary.LittleEndian.Uint64(data[at+4:]), HasDB: flags&flagDB != 0}
		}

		at += slotEntry
	}

	body := data[at:]
	if crc32.Checksum(body, castagnoli) != binary.LittleEndian.Uint32(data[28:]) {
		return nil, errors.New("tokens: body CRC")
	}

	docs, err := decodeDocs(body, count)
	if err != nil {
		return nil, err
	}

	s.Docs = docs

	return s, nil
}

var errCorrupt = errors.New("tokens: corrupt documents")

// decodeDocs reads what encodeDocs wrote: count documents, each with its
// token stream rebuilt.
func decodeDocs(body []byte, count int) ([]Doc, error) {
	raw, err := io.ReadAll(flate.NewReader(bytes.NewReader(body)))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errCorrupt, err)
	}

	r := &uvarints{data: raw}

	streamLen := r.next()
	if r.err != nil || streamLen > uint64(len(r.data)-r.at) {
		return nil, errCorrupt
	}

	vocab, err := key5.ReadTokens(r.data[r.at : r.at+int(streamLen)])
	if err != nil {
		return nil, fmt.Errorf("%w: vocabulary: %v", errCorrupt, err)
	}

	r.at += int(streamLen)

	// Every document takes at least a byte of gap and one of count.
	if count < 0 || count > (len(r.data)-r.at)/2 {
		return nil, errCorrupt
	}

	docs := make([]Doc, count)
	key := uint64(0)

	for i := range docs {
		gap := r.next()
		if i > 0 && gap == 0 || gap > uint64(docid.Max)-key {
			return nil, fmt.Errorf("%w: key gap", errCorrupt)
		}

		key += gap
		docs[i].ID = docid.Key(key)
	}

	counts := make([]uint64, count)
	for i := range counts {
		counts[i] = r.next()
	}

	var toks []key5.Token

	for i := range docs {
		if counts[i] > uint64(len(r.data)-r.at) {
			return nil, fmt.Errorf("%w: token count", errCorrupt)
		}

		toks = toks[:0]

		for range counts[i] {
			n := r.next()
			if n >= uint64(len(vocab)) {
				return nil, fmt.Errorf("%w: token number", errCorrupt)
			}

			toks = append(toks, vocab[n])
		}

		docs[i].Tokens = key5.AppendTokens(nil, toks)
	}

	if r.err != nil || r.at != len(r.data) {
		return nil, errCorrupt
	}

	return docs, nil
}

// uvarints reads uvarints one after the other; the first error sticks.
type uvarints struct {
	data []byte
	at   int
	err  error
}

func (r *uvarints) next() uint64 {
	if r.err != nil {
		return 0
	}

	v, n := binary.Uvarint(r.data[r.at:])
	if n <= 0 {
		r.err = errCorrupt

		return 0
	}

	r.at += n

	return v
}
