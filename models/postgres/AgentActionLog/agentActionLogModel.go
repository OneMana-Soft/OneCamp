package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// UnresolvedAction is one action whose outcome was never recorded, meaning the
// process did not return after saying it was about to act.
type UnresolvedAction struct {
	Id       uuid.UUID
	RunID    *uuid.UUID
	AgentID  uuid.UUID
	ToolName string
	IntentAt time.Time
}

// RecordIntent writes what the agent is about to do, and MUST complete before
// the attempt. Returns the row id so the outcome can be attached to it.
//
// Synchronous and unbuffered on purpose: an intent that is still in a channel
// when the process dies records nothing, which is the state this table exists to
// make impossible.
func RecordIntent(ctx context.Context, in Intent) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		INSERT INTO ai_agent_action_log
			(id, run_id, agent_id, run_as_user_id, tool_name, params_digest, intent_at, signature)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		in.ID, in.RunID, in.AgentID, in.RunAsUserID, in.ToolName, in.ParamsDigest, in.IntentAt, in.Signature)
	return err
}

// Intent is one recorded action intent. IntentAt is set by the caller (to the
// microsecond Postgres keeps) because the signature covers it.
type Intent struct {
	ID           uuid.UUID
	RunID        *uuid.UUID
	AgentID      uuid.UUID
	RunAsUserID  *uuid.UUID
	ToolName     string
	ParamsDigest string
	IntentAt     time.Time
	Signature    []byte
}

// ListIntents returns an agent's most recent intents, newest first, for
// verifying their signatures.
func ListIntents(ctx context.Context, agentID uuid.UUID, limit int) ([]Intent, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, run_id, agent_id, run_as_user_id, tool_name, params_digest, intent_at, signature
		FROM ai_agent_action_log WHERE agent_id = $1
		ORDER BY intent_at DESC LIMIT $2`, agentID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Intent
	for rows.Next() {
		var in Intent
		var runID, runAs uuid.NullUUID
		if err := rows.Scan(&in.ID, &runID, &in.AgentID, &runAs, &in.ToolName, &in.ParamsDigest, &in.IntentAt, &in.Signature); err != nil {
			return nil, err
		}
		if runID.Valid {
			in.RunID = &runID.UUID
		}
		if runAs.Valid {
			in.RunAsUserID = &runAs.UUID
		}
		out = append(out, in)
	}
	return out, rows.Err()
}

// RecordOutcome closes an intent with what actually happened.
//
// Best effort by nature: if this write is lost the row stays unresolved, which
// reads as "we do not know", and that is the honest answer rather than a wrong
// one. It is the intent write that must not be lost.
func RecordOutcome(ctx context.Context, id uuid.UUID, outcome string, errMsg *string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE ai_agent_action_log
		SET outcome = $2, outcome_at = NOW(), error = $3
		WHERE id = $1 AND outcome IS NULL`,
		id, outcome, errMsg)
	return err
}

// ListUnresolved returns actions in a window whose outcome is unknown.
//
// This is the evidence pack's residual uncertainty, stated as rows rather than
// as a disclaimer.
func ListUnresolved(ctx context.Context, from, to time.Time, limit int) ([]UnresolvedAction, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 1000 {
		limit = 500
	}

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbCtx, `
		SELECT id, run_id, agent_id, tool_name, intent_at
		FROM ai_agent_action_log
		WHERE outcome IS NULL AND intent_at >= $1 AND intent_at <= $2
		ORDER BY intent_at DESC
		LIMIT $3`, from, to, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]UnresolvedAction, 0, 16)
	for rows.Next() {
		var a UnresolvedAction
		if err := rows.Scan(&a.Id, &a.RunID, &a.AgentID, &a.ToolName, &a.IntentAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SummariseUnresolved answers "how many actions never concluded, and what is the
// oldest", exactly, in one round trip.
//
// ListUnresolved cannot answer this. It is ordered newest-first and capped, so a
// caller that takes its last element gets the oldest OF THE PAGE and a length
// that stops at the limit — which reports a large backlog as a small recent one,
// the opposite of the truth, precisely when it matters most.
//
// COUNT(*) OVER () is evaluated across the whole matching set before LIMIT, so
// the count is complete while only one row is returned. The partial index on
// (intent_at DESC) WHERE outcome IS NULL serves the ordering in either
// direction, and in a healthy install the matching set is empty.
//
// A nil action with a nil error means nothing is unresolved.
func SummariseUnresolved(ctx context.Context, before time.Time) (int, *UnresolvedAction, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var a UnresolvedAction
	var total int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT id, run_id, agent_id, tool_name, intent_at, COUNT(*) OVER () AS total
		FROM ai_agent_action_log
		WHERE outcome IS NULL AND intent_at <= $1
		ORDER BY intent_at ASC
		LIMIT 1`, before).Scan(&a.Id, &a.RunID, &a.AgentID, &a.ToolName, &a.IntentAt, &total)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, nil
	}
	if err != nil {
		return 0, nil, err
	}
	return total, &a, nil
}
