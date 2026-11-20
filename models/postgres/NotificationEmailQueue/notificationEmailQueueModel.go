package models

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"

	queueDomain "github.com/akashc777/OneCamp/domain/NotificationEmailQueue"
	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

// QueueItem is one outbound email row. Mirrors the github_sync_queue shape:
// status machine + attempts counter + next_retry_at + error_message.
type QueueItem struct {
	ID           uuid.UUID
	UserID       uuid.UUID
	ToEmail      string
	EventType    string
	Subject      string
	HTMLBody     string
	TextBody     string
	CTAURL       *string
	DedupKey     string
	Status       string
	Attempts     int
	NextRetryAt  *time.Time
	ErrorMessage *string
	Metadata     []byte
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// EnqueueInput is the pre-rendered payload handed to the queue. Rendering
// happens at enqueue time (not at send time) so the email reflects the state
// of the world when the event happened, not when the worker drains the queue.
type EnqueueInput struct {
	UserID    uuid.UUID
	ToEmail   string
	EventType string
	Subject   string
	HTMLBody  string
	TextBody  string
	CTAURL    string
	DedupKey  string
	Metadata  []byte
	// NextRetryAt, if set, is written into the row at insert time so a
	// deferred (quiet-hours) email cannot be picked up by the worker
	// before the deferral window closes. nil means "ready to send now".
	NextRetryAt *time.Time
}

// AtomicEnqueue inserts a queue row only if no pending/processing row exists
// for the same dedup_key. The dedup invariant is enforced by both the SQL
// guard here AND the partial unique index in the migration; the WHERE-NOT-EXISTS
// avoids the noisy "duplicate key" error that ON CONFLICT would produce.
//
// Returns (true, nil) on insert, (false, nil) on duplicate skip,
// (false, err) on a real error.
func AtomicEnqueue(in EnqueueInput) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var ctaPtr any
	if in.CTAURL != "" {
		ctaPtr = in.CTAURL
	}
	if in.Metadata == nil {
		in.Metadata = []byte("{}")
	}
	var nextRetryArg any
	if in.NextRetryAt != nil {
		nextRetryArg = *in.NextRetryAt
	}

	query := `
		INSERT INTO notification_email_queue
			(user_id, to_email, event_type, subject, html_body, text_body, cta_url, dedup_key, metadata, next_retry_at)
		-- $8 is CAST because it appears twice: once here, where an INSERT ... SELECT
		-- leaves it 'unknown' and Postgres defaults it to text, and once below in
		-- 'dedup_key = $8', where the varchar column forces varchar. Postgres must
		-- deduce ONE type per parameter, so it refused the statement outright with
		-- 42P08 "inconsistent types deduced for parameter $8: text versus character
		-- varying". That is a PREPARE-time failure, so it never enqueued a single
		-- notification email, and the only symptom was mail that silently never
		-- arrived.
		SELECT $1, $2, $3, $4, $5, $6, $7, $8::varchar, $9::jsonb, $10
		WHERE NOT EXISTS (
			SELECT 1 FROM notification_email_queue
			WHERE dedup_key = $8 AND status IN ('pending', 'processing')
		)
	`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query,
		in.UserID, in.ToEmail, in.EventType, in.Subject, in.HTMLBody, in.TextBody,
		ctaPtr, in.DedupKey, in.Metadata, nextRetryArg,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/AtomicEnqueue failed err: %+v", err)
		return false, err
	}
	rows, _ := res.RowsAffected()
	return rows > 0, nil
}

// Mates is which of a claimed person's other pending rows FetchAndLockPending
// claims with theirs: those of Kinds due within Window from now, at most Max,
// when a claimed row is one of Kinds. The zero Mates claims none.
type Mates struct {
	Kinds  []string
	Window time.Duration
	Max    int
}

// FetchAndLockPending atomically claims up to `limit` pending rows.
// Uses SELECT ... FOR UPDATE SKIP LOCKED so multiple workers don't fight
// over the same rows. Items are flipped to 'processing' inside the same
// transaction, with attempts++.
func FetchAndLockPending(limit int, mates Mates) ([]*QueueItem, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout*5)
	defer cancel()

	tx, err := postgresInit.DBConn.SqlDB.BeginTx(ctx, nil)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/FetchAndLockPending begin tx err: %+v", err)
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	items, err := scanItems(tx.QueryContext(ctx, queueDomain.Due, limit))
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/FetchAndLockPending select err: %+v", err)
		return nil, err
	}
	if len(items) == 0 {
		_ = tx.Commit()
		committed = true
		return nil, nil
	}
	if users := mates.of(items); len(users) > 0 {
		more, err := scanItems(tx.QueryContext(ctx, queueDomain.WindowMates, pgexec.Array(users), pgexec.Array(mates.Kinds),
			pgexec.Array(helpers.UUIDStrings(IDsOf(items))), fmt.Sprintf("%d seconds", int(mates.Window.Seconds())), mates.Max))
		if err != nil {
			helpers.LogErrorWithContext(ctx,
				"models/NotificationEmailQueue/FetchAndLockPending window-mates err: %+v", err)
			return nil, err
		}
		items = append(items, more...)
	}

	if _, err := tx.ExecContext(ctx, queueDomain.Claim, pgexec.Array(helpers.UUIDStrings(IDsOf(items)))); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/FetchAndLockPending update err: %+v", err)
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/FetchAndLockPending commit err: %+v", err)
		return nil, err
	}
	committed = true

	// Reflect the bump locally so the caller sees the right attempts count.
	for _, it := range items {
		it.Attempts++
		it.Status = "processing"
	}
	return items, nil
}

// of is the people among items with a row of m's kinds: those whose
// window-mates the claim takes too. None for the zero Mates.
func (m Mates) of(items []*QueueItem) []string {
	if m.Max <= 0 || len(m.Kinds) == 0 {
		return nil
	}
	var users []string
	for _, it := range items {
		if u := it.UserID.String(); slices.Contains(m.Kinds, it.EventType) && !slices.Contains(users, u) {
			users = append(users, u)
		}
	}
	return users
}

// IDsOf is items' ids. Pure.
func IDsOf(items []*QueueItem) []uuid.UUID {
	ids := make([]uuid.UUID, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	return ids
}

// scanItems reads the queue rows a query answered (queueDomain's columns).
func scanItems(rows *sql.Rows, err error) ([]*QueueItem, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*QueueItem
	for rows.Next() {
		var it QueueItem
		var ctaURL sql.NullString
		var metadata []byte
		if err := rows.Scan(
			&it.ID, &it.UserID, &it.ToEmail, &it.EventType, &it.Subject,
			&it.HTMLBody, &it.TextBody, &ctaURL, &it.DedupKey, &it.Status,
			&it.Attempts, &it.NextRetryAt, &it.ErrorMessage, &metadata,
			&it.CreatedAt, &it.UpdatedAt,
		); err != nil {
			return nil, err
		}
		if ctaURL.Valid {
			s := ctaURL.String
			it.CTAURL = &s
		}
		it.Metadata = metadata
		items = append(items, &it)
	}
	return items, rows.Err()
}

// MarkSent closes the rows ids, one email's, as sent.
func MarkSent(ids []uuid.UUID) error {
	return closeRows(queueDomain.Sent, ids)
}

// MarkFailed closes the rows ids as failed (a terminal error, or retries
// exhausted).
func MarkFailed(ids []uuid.UUID, errMsg string) error {
	return closeRows(queueDomain.Closed, ids, "failed", errMsg)
}

// MarkSuppressed closes the rows ids as suppressed: the recipient is on the
// bounce/complaint list, can't be sent to, or is no longer told.
func MarkSuppressed(ids []uuid.UUID, reason string) error {
	return closeRows(queueDomain.Closed, ids, "suppressed", reason)
}

// RetryLater puts the rows ids back to 'pending' until nextRetryAt.
func RetryLater(ids []uuid.UUID, errMsg string, nextRetryAt time.Time) error {
	return closeRows(queueDomain.Later, ids, errMsg, nextRetryAt)
}

// closeRows runs q, one UPDATE, over the rows ids ($1) with args ($2 on);
// nothing for no ids.
func closeRows(q string, ids []uuid.UUID, args ...any) error {
	if len(ids) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, q, append([]any{pgexec.Array(helpers.UUIDStrings(ids))}, args...)...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/NotificationEmailQueue/closeRows err: %+v", err)
	}
	return err
}

// ReapStale recovers items stuck in 'processing' beyond a sane window
// (worker crashed / pod evicted mid-send). Restored to 'pending' with a
// fresh next_retry_at so they're picked up on the next poll.
func ReapStale(staleAfter time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Postgres treats `$1 || ' seconds'` as a text expression and pgx v5
	// refuses to auto-encode an int into a text-typed parameter. Pass a
	// pre-formatted interval literal and cast directly to interval.
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE notification_email_queue
		 SET status = 'pending', updated_at = NOW()
		 WHERE status = 'processing' AND updated_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(staleAfter.Seconds())))
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/ReapStale failed err: %+v", err)
		return 0, err
	}
	return res.RowsAffected()
}

// CleanupOld deletes terminal rows older than the retention window. Caller
// is expected to schedule this via a cleanup goroutine (see worker.go).
func CleanupOld(olderThan time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout*5)
	defer cancel()

	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`DELETE FROM notification_email_queue
		 WHERE status IN ('sent', 'failed', 'suppressed')
		   AND updated_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
