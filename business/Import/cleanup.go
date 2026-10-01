package business

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	minioInit "github.com/akashc777/OneCamp/initializers/minioInit"
	importModels "github.com/akashc777/OneCamp/models/postgres/Import"
	"github.com/google/uuid"
	"github.com/minio/minio-go/v7"
)

// Default retention windows. Both env-tunable.
const (
	defaultRawRetentionDays = 7
	defaultPendingTTLHours  = 24
)

// StartCleanupLoop runs every hour and:
//  1. Deletes staged ZIPs for jobs that finished N+ days ago.
//  2. Marks abandoned 'pending' jobs (presigned but never finalised)
//     as failed and deletes their orphaned upload (if any).
//
// Mirrors the legacy SlackImport cleanup loop but generic across providers.
func StartCleanupLoop() {
	go func() {
		// Stagger first run to avoid lining up with reaper.
		time.Sleep(15 * time.Minute)
		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			runCleanupTick()
			<-t.C
		}
	}()
}

func runCleanupTick() {
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx,
				"Import cleanup recovered from panic: %v", r)
		}
	}()
	if n, err := reapStaleStagedZips(ctx); err != nil {
		helpers.LogWarnWithContext(ctx, "Import cleanup reap zips err: %+v", err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx,
			"Import cleanup deleted %d stale staged zip(s)", n)
	}
	if n, err := reapAbandonedPendingJobs(ctx); err != nil {
		helpers.LogWarnWithContext(ctx, "Import cleanup reap pending err: %+v", err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx,
			"Import cleanup failed %d abandoned pending upload(s)", n)
	}
}

func reapStaleStagedZips(ctx context.Context) (int, error) {
	retention := envIntDefault("IMPORT_RAW_RETENTION_DAYS", defaultRawRetentionDays)
	if retention <= 0 {
		return 0, nil
	}
	cutoff := time.Now().AddDate(0, 0, -retention)
	jobs, err := importModels.ListJobsForRawCleanup(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(jobs) == 0 {
		return 0, nil
	}
	bucket := stagedZipBucket()
	objectCh := make(chan minio.ObjectInfo, len(jobs))
	var toClear []uuid.UUID
	for _, j := range jobs {
		if j.RawObjectKey == nil || *j.RawObjectKey == "" {
			continue
		}
		objectCh <- minio.ObjectInfo{Key: *j.RawObjectKey}
		toClear = append(toClear, j.Id)
	}
	close(objectCh)

	deleted := 0
	for res := range minioInit.MinioClient.RemoveObjects(ctx, bucket, objectCh, minio.RemoveObjectsOptions{}) {
		if res.Err != nil {
			helpers.LogWarnWithContext(ctx,
				"Import cleanup RemoveObjects(%s) err: %+v", res.ObjectName, res.Err)
			continue
		}
		deleted++
	}
	for _, id := range toClear {
		if err := importModels.ClearRawObjectKey(ctx, id); err != nil {
			helpers.LogWarnWithContext(ctx, "Import cleanup ClearRawObjectKey err: %+v", err)
		}
	}
	return deleted, nil
}

func reapAbandonedPendingJobs(ctx context.Context) (int, error) {
	ttl := envIntDefault("IMPORT_PENDING_TTL_HOURS", defaultPendingTTLHours)
	if ttl <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(ttl) * time.Hour)
	jobs, err := importModels.ListAbandonedPendingJobs(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	bucket := stagedZipBucket()
	objectCh := make(chan minio.ObjectInfo, len(jobs))
	var toFail []uuid.UUID
	for _, j := range jobs {
		if j.RawObjectKey != nil && *j.RawObjectKey != "" {
			objectCh <- minio.ObjectInfo{Key: *j.RawObjectKey}
		}
		toFail = append(toFail, j.Id)
	}
	close(objectCh)
	// Best-effort bulk delete; MinIO returns success-on-not-found.
	for res := range minioInit.MinioClient.RemoveObjects(ctx, bucket, objectCh, minio.RemoveObjectsOptions{}) {
		if res.Err != nil {
			helpers.LogWarnWithContext(ctx,
				"Import cleanup RemoveObjects(%s) err: %+v", res.ObjectName, res.Err)
		}
	}

	failed := 0
	for _, id := range toFail {
		if err := importModels.UpdateStatus(ctx, id, importModels.StatusFailed,
			strPtr("failed"), strPtr("upload abandoned (no /finalize within retention TTL)")); err != nil {
			helpers.LogWarnWithContext(ctx,
				"Import cleanup fail-abandoned %s err: %+v", id, err)
			continue
		}
		_ = importModels.ClearRawObjectKey(ctx, id)
		failed++
	}
	return failed, nil
}

// DeleteStagedZip is the manual "free this storage now" entry point.
// Idempotent.
func DeleteStagedZip(ctx context.Context, jobId uuid.UUID) error {
	job, err := importModels.GetJob(ctx, jobId)
	if err != nil {
		return err
	}
	if job.RawObjectKey == nil || *job.RawObjectKey == "" {
		return nil
	}
	bucket := stagedZipBucket()
	if err := minioInit.MinioClient.RemoveObject(ctx, bucket, *job.RawObjectKey,
		minio.RemoveObjectOptions{}); err != nil {
		if er := minio.ToErrorResponse(err); er.Code != "NoSuchKey" {
			helpers.LogWarnWithContext(ctx,
				"Import DeleteStagedZip(%s) err: %+v", *job.RawObjectKey, err)
		}
	}
	return importModels.ClearRawObjectKey(ctx, jobId)
}

// StagedZipBucket returns the bucket name to use for staged import
// objects (raw ZIPs, board JSON exports). Centralised here so the
// controller, the cleanup loop, and per-provider readers never drift
// from each other on bucket naming.
func StagedZipBucket() string {
	bucket := helpers.UserUploadBucket()
	return bucket
}

// stagedZipBucket is the package-private alias for compatibility with
// the existing in-package callers that don't need to cross packages.
func stagedZipBucket() string {
	return StagedZipBucket()
}
