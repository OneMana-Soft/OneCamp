package models

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/lib/pq"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Reason constants — see migration check constraint.
const (
	ReasonBounced      = "bounced"
	ReasonComplained   = "complained"
	ReasonManual       = "manual"
	ReasonUnsubscribed = "unsubscribed"
)

type Suppression struct {
	ID        uuid.UUID `json:"id"`
	Email     string    `json:"email"`
	Reason    string    `json:"reason"`
	Details   *string   `json:"details,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// IsSuppressed is the hot-path lookup before any send. Returns the reason
// when the email is suppressed, empty string otherwise. Email match is
// case-insensitive.
func IsSuppressed(email string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return "", nil
	}

	var reason string
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT reason FROM notification_email_suppressions WHERE email = $1`, email).Scan(&reason)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailSuppression/IsSuppressed err: %+v", err)
		return "", err
	}
	return reason, nil
}

// Upsert adds or updates a suppression. Used by the bounce/complaint webhook
// handler and by manual admin actions.
func Upsert(email, reason, details string) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	email = strings.TrimSpace(strings.ToLower(email))
	if email == "" {
		return errors.New("empty email")
	}

	var detailsArg any
	if details != "" {
		detailsArg = details
	}

	query := `
		INSERT INTO notification_email_suppressions (email, reason, details)
		VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE
		   SET reason = EXCLUDED.reason,
		       details = EXCLUDED.details
	`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, email, reason, detailsArg)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailSuppression/Upsert err: %+v", err)
		return err
	}
	return nil
}

// IsSuppressedBatch is the batched twin of IsSuppressed. Given a list
// of emails it returns the subset that are suppressed, mapping each
// hit to its reason. Used by the notification dispatcher to avoid N
// round trips when fanning a single event out to many recipients.
//
// Email comparison is case-insensitive: we lower-case both the input
// list and the stored values via the email = ANY(...)::text[] form.
// Empty input returns an empty map.
func IsSuppressedBatch(emails []string) (map[string]string, error) {
	if len(emails) == 0 {
		return map[string]string{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	normalised := make([]string, 0, len(emails))
	for _, e := range emails {
		e = strings.TrimSpace(strings.ToLower(e))
		if e != "" {
			normalised = append(normalised, e)
		}
	}
	if len(normalised) == 0 {
		return map[string]string{}, nil
	}

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT email, reason FROM notification_email_suppressions WHERE email = ANY($1)`,
		pq.Array(normalised))
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/NotificationEmailSuppression/IsSuppressedBatch err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]string, len(normalised))
	for rows.Next() {
		var email, reason string
		if err := rows.Scan(&email, &reason); err != nil {
			return nil, err
		}
		out[email] = reason
	}
	return out, rows.Err()
}
