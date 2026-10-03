package blob

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// FS stores objects as files under Root. Conditional puts and appends are
// atomic within one process (one mutex), which is what tests and the CLI need;
// it is not a multi-process store.
type FS struct {
	Root string
	mu   sync.Mutex
}

// NewFS returns a store rooted at dir, creating it if needed.
func NewFS(dir string) (*FS, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}

	return &FS{Root: dir}, nil
}

func (s *FS) path(key string) (string, error) {
	if key == "" || strings.HasPrefix(key, "/") || strings.Contains(key, "..") {
		return "", fmt.Errorf("blob: invalid key %q", key)
	}

	return filepath.Join(s.Root, filepath.FromSlash(key)), nil
}

func (s *FS) GetRange(_ context.Context, key string, off, n int64) ([]byte, error) {
	path, err := s.path(key)
	if err != nil {
		return nil, err
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, mapNotFound(err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}

	if off < 0 || off > info.Size() {
		return nil, fmt.Errorf("blob: range %d out of %d bytes of %s", off, info.Size(), key)
	}

	if n < 0 || off+n > info.Size() {
		n = info.Size() - off
	}

	data := make([]byte, n)
	if _, err := file.ReadAt(data, off); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	return data, nil
}

func (s *FS) Get(_ context.Context, key string) ([]byte, string, error) {
	path, err := s.path(key)
	if err != nil {
		return nil, "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", mapNotFound(err)
	}

	return data, etag(data), nil
}

func (s *FS) Put(_ context.Context, key string, data []byte, cond PutCond) (string, error) {
	path, err := s.path(key)
	if err != nil {
		return "", err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if cond.IfNoneMatch || cond.IfMatch != "" {
		current, err := os.ReadFile(path)

		switch {
		case err == nil && cond.IfNoneMatch:
			return "", ErrPrecondition
		case err == nil && etag(current) != cond.IfMatch:
			return "", ErrPrecondition
		case err != nil && !errors.Is(err, fs.ErrNotExist):
			return "", err
		case err != nil && cond.IfMatch != "":
			return "", ErrPrecondition
		}
	}

	if err := writeAtomic(path, data); err != nil {
		return "", err
	}

	return etag(data), nil
}

func (s *FS) Append(_ context.Context, key string, data []byte, offset int64) (int64, error) {
	path, err := s.path(key)
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return 0, mapNotFound(err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return 0, err
	}

	if info.Size() != offset {
		return 0, ErrOffsetMismatch
	}

	if _, err := file.Write(data); err != nil {
		return 0, err
	}

	return offset + int64(len(data)), nil
}

func (s *FS) Delete(_ context.Context, keys ...string) error {
	for _, key := range keys {
		path, err := s.path(key)
		if err != nil {
			return err
		}

		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}

	return nil
}

// writeAtomic replaces path in one rename, so a reader sees the old object or
// the new one, never half of it — the guarantee S3 gives.
func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())

		return err
	}

	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())

		return err
	}

	return os.Rename(tmp.Name(), path)
}

func etag(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:16])
}

func mapNotFound(err error) error {
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}

	return err
}
