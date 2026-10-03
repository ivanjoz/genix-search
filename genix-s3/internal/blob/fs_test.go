package blob_test

import (
	"testing"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/blob/blobtest"
)

func TestFSContract(t *testing.T) {
	store, err := blob.NewFS(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	blobtest.Run(t, store, "contract/")
}
