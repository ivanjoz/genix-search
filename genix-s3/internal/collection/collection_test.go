package collection

import (
	"context"
	"errors"
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/layout"
	"github.com/ivanjoz/genix-search/genix-s3/internal/normalize"
	"github.com/ivanjoz/genix-search/genix-s3/internal/plog"
)

// hookStore runs hook right before the n-th read of key.
type hookStore struct {
	blob.Blob
	key   string
	n     int
	reads int
	hook  func()
}

func (h *hookStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if key == h.key {
		h.reads++
		if h.reads == h.n && h.hook != nil {
			h.hook()
		}
	}

	return h.Blob.Get(ctx, key)
}

// TestCompactionRace: a LogOnly compaction claims no generation, so two can
// run at once. The one that commits second must fail, not overwrite p.log
// with a state that misses what the first folded.
func TestCompactionRace(t *testing.T) {
	ctx := context.Background()

	fs, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	col, _ := layout.New("genix/", "race", "default")
	other := New(fs, col, normalize.Default, DefaultOptions())

	if _, err := other.Init(ctx, []Doc{{ID: 1, Text: "polo rojo"}}); err != nil {
		t.Fatal(err)
	}

	if _, err := other.Put(ctx, []Doc{{ID: 2, Text: "polo azul"}}); err != nil {
		t.Fatal(err)
	}

	// The slow compaction reads p.log twice: to build the state, and to
	// rebase. In between, another writer and compaction get through.
	hooked := &hookStore{Blob: fs, key: col.Log(), n: 2}
	hooked.hook = func() {
		if _, err := other.Put(ctx, []Doc{{ID: 3, Text: "polo verde"}}); err != nil {
			t.Fatal(err)
		}

		if res, err := other.Compact(ctx); err != nil || res.Docs != 3 {
			t.Fatalf("fast compaction: %+v %v", res, err)
		}
	}

	slow := New(hooked, col, normalize.Default, DefaultOptions())

	if _, err := slow.Compact(ctx); !errors.Is(err, ErrConflict) {
		t.Fatalf("the slow compaction committed over the fast one: %v", err)
	}

	data, _, err := fs.Get(ctx, col.Log())
	if err != nil {
		t.Fatal(err)
	}

	log, err := plog.Parse(data, normalize.Default.ID())
	if err != nil {
		t.Fatal(err)
	}

	if !log.Base.LogOnly || len(log.Docs) != 3 {
		t.Fatalf("p.log lost a document: %d docs", len(log.Docs))
	}
}

func TestMinIndexDocs(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		min, docs int
		indexed   bool
	}{
		{100, 99, false}, {100, 100, true}, {1, 0, false}, {1, 1, true}, {5, 4, false}, {0, 0, true},
	} {
		fs, err := blob.NewFS(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}

		col, _ := layout.New("genix/", "c", "default")
		opts := DefaultOptions()
		opts.MinIndexDocs = tc.min

		var docs []Doc
		for i := range tc.docs {
			docs = append(docs, Doc{ID: uint32(i), Text: "polo"})
		}

		m, err := New(fs, col, normalize.Default, opts).Init(ctx, docs)
		if err != nil {
			t.Fatal(err)
		}

		_, _, kErr := fs.Get(ctx, col.K(1))
		if (m != nil) != tc.indexed || (kErr == nil) != tc.indexed {
			t.Errorf("min %d, %d docs: manifest %v, k.1 %v, want indexed %v", tc.min, tc.docs, m != nil, kErr == nil, tc.indexed)
		}
	}
}
