package business

import (
	"context"
	"encoding/json"
	"time"

	contentIOModels "github.com/akashc777/OneCamp/models/postgres/ContentIO"
	"github.com/google/uuid"
)

// jobStore keeps jobs: Postgres (content_io_jobs), or a fake in tests.
type jobStore interface {
	Create(ctx context.Context, j *contentIOModels.Job) error
	Get(ctx context.Context, id uuid.UUID) (*contentIOModels.Job, error)
	Queue(ctx context.Context, id uuid.UUID, inputKey string) (bool, error)
	ClaimNext(ctx context.Context) (*contentIOModels.Job, error)
	SetProgress(ctx context.Context, id uuid.UUID, progress int) error
	Finish(ctx context.Context, id uuid.UUID, status string, result json.RawMessage, outputKey, errText *string) error
	FailAbandoned(ctx context.Context, runningBefore, uploadingBefore time.Time) ([]*contentIOModels.Job, error)
	FinishedBefore(ctx context.Context, before time.Time, limit int) ([]*contentIOModels.Job, error)
	ForgetFiles(ctx context.Context, id uuid.UUID) error
	DeleteFinishedBefore(ctx context.Context, before time.Time) (int64, error)
	Recent(ctx context.Context, userUUID uuid.UUID, kind, format string, limit int) ([]*contentIOModels.Job, error)
}

var jobs jobStore = pgJobs{}

type pgJobs struct{}

func (pgJobs) Create(ctx context.Context, j *contentIOModels.Job) error {
	return contentIOModels.Create(ctx, j)
}
func (pgJobs) Get(ctx context.Context, id uuid.UUID) (*contentIOModels.Job, error) {
	return contentIOModels.Get(ctx, id)
}
func (pgJobs) Queue(ctx context.Context, id uuid.UUID, inputKey string) (bool, error) {
	return contentIOModels.Queue(ctx, id, inputKey)
}
func (pgJobs) ClaimNext(ctx context.Context) (*contentIOModels.Job, error) {
	return contentIOModels.ClaimNext(ctx)
}
func (pgJobs) SetProgress(ctx context.Context, id uuid.UUID, progress int) error {
	return contentIOModels.SetProgress(ctx, id, progress)
}
func (pgJobs) Finish(ctx context.Context, id uuid.UUID, status string, result json.RawMessage, outputKey, errText *string) error {
	return contentIOModels.Finish(ctx, id, status, result, outputKey, errText)
}
func (pgJobs) FailAbandoned(ctx context.Context, runningBefore, uploadingBefore time.Time) ([]*contentIOModels.Job, error) {
	return contentIOModels.FailAbandoned(ctx, runningBefore, uploadingBefore)
}
func (pgJobs) FinishedBefore(ctx context.Context, before time.Time, limit int) ([]*contentIOModels.Job, error) {
	return contentIOModels.FinishedBefore(ctx, before, limit)
}
func (pgJobs) ForgetFiles(ctx context.Context, id uuid.UUID) error {
	return contentIOModels.ForgetFiles(ctx, id)
}
func (pgJobs) DeleteFinishedBefore(ctx context.Context, before time.Time) (int64, error) {
	return contentIOModels.DeleteFinishedBefore(ctx, before)
}
func (pgJobs) Recent(ctx context.Context, userUUID uuid.UUID, kind, format string, limit int) ([]*contentIOModels.Job, error) {
	return contentIOModels.Recent(ctx, userUUID, kind, format, limit)
}

// jobParams is what a job was asked, kept in its params column.
type jobParams struct {
	Filename   string `json:"filename"`
	MediaType  string `json:"media_type,omitempty"`
	ParentUUID string `json:"parent_uuid,omitempty"`
	Size       int64  `json:"size,omitempty"`
}

// jobResult is what a job did, kept in its result column.
type jobResult struct {
	Created []Created `json:"created,omitempty"`
	// Exported is how many docs a workspace export wrote.
	Exported int       `json:"exported,omitempty"`
	Skipped  []Skipped `json:"skipped,omitempty"`
}

// toJob is a row as its owner sees it.
func toJob(r *contentIOModels.Job) Job {
	var p jobParams
	_ = json.Unmarshal(r.Params, &p)
	var res jobResult
	_ = json.Unmarshal(r.Result, &res)
	j := Job{
		ID:         r.ID.String(),
		Kind:       r.Kind,
		Format:     r.Format,
		UserUUID:   r.UserUUID.String(),
		Status:     JobStatus(r.Status),
		Progress:   r.Progress,
		Filename:   p.Filename,
		ParentUUID: p.ParentUUID,
		Created:    res.Created,
		Skipped:    res.Skipped,
		Exported:   res.Exported,
		CreatedAt:  r.CreatedAt,
		FinishedAt: r.FinishedAt,
	}
	if r.Error != nil {
		j.Error = *r.Error
	}
	if r.OutputKey != nil && r.FinishedAt != nil {
		kept := r.FinishedAt.Add(keepFilesFor)
		j.KeptUntil = &kept
	}
	return j
}
