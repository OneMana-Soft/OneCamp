package models

// An AI column's fill (migration 509): the request that starts it stores it,
// with the change set its writes go into, and each background turn reads it
// first and moves it on after (business/DataTable aiFillJob). One fill runs
// per column at a time (idx_data_table_ai_fills_running).

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// The states of a fill.
const (
	AIFillRunning = "running"
	AIFillStopped = "stopped"
	AIFillDone    = "done"
)

// AIFill is an AI column's fill: Total cells to take, of which Asked were
// asked of the model and Filled written, the last row it reached (its
// cursor, in the table's order), and its State.
type AIFill struct {
	ID        uuid.UUID  `json:"id"`
	TableID   uuid.UUID  `json:"table_id"`
	FieldID   uuid.UUID  `json:"field_id"`
	StartedBy *uuid.UUID `json:"started_by,omitempty"`
	ChangeSet *uuid.UUID `json:"change_set,omitempty"`
	Total     int        `json:"total"`
	Asked     int        `json:"asked"`
	Filled    int        `json:"filled"`
	CursorKey string     `json:"-"`
	CursorID  uuid.UUID  `json:"-"`
	State     string     `json:"state"`
	StoppedBy *uuid.UUID `json:"stopped_by,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	UpdatedAt time.Time  `json:"updated_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
}

// Left is how many of the fill's cells are still to be asked for.
func (f *AIFill) Left() int { return max(f.Total-f.Asked, 0) }

// ErrAIFillRunning is a fill started while one runs for the column.
var ErrAIFillRunning = errors.New("a fill of this column is running")

// aiFillStaleSQL is how long a running fill may go without moving on before
// it no longer holds its column: its turns stopped coming (an outbox event
// tried until it was dead, say), so it is stopped when the next fill starts,
// and a grid no longer shows it running.
const aiFillStaleSQL = `INTERVAL '1 hour'`

const aiFillColumns = `id, table_id, field_id, started_by, change_set, total, asked, filled,
	COALESCE(cursor_key, ''), cursor_id, state, stopped_by, started_at, updated_at, ended_at`

func scanAIFill(r pgexec.Row) (*AIFill, error) {
	var f AIFill
	var startedBy, changeSet, cursorID, stoppedBy uuid.NullUUID
	var ended sql.NullTime
	if err := r.Scan(&f.ID, &f.TableID, &f.FieldID, &startedBy, &changeSet, &f.Total, &f.Asked, &f.Filled,
		&f.CursorKey, &cursorID, &f.State, &stoppedBy, &f.StartedAt, &f.UpdatedAt, &ended); err != nil {
		return nil, err
	}
	if startedBy.Valid {
		f.StartedBy = &startedBy.UUID
	}
	if changeSet.Valid {
		f.ChangeSet = &changeSet.UUID
	}
	if cursorID.Valid {
		f.CursorID = cursorID.UUID
	}
	if stoppedBy.Valid {
		f.StoppedBy = &stoppedBy.UUID
	}
	if ended.Valid {
		f.EndedAt = &ended.Time
	}
	return &f, nil
}

// StartAIFill stores a fill of total cells of field, and cs, the empty change
// set its writes go into, in one transaction. A fill running for the column
// already is ErrAIFillRunning, with that fill.
func StartAIFill(ctx context.Context, table, field uuid.UUID, by uuid.UUID, total int, cs *ChangeSet) (*AIFill, error) {
	var out *AIFill
	err := pgexec.InTx(ctx, func(ctx context.Context) error {
		if _, err := pgexec.Exec(ctx, `UPDATE data_table_ai_fills SET state = 'stopped', ended_at = NOW()
			WHERE table_id = $1 AND field_id = $2 AND state = 'running' AND updated_at < NOW() - `+aiFillStaleSQL, table, field); err != nil {
			return err
		}
		if running, err := RunningAIFill(ctx, table, field); err != nil || running != nil {
			if running != nil {
				out = running
				return ErrAIFillRunning
			}
			return err
		}
		q, err := pgexec.Current(ctx)
		if err != nil {
			return err
		}
		tx, ok := q.(*sql.Tx)
		if !ok {
			return errors.New("models/StartAIFill: not in a transaction")
		}
		if err := recordEmpty(ctx, tx, cs); err != nil {
			return err
		}
		f, _, err := pgexec.QueryOne(ctx, `INSERT INTO data_table_ai_fills (id, table_id, field_id, started_by, change_set, total)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+aiFillColumns,
			scanAIFill, uuid.New(), table, field, nullUUID(by), cs.ID, total)
		out = f
		return err
	})
	if helpers.IsUniqueViolation(err) { // another request started one meanwhile
		if running, rerr := RunningAIFill(ctx, table, field); rerr == nil && running != nil {
			return running, ErrAIFillRunning
		}
	}
	if err != nil && !errors.Is(err, ErrAIFillRunning) {
		helpers.LogErrorWithContext(ctx, "models/StartAIFill err: %+v", err)
	}
	return out, err
}

// GetAIFill is a table's fill by id; nil when there's none.
func GetAIFill(ctx context.Context, table, id uuid.UUID) (*AIFill, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+aiFillColumns+` FROM data_table_ai_fills WHERE id = $1 AND table_id = $2`,
		scanAIFill, id, table)
	return f, err
}

// RunningAIFill is the fill running for a column; nil when there's none (one
// gone stale, aiFillStaleSQL, doesn't count).
func RunningAIFill(ctx context.Context, table, field uuid.UUID) (*AIFill, error) {
	f, _, err := pgexec.QueryOne(ctx, `SELECT `+aiFillColumns+` FROM data_table_ai_fills
		WHERE table_id = $1 AND field_id = $2 AND state = 'running' AND updated_at >= NOW() - `+aiFillStaleSQL, scanAIFill, table, field)
	return f, err
}

// RecentAIFills are a table's fills that run (and haven't gone stale), and
// those that ended in the last day, newest first: what a grid shows the
// progress of.
func RecentAIFills(ctx context.Context, table uuid.UUID) ([]*AIFill, error) {
	return pgexec.Query(ctx, `SELECT `+aiFillColumns+` FROM data_table_ai_fills
		WHERE table_id = $1 AND ((state = 'running' AND updated_at >= NOW() - `+aiFillStaleSQL+`) OR ended_at > NOW() - INTERVAL '1 day')
		ORDER BY started_at DESC LIMIT 20`, scanAIFill, table)
}

// AdvanceAIFill moves a running fill on by asked cells asked and filled
// written, to the row (key, id) it reached, and ends it (done) when done
// says, or when it has asked for all it takes. It answers the fill as it now
// is; a fill that isn't running is answered as it is, unmoved.
func AdvanceAIFill(ctx context.Context, table, id uuid.UUID, asked, filled int, key string, row uuid.UUID, done bool) (*AIFill, error) {
	f, ok, err := pgexec.QueryOne(ctx, `UPDATE data_table_ai_fills SET
			asked = asked + $3, filled = filled + $4,
			cursor_key = CASE WHEN $6::uuid IS NULL THEN cursor_key ELSE $5 END,
			cursor_id = COALESCE($6::uuid, cursor_id),
			state = CASE WHEN $7 OR asked + $3 >= total THEN 'done' ELSE state END,
			ended_at = CASE WHEN $7 OR asked + $3 >= total THEN NOW() ELSE ended_at END,
			updated_at = NOW()
		WHERE id = $1 AND table_id = $2 AND state = 'running'
		RETURNING `+aiFillColumns, scanAIFill, id, table, asked, filled, key, nullUUID(row), done)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AdvanceAIFill err: %+v", err)
		return nil, err
	}
	if !ok {
		return GetAIFill(ctx, table, id)
	}
	return f, nil
}

// StopAIFill stops a running fill, as by; a fill that isn't running is
// answered as it is.
func StopAIFill(ctx context.Context, table, id, by uuid.UUID) (*AIFill, error) {
	f, ok, err := pgexec.QueryOne(ctx, `UPDATE data_table_ai_fills SET state = 'stopped', stopped_by = $3,
			ended_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND table_id = $2 AND state = 'running' RETURNING `+aiFillColumns, scanAIFill, id, table, nullUUID(by))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/StopAIFill err: %+v", err)
		return nil, err
	}
	if !ok {
		return GetAIFill(ctx, table, id)
	}
	return f, nil
}

// StopAIFillOf stops the running fill whose writes are the change set cs, as
// by: an undo of a fill stops it first. It answers the fill it stopped, nil
// when none ran.
func StopAIFillOf(ctx context.Context, table, cs, by uuid.UUID) (*AIFill, error) {
	f, _, err := pgexec.QueryOne(ctx, `UPDATE data_table_ai_fills SET state = 'stopped', stopped_by = $3,
			ended_at = NOW(), updated_at = NOW()
		WHERE change_set = $1 AND table_id = $2 AND state = 'running' RETURNING `+aiFillColumns, scanAIFill, cs, table, nullUUID(by))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/StopAIFillOf err: %+v", err)
	}
	return f, err
}

// RowsToFillAfter is up to limit of a table's rows whose cell of field is
// empty and no person's (fillableSQL), in the table's order, after the row
// (key, id) a fill reached: a fill walks the table once, so a cell the model
// failed isn't asked for again by its next turn. With no row reached (id
// nil) it starts at the first. A row with no key yet (made by a server from
// before row keys, until the server's first look keys it: KeyRowsWithout)
// has no place to walk past, so a fill leaves it out: taken at every turn,
// its cell would be asked again each time the model failed it (CR-32's SF1).
// A fill after it has its key takes it.
func RowsToFillAfter(ctx context.Context, tableId uuid.UUID, field string, key string, id uuid.UUID, limit int) ([]*Row, error) {
	rows, err := pgexec.Query(ctx, `SELECT `+rowColumns+` FROM data_table_rows r
		WHERE r.table_id = $1 AND `+fillableSQL+`
			AND ($4::uuid IS NULL OR (r.position_key, r.id) > ($3::text COLLATE "C", $4::uuid))
		`+rowOrder+` LIMIT $5`,
		func(r pgexec.Row) (*Row, error) { return scanRow(r) }, tableId, field, key, nullUUID(id), limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/RowsToFillAfter err: %+v", err)
		return nil, err
	}
	return rows, nil
}

// StartChangeSet stores cs, with no changes yet: the change set a fill's
// writes are added to (ChangeSet.Append).
func StartChangeSet(ctx context.Context, cs *ChangeSet) error {
	return pgexec.InTx(ctx, func(ctx context.Context) error {
		q, err := pgexec.Current(ctx)
		if err != nil {
			return err
		}
		tx, ok := q.(*sql.Tx)
		if !ok {
			return errors.New("models/StartChangeSet: not in a transaction")
		}
		return recordEmpty(ctx, tx, cs)
	})
}
