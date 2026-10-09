package models

// Persistence for agent_routines (migration 120): named recurring jobs a user
// hands an agent from chat ("every weekday at 9am summarize this channel").
//
// Thin persistence only — validation/clamping (name, prompt, recurrence, fire
// time) lives in the business layer (routineSpec.go); the dispatcher's due-check
// and firing live in business/AIAgent. Recurrence is the Scheduler's RRULE-lite
// string; at_minute_utc is minutes past UTC midnight.

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// AgentRoutine mirrors a row of agent_routines. Exactly one of ChannelID /
// GroupID identifies the surface the routine posts to.
type AgentRoutine struct {
	Id          uuid.UUID  `json:"id"`
	AgentId     uuid.UUID  `json:"agent_id"`
	CreatedBy   uuid.UUID  `json:"created_by"`
	ChannelId   *uuid.UUID `json:"channel_id,omitempty"`
	GroupId     string     `json:"group_id"`
	Name        string     `json:"name"`
	Prompt      string     `json:"prompt"`
	Recurrence  string     `json:"recurrence"`
	AtMinuteUTC int        `json:"at_minute_utc"`
	Enabled     bool       `json:"enabled"`
	LastRunAt   *time.Time `json:"last_run_at,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
}

const routineColumns = `id, agent_id, created_by, channel_id, group_id, name,
	prompt, recurrence, at_minute_utc, enabled, last_run_at, created_at, updated_at`

func scanRoutine(s scanner) (*AgentRoutine, error) {
	var r AgentRoutine
	var channelID uuid.NullUUID
	var lastRunAt sql.NullTime
	if err := s.Scan(
		&r.Id, &r.AgentId, &r.CreatedBy, &channelID, &r.GroupId, &r.Name,
		&r.Prompt, &r.Recurrence, &r.AtMinuteUTC, &r.Enabled, &lastRunAt,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return nil, err
	}
	if channelID.Valid {
		id := channelID.UUID
		r.ChannelId = &id
	}
	if lastRunAt.Valid {
		r.LastRunAt = &lastRunAt.Time
	}
	return &r, nil
}

// CreateRoutine inserts a routine and returns its id.
func CreateRoutine(ctx context.Context, r *AgentRoutine) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	id := uuid.New()
	const q = `INSERT INTO agent_routines
		(id, agent_id, created_by, channel_id, group_id, name, prompt, recurrence, at_minute_utc, enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	var channelArg interface{}
	if r.ChannelId != nil {
		channelArg = *r.ChannelId
	}
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, r.AgentId, r.CreatedBy, channelArg, r.GroupId, r.Name, r.Prompt,
		r.Recurrence, r.AtMinuteUTC, r.Enabled); err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateRoutine err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetRoutine returns one routine by id (nil, nil when not found/deleted).
func GetRoutine(ctx context.Context, id uuid.UUID) (*AgentRoutine, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + routineColumns + ` FROM agent_routines WHERE id = $1 AND deleted_at IS NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id)
	r, err := scanRoutine(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return r, err
}

// ListRoutinesForAgent returns an agent's active routines, newest first.
func ListRoutinesForAgent(ctx context.Context, agentID uuid.UUID) ([]*AgentRoutine, error) {
	return queryRoutines(ctx,
		`SELECT `+routineColumns+` FROM agent_routines
		 WHERE agent_id = $1 AND deleted_at IS NULL ORDER BY created_at DESC`, agentID)
}

// ListEnabledRoutines returns every enabled, non-deleted routine — the
// dispatcher's scan set. Bounded by an explicit limit so a runaway count can't
// blow up a tick.
func ListEnabledRoutines(ctx context.Context, limit int) ([]*AgentRoutine, error) {
	if limit <= 0 {
		limit = 500
	}
	return queryRoutines(ctx,
		`SELECT `+routineColumns+` FROM agent_routines
		 WHERE enabled = true AND deleted_at IS NULL ORDER BY created_at ASC LIMIT $1`, limit)
}

func queryRoutines(ctx context.Context, q string, args ...interface{}) ([]*AgentRoutine, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AgentRoutine
	for rows.Next() {
		r, err := scanRoutine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ClaimDueRoutineRun atomically claims a routine's due occurrence (optimistic
// compare-and-set on last_run_at) so EXACTLY ONE replica fires it. It advances
// last_run_at to `when` only if the row still holds the value the caller
// observed when it judged the routine due. This is the multi-replica-safe
// replacement for an unconditional MarkRoutineRun stamp: without the CAS, two
// replicas ticking the same minute would both stamp and both launch the routine
// (duplicate channel posts, doubled spend). Returns true only for the winner.
// observedLastRun may be nil (a routine that has never run).
func ClaimDueRoutineRun(ctx context.Context, id uuid.UUID, observedLastRun *time.Time, when time.Time) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var obs interface{}
	if observedLastRun != nil {
		obs = observedLastRun.UTC()
	}
	const q = `UPDATE agent_routines
		SET last_run_at = $2, updated_at = NOW()
		WHERE id = $1 AND deleted_at IS NULL AND last_run_at IS NOT DISTINCT FROM $3`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, when.UTC(), obs)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ClaimDueRoutineRun err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetRoutineEnabled toggles a routine on/off (pause without deleting).
func SetRoutineEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE agent_routines SET enabled = $2, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL`, id, enabled)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListRoutinesRecordedAsSponsorBefore returns the live routines created before
// t that are recorded as created by their agent's sponsor: every routine from
// before the person who asked for one was recorded looks like that.
func ListRoutinesRecordedAsSponsorBefore(ctx context.Context, t time.Time) ([]*AgentRoutine, error) {
	return queryRoutines(ctx,
		`SELECT `+routineColumns+` FROM agent_routines
		 WHERE deleted_at IS NULL AND created_at < $1
		   AND created_by = (SELECT created_by FROM ai_agents WHERE ai_agents.id = agent_routines.agent_id)
		 ORDER BY created_at ASC`, t.UTC())
}

// ForgetRoutineAsker pauses a routine and records that nobody known asked for
// it (created_by is the nil uuid), unless that is already so. changed reports
// whether this call did it.
func ForgetRoutineAsker(ctx context.Context, id uuid.UUID) (changed bool, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE agent_routines SET enabled = false, created_by = $2, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL AND created_by <> $2`, id, uuid.Nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ForgetRoutineAsker err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimRoutine turns on a routine nobody known asked for, recording createdBy
// as the person who did. sql.ErrNoRows when there is no such routine left.
func ClaimRoutine(ctx context.Context, id, createdBy uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE agent_routines SET enabled = true, created_by = $2, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL AND created_by = $3`, id, createdBy, uuid.Nil)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DeleteRoutine soft-deletes a routine (cancel).
func DeleteRoutine(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE agent_routines SET deleted_at = NOW(), enabled = false, updated_at = NOW()
		 WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
