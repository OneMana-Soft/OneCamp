package business

// The Slack import's jobs as its controller reads and changes them (backend
// audit U1a): the controller called the models itself.

import (
	"context"
	"encoding/json"

	importModels "github.com/akashc777/OneCamp/models/postgres/SlackImport"
	"github.com/google/uuid"
)

// CreateJob records a new import job (importModels.ErrConflictActiveJob when one is under way).
func CreateJob(ctx context.Context, j *importModels.Job) error { return importModels.CreateJob(ctx, j) }

// GetJob is one import job.
func GetJob(ctx context.Context, jobId uuid.UUID) (*importModels.Job, error) {
	return importModels.GetJob(ctx, jobId)
}

// ListJobs is the newest import jobs, at most limit.
func ListJobs(ctx context.Context, limit int) ([]*importModels.Job, error) {
	return importModels.ListJobs(ctx, limit)
}

// JobCounts is each job's chunk, item and error counts, in one statement.
func JobCounts(ctx context.Context, jobIds []uuid.UUID) (map[uuid.UUID]importModels.JobCount, error) {
	return importModels.JobCounts(ctx, jobIds)
}

// ListErrors is a page of a job's logged errors.
func ListErrors(ctx context.Context, jobId uuid.UUID, severity string, limit, offset int) ([]map[string]interface{}, error) {
	return importModels.ListErrors(ctx, jobId, severity, limit, offset)
}

// UpdateStatus sets a job's status.
func UpdateStatus(ctx context.Context, jobId uuid.UUID, status string, stage, errMsg *string) error {
	return importModels.UpdateStatus(ctx, jobId, status, stage, errMsg)
}

// UpdateStatusFrom sets a job's status when it is one of from.
func UpdateStatusFrom(ctx context.Context, jobId uuid.UUID, from []string, status string, stage, errMsg *string) (bool, error) {
	return importModels.UpdateStatusFrom(ctx, jobId, from, status, stage, errMsg)
}

// UpdateOptions saves a job's options.
func UpdateOptions(ctx context.Context, jobId uuid.UUID, options json.RawMessage) error {
	return importModels.UpdateOptions(ctx, jobId, options)
}

// FindCompletedJobByHash is a finished import of the same export, if any.
func FindCompletedJobByHash(ctx context.Context, workspaceName, hash string) (*importModels.Job, error) {
	return importModels.FindCompletedJobByHash(ctx, workspaceName, hash)
}

// SetContentHash records the export's hash on its job.
func SetContentHash(ctx context.Context, jobId uuid.UUID, hash string) error {
	return importModels.SetContentHash(ctx, jobId, hash)
}
