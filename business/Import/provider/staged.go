package provider

import (
	"context"
	"io"
	"sync"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// A file the admin uploaded for an import (Trello's board JSON, a CSV) is
// staged in MinIO under the job's raw_object_key, and read from there.

// OpenStagedFile streams a staged upload. The caller closes it.
func OpenStagedFile(ctx context.Context, objectKey string) (io.ReadCloser, error) {
	return minioInit.MinioClient.GetObject(ctx, helpers.UserUploadBucket(), objectKey, minio.GetObjectOptions{})
}

// JobCache keeps what a provider read for a job (an uploaded file, parsed)
// while the job's stages run: they run in their own goroutines, and each asks
// for it. The provider evicts it when the job ends (JobCleaner).
type JobCache[T any] struct {
	mu sync.RWMutex
	m  map[uuid.UUID]T
}

// NewJobCache is an empty JobCache.
func NewJobCache[T any]() *JobCache[T] {
	return &JobCache[T]{m: map[uuid.UUID]T{}}
}

// Load is the job's entry, or T's zero value.
func (c *JobCache[T]) Load(id uuid.UUID) T {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.m[id]
}

// Store keeps v for the job.
func (c *JobCache[T]) Store(id uuid.UUID, v T) {
	c.mu.Lock()
	c.m[id] = v
	c.mu.Unlock()
}

// Evict forgets the job's entry.
func (c *JobCache[T]) Evict(id uuid.UUID) {
	c.mu.Lock()
	delete(c.m, id)
	c.mu.Unlock()
}
