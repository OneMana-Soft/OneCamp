// Package models (Workflow) is the Postgres data-access layer for the Workflow
// Builder — event-triggered "when X happens, do Y" automation. A workflow is a
// saved rule the engine evaluates against the workspace event bus; this package
// only persists and reads those rules.
package models

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Trigger type constants (keep aligned with the migration CHECK).
const (
	TriggerMessagePosted     = "message_posted"
	TriggerReactionAdded     = "reaction_added"
	TriggerUserJoinedChannel = "user_joined_channel"
	TriggerTaskCreated       = "task_created"
	TriggerTaskStatusChanged = "task_status_changed"
	// TriggerMeetingEnded fires when a call in a channel, DM or group finishes.
	// The recap agent already ran by this point, so a workflow on this trigger
	// acts on a meeting that has been summarised rather than one still being
	// transcribed.
	TriggerMeetingEnded = "meeting_ended"
)

// ValidTriggerType reports whether t is a supported trigger kind (mirrors the
// migration CHECK so the business layer can reject bad input early).
func ValidTriggerType(t string) bool {
	switch t {
	case TriggerMessagePosted, TriggerReactionAdded, TriggerUserJoinedChannel,
		TriggerTaskCreated, TriggerTaskStatusChanged, TriggerMeetingEnded:
		return true
	default:
		return false
	}
}

// Match type constants.
const (
	MatchAny = "any"
	MatchAll = "all"
)

// Workflow mirrors a row of the workflows table. Keywords and Actions are raw
// JSON, parsed/validated in the business layer.
type Workflow struct {
	Id            uuid.UUID  `json:"id"`
	Name          string     `json:"name"`
	IsActive      bool       `json:"is_active"`
	CreatedBy     uuid.UUID  `json:"created_by"`
	TriggerType   string     `json:"trigger_type"`
	TriggerConfig string     `json:"trigger_config"` // raw JSON object
	BotName       *string    `json:"bot_name,omitempty"`
	ChannelId     *uuid.UUID `json:"channel_id,omitempty"`
	Keywords      string     `json:"keywords"` // raw JSON array
	MatchType     string     `json:"match_type"`
	Actions       string     `json:"actions"` // raw JSON array
	RunCount      int64      `json:"run_count"`
	LastRunAt     *time.Time `json:"last_run_at,omitempty"`
	LastError     *string    `json:"last_error,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	DeletedAt     *time.Time `json:"deleted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

const selectColumns = `id, name, is_active, created_by, trigger_type, trigger_config,
	bot_name, channel_id, keywords, match_type, actions, run_count, last_run_at,
	last_error, created_at, updated_at, deleted_at`

func scanWorkflow(s scanner) (*Workflow, error) {
	var w Workflow
	var channelId uuid.NullUUID
	var botName, lastError sql.NullString
	var lastRunAt, deletedAt sql.NullTime

	err := s.Scan(
		&w.Id,
		&w.Name,
		&w.IsActive,
		&w.CreatedBy,
		&w.TriggerType,
		&w.TriggerConfig,
		&botName,
		&channelId,
		&w.Keywords,
		&w.MatchType,
		&w.Actions,
		&w.RunCount,
		&lastRunAt,
		&lastError,
		&w.CreatedAt,
		&w.UpdatedAt,
		&deletedAt,
	)
	if err != nil {
		return nil, err
	}
	if botName.Valid {
		w.BotName = &botName.String
	}
	if channelId.Valid {
		w.ChannelId = &channelId.UUID
	}
	if lastRunAt.Valid {
		w.LastRunAt = &lastRunAt.Time
	}
	if lastError.Valid {
		w.LastError = &lastError.String
	}
	if deletedAt.Valid {
		w.DeletedAt = &deletedAt.Time
	}
	return &w, nil
}

// CreateWorkflow inserts a new workflow and returns the generated id.
func CreateWorkflow(ctx context.Context, name string, createdBy uuid.UUID, triggerType string, triggerConfigJSON string, botName *string, channelId *uuid.UUID, keywordsJSON string, matchType string, actionsJSON string) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(triggerConfigJSON) == "" {
		triggerConfigJSON = "{}"
	}
	id := uuid.New()
	const q = `INSERT INTO workflows (id, name, created_by, trigger_type, trigger_config, bot_name, channel_id, keywords, match_type, actions)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, name, createdBy, triggerType, triggerConfigJSON, botName, channelId, keywordsJSON, matchType, actionsJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateWorkflow Failed to insert workflow err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateWorkflow updates the editable fields of a workflow.
func UpdateWorkflow(ctx context.Context, id uuid.UUID, name string, isActive bool, triggerType string, triggerConfigJSON string, botName *string, channelId *uuid.UUID, keywordsJSON string, matchType string, actionsJSON string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(triggerConfigJSON) == "" {
		triggerConfigJSON = "{}"
	}
	const q = `UPDATE workflows
		SET name = $2, is_active = $3, trigger_type = $4, trigger_config = $5, bot_name = $6,
		    channel_id = $7, keywords = $8, match_type = $9, actions = $10, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, name, isActive, triggerType, triggerConfigJSON, botName, channelId, keywordsJSON, matchType, actionsJSON)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateWorkflow Failed to update workflow err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetWorkflowActive toggles a workflow's enabled state.
func SetWorkflowActive(ctx context.Context, id uuid.UUID, isActive bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE workflows SET is_active = $2, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, isActive)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetWorkflowActive Failed err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SoftDeleteWorkflow marks a workflow deleted.
func SoftDeleteWorkflow(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE workflows SET deleted_at = NOW(), is_active = false, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteWorkflow Failed err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetWorkflowByID returns a single non-deleted workflow, or (nil, nil) if absent.
func GetWorkflowByID(ctx context.Context, id uuid.UUID) (*Workflow, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + selectColumns + ` FROM workflows WHERE id = $1 AND deleted_at IS NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id)
	w, err := scanWorkflow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetWorkflowByID Failed err: %+v", err)
		return nil, err
	}
	return w, nil
}

// ListWorkflows returns all non-deleted workflows, newest first.
func ListWorkflows(ctx context.Context) ([]*Workflow, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + selectColumns + ` FROM workflows WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListWorkflows Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*Workflow
	for rows.Next() {
		w, scanErr := scanWorkflow(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListWorkflows scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ListWorkflowsByCreator returns a single user's non-deleted workflows, newest
// first. Used for non-admin members so the filter runs in SQL (served by the
// created_by partial index) instead of fetching all workflows and filtering in Go.
func ListWorkflowsByCreator(ctx context.Context, createdBy uuid.UUID) ([]*Workflow, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + selectColumns + ` FROM workflows WHERE deleted_at IS NULL AND created_by=$1 ORDER BY created_at DESC LIMIT 1000`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, createdBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListWorkflowsByCreator Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*Workflow
	for rows.Next() {
		w, scanErr := scanWorkflow(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListWorkflowsByCreator scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
func ListActiveByTrigger(ctx context.Context, triggerType string) ([]*Workflow, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + selectColumns + ` FROM workflows
		WHERE trigger_type = $1 AND is_active = true AND deleted_at IS NULL`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, triggerType)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListActiveByTrigger Failed err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*Workflow
	for rows.Next() {
		w, scanErr := scanWorkflow(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/ListActiveByTrigger scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// RecordRun bumps run bookkeeping after the engine executes a workflow.
// A non-empty errMsg is stored in last_error; an empty errMsg clears it.
func RecordRun(ctx context.Context, id uuid.UUID, runAt time.Time, errMsg string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var errArg interface{}
	if errMsg != "" {
		errArg = errMsg
	}
	const q = `UPDATE workflows
		SET run_count = run_count + 1, last_run_at = $2, last_error = $3, updated_at = NOW()
		WHERE id = $1`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, runAt, errArg)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RecordRun Failed err: %+v", err)
		return err
	}
	return nil
}
