package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
)

// InventoryWindow is how far back the inventory counts actions and refusals.
// A week is what an admin means by "lately" and keeps both reads on the
// newest-first indexes.
const InventoryWindow = 7 * 24 * time.Hour

// AgentActivity is what an agent did, and was stopped from doing, in the
// inventory window. Keyed by agent id or credential id, both as text, because
// refusals carry them as text in audit metadata.
type AgentActivity struct {
	Actions       int64      `json:"actions"`
	Refusals      int64      `json:"refusals"`
	LastRefusalAt *time.Time `json:"last_refusal_at,omitempty"`
}

// InventoryActivity returns, for the window ending now:
//   - byAgent: effecting actions that completed (action log, outcome ok) and
//     refusals (audit log, mcp.tool_call.refused) per agent id;
//   - byToken: refusals per credential id, so a credential that is not bound
//     to an agent still shows what it tried.
//
// Two grouped reads, whatever the number of agents.
func InventoryActivity(ctx context.Context, since time.Time) (byAgent, byToken map[string]*AgentActivity, err error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	byAgent = map[string]*AgentActivity{}
	byToken = map[string]*AgentActivity{}
	get := func(m map[string]*AgentActivity, k string) *AgentActivity {
		if m[k] == nil {
			m[k] = &AgentActivity{}
		}
		return m[k]
	}

	const actionsQ = `SELECT agent_id::text, COUNT(*) FROM ai_agent_action_log
		WHERE intent_at > $1 AND outcome = 'ok' GROUP BY agent_id`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, actionsQ, since)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/InventoryActivity actions err: %+v", err)
		return nil, nil, err
	}
	for rows.Next() {
		var id string
		var n int64
		if err = rows.Scan(&id, &n); err != nil {
			rows.Close()
			return nil, nil, err
		}
		get(byAgent, id).Actions = n
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}

	// GROUPING SETS gives both rollups from one scan of the window. A row with
	// both keys NULL is a refusal that named neither, and is not anyone's.
	const refusalsQ = `SELECT metadata->>'agent_id', metadata->>'token_id',
			GROUPING(metadata->>'agent_id'), COUNT(*), MAX(created_at)
		FROM admin_audit_log
		WHERE created_at > $1 AND action = 'mcp.tool_call.refused'
		GROUP BY GROUPING SETS ((metadata->>'agent_id'), (metadata->>'token_id'))`
	rows, err = postgresInit.DBConn.SqlDB.QueryContext(dbctx, refusalsQ, since)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/InventoryActivity refusals err: %+v", err)
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var agentID, tokenID sql.NullString
		var agentGrouped int
		var n int64
		var last sql.NullTime
		if err = rows.Scan(&agentID, &tokenID, &agentGrouped, &n, &last); err != nil {
			return nil, nil, err
		}
		var a *AgentActivity
		switch {
		case agentGrouped == 0 && agentID.Valid && agentID.String != "":
			a = get(byAgent, agentID.String)
		case agentGrouped == 1 && tokenID.Valid && tokenID.String != "":
			a = get(byToken, tokenID.String)
		default:
			continue
		}
		a.Refusals = n
		if last.Valid {
			t := last.Time
			a.LastRefusalAt = &t
		}
	}
	return byAgent, byToken, rows.Err()
}
