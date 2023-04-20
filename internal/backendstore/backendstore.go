package backendstore

import (
	"sync"

	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3Backends is a map of S3 backend name (eg ceph cluster name) to S3 client.
type s3Backends map[string]*s3.Client

// BackendStore stores the active s3 backends.
type BackendStore struct {
	mu         sync.Mutex
	s3Backends s3Backends
}

func NewBackendStore() *BackendStore {
	return &BackendStore{
		s3Backends: make(s3Backends),
	}
}

func (b *BackendStore) DeleteBackend(backendName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	delete(b.s3Backends, backendName)
}

func (b *BackendStore) AddOrUpdateBackend(backendName string, backendClient *s3.Client) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.s3Backends[backendName] = backendClient
}

func (b *BackendStore) GetBackend(backendName string) *s3.Client {
	b.mu.Lock()
	defer b.mu.Unlock()
	if backend, ok := b.s3Backends[backendName]; ok {
		return backend
	}

	return nil
}

func (b *BackendStore) GetAllBackends() s3Backends {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b.s3Backends
}

func (b *BackendStore) GeBackendStore() *BackendStore {
	b.mu.Lock()
	defer b.mu.Unlock()

	return b
}

func (b *BackendStore) BackendsAreStored() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	return len(b.s3Backends) != 0
}
