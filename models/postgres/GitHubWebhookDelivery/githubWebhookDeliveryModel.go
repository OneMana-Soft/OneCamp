package models

import (
	"context"
	"database/sql"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/lib/pq"
)

// Status values for github_webhook_deliveries.status. Kept narrow on
// purpose — every transition is a single UPDATE with a where-clause
// filter, so adding states means adding migration constraints we don't
// want today.
const (
	WebhookDeliveryProcessing = "processing"
	WebhookDeliveryCompleted  = "completed"
	WebhookDeliveryFailed     = "failed"
)

// ClaimDelivery atomically inserts a brand-new delivery row in
// 'processing' state, or — if the row already exists and is NOT yet
// completed — bumps its attempts counter and re-claims it. This is the
// dedup primitive used by the webhook controller.
//
// Returns:
//   - shouldProcess=true : the caller is the rightful owner of this
//     attempt and should run the handler.
//   - shouldProcess=false: another worker / a previous successful run
//     has already finished this delivery; skip.
//
// The two outcomes look like this:
//  1. Brand-new delivery_id      → INSERT … 'processing'  → process
//  2. Existing & 'completed'     → conflict, no UPDATE     → skip
//  3. Existing & not completed   → UPDATE → 'processing'   → process
//     (this is the retry path: GitHub re-delivers, we re-attempt
//     because the previous attempt didn't finish)
func ClaimDelivery(ctx context.Context, deliveryID, eventType string) (shouldProcess bool, err error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const insertQ = `
		INSERT INTO github_webhook_deliveries
		    (delivery_id, event_type, status, attempts, received_at, updated_at)
		VALUES ($1, $2, 'processing', 1, NOW(), NOW())
		ON CONFLICT (delivery_id) DO UPDATE
		    SET status      = 'processing',
		        attempts    = github_webhook_deliveries.attempts + 1,
		        updated_at  = NOW()
		    -- Crucial: only re-claim non-completed rows. The DO UPDATE
		    -- without a WHERE always rewrites; the WHERE here makes
		    -- "row exists and is completed" a no-op so RETURNING can
		    -- distinguish that case.
		    WHERE github_webhook_deliveries.status <> 'completed'
		RETURNING status
	`
	var status string
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, insertQ, deliveryID, eventType)
	err = row.Scan(&status)
	if err == sql.ErrNoRows {
		// No row returned → the conflict path matched a completed row
		// and the WHERE rejected the UPDATE. Skip.
		return false, nil
	}
	if err != nil {
		// Defensive: pq sometimes surfaces unique-violations even with
		// ON CONFLICT under heavy load; treat as "already exists, skip".
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
			return false, nil
		}
		helpers.LogErrorWithContext(ctx, "models/ClaimDelivery err: %+v", err)
		return false, err
	}
	return true, nil
}

// MarkDeliveryCompleted flips a 'processing' row to 'completed'. Idempotent.
func MarkDeliveryCompleted(ctx context.Context, deliveryID string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE github_webhook_deliveries
		   SET status = 'completed',
		       processed_at = NOW(),
		       updated_at = NOW(),
		       error_message = NULL
		 WHERE delivery_id = $1
		   AND status <> 'completed'
	`, deliveryID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/MarkDeliveryCompleted err: %+v", err)
	}
	return err
}

// MarkDeliveryFailed records a terminal failure for the current attempt.
// GitHub will retry on its own; the next ClaimDelivery will re-run the
// handler because the row is not 'completed'.
func MarkDeliveryFailed(ctx context.Context, deliveryID, errMsg string) error {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if len(errMsg) > 1000 {
		errMsg = errMsg[:1000]
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbCtx, `
		UPDATE github_webhook_deliveries
		   SET status = 'failed',
		       error_message = $2,
		       updated_at = NOW()
		 WHERE delivery_id = $1
		   AND status <> 'completed'
	`, deliveryID, errMsg)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/MarkDeliveryFailed err: %+v", err)
	}
	return err
}

// ─────────────────────────────────────────────────────────────────────
// Legacy entry point. Kept to avoid breaking older callers; new code
// should use ClaimDelivery / MarkDeliveryCompleted / MarkDeliveryFailed.
// ─────────────────────────────────────────────────────────────────────

// InsertGitHubWebhookDelivery records a webhook delivery for deduplication.
// Returns true if inserted (new delivery), false if already exists.
//
// Deprecated: use ClaimDelivery + MarkDeliveryCompleted instead. This
// shim continues the legacy "insert as completed on receipt" behaviour
// for callers that haven't migrated yet.
func InsertGitHubWebhookDelivery(query string, deliveryID string, eventType string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, deliveryID, eventType)
	if err != nil {
		if isDuplicateError(err) {
			return false, nil
		}
		helpers.LogErrorWithContext(ctx, "models/InsertGitHubWebhookDelivery Failed err: %+v", err)
		return false, err
	}
	return true, nil
}

// isDuplicateError checks if the error is a unique violation.
func isDuplicateError(err error) bool {
	if err == nil {
		return false
	}
	if pqErr, ok := err.(*pq.Error); ok {
		return pqErr.Code == "23505"
	}
	return false
}

// HealthSummary captures the health-check shape returned to the admin
// UI: counts of recent statuses + the last successful processing time.
// Aggregating in SQL means a single round-trip regardless of fleet size.
type HealthSummary struct {
	Completed24h     int        `json:"completed_24h"`
	Failed24h        int        `json:"failed_24h"`
	Processing24h    int        `json:"processing_24h"`
	LastCompletedAt  *time.Time `json:"last_completed_at,omitempty"`
	LastFailedAt     *time.Time `json:"last_failed_at,omitempty"`
	LastErrorMessage *string    `json:"last_error_message,omitempty"`
}

// GetHealthSummary returns a single-row aggregate over the last 24h.
// Cheap because the partial index on non-completed rows means the
// "recent failures" path is O(failures), and the "last_completed_at"
// path uses the existing received_at index.
func GetHealthSummary(ctx context.Context) (*HealthSummary, error) {
	dbCtx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	var s HealthSummary
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbCtx, `
		SELECT
		    COUNT(*) FILTER (WHERE status = 'completed' AND received_at > NOW() - INTERVAL '24 hours'),
		    COUNT(*) FILTER (WHERE status = 'failed'    AND received_at > NOW() - INTERVAL '24 hours'),
		    COUNT(*) FILTER (WHERE status = 'processing' AND received_at > NOW() - INTERVAL '24 hours'),
		    MAX(processed_at) FILTER (WHERE status = 'completed'),
		    MAX(updated_at)   FILTER (WHERE status = 'failed'),
		    (SELECT error_message
		       FROM github_webhook_deliveries
		      WHERE status = 'failed'
		      ORDER BY updated_at DESC
		      LIMIT 1)
		FROM github_webhook_deliveries
	`).Scan(&s.Completed24h, &s.Failed24h, &s.Processing24h,
		&s.LastCompletedAt, &s.LastFailedAt, &s.LastErrorMessage)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetHealthSummary err: %+v", err)
		return nil, err
	}
	return &s, nil
}
