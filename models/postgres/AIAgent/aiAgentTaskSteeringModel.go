package models

// Mid-run steering for a durable agent job (migration 135).
//
// A reply that arrived while an agent was working used to be dropped: the
// continuation path saw a 'running' job, suppressed a duplicate launch, and the
// message went nowhere. So "use the other repo" or "don't touch prod" landed only
// after the agent had already done it the wrong way, and the only remedy was to
// stop the run and start over.
//
// Steering is an append-only inbox on the job that the LEASE HOLDER drains. Two
// writers, two disciplines:
//   - humans APPEND (any number of repliers, no lease, no coordination);
//   - the worker running the job DRAINS in one statement and folds the messages
//     into the live conversation between steps.
// It is deliberately separate from the `messages` conversation, which a live run
// owns in memory and rewrites on every checkpoint — appending a turn there would
// simply be overwritten.

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
)

const (
	// steeringMaxEntries bounds the inbox so a comment burst can't inflate the
	// row or the next prompt. Oldest entries are dropped first: a later
	// instruction supersedes an earlier one.
	steeringMaxEntries = 10
	// steeringMaxChars bounds ONE message. A human steering a run writes a
	// sentence, not a document; a pasted wall of text would crowd out the
	// conversation it is meant to adjust.
	steeringMaxChars = 1000
)

// SteeringMessage is one human instruction handed to a job while it works.
type SteeringMessage struct {
	At   time.Time  `json:"at"`
	By   *uuid.UUID `json:"by,omitempty"`
	Text string     `json:"text"`
}

// AppendAgentTaskSteering adds a human instruction to an OPEN, not-yet-terminal
// job's inbox, for the worker to fold into the run.
//
// Only 'queued' and 'running' jobs accept steering, and only when no stop is
// pending: a parked job resumes through ResumeAgentTaskWithFollowup (which
// appends a real conversation turn), and a job somebody is stopping should not be
// given new work. Returns false when the job was in no state to be steered, so
// the caller can fall back to its normal handling rather than silently swallowing
// the message.
//
// The inbox is trimmed to the newest steeringMaxEntries inside the same statement,
// so concurrent repliers can never grow it without bound.
func AppendAgentTaskSteering(ctx context.Context, id uuid.UUID, by *uuid.UUID, text string) (bool, error) {
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		return false, nil
	}
	if len([]rune(text)) > steeringMaxChars {
		text = string([]rune(text)[:steeringMaxChars-1]) + "…"
	}
	entry, merr := json.Marshal([]SteeringMessage{{At: time.Now().UTC(), By: by, Text: text}})
	if merr != nil {
		return false, merr
	}

	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// The trim keeps the NEWEST entries: jsonb array slicing isn't available, so
	// the tail is rebuilt from the concatenated array with a bounded offset.
	const q = `WITH merged AS (
			SELECT id, (steering || $2::jsonb) AS all_msgs FROM ai_agent_tasks
			WHERE id=$1 AND state IN ('queued','running') AND cancel_requested_at IS NULL
		), trimmed AS (
			SELECT m.id,
			       COALESCE((
			         SELECT jsonb_agg(e ORDER BY ord)
			         FROM (
			           SELECT e, ord FROM jsonb_array_elements(m.all_msgs) WITH ORDINALITY AS t(e, ord)
			           ORDER BY ord DESC LIMIT $3
			         ) newest
			       ), '[]'::jsonb) AS kept
			FROM merged m
		)
		UPDATE ai_agent_tasks t SET steering = trimmed.kept, updated_at = now()
		FROM trimmed WHERE t.id = trimmed.id`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, string(entry), steeringMaxEntries)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AppendAgentTaskSteering err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// DrainAgentTaskSteering atomically takes and clears the steering messages for a
// job THIS worker leases, returning them oldest-first.
//
// Take-and-clear in one statement is what makes the messages exactly-once from
// the run's point of view: a crash after the drain loses the instruction (the
// human can repeat it), whereas leaving it in place would re-inject the same
// instruction on every step. Lease-guarded, so only the worker actually running
// the job can consume its inbox.
func DrainAgentTaskSteering(ctx context.Context, id, leaseToken uuid.UUID) ([]SteeringMessage, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `WITH cur AS (
			SELECT id, steering FROM ai_agent_tasks
			WHERE id=$1 AND lease_token=$2 AND jsonb_array_length(steering) > 0
			FOR UPDATE
		), cleared AS (
			UPDATE ai_agent_tasks t SET steering='[]'::jsonb FROM cur WHERE t.id = cur.id
		)
		SELECT steering FROM cur`
	var raw []byte
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id, leaseToken).Scan(&raw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil // nothing waiting (the common case)
		}
		helpers.LogErrorWithContext(ctx, "models/DrainAgentTaskSteering err: %+v", err)
		return nil, err
	}
	var msgs []SteeringMessage
	if err := json.Unmarshal(raw, &msgs); err != nil {
		// A malformed inbox has already been cleared, so it can't wedge the run.
		helpers.LogErrorWithContext(ctx, "models/DrainAgentTaskSteering decode err: %+v", err)
		return nil, nil
	}
	return msgs, nil
}
