package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// MaxRulesPerProject bounds one project's rules: a list a person can read,
// and a bound on what one event fans out to.
const MaxRulesPerProject = 50

// ListByProject is a project's rules, oldest first (the order they run in).
func ListByProject(ctx context.Context, projectID uuid.UUID) ([]*Workflow, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + selectColumns + ` FROM workflows
		WHERE deleted_at IS NULL AND project_id = $1 ORDER BY created_at, id LIMIT 200`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, projectID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListByProject err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Workflow
	for rows.Next() {
		w, err := scanWorkflow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// CountByProject is how many live rules a project has.
func CountByProject(ctx context.Context, projectID uuid.UUID) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`SELECT count(*) FROM workflows WHERE deleted_at IS NULL AND project_id = $1`, projectID).Scan(&n)
	return n, err
}

// NextTurn takes a rule's next round-robin turn: each call answers a new
// number, so two runs at once never give the same turn.
func NextTurn(ctx context.Context, id uuid.UUID) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int64
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
		`UPDATE workflows SET rr_cursor = rr_cursor + 1 WHERE id = $1 RETURNING rr_cursor`, id).Scan(&n)
	return n, err
}

// Pause turns a rule off with the reason it stopped. Answers false when it
// was already off (so its owner is told once).
func Pause(ctx context.Context, id uuid.UUID, reason string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE workflows SET is_active = false, paused_reason = $2, updated_at = NOW()
		 WHERE id = $1 AND is_active AND deleted_at IS NULL`, id, reason)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Run statuses.
const (
	RunRunning = "running"
	RunOK      = "ok"
	RunFailed  = "failed"
	RunSkipped = "skipped"
	RunUndone  = "undone"
)

// Step is one action of a run, as it happened. Before and After are the
// values a change replaced and set, kept so the run can be undone.
type Step struct {
	Type   string          `json:"type"`
	Status string          `json:"status"` // ok, failed, skipped
	Detail string          `json:"detail,omitempty"`
	TaskID string          `json:"task_id,omitempty"`
	Field  string          `json:"field,omitempty"`
	Before json.RawMessage `json:"before,omitempty"`
	After  json.RawMessage `json:"after,omitempty"`
	// Made is what the step created (a comment, a sub-task, a task), for the
	// history to link to.
	Made string `json:"made,omitempty"`
}

// Run is one run of a rule.
type Run struct {
	ID         uuid.UUID  `json:"id"`
	WorkflowID uuid.UUID  `json:"workflow_id"`
	Trigger    string     `json:"trigger"`
	ProjectID  *uuid.UUID `json:"project_id,omitempty"`
	SubjectID  string     `json:"subject_id,omitempty"`
	Subject    string     `json:"subject,omitempty"`
	Status     string     `json:"status"`
	Branch     string     `json:"branch,omitempty"`
	Error      string     `json:"error,omitempty"`
	Steps      []Step     `json:"steps"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	UndoneBy   *uuid.UUID `json:"undone_by,omitempty"`
	UndoneAt   *time.Time `json:"undone_at,omitempty"`
}

// ClaimRun records that a rule starts a run for one event. It answers false
// when that event already has a run (a second delivery), which then doesn't
// run again.
func ClaimRun(ctx context.Context, r *Run, eventKey string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if r.ID == uuid.Nil {
		r.ID = uuid.New()
	}
	if r.StartedAt.IsZero() {
		r.StartedAt = time.Now()
	}
	if r.Status == "" {
		r.Status = RunRunning
	}
	steps, _ := json.Marshal(nonNil(r.Steps))
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`INSERT INTO workflow_runs (id, workflow_id, event_key, trigger, project_id, subject_id, subject, status, branch, error, steps, started_at, finished_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, ''), NULLIF($10, ''), $11, $12, $13)
		 ON CONFLICT (workflow_id, event_key) DO NOTHING`,
		r.ID, r.WorkflowID, clip(eventKey, 200), clip(r.Trigger, 40), r.ProjectID, clip(r.SubjectID, 64), clip(r.Subject, 300),
		r.Status, r.Branch, r.Error, steps, r.StartedAt, r.FinishedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimRun err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// FinishRun writes how a run ended.
func FinishRun(ctx context.Context, r *Run) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	now := time.Now()
	r.FinishedAt = &now
	steps, _ := json.Marshal(nonNil(r.Steps))
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE workflow_runs SET status = $2, branch = NULLIF($3, ''), error = NULLIF($4, ''), steps = $5, finished_at = $6, subject = COALESCE(NULLIF($7, ''), subject)
		 WHERE id = $1`, r.ID, r.Status, r.Branch, clip(r.Error, 2000), steps, now, clip(r.Subject, 300))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/FinishRun err: %+v", err)
	}
	return err
}

// MarkUndone records that someone undid a run; it answers false when the run
// was already undone or isn't one that finished well.
func MarkUndone(ctx context.Context, id, by uuid.UUID, steps []Step) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	b, _ := json.Marshal(nonNil(steps))
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE workflow_runs SET status = 'undone', undone_by = $2, undone_at = NOW(), steps = $3
		 WHERE id = $1 AND status IN ('ok', 'failed')`, id, by, b)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// ErrRunNotFound is a run that isn't there.
var ErrRunNotFound = errors.New("that run isn't there")

const runColumns = `id, workflow_id, trigger, project_id, COALESCE(subject_id, ''), COALESCE(subject, ''), status,
	COALESCE(branch, ''), COALESCE(error, ''), steps, started_at, finished_at, undone_by, undone_at`

func scanRun(s scanner) (*Run, error) {
	var r Run
	var project, undoneBy uuid.NullUUID
	var finished, undoneAt sql.NullTime
	var steps []byte
	if err := s.Scan(&r.ID, &r.WorkflowID, &r.Trigger, &project, &r.SubjectID, &r.Subject, &r.Status,
		&r.Branch, &r.Error, &steps, &r.StartedAt, &finished, &undoneBy, &undoneAt); err != nil {
		return nil, err
	}
	if project.Valid {
		r.ProjectID = &project.UUID
	}
	if undoneBy.Valid {
		r.UndoneBy = &undoneBy.UUID
	}
	if finished.Valid {
		r.FinishedAt = &finished.Time
	}
	if undoneAt.Valid {
		r.UndoneAt = &undoneAt.Time
	}
	_ = json.Unmarshal(steps, &r.Steps)
	r.Steps = nonNil(r.Steps)
	return &r, nil
}

// GetRun is one run.
func GetRun(ctx context.Context, id uuid.UUID) (*Run, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	r, err := scanRun(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, `SELECT `+runColumns+` FROM workflow_runs WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrRunNotFound
	}
	return r, err
}

// ListRuns is the newest runs of a project's rules (workflowID nil) or of one
// rule, before a time (nil for the newest), at most limit.
func ListRuns(ctx context.Context, projectID *uuid.UUID, workflowID *uuid.UUID, before *time.Time, limit int) ([]*Run, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	q := `SELECT ` + runColumns + ` FROM workflow_runs WHERE `
	var args []any
	if workflowID != nil {
		q += `workflow_id = $1`
		args = append(args, *workflowID)
	} else if projectID != nil {
		q += `project_id = $1`
		args = append(args, *projectID)
	} else {
		return nil, nil
	}
	if before != nil {
		q += ` AND started_at < $2`
		args = append(args, *before)
	}
	q += ` ORDER BY started_at DESC LIMIT ` + strconv.Itoa(limit)
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListRuns err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RunsSince counts a rule's runs since a time (workflowID nil: every rule's,
// the workspace's rate).
func RunsSince(ctx context.Context, workflowID *uuid.UUID, since time.Time) (int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var n int
	var err error
	if workflowID != nil {
		err = postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
			`SELECT count(*) FROM workflow_runs WHERE workflow_id = $1 AND started_at >= $2`, *workflowID, since).Scan(&n)
	} else {
		err = postgresInit.DBConn.SqlDB.QueryRowContext(dbctx,
			`SELECT count(*) FROM workflow_runs WHERE started_at >= $1`, since).Scan(&n)
	}
	return n, err
}

// PurgeRuns deletes runs that started before a time, at most limit of them,
// and answers how many went.
func PurgeRuns(ctx context.Context, before time.Time, limit int) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`DELETE FROM workflow_runs WHERE id IN (SELECT id FROM workflow_runs WHERE started_at < $1 LIMIT $2)`, before, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func nonNil(s []Step) []Step {
	if s == nil {
		return []Step{}
	}
	return s
}

// clip cuts s to at most n bytes on a rune boundary.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && (s[n]&0xC0) == 0x80 {
		n--
	}
	return s[:n]
}

// SetOwner makes someone the person a rule acts as.
func SetOwner(ctx context.Context, id, owner uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE workflows SET created_by = $2, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, id, owner)
	return err
}
