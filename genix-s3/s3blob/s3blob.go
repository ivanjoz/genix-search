// Package s3blob is the object store on Amazon S3 (PLAN_5BIT.md §8).
//
// On a directory bucket of S3 Express One Zone (its name ends in "--x-s3")
// Append is the bucket's native append: a PutObject with WriteOffsetBytes,
// which S3 rejects when the offset is not the object's size. On a general
// purpose bucket, which has no append, it is emulated with a read and a put
// conditioned on the ETag of that read: same semantics, but it rewrites the
// whole object.
//
//	cfg, _ := config.LoadDefaultConfig(ctx)
//	store := s3blob.New(s3.NewFromConfig(cfg), "genix--use1-az4--x-s3")
//	ix, _ := genix.NewIndexer(store, genix.Config{Collection: "productos"})
package s3blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/ivanjoz/genix-search/genix-s3/internal/blob"
)

// deleteBatch is the most keys one DeleteObjects takes.
const deleteBatch = 1000

// Store is a bucket. It is safe for concurrent use.
type Store struct {
	client  *s3.Client
	bucket  string
	express bool
}

var _ blob.Blob = (*Store)(nil)

// New returns the store of bucket. The client must be configured for the
// bucket's region; for a directory bucket the SDK opens the S3 Express
// session by itself.
func New(client *s3.Client, bucket string) *Store {
	return &Store{client: client, bucket: bucket, express: IsDirectoryBucket(bucket)}
}

// IsDirectoryBucket reports whether bucket is an S3 Express One Zone
// directory bucket, which is named "<base>--<zone-id>--x-s3".
func IsDirectoryBucket(bucket string) bool {
	return strings.HasSuffix(bucket, "--x-s3")
}

// NativeAppend reports whether Append is the bucket's own append, as opposed
// to a read and a conditional rewrite.
func (s *Store) NativeAppend() bool { return s.express }

func (s *Store) GetRange(ctx context.Context, key string, off, n int64) ([]byte, string, error) {
	if off < 0 {
		return nil, "", fmt.Errorf("s3blob: negative offset %d of %s", off, key)
	}

	if n == 0 {
		return s.rangeAtEnd(ctx, key, -1)
	}

	rng := fmt.Sprintf("bytes=%d-", off)
	if n > 0 {
		rng = fmt.Sprintf("bytes=%d-%d", off, off+n-1)
	}

	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key, Range: &rng}, quietRange)
	if err != nil {
		if httpStatus(err) == http.StatusRequestedRangeNotSatisfiable {
			return s.rangeAtEnd(ctx, key, off)
		}

		return nil, "", fail("get", key, err)
	}

	data, err := readBody(out.Body, out.ContentLength)

	return data, aws.ToString(out.ETag), err
}

// quietRange drops the log line the SDK writes for every ranged GET: S3 has
// no checksum of a part of an object, so there is nothing to validate.
func quietRange(o *s3.Options) { o.DisableLogOutputChecksumValidationSkipped = true }

// rangeAtEnd answers an empty range: one S3 refused with 416 because it
// starts at or past the end, or one of 0 bytes (off < 0), which is never
// sent. At the end it is empty, as it is for the local store; past it, an
// error.
func (s *Store) rangeAtEnd(ctx context.Context, key string, off int64) ([]byte, string, error) {
	head, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, "", fail("head", key, err)
	}

	if size := aws.ToInt64(head.ContentLength); off >= 0 && off != size {
		return nil, "", fmt.Errorf("s3blob: range %d out of %d bytes of %s", off, size, key)
	}

	return []byte{}, aws.ToString(head.ETag), nil
}

func (s *Store) Get(ctx context.Context, key string) ([]byte, string, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, "", fail("get", key, err)
	}

	data, err := readBody(out.Body, out.ContentLength)

	return data, aws.ToString(out.ETag), err
}

func (s *Store) Put(ctx context.Context, key string, data []byte, cond blob.PutCond) (string, error) {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: bytes.NewReader(data), ContentLength: aws.Int64(int64(len(data)))}

	if cond.IfNoneMatch {
		in.IfNoneMatch = aws.String("*")
	}

	if cond.IfMatch != "" {
		in.IfMatch = aws.String(cond.IfMatch)
	}

	out, err := s.client.PutObject(ctx, in)
	if err == nil {
		return aws.ToString(out.ETag), nil
	}

	conditional := cond.IfNoneMatch || cond.IfMatch != ""

	switch status := httpStatus(err); {
	// 409 is a conditional write that raced another one; the caller re-reads
	// and retries, as it does after a 412.
	case conditional && (status == http.StatusPreconditionFailed || status == http.StatusConflict):
		return "", wrap("put", key, blob.ErrPrecondition, err)
	// If-Match on a missing key: the ETag cannot match, as in the local store.
	case cond.IfMatch != "" && status == http.StatusNotFound:
		return "", wrap("put", key, blob.ErrPrecondition, err)
	}

	return "", fail("put", key, err)
}

// Append writes data at offset. On a directory bucket an offset of 0 creates
// a missing object, where the local store fails; plog never appends at 0.
func (s *Store) Append(ctx context.Context, key string, data []byte, offset int64) (int64, error) {
	if !s.express {
		return s.emulatedAppend(ctx, key, data, offset)
	}

	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:           &s.bucket,
		Key:              &key,
		Body:             bytes.NewReader(data),
		ContentLength:    aws.Int64(int64(len(data))),
		WriteOffsetBytes: aws.Int64(offset),
	})
	if err == nil {
		return offset + int64(len(data)), nil
	}

	var invalidOffset *types.InvalidWriteOffset

	switch status := httpStatus(err); {
	case errors.As(err, &invalidOffset), apiCode(err) == "InvalidWriteOffset",
		status == http.StatusConflict, status == http.StatusPreconditionFailed:
		return 0, wrap("append", key, blob.ErrOffsetMismatch, err)
	}

	return 0, fail("append", key, err)
}

// emulatedAppend is Append on a bucket without one: read the object, check
// its size is offset, and put it back with the data only if nobody wrote it
// in between.
func (s *Store) emulatedAppend(ctx context.Context, key string, data []byte, offset int64) (int64, error) {
	current, etag, err := s.Get(ctx, key)
	if err != nil {
		return 0, err
	}

	if int64(len(current)) != offset {
		return 0, fmt.Errorf("s3blob: append %s at %d of %d bytes: %w", key, offset, len(current), blob.ErrOffsetMismatch)
	}

	if _, err := s.Put(ctx, key, append(current, data...), blob.PutCond{IfMatch: etag}); err != nil {
		if errors.Is(err, blob.ErrPrecondition) {
			return 0, fmt.Errorf("s3blob: append %s: %w (%w)", key, blob.ErrOffsetMismatch, err)
		}

		return 0, err
	}

	return offset + int64(len(data)), nil
}

// Delete removes keys; a missing key is not an error.
func (s *Store) Delete(ctx context.Context, keys ...string) error {
	if len(keys) == 1 {
		_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &s.bucket, Key: &keys[0]})
		if err != nil && httpStatus(err) != http.StatusNotFound {
			return fail("delete", keys[0], err)
		}

		return nil
	}

	for start := 0; start < len(keys); start += deleteBatch {
		batch := keys[start:min(start+deleteBatch, len(keys))]
		objects := make([]types.ObjectIdentifier, len(batch))

		for i := range batch {
			objects[i] = types.ObjectIdentifier{Key: &batch[i]}
		}

		out, err := s.client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: &s.bucket,
			Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(true)},
		})
		if err != nil {
			return fail("delete", batch[0], err)
		}

		for _, e := range out.Errors {
			if aws.ToString(e.Code) != "NoSuchKey" {
				return fmt.Errorf("s3blob: delete %s: %s: %s", aws.ToString(e.Key), aws.ToString(e.Code), aws.ToString(e.Message))
			}
		}
	}

	return nil
}

func readBody(body io.ReadCloser, size *int64) ([]byte, error) {
	defer body.Close()

	buf := bytes.NewBuffer(make([]byte, 0, max(aws.ToInt64(size), 0)))
	if _, err := buf.ReadFrom(body); err != nil {
		return nil, fmt.Errorf("s3blob: read body: %w", err)
	}

	return buf.Bytes(), nil
}

// fail wraps an SDK error, mapping a missing key to blob.ErrNotFound.
func fail(op, key string, err error) error {
	var noKey *types.NoSuchKey
	var notFound *types.NotFound

	if errors.As(err, &noKey) || errors.As(err, &notFound) || httpStatus(err) == http.StatusNotFound {
		return wrap(op, key, blob.ErrNotFound, err)
	}

	return fmt.Errorf("s3blob: %s %s: %w", op, key, err)
}

func wrap(op, key string, sentinel, err error) error {
	return fmt.Errorf("s3blob: %s %s: %w (%w)", op, key, sentinel, err)
}

func httpStatus(err error) int {
	var re interface{ HTTPStatusCode() int }
	if errors.As(err, &re) {
		return re.HTTPStatusCode()
	}

	return 0
}

func apiCode(err error) string {
	var ae smithy.APIError
	if errors.As(err, &ae) {
		return ae.ErrorCode()
	}

	return ""
}
