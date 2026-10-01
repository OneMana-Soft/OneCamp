package trello

import (
	"context"
	"io"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
)

// openStagedFile streams a single staged object from MinIO. Returns a
// reader plus a cleanup callback. We use plain GetObject (no range
// reader) because Trello board JSON is small (<10 MB even for huge
// boards). For ZIP-shaped providers (Notion export) the legacy slack
// minio_reader is more appropriate.
func openStagedFile(ctx context.Context, objectKey string) (io.ReadCloser, func(), error) {
	bucket := helpers.UserUploadBucket()
	obj, err := minioInit.MinioClient.GetObject(ctx, bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return nil, nil, err
	}
	return obj, func() { _ = obj.Close() }, nil
}
