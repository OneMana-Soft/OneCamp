// Package adapter holds the wire types for the generic import admin API.
//
// The Slack-only adapter (adapter/SlackImport) remains for backward
// compatibility with the existing Slack FE. New providers route through
// these provider-agnostic shapes.
package adapter

import (
	"encoding/json"
	"time"

	importProvider "github.com/akashc777/OneCamp/business/Import/provider"
	"github.com/google/uuid"
)

// JobOptions is the persisted operator-knob bag. Each provider declares
// its own keys; the FE renders a generic form bound by JSON Schema-ish
// metadata returned from /admin/import/{provider}/options-schema.
type JobOptions = map[string]any

// PresignRequest is the body of POST /admin/import/{provider}/presign.
type PresignRequest struct {
	SourceWorkspaceName string `json:"source_workspace_name"`
	Source              string `json:"source"` // "export_zip"|"board_json"|"backup_xml" depending on provider
	FileSize            int64  `json:"file_size"`
}

// PresignResponse mirrors the legacy Slack one with an added `provider` field.
type PresignResponse struct {
	JobId               uuid.UUID         `json:"job_id"`
	Provider            string            `json:"provider"`
	SourceWorkspaceName string            `json:"source_workspace_name"`
	RawObjectKey        string            `json:"raw_object_key"`
	UploadURL           string            `json:"upload_url"`
	ExpiresIn           int64             `json:"expires_in"`
	Method              string            `json:"method"`
	Headers             map[string]string `json:"headers"`
}

// ConnectRequest is the body of POST /admin/import/{provider}/connect for
// providers that use a token (Trello PAT, Asana PAT). OAuth providers
// use the /oauth/start + callback flow instead.
type ConnectRequest struct {
	AccessToken       string            `json:"access_token"`
	RefreshToken      string            `json:"refresh_token,omitempty"`
	Scopes            string            `json:"scopes,omitempty"`
	ExpiresAtUnix     int64             `json:"expires_at_unix,omitempty"`
	SourceAccountId   string            `json:"source_account_id,omitempty"`
	SourceAccountName string            `json:"source_account_name,omitempty"`
	Metadata          map[string]string `json:"metadata,omitempty"`
}

// ConnectResponse confirms the connection without echoing the token.
type ConnectResponse struct {
	Provider          string     `json:"provider"`
	SourceAccountName string     `json:"source_account_name,omitempty"`
	ExpiresAt         *time.Time `json:"expires_at,omitempty"`
	Scopes            *string    `json:"scopes,omitempty"`
}

// CreateJobRequest is the body of POST /admin/import/{provider}/jobs.
// For api-source providers (Trello/Asana/Jira/Notion/Todoist via live
// API) this is the entry point — no upload needed.
type CreateJobRequest struct {
	SourceWorkspaceName string     `json:"source_workspace_name"`
	Source              string     `json:"source"`
	Options             JobOptions `json:"options,omitempty"`
}

// PlanRequest is POST /admin/import/jobs/{jobId}/plan.
type PlanRequest struct {
	Options          JobOptions        `json:"options,omitempty"`
	StatusMappings   map[string]string `json:"status_mappings,omitempty"`
	PriorityMappings map[string]string `json:"priority_mappings,omitempty"`
}

// RunRequest is POST /admin/import/jobs/{jobId}/run.
type RunRequest struct {
	Options          *JobOptions       `json:"options,omitempty"`
	StatusMappings   map[string]string `json:"status_mappings,omitempty"`
	PriorityMappings map[string]string `json:"priority_mappings,omitempty"`
}

// JobView is the FE-facing job snapshot.
type JobView struct {
	Id                  uuid.UUID            `json:"id"`
	Provider            string               `json:"provider"`
	SourceWorkspaceName string               `json:"source_workspace_name"`
	Source              string               `json:"source"`
	Status              string               `json:"status"`
	Stage               *string              `json:"stage,omitempty"`
	StartedAt           *time.Time           `json:"started_at,omitempty"`
	CompletedAt         *time.Time           `json:"completed_at,omitempty"`
	Options             JobOptions           `json:"options"`
	Plan                *importProvider.Plan `json:"plan,omitempty"`
	Progress            json.RawMessage      `json:"progress,omitempty"`
	ErrorMessage        *string              `json:"error_message,omitempty"`
	TriggeredBy         *uuid.UUID           `json:"triggered_by,omitempty"`
	CreatedAt           time.Time            `json:"created_at"`
	UpdatedAt           time.Time            `json:"updated_at"`
	ChunksTotal         int                  `json:"chunks_total"`
	ChunksDone          int                  `json:"chunks_done"`
	ChunksFailed        int                  `json:"chunks_failed"`
	ItemsImported       int                  `json:"items_imported"`
	ErrorsTotal         int                  `json:"errors_total"`
	StatusMappings      map[string]string    `json:"status_mappings,omitempty"`
	PriorityMappings    map[string]string    `json:"priority_mappings,omitempty"`
}

// ProviderInfo is returned by GET /admin/import/providers and tells the
// FE what providers are installed and which capabilities they have.
type ProviderInfo struct {
	Name             string            `json:"name"`
	Sources          []string          `json:"sources"`
	Capabilities     []string          `json:"capabilities"`
	DefaultStatusMap map[string]string `json:"default_status_map"`
	DefaultPriority  map[string]string `json:"default_priority_map"`
}
