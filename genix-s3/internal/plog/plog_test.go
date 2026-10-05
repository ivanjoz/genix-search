package plog

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/docid"
	"github.com/ivanjoz/genix-search/genix-s3/internal/key5"
)

const normID = 0xABCD

func tokens(words ...string) []byte {
	return key5.AppendTokens(nil, key5.FromTokens(words))
}

func TestRoundTrip(t *testing.T) {
	b := Base{NormID: normID, Epoch: 3, LastSeq: 10, PrevSeq: 4, UnixMs: 99, Docs: 1234, Grouping: 1}
	b.Touched.Set(0)
	b.Touched.Set(97)
	b.Touched.Set(Slots - 1)
	data := EncodeBase(b)

	ops := []Op{
		{Seq: 11, ID: 7, Tokens: tokens("polo", "rojo")},
		{Seq: 12, ID: docid.New(200, 4000000000), Del: true},
		{Seq: 13, ID: docid.New(1, 7), Tokens: tokens("polo", "azul", "50", "ml")},
	}

	data, err := AppendBatch(data, ops)
	if err != nil {
		t.Fatal(err)
	}

	data = AppendCompact(data, Compact{TargetSeq: 13, UnixMs: 5})

	log, err := Parse(data, normID)
	if err != nil {
		t.Fatal(err)
	}

	if log.Base != b || log.LastSeq() != 13 || log.Size != int64(len(data)) || log.TornTail {
		t.Fatalf("log %+v", log)
	}

	if !log.Base.Touched.Has(97) || log.Base.Touched.Has(96) {
		t.Error("Touched bits")
	}

	for i := range ops {
		got := log.Ops[i]
		if got.Seq != ops[i].Seq || got.ID != ops[i].ID || got.Del != ops[i].Del || !slices.Equal(got.Tokens, ops[i].Tokens) {
			t.Errorf("op %d = %+v, want %+v", i, got, ops[i])
		}
	}

	// ID 7 of group 0 and of group 1 are two documents.
	pending := log.Pending()
	if len(pending) != 3 || pending[7].Seq != 11 || pending[docid.New(1, 7)].Seq != 13 || !pending[docid.New(200, 4000000000)].Del {
		t.Errorf("pending %+v", pending)
	}

	if !log.CompactPending() {
		t.Error("compaction request not seen")
	}

	if _, err := Parse(data, normID+1); err != ErrNormID {
		t.Errorf("another NormID: %v", err)
	}
}

func TestTornTail(t *testing.T) {
	data := EncodeBase(Base{NormID: normID})
	whole, _ := AppendBatch(data, []Op{{Seq: 1, ID: 1, Tokens: tokens("a")}})
	full, _ := AppendBatch(slices.Clone(whole), []Op{{Seq: 2, ID: 2, Tokens: tokens("polo", "spiderman")}})

	for cut := len(whole) + 1; cut < len(full); cut++ {
		log, err := Parse(full[:cut], normID)
		if err != nil {
			t.Fatal(err)
		}

		if !log.TornTail || len(log.Ops) != 1 || log.Size != int64(len(whole)) {
			t.Fatalf("cut at %d: %+v", cut, log)
		}
	}
}

func TestLogOnly(t *testing.T) {
	docs := []Doc{{ID: 3, Tokens: tokens("polo", "rojo")}, {ID: 9, Tokens: tokens("pollo")}}

	data, err := AppendDocs(EncodeBase(Base{NormID: normID, Epoch: 2, LastSeq: 5, LogOnly: true}), docs)
	if err != nil {
		t.Fatal(err)
	}

	baseEnd := len(data)

	data, _ = AppendBatch(data, []Op{{Seq: 6, ID: 3, Del: true}, {Seq: 7, ID: 11, Tokens: tokens("azul")}})

	log, err := Parse(data, normID)
	if err != nil {
		t.Fatal(err)
	}

	if !log.Base.LogOnly || log.Base.Epoch != 2 || len(log.Docs) != 2 || log.Docs[1].ID != 9 || log.BaseEnd != int64(baseEnd) || len(log.Ops) != 2 {
		t.Fatalf("log %+v", log)
	}

	if !slices.Equal(log.Docs[0].Tokens, docs[0].Tokens) {
		t.Error("Docs tokens differ")
	}

	if n := log.LiveDocs(); n != 2 { // 3 deleted, 9 kept, 11 added
		t.Errorf("LiveDocs = %d", n)
	}

	// The Docs frame belongs to the base: without it the log is corrupt, and
	// it cannot appear anywhere else.
	if _, err := Parse(EncodeBase(Base{NormID: normID, LogOnly: true}), normID); err == nil {
		t.Error("a LogOnly base without Docs parsed")
	}

	stray, _ := AppendDocs(EncodeBase(Base{NormID: normID}), docs)
	if _, err := Parse(stray, normID); err == nil {
		t.Error("a Docs frame after an indexed base parsed")
	}

	if _, err := AppendDocs(nil, []Doc{{ID: 2}, {ID: 1}}); err == nil {
		t.Error("unsorted documents accepted")
	}
}

// TestCompactTrigger: the trigger counts the appends, not the Docs frame,
// and a LogOnly log asks for a compaction when it reaches MinIndexDocs.
func TestCompactTrigger(t *testing.T) {
	ctx := context.Background()

	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	var docs []Doc
	for i := range 90 {
		docs = append(docs, Doc{ID: docid.Key(i), Tokens: tokens("producto", "numero", fmt.Sprint(i))})
	}

	start, _ := AppendDocs(EncodeBase(Base{NormID: normID, LogOnly: true}), docs)
	if _, err := store.Put(ctx, "p.log", start, blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	opts := AppendOptions{CompactBytes: 512, MinIndexDocs: 100}

	// The Docs frame alone is bigger than CompactBytes: no request.
	res, err := Append(ctx, store, "p.log", normID, []Change{{ID: 1, Tokens: tokens("x")}}, opts)
	if err != nil || res.CompactRequested {
		t.Fatalf("first append: %+v %v", res, err)
	}

	// The 100th document asks for the index.
	for i := 90; i < 100; i++ {
		res, err = Append(ctx, store, "p.log", normID, []Change{{ID: docid.Key(i), Tokens: tokens("y")}}, opts)
		if err != nil {
			t.Fatal(err)
		}

		if res.CompactRequested != (i == 99) {
			t.Fatalf("document %d: compaction requested %v", i+1, res.CompactRequested)
		}
	}
}

func TestConcurrentAppends(t *testing.T) {
	ctx := context.Background()

	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	if _, err := store.Put(ctx, "p.log", EncodeBase(Base{NormID: normID}), blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup

	for i := range 20 {
		wg.Go(func() {
			if _, err := Append(ctx, store, "p.log", normID, []Change{{ID: docid.Key(i), Tokens: tokens("x")}}, AppendOptions{CompactBytes: 1 << 20}); err != nil {
				t.Error(err)
			}
		})
	}

	wg.Wait()

	data, _, _ := store.Get(ctx, "p.log")

	log, err := Parse(data, normID)
	if err != nil {
		t.Fatal(err)
	}

	if len(log.Ops) != 20 || log.LastSeq() != 20 {
		t.Fatalf("%d ops, last seq %d", len(log.Ops), log.LastSeq())
	}
}
