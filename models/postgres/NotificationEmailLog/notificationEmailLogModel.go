package models

import (
	"context"
	"fmt"
	queueDomain "github.com/akashc777/OneCamp/domain/NotificationEmailQueue"
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
	return InsertMany([]LogRow{row})
}

// InsertMany writes rows to the log in one INSERT (queueDomain.Log); nothing
// for no rows.
func InsertMany(rows []LogRow) error {
	if len(rows) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	args := make([]any, 0, len(rows)*queueDomain.LogParams)
	for _, row := range rows {
		var userArg, providerArg, errArg, queueArg any
		if row.UserID != nil {
			userArg = *row.UserID
		}
		if row.ProviderMessageID != nil {
			providerArg = *row.ProviderMessageID
		}
		if row.ErrorMessage != nil {
			errArg = *row.ErrorMessage
		}
		if row.QueueID != nil {
			queueArg = *row.QueueID
		}
		args = append(args, userArg, row.ToEmail, row.EventType, row.Subject, row.Status,
			providerArg, errArg, queueArg, row.Attempts)
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, queueDomain.Log(len(rows)), args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailLog/InsertMany failed err: %+v", err)
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
