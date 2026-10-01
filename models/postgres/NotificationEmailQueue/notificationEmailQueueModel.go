package models

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
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

// FetchAndLockPending atomically claims up to `limit` pending rows.
// Uses SELECT ... FOR UPDATE SKIP LOCKED so multiple workers don't fight
// over the same rows. Items are flipped to 'processing' inside the same
// transaction, with attempts++.
func FetchAndLockPending(limit int) ([]*QueueItem, error) {
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

	selectQ := `
		SELECT id, user_id, to_email, event_type, subject, html_body, text_body,
		       cta_url, dedup_key, status, attempts, next_retry_at, error_message,
		       metadata, created_at, updated_at
		FROM notification_email_queue
		WHERE status = 'pending'
		  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		ORDER BY created_at ASC
		LIMIT $1
		FOR UPDATE SKIP LOCKED
	`
	rows, err := tx.QueryContext(ctx, selectQ, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailQueue/FetchAndLockPending select err: %+v", err)
		return nil, err
	}

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
			rows.Close()
			helpers.LogErrorWithContext(ctx,
				"models/NotificationEmailQueue/FetchAndLockPending scan err: %+v", err)
			return nil, err
		}
		if ctaURL.Valid {
			s := ctaURL.String
			it.CTAURL = &s
		}
		it.Metadata = metadata
		items = append(items, &it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(items) == 0 {
		_ = tx.Commit()
		committed = true
		return nil, nil
	}

	ids := make([]any, len(items))
	placeholders := ""
	for i, it := range items {
		if i > 0 {
			placeholders += ","
		}
		placeholders += "$" + strconv.Itoa(i+1)
		ids[i] = it.ID
	}
	updateQ := `UPDATE notification_email_queue
	            SET status = 'processing', attempts = attempts + 1, updated_at = NOW()
	            WHERE id IN (` + placeholders + `)`
	if _, err := tx.ExecContext(ctx, updateQ, ids...); err != nil {
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

// MarkSent transitions a row to terminal 'sent'.
func MarkSent(id uuid.UUID) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE notification_email_queue SET status = 'sent', updated_at = NOW(), error_message = NULL WHERE id = $1`,
		id)
	return err
}

// MarkFailed transitions a row to terminal 'failed' (max retries exhausted).
func MarkFailed(id uuid.UUID, errMsg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE notification_email_queue
		 SET status = 'failed', error_message = $1, updated_at = NOW()
		 WHERE id = $2`,
		errMsg, id)
	return err
}

// MarkSuppressed transitions a row to 'suppressed' — recipient is on the
// bounce/complaint list, or recipient prefs disable the channel.
func MarkSuppressed(id uuid.UUID, reason string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE notification_email_queue
		 SET status = 'suppressed', error_message = $1, updated_at = NOW()
		 WHERE id = $2`,
		reason, id)
	return err
}

// RetryLater pushes a row back to 'pending' with a future next_retry_at.
func RetryLater(id uuid.UUID, errMsg string, nextRetryAt time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE notification_email_queue
		 SET status = 'pending', error_message = $1, next_retry_at = $2, updated_at = NOW()
		 WHERE id = $3`,
		errMsg, nextRetryAt, id)
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
