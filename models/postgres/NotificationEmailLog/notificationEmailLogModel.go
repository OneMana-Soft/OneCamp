package models

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Status constants for the email log.
const (
	StatusSent       = "sent"
	StatusFailed     = "failed"
	StatusSuppressed = "suppressed"
	StatusBounced    = "bounced"
	StatusComplained = "complained"
)

type LogRow struct {
	ID                uuid.UUID  `json:"id"`
	UserID            *uuid.UUID `json:"user_id,omitempty"`
	ToEmail           string     `json:"to_email"`
	EventType         string     `json:"event_type"`
	Subject           string     `json:"subject"`
	Status            string     `json:"status"`
	ProviderMessageID *string    `json:"provider_message_id,omitempty"`
	ErrorMessage      *string    `json:"error_message,omitempty"`
	QueueID           *uuid.UUID `json:"queue_id,omitempty"`
	Attempts          int        `json:"attempts"`
	CreatedAt         time.Time  `json:"created_at"`
}

// Insert records a terminal email outcome. Best-effort: log failures should
// not fail the parent send.
func Insert(row LogRow) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var userArg any
	if row.UserID != nil {
		userArg = *row.UserID
	}
	var providerArg any
	if row.ProviderMessageID != nil {
		providerArg = *row.ProviderMessageID
	}
	var errArg any
	if row.ErrorMessage != nil {
		errArg = *row.ErrorMessage
	}
	var queueArg any
	if row.QueueID != nil {
		queueArg = *row.QueueID
	}

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		INSERT INTO notification_email_log
			(user_id, to_email, event_type, subject, status, provider_message_id, error_message, queue_id, attempts)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`, userArg, row.ToEmail, row.EventType, row.Subject, row.Status,
		providerArg, errArg, queueArg, row.Attempts)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailLog/Insert failed err: %+v", err)
	}
	return err
}

// UpdateStatusByProviderID flips the status for a row that was previously
// sent successfully. Used when the Resend bounce/complaint webhook fires.
func UpdateStatusByProviderID(providerID, status, errMsg string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	var errArg any
	if errMsg != "" {
		errArg = errMsg
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE notification_email_log
		SET status = $1, error_message = $2
		WHERE provider_message_id = $3
	`, status, errArg, providerID)
	return err
}

// CleanupOld trims old log rows beyond the retention window.
func CleanupOld(olderThan time.Duration) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout*5)
	defer cancel()

	// Pre-format the interval literal: pgx v5 cannot encode an int into
	// the text-typed parameter that `$1 || ' seconds'` would imply.
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`DELETE FROM notification_email_log WHERE created_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int64(olderThan.Seconds())))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ListByUser returns the most recent log rows for a user. Used by the user
// settings page to show "your delivery history".
func ListByUser(userID uuid.UUID, limit int) ([]LogRow, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	if limit <= 0 || limit > 100 {
		limit = 25
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT id, user_id, to_email, event_type, subject, status,
		       provider_message_id, error_message, queue_id, attempts, created_at
		FROM notification_email_log
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogRow
	for rows.Next() {
		var l LogRow
		var userIDNS sql.NullString
		var providerNS sql.NullString
		var errNS sql.NullString
		var queueNS sql.NullString
		if err := rows.Scan(&l.ID, &userIDNS, &l.ToEmail, &l.EventType, &l.Subject,
			&l.Status, &providerNS, &errNS, &queueNS, &l.Attempts, &l.CreatedAt); err != nil {
			return nil, err
		}
		if userIDNS.Valid {
			if u, err := uuid.Parse(userIDNS.String); err == nil {
				l.UserID = &u
			}
		}
		if providerNS.Valid {
			s := providerNS.String
			l.ProviderMessageID = &s
		}
		if errNS.Valid {
			s := errNS.String
			l.ErrorMessage = &s
		}
		if queueNS.Valid {
			if q, err := uuid.Parse(queueNS.String); err == nil {
				l.QueueID = &q
			}
		}
		out = append(out, l)
	}
	// Iteration can stop on a mid-query failure (dropped connection, server-side
	// error) rather than on end-of-rows. Without it this returns a PARTIAL result
	// with a nil error, and the caller cannot tell truncated data from a short list.
	if err := rows.Err(); err != nil {
		helpers.LogErrorWithContext(ctx, "models/NotificationEmailLog rows iteration failed err: %+v", err)
		return nil, err
	}
	return out, nil
}
