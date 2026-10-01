// Package adapter holds the request/response DTOs for the Slack import
// admin API. These are the wire types the FE binds to. Domain/business
// types live in business/SlackImport.
package adapter

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// JobOptions are operator-tunable knobs for a single import. Stored in
// slack_import_jobs.options as JSONB, decoded back here when read.
type JobOptions struct {
	// SkipSubtypes drops noisy system messages (channel_join, channel_leave,
	// channel_topic, channel_purpose, etc.) from the import. Default true.
	// Operators rarely want to keep "Akash has joined the channel" noise.
	SkipSubtypes *bool `json:"skip_subtypes,omitempty"`

	// SendInvites, when true, queues an invitation email to provisioned
	// external users so they can take over their imported account. Default
	// false to avoid surprising people during a dry import.
	SendInvites *bool `json:"send_invites,omitempty"`

	// ChannelPrefix optionally prefixes every imported channel name to
	// avoid collisions with existing OneCamp channels. Empty means no
	// prefix; collisions are resolved by suffixing "-from-slack".
	ChannelPrefix string `json:"channel_prefix,omitempty"`

	// MaxFileBytes caps individual file downloads from Slack. Larger
	// files become placeholder attachments with the original Slack URL
	// in metadata. Zero means use the env default (DEFAULT_MAX_FILE_BYTES).
	MaxFileBytes int64 `json:"max_file_bytes,omitempty"`

	// DryRun runs the entire pipeline but never commits to PG/Dgraph/MinIO.
	// Useful to preview the plan without side-effects. Default false.
	DryRun bool `json:"dry_run,omitempty"`
}

// UploadResponse is returned after a successful zip upload. The FE then
// hits Plan with this id.
type UploadResponse struct {
	JobId              uuid.UUID `json:"job_id"`
	SlackWorkspaceName string    `json:"slack_workspace_name"`
	RawObjectKey       string    `json:"raw_object_key"`
}

// PlanRequest is the body of POST /admin/import/slack/plan/{jobId}.
type PlanRequest struct {
	Options JobOptions `json:"options"`
}

// PlanResponse summarises what would happen if we ran this import.
// Generated at planning time and stored in slack_import_jobs.plan so a
// resume after FE reload can render the same screen.
type PlanResponse struct {
	JobId           uuid.UUID `json:"job_id"`
	UserCount       int       `json:"user_count"`
	UserNew         int       `json:"user_new"`   // would create as external
	UserMerge       int       `json:"user_merge"` // would merge into existing email
	ChannelCount    int       `json:"channel_count"`
	ChannelConflict int       `json:"channel_conflict"` // collision with existing channel name
	MessageCount    int       `json:"message_count"`
	ThreadCount     int       `json:"thread_count"`
	FileCount       int       `json:"file_count"`
	FileBytes       int64     `json:"file_bytes"`
	Warnings        []string  `json:"warnings,omitempty"`
}

// RunRequest is the body of POST /admin/import/slack/run/{jobId}.
// Re-supplying options here lets the operator tweak knobs after the plan
// without re-uploading.
type RunRequest struct {
	Options *JobOptions `json:"options,omitempty"`
}

// JobView is the FE-facing job snapshot. Keep in sync with the FE
// SlackImportJob TypeScript interface.
type JobView struct {
	Id                 uuid.UUID       `json:"id"`
	SlackWorkspaceName string          `json:"slack_workspace_name"`
	Source             string          `json:"source"`
	Status             string          `json:"status"`
	Stage              *string         `json:"stage,omitempty"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	CompletedAt        *time.Time      `json:"completed_at,omitempty"`
	Options            JobOptions      `json:"options"`
	Plan               *PlanResponse   `json:"plan,omitempty"`
	Progress           json.RawMessage `json:"progress,omitempty"`
	ErrorMessage       *string         `json:"error_message,omitempty"`
	// Digest is the AI's account of what the import brought in. Omitted rather
	// than empty when absent, so the panel can tell "no digest" (AI-free build,
	// AI off, or an older job) from "the model returned nothing".
	Digest      *string    `json:"digest,omitempty"`
	TriggeredBy *uuid.UUID `json:"triggered_by,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	// Live counts (denormalised from chunks for the FE; cheap to compute).
	ChunksTotal   int `json:"chunks_total"`
	ChunksDone    int `json:"chunks_done"`
	ChunksFailed  int `json:"chunks_failed"`
	ItemsImported int `json:"items_imported"`
	ErrorsTotal   int `json:"errors_total"`
}

// ErrorView is one row of the errors panel.
type ErrorView struct {
	Id         uuid.UUID       `json:"id"`
	EntityType *string         `json:"entity_type,omitempty"`
	SlackId    *string         `json:"slack_id,omitempty"`
	Severity   string          `json:"severity"`
	Code       *string         `json:"code,omitempty"`
	Message    string          `json:"message"`
	Context    json.RawMessage `json:"context,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}
