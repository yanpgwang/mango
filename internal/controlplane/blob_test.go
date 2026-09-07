package controlplane

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sync"

	"github.com/yanpgwang/mango/internal/app"
)

// resourceBlobStore is a test-only object store used by Files-backed control
// plane tests. It is not a sandbox implementation.
type resourceBlobStore struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func newResourceBlobStore() *resourceBlobStore {
	return &resourceBlobStore{objects: make(map[string][]byte)}
}

func (s *resourceBlobStore) Put(
	_ context.Context,
	key string,
	_ string,
	body io.Reader,
	maxBytes int64,
) (app.BlobInfo, error) {
	data, err := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err != nil {
		return app.BlobInfo{}, err
	}
	if int64(len(data)) > maxBytes {
		return app.BlobInfo{}, app.ErrBlobTooLarge
	}
	s.mu.Lock()
	s.objects[key] = append([]byte(nil), data...)
	s.mu.Unlock()
	return app.ComputeBlobInfo(data), nil
}

func (s *resourceBlobStore) Open(_ context.Context, key string) (io.ReadCloser, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, ok := s.objects[key]
	if !ok {
		return nil, errors.New("missing object")
	}
	return io.NopCloser(bytes.NewReader(append([]byte(nil), data...))), nil
}

func (s *resourceBlobStore) Delete(_ context.Context, key string) error {
	s.mu.Lock()
	delete(s.objects, key)
	s.mu.Unlock()
	return nil
}
