package models

// Postgres access for webhook deliveries (migrations/*_webhook_deliveries.up.sql):
// one row per (webhook, event), written before anything is sent and sent by
// business/Hooks. This file only reads and writes rows; when a row is tried
// again, given up on or paused is business/Hooks's.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// The states a delivery is in.
const (
	StatePending   = "pending"
	StateDone      = "done"
	StateDead      = "dead"
	StateCancelled = "cancelled"
)

// Row is one delivery.
type Row struct {
	ID         int64      `json:"id"`
	WebhookID  uuid.UUID  `json:"webhook_id"`
	EventID    uuid.UUID  `json:"event_id"`
	EventType  string     `json:"event_type"`
	Payload    []byte     `json:"-"`
	State      string     `json:"state"`
	Attempts   int        `json:"attempts"`
	NextAt     time.Time  `json:"next_at"`
	LastStatus *int       `json:"last_status,omitempty"`
	LastError  *string    `json:"last_error,omitempty"`
	LastMs     *int       `json:"last_ms,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// New is a delivery to write: the event's id and kind, and the body this
// webhook is sent.
type New struct {
	WebhookID uuid.UUID
	EventID   uuid.UUID
	EventType string
	Payload   []byte
}

func db() *sql.DB { return postgresInit.DBConn.SqlDB }

func timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

const columns = `id, webhook_id, event_id, event_type, payload, state, attempts, next_at, last_status, last_error, last_ms, created_at, finished_at`

func scanRow(sc interface{ Scan(...any) error }) (Row, error) {
	var r Row
	err := sc.Scan(&r.ID, &r.WebhookID, &r.EventID, &r.EventType, &r.Payload, &r.State, &r.Attempts, &r.NextAt, &r.LastStatus, &r.LastError, &r.LastMs, &r.CreatedAt, &r.FinishedAt)
	return r, err
}

func scanRows(rows *sql.Rows) ([]Row, error) {
	defer rows.Close()
	out := []Row{}
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Insert writes the deliveries, skipping any whose (webhook, event) is
// written already, and answers the ids of those it wrote.
func Insert(ctx context.Context, rows []New) ([]int64, error) {
	if len(rows) == 0 {
		return nil, nil
	}
	webhooks := make([]string, len(rows))
	events := make([]string, len(rows))
	kinds := make([]string, len(rows))
	payloads := make([]string, len(rows))
	for i, r := range rows {
		webhooks[i], events[i], kinds[i], payloads[i] = r.WebhookID.String(), r.EventID.String(), r.EventType, string(r.Payload)
	}
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().QueryContext(cctx, `
		INSERT INTO webhook_deliveries (webhook_id, event_id, event_type, payload)
		SELECT w::uuid, e::uuid, k, p::jsonb FROM unnest($1::text[], $2::text[], $3::text[], $4::text[]) AS t(w, e, k, p)
		ON CONFLICT (webhook_id, event_id) DO NOTHING
		RETURNING id`, pq.Array(webhooks), pq.Array(events), pq.Array(kinds), pq.Array(payloads))
	if err != nil {
		return nil, err
	}
	defer res.Close()
	var ids []int64
	for res.Next() {
		var id int64
		if err := res.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, res.Err()
}

// claimable is a pending row that is due, held by nobody, of a webhook that
// isn't paused.
const claimable = `d.state = 'pending' AND d.next_at <= NOW() AND (d.claimed_until IS NULL OR d.claimed_until < NOW())
	AND NOT EXISTS (SELECT 1 FROM webhooks w WHERE w.id = d.webhook_id AND w.paused_at IS NOT NULL)`

// ClaimDue claims up to limit due rows for by, for lease, counting the try.
func ClaimDue(ctx context.Context, by string, lease time.Duration, limit int) ([]Row, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		WITH due AS (
			SELECT d.id FROM webhook_deliveries d WHERE `+claimable+`
			ORDER BY d.next_at LIMIT $1
			FOR UPDATE OF d SKIP LOCKED
		)
		UPDATE webhook_deliveries SET claimed_by = $2, claimed_until = NOW() + make_interval(secs => $3), attempts = webhook_deliveries.attempts + 1
		FROM due WHERE webhook_deliveries.id = due.id
		RETURNING `+qualified, limit, by, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// qualified is columns, as an UPDATE … FROM names them.
const qualified = `webhook_deliveries.id, webhook_deliveries.webhook_id, webhook_deliveries.event_id, webhook_deliveries.event_type,
	webhook_deliveries.payload, webhook_deliveries.state, webhook_deliveries.attempts, webhook_deliveries.next_at,
	webhook_deliveries.last_status, webhook_deliveries.last_error, webhook_deliveries.last_ms, webhook_deliveries.created_at,
	webhook_deliveries.finished_at`

// ClaimOne claims row id for by when it is claimable, as a sender does for the
// rows it just wrote; ok is false when it isn't.
func ClaimOne(ctx context.Context, id int64, by string, lease time.Duration) (Row, bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	r, err := scanRow(db().QueryRowContext(cctx, `
		UPDATE webhook_deliveries d SET claimed_by = $2, claimed_until = NOW() + make_interval(secs => $3), attempts = d.attempts + 1
		WHERE d.id = $1 AND `+claimable+`
		RETURNING `+columns, id, by, lease.Seconds()))
	if errors.Is(err, sql.ErrNoRows) {
		return Row{}, false, nil
	}
	return r, err == nil, err
}

// Outcome is what one try ended with.
type Outcome struct {
	Status *int
	Error  string
	Ms     int
}

// Finish records a try by claimed as the row's last: done, dead or cancelled.
func Finish(ctx context.Context, id int64, by, state string, o Outcome) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE webhook_deliveries SET state = $3, last_status = $4, last_error = NULLIF($5, ''), last_ms = $6,
			claimed_by = NULL, claimed_until = NULL, finished_at = NOW()
		WHERE id = $1 AND claimed_by = $2`, id, by, state, o.Status, o.Error, o.Ms)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Retry records a failed try by claimed and makes the row due again at at.
func Retry(ctx context.Context, id int64, by string, at time.Time, o Outcome) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE webhook_deliveries SET next_at = $3, last_status = $4, last_error = NULLIF($5, ''), last_ms = $6,
			claimed_by = NULL, claimed_until = NULL
		WHERE id = $1 AND claimed_by = $2`, id, by, at, o.Status, o.Error, o.Ms)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// List is a webhook's deliveries, newest first, before the id before (0 is
// from the newest), in state when it isn't empty.
func List(ctx context.Context, webhookID uuid.UUID, state string, before int64, limit int) ([]Row, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT `+columns+` FROM webhook_deliveries
		WHERE webhook_id = $1 AND ($2 = '' OR state = $2) AND ($3 = 0 OR id < $3)
		ORDER BY id DESC LIMIT $4`, webhookID, state, before, limit)
	if err != nil {
		return nil, err
	}
	return scanRows(rows)
}

// Replay makes a webhook's delivery go again from its first try, with the same
// body and event id: false when there is no such delivery, or one is going now.
func Replay(ctx context.Context, webhookID uuid.UUID, id int64) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE webhook_deliveries SET state = 'pending', attempts = 0, next_at = NOW(), finished_at = NULL,
			claimed_by = NULL, claimed_until = NULL
		WHERE id = $1 AND webhook_id = $2 AND (claimed_until IS NULL OR claimed_until < NOW())`, id, webhookID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReplayDead sends a webhook's dead deliveries again from their first try, at
// most limit of them, oldest first, and answers their ids. One being sent
// right now is left as it is.
func ReplayDead(ctx context.Context, webhookID uuid.UUID, limit int) ([]int64, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		UPDATE webhook_deliveries SET state = 'pending', attempts = 0, next_at = NOW(), finished_at = NULL,
			claimed_by = NULL, claimed_until = NULL
		WHERE id IN (
			SELECT id FROM webhook_deliveries
			WHERE webhook_id = $1 AND state = 'dead' AND (claimed_until IS NULL OR claimed_until < NOW())
			ORDER BY id LIMIT $2)
		RETURNING id`, webhookID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// DueNow makes a webhook's pending deliveries due at once (it was resumed).
func DueNow(ctx context.Context, webhookID uuid.UUID) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `UPDATE webhook_deliveries SET next_at = NOW() WHERE webhook_id = $1 AND state = 'pending'`, webhookID)
	return err
}

// SetPaused pauses a webhook (at, with why) or resumes it (at nil), and
// answers whether it changed. Resuming starts its count of failed tries again.
func SetPaused(ctx context.Context, webhookID uuid.UUID, at *time.Time, why string) (bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	q := `UPDATE webhooks SET paused_at = $2, paused_reason = NULLIF($3, ''), updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL AND paused_at IS NULL`
	if at == nil {
		q = `UPDATE webhooks SET paused_at = NULL, paused_reason = NULL, failure_count = 0, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL AND paused_at IS NOT NULL`
		res, err := db().ExecContext(cctx, q, webhookID)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		return n == 1, err
	}
	res, err := db().ExecContext(cctx, q, webhookID, *at, why)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// Target is what a try needs to know of its webhook as it is now.
type Target struct {
	URL       string
	Secret    string
	CreatedBy uuid.UUID
	Active    bool
	Deleted   bool
	PausedAt  *time.Time
}

// TargetOf reads a webhook as a try needs it; ok is false when it's gone.
func TargetOf(ctx context.Context, webhookID uuid.UUID) (Target, bool, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var t Target
	var url, secret sql.NullString
	var deletedAt *time.Time
	err := db().QueryRowContext(cctx, `SELECT target_url, secret, created_by, is_active, deleted_at, paused_at FROM webhooks WHERE id = $1`, webhookID).
		Scan(&url, &secret, &t.CreatedBy, &t.Active, &deletedAt, &t.PausedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Target{}, false, nil
	}
	if err != nil {
		return Target{}, false, err
	}
	t.URL, t.Secret, t.Deleted = url.String, secret.String, deletedAt != nil
	return t, true, nil
}

// Tried counts a try against its webhook: a success sets its count of failed
// tries in a row to zero, a failure adds one and answers the count.
func Tried(ctx context.Context, webhookID uuid.UUID, ok bool) (int, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var n int
	q := `UPDATE webhooks SET failure_count = failure_count + 1 WHERE id = $1 RETURNING failure_count`
	if ok {
		q = `UPDATE webhooks SET failure_count = 0, last_triggered_at = NOW() WHERE id = $1 RETURNING failure_count`
	}
	err := db().QueryRowContext(cctx, q, webhookID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return n, err
}

// Health is how one webhook's deliveries stand.
type Health struct {
	WebhookID    uuid.UUID  `json:"webhook_id"`
	PausedAt     *time.Time `json:"paused_at,omitempty"`
	PausedReason *string    `json:"paused_reason,omitempty"`
	// FailingTries is its failed tries in a row.
	FailingTries  int        `json:"failing_tries"`
	Pending       int        `json:"pending"`
	Dead          int        `json:"dead"`
	LastDelivered *time.Time `json:"last_delivered_at,omitempty"`
	LastFailed    *time.Time `json:"last_failed_at,omitempty"`
	OldestPending *time.Time `json:"oldest_pending_at,omitempty"`
}

// HealthOf is how every outgoing webhook's deliveries stand.
func HealthOf(ctx context.Context) ([]Health, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	rows, err := db().QueryContext(cctx, `
		SELECT w.id, w.paused_at, w.paused_reason, w.failure_count,
			COUNT(d.id) FILTER (WHERE d.state = 'pending'),
			COUNT(d.id) FILTER (WHERE d.state = 'dead'),
			MAX(d.finished_at) FILTER (WHERE d.state = 'done'),
			MAX(COALESCE(d.finished_at, d.next_at)) FILTER (WHERE d.last_error IS NOT NULL OR d.state = 'dead'),
			MIN(d.created_at) FILTER (WHERE d.state = 'pending')
		FROM webhooks w LEFT JOIN webhook_deliveries d ON d.webhook_id = w.id
		WHERE w.deleted_at IS NULL AND w.type = 'outgoing'
		GROUP BY w.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Health{}
	for rows.Next() {
		var h Health
		if err := rows.Scan(&h.WebhookID, &h.PausedAt, &h.PausedReason, &h.FailingTries, &h.Pending, &h.Dead, &h.LastDelivered, &h.LastFailed, &h.OldestPending); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// Expire gives up on deliveries that have waited longer than olderThan (their
// webhook paused all that while), as dead, with why.
func Expire(ctx context.Context, olderThan time.Duration, why string) (int64, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		UPDATE webhook_deliveries SET state = 'dead', last_error = $2, finished_at = NOW()
		WHERE state = 'pending' AND created_at < NOW() - make_interval(secs => $1)
			AND (claimed_until IS NULL OR claimed_until < NOW())`, olderThan.Seconds(), why)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Purge deletes up to limit finished rows: done and cancelled ones finished
// before doneBefore, dead ones before deadBefore.
func Purge(ctx context.Context, doneBefore, deadBefore time.Time, limit int) (int64, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	res, err := db().ExecContext(cctx, `
		DELETE FROM webhook_deliveries WHERE id IN (
			SELECT id FROM webhook_deliveries
			WHERE (state IN ('done', 'cancelled') AND finished_at < $1) OR (state = 'dead' AND finished_at < $2)
			LIMIT $3
		)`, doneBefore, deadBefore, limit)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Stats is how every delivery stands, for the admins' system check.
type Stats struct {
	// Pending is the rows waiting, OldestDue how long the oldest of those
	// that may go (its webhook isn't paused) has been due.
	Pending   int64
	OldestDue time.Duration
	// DeadToday is the rows given up on in the last day.
	DeadToday int64
}

// ReadStats reads Stats.
func ReadStats(ctx context.Context) (Stats, error) {
	cctx, cancel := timeout(ctx)
	defer cancel()
	var s Stats
	var oldest float64
	err := db().QueryRowContext(cctx, `
		SELECT
			COUNT(*) FILTER (WHERE d.state = 'pending'),
			COALESCE(EXTRACT(EPOCH FROM NOW() - MIN(d.next_at) FILTER (WHERE d.state = 'pending' AND w.paused_at IS NULL AND d.next_at <= NOW())), 0),
			COUNT(*) FILTER (WHERE d.state = 'dead' AND d.finished_at > NOW() - INTERVAL '1 day')
		FROM webhook_deliveries d JOIN webhooks w ON w.id = d.webhook_id
		WHERE d.state IN ('pending', 'dead')`).Scan(&s.Pending, &oldest, &s.DeadToday)
	s.OldestDue = time.Duration(oldest * float64(time.Second))
	return s, err
}

// TakeOver makes owner a webhook's creator: an admin who resumes a webhook
// whose creator left takes on what it sends.
func TakeOver(ctx context.Context, webhookID, owner uuid.UUID) error {
	cctx, cancel := timeout(ctx)
	defer cancel()
	_, err := db().ExecContext(cctx, `UPDATE webhooks SET created_by = $2, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`, webhookID, owner)
	return err
}
