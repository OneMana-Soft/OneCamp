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
	"github.com/akashc777/OneCamp/helpers"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
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

// Insert adds rs as held in one statement, inside ex (the change's
// transaction), and answers their ids in rs's order. The rows are inserted in
// that order (WITH ORDINALITY), so their ids rise with it and a key's events
// in rs run oldest first. Each row's sinks go as a JSON list, since unnest
// flattens an array of arrays.
func Insert(ctx context.Context, ex Execer, rs []Row) ([]int64, error) {
	n := len(rs)
	eventIDs, topics, kinds, keys := make([]string, n), make([]string, n), make([]string, n), make([]string, n)
	entities, actors, sinks := make([]string, n), make([]string, n), make([]string, n)
	for i, r := range rs {
		e := []byte("{}")
		if r.Entities != nil {
			var err error
			if e, err = json.Marshal(r.Entities); err != nil {
				return nil, err
			}
		}
		s, err := json.Marshal(helpers.NonNil(r.Sinks))
		if err != nil {
			return nil, err
		}
		eventIDs[i], topics[i], kinds[i], keys[i], entities[i], sinks[i] = r.EventID.String(), r.Topic, r.Kind, r.Key, string(e), string(s)
		if r.Actor != nil {
			actors[i] = r.Actor.String()
		}
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := ex.QueryContext(cctx, `
		INSERT INTO outbox (event_id, topic, kind, key, entities, actor, sinks, state, created_at, available_at)
		SELECT u.event_id, u.topic, u.kind, u.key, u.entities::jsonb, NULLIF(u.actor, '')::uuid,
			ARRAY(SELECT jsonb_array_elements_text(u.sinks::jsonb)), 'held', NOW(), NOW()
		FROM unnest($1::uuid[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::text[])
			WITH ORDINALITY AS u(event_id, topic, kind, key, entities, actor, sinks, ord)
		ORDER BY u.ord
		RETURNING id, event_id`,
		pq.Array(eventIDs), pq.Array(topics), pq.Array(kinds), pq.Array(keys), pq.Array(entities), pq.Array(actors), pq.Array(sinks))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// RETURNING keeps no order, so each id is matched to its row by event id.
	idOf := make(map[uuid.UUID]int64, n)
	for rows.Next() {
		var id int64
		var ev uuid.UUID
		if err := rows.Scan(&id, &ev); err != nil {
			return nil, err
		}
		idOf[ev] = id
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, n)
	for i, r := range rs {
		ids[i] = idOf[r.EventID]
	}
	return ids, nil
}

// Release makes the held rows of ids ready, in one statement.
func Release(ctx context.Context, ids []int64) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `
		UPDATE outbox SET state = 'ready', available_at = NOW() WHERE id = ANY($1) AND state = 'held'`, pq.Array(ids))
	return err
}

// ReleaseWith makes a held row ready with more entities merged into its own,
// answering whether it was held.
func ReleaseWith(ctx context.Context, id int64, more map[string]string) (bool, error) {
	raw, err := json.Marshal(more)
	if err != nil {
		return false, err
	}
	n, err := pgexec.Exec(ctx, `
		UPDATE outbox SET state = 'ready', available_at = NOW(), entities = entities || $2::jsonb
		WHERE id = $1 AND state = 'held'`, id, raw)
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

// Claim claims for by the rows of ids that are claimable, in one statement, as
// the writer does for the rows it just released. One isn't taken when another
// drain has it, it isn't due, or an earlier row of its key is due.
func Claim(ctx context.Context, ids []int64, by string, lease time.Duration) ([]Row, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		UPDATE outbox o SET claimed_by = $2, claimed_until = NOW() + make_interval(secs => $3), attempts = o.attempts + 1
		WHERE o.id = ANY($1) AND `+claimable+`
		RETURNING `+rowColumns, pq.Array(ids), by, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
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
		WHERE id = $1 AND claimed_by = $2`, id, by, pq.Array(helpers.NonNil(done)))
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
		WHERE id = $1 AND claimed_by = $2`, id, by, pq.Array(helpers.NonNil(done)), at, lastErr, dead)
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
