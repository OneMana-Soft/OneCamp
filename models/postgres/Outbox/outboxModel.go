package models

// Postgres access for the outbox (migrations/*_outbox.up.sql): the events
// changes made, written beside the change and handed to their sinks by
// business/Outbox. This file only reads and writes rows; the rules (when a row
// is released, retried, given up on) are business/Outbox's.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The states a row is in.
const (
	StateHeld      = "held"
	StateReady     = "ready"
	StateDone      = "done"
	StateDead      = "dead"
	StateCancelled = "cancelled"
)

// Row is one outbox row.
type Row struct {
	ID        int64
	EventID   uuid.UUID
	Topic     string
	Kind      string
	Key       string
	Entities  map[string]string
	Actor     *uuid.UUID
	Sinks     []string
	State     string
	SinksDone []string
	Attempts  int
	CreatedAt time.Time
}

// Execer is a pool or a transaction: a row is added inside the change's own.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func db() Execer { return postgresInit.DBConn.SqlDB }

func timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

const rowColumns = `id, event_id, topic, kind, key, entities, actor, sinks, state, sinks_done, attempts, created_at`

func scanRow(sc interface{ Scan(...any) error }) (Row, error) {
	var r Row
	var entities []byte
	if err := sc.Scan(&r.ID, &r.EventID, &r.Topic, &r.Kind, &r.Key, &entities, &r.Actor, pq.Array(&r.Sinks), &r.State, pq.Array(&r.SinksDone), &r.Attempts, &r.CreatedAt); err != nil {
		return Row{}, err
	}
	if len(entities) > 0 {
		if err := json.Unmarshal(entities, &r.Entities); err != nil {
			return Row{}, err
		}
	}
	return r, nil
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	defer rows.Close()
	var out []Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Insert adds r as held, inside ex (the change's transaction), and answers its id.
func Insert(ctx context.Context, ex Execer, r Row) (int64, error) {
	entities, err := json.Marshal(r.Entities)
	if err != nil {
		return 0, err
	}
	if r.Entities == nil {
		entities = []byte("{}")
	}
	sinks := r.Sinks
	if sinks == nil {
		sinks = []string{}
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	var id int64
	err = ex.QueryRowContext(cctx, `
		INSERT INTO outbox (event_id, topic, kind, key, entities, actor, sinks, state, created_at, available_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, 'held', NOW(), NOW())
		RETURNING id`, r.EventID, r.Topic, r.Kind, r.Key, entities, r.Actor, pq.Array(sinks)).Scan(&id)
	return id, err
}

// Release makes a held row ready, answering whether it was held.
func Release(ctx context.Context, id int64) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE outbox SET state = 'ready', available_at = NOW() WHERE id = $1 AND state = 'held'`, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Cancel drops a row that hasn't run (held or ready), inside ex (nil is the
// pool): the change it was for was undone.
func Cancel(ctx context.Context, ex Execer, id int64) error {
	if ex == nil {
		ex = db()
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := ex.ExecContext(cctx, `
		UPDATE outbox SET state = 'cancelled', finished_at = NOW(), claimed_by = NULL, claimed_until = NULL
		WHERE id = $1 AND state IN ('held', 'ready')`, id)
	return err
}

// claimable is the condition a row is taken on: ready and due, held by no
// claim, and with no row of its key before it that is due, claimed or not (a
// claimed row was due when it was taken, and stays so until it is finished or
// put back), so a key's rows reach their sinks one at a time and oldest first,
// by the writer's delivery as by the drain. A row in backoff doesn't hold back
// the ones after it, nor does a held one: its writer hasn't finished.
const claimable = `o.state = 'ready' AND o.available_at <= NOW()
	AND (o.claimed_until IS NULL OR o.claimed_until < NOW())
	AND NOT EXISTS (
		SELECT 1 FROM outbox p
		WHERE p.key = o.key AND p.id < o.id AND p.state = 'ready' AND p.available_at <= NOW()
	)`

// ClaimDue claims up to limit due rows for by, at most one a key and the
// oldest of each, until lease runs out, and counts the attempt.
func ClaimDue(ctx context.Context, by string, lease time.Duration, limit int) ([]Row, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		WITH candidates AS (
			SELECT o.id, o.key FROM outbox o WHERE `+claimable+`
			ORDER BY o.id LIMIT $1
		), firsts AS (
			SELECT DISTINCT ON (key) id FROM candidates ORDER BY key, id
		), locked AS (
			SELECT o.id FROM outbox o WHERE o.id IN (SELECT id FROM firsts)
				AND o.state = 'ready' AND (o.claimed_until IS NULL OR o.claimed_until < NOW())
			FOR UPDATE SKIP LOCKED
		)
		UPDATE outbox SET claimed_by = $2, claimed_until = NOW() + make_interval(secs => $3), attempts = outbox.attempts + 1
		FROM locked WHERE outbox.id = locked.id
		RETURNING outbox.id, outbox.event_id, outbox.topic, outbox.kind, outbox.key, outbox.entities, outbox.actor,
			outbox.sinks, outbox.state, outbox.sinks_done, outbox.attempts, outbox.created_at`,
		limit, by, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// ClaimOne claims row id for by when it is claimable, as the writer does for
// the row it just released; ok is false when it isn't (another drain has it,
// it isn't due, or an earlier row of its key is running).
func ClaimOne(ctx context.Context, id int64, by string, lease time.Duration) (Row, bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	r, err := scanRow(db().QueryRowContext(cctx, `
		UPDATE outbox o SET claimed_by = $2, claimed_until = NOW() + make_interval(secs => $3), attempts = o.attempts + 1
		WHERE o.id = $1 AND `+claimable+`
		RETURNING `+rowColumns, id, by, lease.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, false, nil
	}
	return r, err == nil, err
}

// ClaimNextOfKey claims the oldest claimable row of key after id, so a drain
// that finished one row of a conversation goes straight on to its next.
func ClaimNextOfKey(ctx context.Context, key string, after int64, by string, lease time.Duration) (Row, bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	r, err := scanRow(db().QueryRowContext(cctx, `
		UPDATE outbox SET claimed_by = $3, claimed_until = NOW() + make_interval(secs => $4), attempts = outbox.attempts + 1
		WHERE outbox.id = (
			SELECT o.id FROM outbox o WHERE o.key = $1 AND o.id > $2 AND `+claimable+`
			ORDER BY o.id LIMIT 1 FOR UPDATE SKIP LOCKED
		)
		RETURNING `+rowColumns, key, after, by, lease.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, false, nil
	}
	return r, err == nil, err
}

// Finish marks a row by claimed as done, with the sinks that had it. false
// when by no longer holds it (its lease ran out and another drain took it).
func Finish(ctx context.Context, id int64, by string, done []string) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE outbox SET state = 'done', sinks_done = $3, claimed_by = NULL, claimed_until = NULL, finished_at = NOW(), last_error = NULL
		WHERE id = $1 AND claimed_by = $2`, id, by, pq.Array(nonNil(done)))
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Retry hands a row by claimed back with the sinks that had it, due again at
// at, or as dead when the drain gives up on it.
func Retry(ctx context.Context, id int64, by string, done []string, at time.Time, lastErr string, dead bool) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE outbox SET sinks_done = $3, claimed_by = NULL, claimed_until = NULL, available_at = $4, last_error = $5,
			state = CASE WHEN $6::boolean THEN 'dead' ELSE 'ready' END,
			finished_at = CASE WHEN $6::boolean THEN NOW() ELSE NULL END
		WHERE id = $1 AND claimed_by = $2`, id, by, pq.Array(nonNil(done)), at, lastErr, dead)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// StaleHeld is up to limit rows held longer than olderThan: their writer never
// released them, and their topic decides.
func StaleHeld(ctx context.Context, olderThan time.Duration, limit int) ([]Row, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT `+rowColumns+` FROM outbox
		WHERE state = 'held' AND created_at < NOW() - make_interval(secs => $1)
		ORDER BY id LIMIT $2`, olderThan.Seconds(), limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// Purge deletes up to limit rows finished before their keep: done and
// cancelled ones before done, dead ones before dead. It answers how many went.
func Purge(ctx context.Context, doneBefore, deadBefore time.Time, limit int) (int64, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		DELETE FROM outbox WHERE id IN (
			SELECT id FROM outbox
			WHERE (state IN ('done', 'cancelled') AND finished_at < $1) OR (state = 'dead' AND finished_at < $2)
			LIMIT $3
		)`, doneBefore, deadBefore, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats is how the outbox stands now.
type Stats struct {
	// Pending is held and ready rows; Held the held ones of them.
	Pending int64
	Held    int64
	// OldestPending is how long the oldest pending row has waited.
	OldestPending time.Duration
	// Dead is the rows the drain gave up on, still kept; LastDead the newest's error.
	Dead     int64
	LastDead string
}

// ReadStats reads the outbox's state for the system check.
func ReadStats(ctx context.Context) (Stats, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var s Stats
	var oldest float64
	err := db().QueryRowContext(cctx, `
		SELECT
			COUNT(*) FILTER (WHERE state IN ('held', 'ready')),
			COUNT(*) FILTER (WHERE state = 'held'),
			COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(created_at) FILTER (WHERE state IN ('held', 'ready'))), 0),
			COUNT(*) FILTER (WHERE state = 'dead'),
			COALESCE((SELECT last_error FROM outbox WHERE state = 'dead' ORDER BY finished_at DESC LIMIT 1), '')
		FROM outbox WHERE state IN ('held', 'ready', 'dead')`).Scan(&s.Pending, &s.Held, &oldest, &s.Dead, &s.LastDead)
	s.OldestPending = time.Duration(oldest * float64(time.Second))
	return s, err
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
