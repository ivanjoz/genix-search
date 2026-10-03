package s3blob_test

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
	"github.com/ivanjoz/genix-search/genix-s3/internal/blob/blobtest"
	"github.com/ivanjoz/genix-search/genix-s3/s3blob"
)

// The integration tests run against a real bucket, named by
// GENIX_S3_TEST_BUCKET (a directory bucket, "...--x-s3", or a general purpose
// one), with the default AWS credentials and region. They write and delete
// keys under genix-blobtest/.
func testBucket() string { return os.Getenv("GENIX_S3_TEST_BUCKET") }

func testStore(t *testing.T) (*s3blob.Store, string) {
	bucket := testBucket()
	if bucket == "" {
		t.Skip("GENIX_S3_TEST_BUCKET is not set")
	}

	cfg, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	prefix := fmt.Sprintf("genix-blobtest/%d/", time.Now().UnixNano())

	return s3blob.New(s3.NewFromConfig(cfg), bucket), prefix
}

func TestContract(t *testing.T) {
	store, prefix := testStore(t)
	t.Logf("native append: %v", store.NativeAppend())

	blobtest.Run(t, store, prefix)
}

// TestLatency logs the round trip of each operation the writer and the query
// use: what one round of PLAN_5BIT.md §7.2 costs on this bucket.
func TestLatency(t *testing.T) {
	store, prefix := testStore(t)
	ctx := context.Background()
	key := prefix + "latency"

	t.Cleanup(func() { store.Delete(context.Background(), key) })

	const rounds = 20

	data := make([]byte, 4096)
	size := int64(len(data))

	if _, err := store.Put(ctx, key, data, blob.PutCond{}); err != nil {
		t.Fatal(err)
	}

	ops := []struct {
		name string
		run  func() error
	}{
		{"get 4 KB", func() error { _, _, err := store.Get(ctx, key); return err }},
		{"range 1 KB", func() error { _, _, err := store.GetRange(ctx, key, 1024, 1024); return err }},
		{"append 64 B", func() error {
			n, err := store.Append(ctx, key, data[:64], size)
			size = n

			return err
		}},
		{"put 4 KB", func() error { _, err := store.Put(ctx, key+".put", data, blob.PutCond{}); return err }},
	}

	t.Cleanup(func() { store.Delete(context.Background(), key+".put") })

	for _, op := range ops {
		took := make([]time.Duration, rounds)

		for i := range rounds {
			start := time.Now()
			if err := op.run(); err != nil {
				t.Fatalf("%s: %v", op.name, err)
			}

			took[i] = time.Since(start)
		}

		slices.Sort(took)
		t.Logf("%-12s p50 %6.1f ms   p90 %6.1f ms   max %6.1f ms", op.name, ms(took[rounds/2]), ms(took[rounds*9/10]), ms(took[rounds-1]))
	}
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
