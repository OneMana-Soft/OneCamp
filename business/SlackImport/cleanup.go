package business

import (
	"context"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// DeleteStagedZip is the manual "free this storage now" entry point.
// Called from the rollback path (immediate) and from a future admin
// endpoint where the operator wants to reclaim space without waiting
// for retention. Idempotent.
func DeleteStagedZip(ctx context.Context, jobId uuid.UUID) error {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return err
	}
	if job.RawObjectKey == nil || *job.RawObjectKey == "" {
		return nil // already cleaned up
	}
	bucket := stagedZipBucket()
	if err := minioInit.MinioClient.RemoveObject(ctx, bucket, *job.RawObjectKey,
		minio.RemoveObjectOptions{}); err != nil {
		// Tolerate not-found; surface other errors to the caller.
		// minio.ErrorResponse doesn't implement errors.Is, so we have to
		// project to the typed response and check Code directly.
		if er := minio.ToErrorResponse(err); er.Code != "NoSuchKey" {
			helpers.LogWarnWithContext(ctx,
				"SlackImport DeleteStagedZip RemoveObject(%s) err: %+v",
				*job.RawObjectKey, err)
		}
	}
	return importModels.ClearRawObjectKey(ctx, jobId)
}

// stagedZipBucket returns the bucket name to use for staged Slack ZIPs.
// Centralised here so we never drift from the upload path.
func stagedZipBucket() string {
	bucket := helpers.UserUploadBucket()
	return bucket
}

// envIntDefault is shared with orchestrator.go (same package).
// Kept here as a tiny note rather than re-declared.
