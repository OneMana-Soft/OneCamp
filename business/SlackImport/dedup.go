package business

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	"github.com/minio/minio-go/v7"
)

// HashStagedZip computes the SHA-256 of the uploaded export. The hash
// is the deduplication key for "operator uploaded the same file twice".
// We stream the object from MinIO so disk and memory stay flat
// regardless of file size.
//
// For multi-GB files this takes a while (~1 GB/s on local SSD-backed
// MinIO; bandwidth-bound for remote S3). The caller invokes it from
// the finalize endpoint, where the wait is acceptable — the operator
// is already past the "uploading" stage and waiting on a server-side
// check.
//
// Implementation note: GetObject returns a streaming reader; sha256
// implements io.Writer, so io.Copy is the natural composition. We use
// a 256 KB buffer to amortise syscalls without holding meaningful RAM.
func HashStagedZip(ctx context.Context, objectKey string) (string, error) {
	bucket := helpers.UserUploadBucket()
	obj, err := minioInit.MinioClient.GetObject(ctx, bucket, objectKey, minio.GetObjectOptions{})
	if err != nil {
		return "", fmt.Errorf("get staged zip for hashing: %w", err)
	}
	defer obj.Close()

	h := sha256.New()
	buf := make([]byte, 256*1024)
	if _, err := io.CopyBuffer(h, obj, buf); err != nil {
		return "", fmt.Errorf("hash staged zip: %w", err)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ─── Cross-import resolution helpers ─────────────────────────────────────
//
// When a Slack workspace is re-imported (e.g., a fresh export taken
// 30 days after the first), the per-import import_id_map starts
// empty. Without dedup, every user/channel/message would be re-created.
//
// resolveExisting* helpers consult the workspace-wide map (populated by
// successful prior imports) and return the OneCamp UUID if the entity
// is already known. Workers use these BEFORE doing any creation work.
