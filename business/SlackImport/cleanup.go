package business

import (
	"context"
	"time"

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

// Default retention windows. Both env-tunable.
//
//	SLACK_IMPORT_RAW_RETENTION_DAYS — how long to keep the staged ZIP
//	  after the job reaches a terminal state (completed/failed/cancelled).
//	  7 days balances "operator wants to retry" against storage cost.
//
//	SLACK_IMPORT_PENDING_TTL_HOURS  — how long to wait before reaping a
//	  job that was created via /presign but never followed up with
//	  /finalize. 24 hours is enough for a slow transcontinental upload.
const (
	defaultRawRetentionDays = 7
	defaultPendingTTLHours  = 24
)

// StartCleanupLoop runs every hour and:
//
//  1. Deletes staged ZIPs for jobs that finished N+ days ago.
//  2. Marks abandoned 'pending' jobs (presigned but never finalised) as
//     failed and deletes their orphaned upload (if any reached MinIO).
//
// Mirrors the StartWebhookCleanupScheduler pattern. Wire from main.go
// alongside StartReaperLoop.
func StartCleanupLoop() {
	go func() {
		// Stagger first run to avoid lining up with reaper. Both loops are
		// cheap, but staggering keeps PG load smooth.
		time.Sleep(15 * time.Minute)

		t := time.NewTicker(1 * time.Hour)
		defer t.Stop()
		for {
			runCleanupTick()
			<-t.C
		}
	}()
}

// runCleanupTick is exported via testing/private path; called once per
// hour by StartCleanupLoop. Each operation is best-effort and isolated
// so a failure in one path doesn't block the others.
func runCleanupTick() {
	ctx := context.Background()
	defer func() {
		if r := recover(); r != nil {
			helpers.LogErrorWithContext(ctx,
				"SlackImport cleanup recovered from panic: %v", r)
		}
	}()

	if n, err := reapStaleStagedZips(ctx); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport cleanup: reap stale staged zips failed: %+v", err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx,
			"SlackImport cleanup: deleted %d stale staged zip(s)", n)
	}

	if n, err := reapAbandonedPendingJobs(ctx); err != nil {
		helpers.LogWarnWithContext(ctx,
			"SlackImport cleanup: reap abandoned pending failed: %+v", err)
	} else if n > 0 {
		helpers.LogInfoWithContext(ctx,
			"SlackImport cleanup: failed %d abandoned pending upload(s)", n)
	}
}

// reapStaleStagedZips deletes the MinIO object for jobs whose status is
// terminal AND completed_at older than the retention window. Sets
// raw_object_key to NULL so the same row isn't reaped twice.
//
// Status filter: completed/failed/cancelled. Rolled_back jobs have
// already had their ZIP deleted at rollback time, but we include them
// in the filter as a belt-and-braces — reading raw_object_key IS NOT NULL
// keeps us from re-deleting.
func reapStaleStagedZips(ctx context.Context) (int, error) {
	retention := envIntDefault("SLACK_IMPORT_RAW_RETENTION_DAYS", defaultRawRetentionDays)
	if retention <= 0 {
		// Operator turned retention off (zero/negative env). Skip.
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
				"SlackImport cleanup: RemoveObjects(%s) err: %+v",
				res.ObjectName, res.Err)
			continue
		}
		deleted++
	}
	for _, id := range toClear {
		if err := importModels.ClearRawObjectKey(ctx, id); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport cleanup: ClearRawObjectKey(%s) failed: %+v",
				id, err)
		}
	}
	return deleted, nil
}

// reapAbandonedPendingJobs handles the case where the operator hit
// /presign (so a job row exists with status='pending') but never
// followed up with /finalize. We can't tell if the upload PUT succeeded
// without a stat; we try both arms so storage doesn't leak either way.
func reapAbandonedPendingJobs(ctx context.Context) (int, error) {
	ttlHours := envIntDefault("SLACK_IMPORT_PENDING_TTL_HOURS", defaultPendingTTLHours)
	if ttlHours <= 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-time.Duration(ttlHours) * time.Hour)

	jobs, err := importModels.ListAbandonedPendingJobs(ctx, cutoff)
	if err != nil {
		return 0, err
	}
	if len(jobs) == 0 {
		return 0, nil
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
				"SlackImport cleanup: RemoveObjects(%s) err: %+v", res.ObjectName, res.Err)
		}
	}

	failed := 0
	for _, id := range toFail {
		if err := importModels.UpdateStatus(ctx, id, importModels.StatusFailed,
			strPtr("failed"), strPtr("upload abandoned (no /finalize within retention TTL)")); err != nil {
			helpers.LogWarnWithContext(ctx,
				"SlackImport cleanup: failing abandoned job %s failed: %+v", id, err)
			continue
		}
		_ = importModels.ClearRawObjectKey(ctx, id)
		failed++
	}
	return failed, nil
}
