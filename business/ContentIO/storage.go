package business

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
)

// fileStore keeps an import's file between the request that brings it and the
// worker that reads it: object storage, so the worker may be another replica.
type fileStore interface {
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (int64, error)
	// Fetch copies an object into a temporary file, which the caller removes.
	Fetch(ctx context.Context, key string, maxBytes int64) (*os.File, int64, error)
	Size(ctx context.Context, key string) (int64, error)
	PresignPut(ctx context.Context, key string, expires time.Duration) (string, error)
	// PresignGet is an address to download an object from, as filename.
	PresignGet(ctx context.Context, key string, expires time.Duration, filename string) (string, error)
	Remove(ctx context.Context, key string) error
}

// store is the file store everything uses; tests swap it.
var store fileStore = minioStore{}

type minioStore struct{}

func (minioStore) client() (*minio.Client, error) {
	if minioInit.MinioClient == nil {
		return nil, ErrUnavailable
	}
	return minioInit.MinioClient, nil
}

func (s minioStore) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (int64, error) {
	c, err := s.client()
	if err != nil {
		return 0, err
	}
	info, err := c.PutObject(ctx, helpers.UserUploadBucket(), key, r, size, minio.PutObjectOptions{ContentType: contentType})
	if err != nil {
		return 0, err
	}
	return info.Size, nil
}

func (s minioStore) Fetch(ctx context.Context, key string, maxBytes int64) (*os.File, int64, error) {
	c, err := s.client()
	if err != nil {
		return nil, 0, err
	}
	obj, err := c.GetObject(ctx, helpers.UserUploadBucket(), key, minio.GetObjectOptions{})
	if err != nil {
		return nil, 0, err
	}
	defer obj.Close()
	f, err := os.CreateTemp("", "content-io-*")
	if err != nil {
		return nil, 0, err
	}
	n, err := io.Copy(f, io.LimitReader(obj, maxBytes+1))
	if err == nil && n > maxBytes {
		err = ErrTooLarge
	}
	if err != nil {
		f.Close()
		os.Remove(f.Name())
		return nil, 0, err
	}
	return f, n, nil
}

func (s minioStore) Size(ctx context.Context, key string) (int64, error) {
	c, err := s.client()
	if err != nil {
		return 0, err
	}
	st, err := c.StatObject(ctx, helpers.UserUploadBucket(), key, minio.StatObjectOptions{})
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return st.Size, nil
}

func (s minioStore) PresignPut(ctx context.Context, key string, expires time.Duration) (string, error) {
	c, err := s.client()
	if err != nil {
		return "", err
	}
	u, err := c.PresignedPutObject(ctx, helpers.UserUploadBucket(), key, expires)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s minioStore) PresignGet(ctx context.Context, key string, expires time.Duration, filename string) (string, error) {
	c, err := s.client()
	if err != nil {
		return "", err
	}
	params := url.Values{}
	params.Set("response-content-disposition", mime.FormatMediaType("attachment", map[string]string{"filename": filename}))
	u, err := c.PresignedGetObject(ctx, helpers.UserUploadBucket(), key, expires, params)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func (s minioStore) Remove(ctx context.Context, key string) error {
	c, err := s.client()
	if err != nil {
		return err
	}
	return c.RemoveObject(ctx, helpers.UserUploadBucket(), key, minio.RemoveObjectOptions{})
}
