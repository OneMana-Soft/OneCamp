// Package models (PendingAction) is the Postgres data-access layer for durable
// AI write approvals (migration 101).
//
// When the AI assistant / a bot / an agent proposes a WRITE action, it is
// persisted here as a durable record instead of an ephemeral client-side
// dialog. The action survives tab close / navigation / reload and is surfaced
// as an in-thread Approve/Deny card. On approval it executes server-side, AT
// MOST ONCE, AS the approver, with that user's permissions re-checked at
// execution time (no confused-deputy: the bot holds no standalone privilege).
//
// Status lifecycle (enforced by the business layer + the guarded transition):
//
//	pending -> executing -> executed | failed   (approve)
//	pending -> rejected                          (deny)
//	pending -> expired                           (TTL passed, never executed)
//
// The pending -> executing transition is a guarded UPDATE so two concurrent
// approvals (double click / two tabs / two devices) execute the action only
// once.
package models

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Surface types: where the proposal was raised, so the FE can render the card
// in place and reconcile on load.
const (
	SurfaceChannel   = "channel"
	SurfaceDM        = "dm"
	SurfaceGroup     = "group"
	SurfaceAssistant = "assistant"
)

// Status values.
const (
	StatusPending   = "pending"
	StatusExecuting = "executing"
	StatusExecuted  = "executed"
	StatusFailed    = "failed"
	StatusRejected  = "rejected"
	StatusExpired   = "expired"
)

// PendingAction mirrors a row of ai_pending_actions.
type PendingAction struct {
	Id          uuid.UUID         `json:"id"`
	RequestedBy uuid.UUID         `json:"requested_by"`
	SurfaceType string            `json:"surface_type"`
	SurfaceID   string            `json:"surface_id"`
	ToolName    string            `json:"tool_name"`
	Params      map[string]string `json:"params"`
	Description string            `json:"description"`
	// Destructive is DERIVED, not a stored column: whether the proposed tool
	// performs an irreversible/high-risk mutation. The business layer sets it
	// from the live tool registry so the FE can flag the approval card; it is
	// always recomputable from tool_name (so audit history loses nothing).
	Destructive bool `json:"destructive"`
	// Fresh is true only on the call that inserted the row. A retry with the
	// same idempotency key gets the existing row back with Fresh false, so a
	// side effect meant once per proposal (a push to the person) happens once.
	Fresh          bool       `json:"-"`
	Status         string     `json:"status"`
	IdempotencyKey *string    `json:"idempotency_key,omitempty"`
	Result         *string    `json:"result,omitempty"`
	Error          *string    `json:"error,omitempty"`
	ExpiresAt      time.Time  `json:"expires_at"`
	CreatedAt      time.Time  `json:"created_at"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy     *uuid.UUID `json:"resolved_by,omitempty"`

	// AgentID / RunID name the agent and run that proposed this, when an agent
	// did. Nil for everything else that proposes actions (the assistant, the
	// coworker, an MCP server), which is most of them.
	AgentID *uuid.UUID `json:"agent_id,omitempty"`
	RunID   *uuid.UUID `json:"run_id,omitempty"`
}

// Attribution links a proposal to the agent and run that produced it, so the
// approve/deny a person makes anyway becomes a quality signal for that agent.
//
// A struct rather than two more positional arguments on an already long
// constructor, and its zero value is the common case: not from an agent.
type Attribution struct {
	AgentID *uuid.UUID
	RunID   *uuid.UUID
}

// Expired reports whether the action's TTL has passed (pure helper; does not
// touch the DB).
func (p *PendingAction) Expired() bool {
	return time.Now().After(p.ExpiresAt)
}

type scanner interface {
	Scan(dest ...any) error
}

const columns = `id, requested_by, surface_type, surface_id, tool_name, params, description, status, idempotency_key, result, error, expires_at, created_at, resolved_at, resolved_by, agent_id, run_id`

func scanAction(s scanner) (*PendingAction, error) {
	var p PendingAction
	var paramsRaw []byte
	var idem, result, errStr sql.NullString
	var resolvedAt sql.NullTime
	var resolvedBy uuid.NullUUID
	var agentID, runID uuid.NullUUID
	if err := s.Scan(&p.Id, &p.RequestedBy, &p.SurfaceType, &p.SurfaceID, &p.ToolName,
		&paramsRaw, &p.Description, &p.Status, &idem, &result, &errStr,
		&p.ExpiresAt, &p.CreatedAt, &resolvedAt, &resolvedBy, &agentID, &runID); err != nil {
		return nil, err
	}
	if agentID.Valid {
		id := agentID.UUID
		p.AgentID = &id
	}
	if runID.Valid {
		id := runID.UUID
		p.RunID = &id
	}
	p.Params = map[string]string{}
	if len(paramsRaw) > 0 {
		// Best-effort: a malformed params blob should not break the read path;
		// the action just surfaces with no params (and would fail validation
		// at approval time, which is the safe outcome).
		_ = json.Unmarshal(paramsRaw, &p.Params)
	}
	if idem.Valid {
		p.IdempotencyKey = &idem.String
	}
	if result.Valid {
		p.Result = &result.String
	}
	if errStr.Valid {
		p.Error = &errStr.String
	}
	if resolvedAt.Valid {
		p.ResolvedAt = &resolvedAt.Time
	}
	if resolvedBy.Valid {
		id := resolvedBy.UUID
		p.ResolvedBy = &id
	}
	return &p, nil
}

// CreatePendingAction inserts a new pending action and returns it. When
// idempotencyKey is non-empty and a row with that key already exists, the
// existing row is returned instead of creating a duplicate (so a retried
// generation does not stack two cards for the same proposed write).
func CreatePendingAction(ctx context.Context, requestedBy uuid.UUID, surfaceType, surfaceID, toolName string, params map[string]string, description, idempotencyKey string, expiresAt time.Time, attr Attribution) (*PendingAction, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if params == nil {
		params = map[string]string{}
	}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/CreatePendingAction marshal params err: %+v", err)
		return nil, err
	}

	var idem *string
	if idempotencyKey != "" {
		idem = &idempotencyKey
	}

	id := uuid.New()
	const q = `INSERT INTO ai_pending_actions
		(id, requested_by, surface_type, surface_id, tool_name, params, description, idempotency_key, expires_at, agent_id, run_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING
		RETURNING ` + columns
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, requestedBy, surfaceType, surfaceID, toolName, paramsRaw, description, idem, expiresAt, attr.AgentID, attr.RunID)
	p, err := scanAction(row)
	if errors.Is(err, sql.ErrNoRows) {
		// A row with this idempotency key already exists (the INSERT was a
		// no-op). Return the existing one.
		if idem != nil {
			return GetByIdempotencyKey(ctx, idempotencyKey)
		}
		return nil, err
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/CreatePendingAction err: %+v", err)
		return nil, err
	}
	p.Fresh = true
	return p, nil
}

// GetByID returns an action by id regardless of state, or (nil, nil) if none.
func GetByID(ctx context.Context, id uuid.UUID) (*PendingAction, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + columns + ` FROM ai_pending_actions WHERE id=$1`
	p, err := scanAction(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/GetByID err: %+v", err)
		return nil, err
	}
	return p, nil
}

// GetByIdempotencyKey returns the action with the given idempotency key, or
// (nil, nil) if none.
func GetByIdempotencyKey(ctx context.Context, key string) (*PendingAction, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + columns + ` FROM ai_pending_actions WHERE idempotency_key=$1`
	p, err := scanAction(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, key))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/GetByIdempotencyKey err: %+v", err)
		return nil, err
	}
	return p, nil
}

// ListOpenByUser returns a user's open (pending, not yet expired) actions,
// newest first. Used for reconcile-on-load so a card survives tab close.
func ListOpenByUser(ctx context.Context, userUUID uuid.UUID) ([]*PendingAction, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + columns + ` FROM ai_pending_actions
		WHERE requested_by=$1 AND status='pending' AND expires_at > NOW()
		ORDER BY created_at DESC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, userUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/ListOpenByUser err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*PendingAction
	for rows.Next() {
		p, scanErr := scanAction(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// TransitionToExecuting atomically claims a pending action for execution. It is
// the idempotency guard: the UPDATE only matches a row still in 'pending', so
// of two concurrent approvals exactly one observes rowsAffected==1 (and runs
// the action); the loser observes 0 and must not execute.
//
// It also enforces the TTL: a row whose expires_at has passed is NOT claimed
// (so an expired card can never execute). The caller should treat a 0-row
// result as "already handled or expired" and reconcile, never as an error.
// ClaimIdempotencyKey reserves a key for a write that is about to run, in ONE atomic
// insert that lands directly in the 'executing' state.
//
// FOR ADDITIVE WRITES THAT NEED NO APPROVAL. An MCP client may retry a call whose
// response was lost, so a write that is not naturally idempotent has to be recognised
// on its second arrival. The approval flow gets that for free — it already persists a
// row keyed on the idempotency key — but a write that proceeds without asking anyone
// had no row, so nothing recognised the retry.
//
// WHY NOT CreatePendingAction THEN TransitionToExecuting. Two reasons, both about the
// window between them. The row would sit in 'pending' where the open-actions query
// would find it, so a client polling at that instant would render an approval card for
// something nobody needs to approve. And two concurrent retries could both insert-then-
// transition with only the transition to separate them, which is a narrower race than
// letting the unique index decide. One insert in the terminal-ish state has neither
// problem: the partial unique index on idempotency_key is the arbiter, and the loser of
// the race gets a conflict it can interpret.
//
// Returns (nil, nil) when the key is already claimed, so the caller reads the existing
// row and decides what the earlier call became. A conflict is an expected outcome here,
// not an error.
func ClaimIdempotencyKey(ctx context.Context, requestedBy uuid.UUID, surfaceType, surfaceID, toolName string, params map[string]string, description, idempotencyKey string, expiresAt time.Time) (*PendingAction, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if params == nil {
		params = map[string]string{}
	}
	if strings.TrimSpace(idempotencyKey) == "" {
		// Without a key there is nothing to claim, and a NULL key is exempt from the
		// unique index — so proceeding would insert an unclaimable row on every call.
		return nil, errors.New("models/PendingAction: a claim needs an idempotency key")
	}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/ClaimIdempotencyKey marshal params err: %+v", err)
		return nil, err
	}

	id := uuid.New()
	// ON CONFLICT DO NOTHING against the partial unique index, so a duplicate returns
	// no row rather than an error the caller would have to pattern-match on a driver
	// message.
	const q = `INSERT INTO ai_pending_actions
		(id, requested_by, surface_type, surface_id, tool_name, params, description, status, idempotency_key, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'executing',$8,$9)
		ON CONFLICT (idempotency_key) WHERE idempotency_key IS NOT NULL DO NOTHING`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, requestedBy, surfaceType, surfaceID,
		toolName, paramsRaw, description, idempotencyKey, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/ClaimIdempotencyKey err: %+v", err)
		return nil, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// Already claimed by an earlier arrival of the same call.
		return nil, nil
	}
	return GetByID(ctx, id)
}

// RetakeIdempotencyKey re-claims a key whose previous attempt ended without taking
// effect, moving that row back to 'executing' for a fresh attempt.
//
// WHY THIS IS NEEDED, and it is not an optimisation. A settled row KEEPS its idempotency
// key: the unique index is on the key, not on the key plus a status. So once an attempt
// ends as 'failed' — or is reclaimed as 'failed' after a crash — every later retry of that
// exact call conflicts on the insert and finds a row. Reading that row says "no prior call
// took effect", which is true, but the insert can never succeed again.
//
// Without this, a single transient failure made that exact call permanently
// unrepeatable, and the "a failed write frees the key" property was false. Nothing in the
// Go code was wrong to look at; the behaviour only shows up when the statements actually
// run against the index.
//
// GUARDED ON THE NON-APPLIED TERMINAL STATES, and the exclusions matter as much as the
// inclusions:
//
//	failed / expired  -> retakeable. The work did not happen; asking again is correct.
//	executing         -> NOT retakeable. Someone is doing it right now.
//	executed          -> NOT retakeable. It happened; a retry must be told so.
//	rejected          -> NOT retakeable. A HUMAN said no, and a retry must not
//	                     quietly convert that into another attempt.
//	pending           -> NOT retakeable. It is waiting on a person.
//
// created_at IS RESET, which is easy to miss and would be a live bug. The
// abandoned-execution reclaimer selects on created_at, so retaking a row that was created
// two hours ago without moving the timestamp would let the reclaimer free it again
// immediately — mid-execution. created_at means "when this attempt was claimed", which is
// exactly what the reclaimer needs to measure.
//
// Returns (nil, nil) when the row was not retakeable, so the caller reads the existing
// state and answers from it. The UPDATE is the serialization point: two concurrent retries
// both attempt it, one affects a row and owns the attempt, the other affects none and is
// answered as a duplicate.
func RetakeIdempotencyKey(ctx context.Context, requestedBy uuid.UUID, surfaceType, surfaceID, toolName string, params map[string]string, description, idempotencyKey string, expiresAt time.Time) (*PendingAction, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(idempotencyKey) == "" {
		return nil, errors.New("models/PendingAction: a retake needs an idempotency key")
	}
	if params == nil {
		params = map[string]string{}
	}
	paramsRaw, err := json.Marshal(params)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/RetakeIdempotencyKey marshal params err: %+v", err)
		return nil, err
	}

	// result / error / resolved_* are cleared: they describe the attempt that failed, and
	// leaving them would make a successful retry look like it had also failed.
	const q = `UPDATE ai_pending_actions
		SET status='executing',
		    requested_by=$2, surface_type=$3, surface_id=$4, tool_name=$5,
		    params=$6, description=$7, expires_at=$8,
		    created_at=NOW(), result=NULL, error=NULL, resolved_at=NULL, resolved_by=NULL
		WHERE idempotency_key=$1 AND status IN ('failed','expired')
		RETURNING id`
	var id uuid.UUID
	err = postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, idempotencyKey, requestedBy,
		surfaceType, surfaceID, toolName, paramsRaw, description, expiresAt).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// Not retakeable: applied, in flight, rejected, or awaiting a person.
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/RetakeIdempotencyKey err: %+v", err)
		return nil, err
	}
	return GetByID(ctx, id)
}

func TransitionToExecuting(ctx context.Context, id uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_pending_actions
		SET status='executing'
		WHERE id=$1 AND status='pending' AND expires_at > NOW()`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/TransitionToExecuting err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Finalize records the terminal outcome of an executed action (executed or
// failed) along with its result/error and the approver. Guarded on the
// 'executing' state so it only ever finalizes a row this server claimed.
func Finalize(ctx context.Context, id uuid.UUID, status, result, errMsg string, resolvedBy uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	var resultVal, errVal *string
	if result != "" {
		resultVal = &result
	}
	if errMsg != "" {
		errVal = &errMsg
	}
	const q = `UPDATE ai_pending_actions
		SET status=$2, result=$3, error=$4, resolved_at=NOW(), resolved_by=$5
		WHERE id=$1 AND status='executing'`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, status, resultVal, errVal, resolvedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/Finalize err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// Reject denies a pending action (human said no). Guarded on the 'pending'
// state and idempotent: a second deny on an already-rejected row is a no-op
// (returns sql.ErrNoRows so the caller can reconcile).
func Reject(ctx context.Context, id uuid.UUID, resolvedBy uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_pending_actions
		SET status='rejected', resolved_at=NOW(), resolved_by=$2
		WHERE id=$1 AND status='pending'`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, resolvedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/Reject err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ExpireStale marks all overdue pending actions as expired (best-effort
// housekeeping; the open-list query already filters by expires_at, so this is
// only to keep terminal state tidy for audit). Returns the number expired.
func ExpireStale(ctx context.Context) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_pending_actions SET status='expired', resolved_at=NOW()
		WHERE status='pending' AND expires_at <= NOW()`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/ExpireStale err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// staleExecutingGrace is how long an 'executing' row may sit untouched before it is
// treated as abandoned. Generously longer than any tool call, which is bounded by the
// request's own timeout, so a slow-but-alive execution is never reclaimed.
const staleExecutingGrace = 30 * time.Minute

// ReclaimAbandonedExecutions frees rows stuck in 'executing' because the process that
// claimed them died.
//
// THE FAILURE THIS PREVENTS IS SILENT AND PERMANENT. 'executing' is read as "already
// applied" — deliberately, so a retry arriving mid-execution does not run the write a
// second time. But nothing moved a row OUT of that state except the process that
// claimed it, and ExpireStale only ever looked at 'pending'. So a server killed
// between claiming and settling left the row executing forever, and from then on every
// retry of that exact call was answered "already applied" when it had never happened.
// The caller is told the write succeeded and it never did.
//
// This is not specific to MCP: the in-app approval path claims the same way, so an
// approved action whose executor crashed was stuck identically. One reclaimer fixes
// both, which is why it lives here rather than beside the caller that noticed.
//
// FAILED RATHER THAN EXECUTED, and the direction is a real choice. After a crash we
// genuinely cannot know whether the write landed. 'failed' frees the key so a retry may
// try again, which risks a duplicate in the narrow case where the write DID land and
// only the settle was lost. Leaving it claimed guarantees the opposite: the write never
// happens and every retry is told it did. A duplicate is visible in the channel or the
// table and a person can undo it; a write that silently never happened is invisible and
// nobody knows to look. So this fails towards the recoverable outcome.
//
// The error column records why, so a duplicate found later can be traced to the crash
// that caused it rather than looking like the deduplication simply not working.
func ReclaimAbandonedExecutions(ctx context.Context) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE ai_pending_actions
		SET status='failed', resolved_at=NOW(),
		    error='execution abandoned: the server that claimed this action did not finish it'
		WHERE status='executing' AND created_at <= NOW() - $1::interval`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, staleExecutingGrace.String())
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/ReclaimAbandonedExecutions err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// AgentOutcomeCounts is how an agent's proposals were resolved by the people
// they were shown to.
//
// The statuses collapse into three different questions, which is why they are
// kept apart rather than reduced to one number in SQL:
//
//   - Approved (executed + failed) is "a person wanted this to happen".
//     A failure AFTER approval is an execution bug, not a rejected proposal,
//     so folding it into Rejected would blame the agent for the wrong thing.
//   - Rejected is "a person looked at it and said no".
//   - Expired is nobody deciding at all before the TTL, which is its own
//     signal and often a worse one: an agent people ignore is not an agent
//     people disagree with.
type AgentOutcomeCounts struct {
	Approved int `json:"approved"`
	Rejected int `json:"rejected"`
	Expired  int `json:"expired"`
	Pending  int `json:"pending"`
	// Failed is the subset of Approved whose execution then failed. Reported so
	// a caller can tell "people want this and it breaks" from "people want this".
	Failed int `json:"failed"`
}

// AgentOutcomeCountsBatch returns, per agent id, how that agent's proposals
// were resolved.
//
// One grouped query for every agent rather than a query per agent, because the
// agents list renders this for the whole page at once and the per-agent shape
// is the same read with a narrower filter.
func AgentOutcomeCountsBatch(ctx context.Context, agentIDs []uuid.UUID) (map[string]*AgentOutcomeCounts, error) {
	out := map[string]*AgentOutcomeCounts{}
	if len(agentIDs) == 0 {
		return out, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		SELECT agent_id, status, COUNT(*)
		FROM ai_pending_actions
		WHERE agent_id = ANY($1)
		GROUP BY agent_id, status`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, pq.Array(agentIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/AgentOutcomeCountsBatch err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var agentID uuid.UUID
		var status string
		var n int
		if err := rows.Scan(&agentID, &status, &n); err != nil {
			helpers.LogErrorWithContext(ctx, "models/PendingAction/AgentOutcomeCountsBatch scan err: %+v", err)
			return nil, err
		}
		key := agentID.String()
		c, ok := out[key]
		if !ok {
			c = &AgentOutcomeCounts{}
			out[key] = c
		}
		switch status {
		case StatusExecuted:
			c.Approved += n
		case StatusFailed:
			c.Approved += n
			c.Failed += n
		case StatusRejected:
			c.Rejected += n
		case StatusExpired:
			c.Expired += n
		case StatusPending, StatusExecuting:
			c.Pending += n
		}
	}
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/PendingAction/AgentOutcomeCountsBatch rows err: %+v", err)
		return nil, err
	}
	return out, nil
}
