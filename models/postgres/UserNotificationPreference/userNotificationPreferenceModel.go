package models

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// UserNotificationPreference is the per-user master switchboard for emails.
// All flags are sticky-on at creation time so users who never visit the
// settings page still get the documented default (everything except digests).
type UserNotificationPreference struct {
	ID                   uuid.UUID `json:"id"`
	UserID               uuid.UUID `json:"user_id"`
	EmailEnabled         bool      `json:"email_enabled"`
	EmailMentions        bool      `json:"email_mentions"`
	EmailDMs             bool      `json:"email_dms"`
	EmailTaskAssigned    bool      `json:"email_task_assigned"`
	EmailTaskStatus      bool      `json:"email_task_status"`
	EmailComments        bool      `json:"email_comments"`
	EmailCalls           bool      `json:"email_calls"`
	EmailChannelInvites  bool      `json:"email_channel_invites"`
	EmailOnlyWhenOffline bool      `json:"email_only_when_offline"`
	EmailDigestFrequency string    `json:"email_digest_frequency"`
	QuietHoursEnabled    bool      `json:"quiet_hours_enabled"`
	QuietHoursStart      *string   `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd        *string   `json:"quiet_hours_end,omitempty"`
	QuietHoursTZ         *string   `json:"quiet_hours_tz,omitempty"`
	// NotificationsPausedUntil is a pause the person set (Slack's "pause
	// notifications"); nil or past means not paused. See domain/UserFCMToken.
	NotificationsPausedUntil *time.Time `json:"notifications_paused_until,omitempty"`
	UnsubscribeToken         string     `json:"-"` // never expose to the client
	CreatedAt                time.Time  `json:"created_at"`
	UpdatedAt                time.Time  `json:"updated_at"`
}

const allColumns = `id, user_id, email_enabled, email_mentions, email_dms, email_task_assigned,
		email_task_status, email_comments, email_calls, email_channel_invites,
		email_only_when_offline, email_digest_frequency, quiet_hours_enabled,
		quiet_hours_start, quiet_hours_end, quiet_hours_tz, unsubscribe_token,
		created_at, updated_at, notifications_paused_until`

func scanRow(row *sql.Row) (*UserNotificationPreference, error) {
	var p UserNotificationPreference
	err := row.Scan(
		&p.ID, &p.UserID, &p.EmailEnabled, &p.EmailMentions, &p.EmailDMs,
		&p.EmailTaskAssigned, &p.EmailTaskStatus, &p.EmailComments, &p.EmailCalls,
		&p.EmailChannelInvites, &p.EmailOnlyWhenOffline, &p.EmailDigestFrequency,
		&p.QuietHoursEnabled, &p.QuietHoursStart, &p.QuietHoursEnd, &p.QuietHoursTZ,
		&p.UnsubscribeToken, &p.CreatedAt, &p.UpdatedAt, &p.NotificationsPausedUntil,
	)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// GetByUserID returns the preference row for the given user, or sql.ErrNoRows
// if none exists. Callers should treat a missing row as "use defaults".
func GetByUserID(userID uuid.UUID) (*UserNotificationPreference, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `SELECT ` + allColumns + ` FROM users_notification_preferences WHERE user_id = $1`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userID)
	pref, err := scanRow(row)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			helpers.LogErrorWithContext(ctx,
				"models/UserNotificationPreference/GetByUserID failed err: %+v", err)
		}
		return nil, err
	}
	return pref, nil
}

// GetByUnsubscribeToken returns the preference row for a one-click unsubscribe.
func GetByUnsubscribeToken(token string) (*UserNotificationPreference, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `SELECT ` + allColumns + ` FROM users_notification_preferences WHERE unsubscribe_token = $1`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, token)
	pref, err := scanRow(row)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			helpers.LogErrorWithContext(ctx,
				"models/UserNotificationPreference/GetByUnsubscribeToken failed err: %+v", err)
		}
		return nil, err
	}
	return pref, nil
}

// EnsureForUser inserts a preference row with defaults if one does not exist
// and returns the resulting row. Idempotent.
func EnsureForUser(userID uuid.UUID, unsubToken string) (*UserNotificationPreference, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `
		INSERT INTO users_notification_preferences (user_id, unsubscribe_token)
		VALUES ($1, $2)
		ON CONFLICT (user_id) DO UPDATE
		    SET unsubscribe_token = users_notification_preferences.unsubscribe_token
		RETURNING ` + allColumns
	row := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, query, userID, unsubToken)
	pref, err := scanRow(row)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UserNotificationPreference/EnsureForUser failed err: %+v", err)
		return nil, err
	}
	return pref, nil
}

// UpdatePreferenceInput is the partial-update payload accepted by the user
// settings endpoint. Pointers let callers omit fields they don't want changed.
type UpdatePreferenceInput struct {
	EmailEnabled         *bool   `json:"email_enabled,omitempty"`
	EmailMentions        *bool   `json:"email_mentions,omitempty"`
	EmailDMs             *bool   `json:"email_dms,omitempty"`
	EmailTaskAssigned    *bool   `json:"email_task_assigned,omitempty"`
	EmailTaskStatus      *bool   `json:"email_task_status,omitempty"`
	EmailComments        *bool   `json:"email_comments,omitempty"`
	EmailCalls           *bool   `json:"email_calls,omitempty"`
	EmailChannelInvites  *bool   `json:"email_channel_invites,omitempty"`
	EmailOnlyWhenOffline *bool   `json:"email_only_when_offline,omitempty"`
	EmailDigestFrequency *string `json:"email_digest_frequency,omitempty"`
	QuietHoursEnabled    *bool   `json:"quiet_hours_enabled,omitempty"`
	QuietHoursStart      *string `json:"quiet_hours_start,omitempty"`
	QuietHoursEnd        *string `json:"quiet_hours_end,omitempty"`
	QuietHoursTZ         *string `json:"quiet_hours_tz,omitempty"`
}

// Update writes the supplied changes to the database. The row must already
// exist (callers should call EnsureForUser first).
func Update(userID uuid.UUID, in UpdatePreferenceInput) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Build a dynamic SET clause. Using positional args + a builder keeps us
	// safe from SQL injection (no string interpolation of user content).
	var sets []string
	var args []any
	idx := 1
	add := func(col string, v any) {
		sets = append(sets, col+" = $"+strconv.Itoa(idx))
		args = append(args, v)
		idx++
	}
	if in.EmailEnabled != nil {
		add("email_enabled", *in.EmailEnabled)
	}
	if in.EmailMentions != nil {
		add("email_mentions", *in.EmailMentions)
	}
	if in.EmailDMs != nil {
		add("email_dms", *in.EmailDMs)
	}
	if in.EmailTaskAssigned != nil {
		add("email_task_assigned", *in.EmailTaskAssigned)
	}
	if in.EmailTaskStatus != nil {
		add("email_task_status", *in.EmailTaskStatus)
	}
	if in.EmailComments != nil {
		add("email_comments", *in.EmailComments)
	}
	if in.EmailCalls != nil {
		add("email_calls", *in.EmailCalls)
	}
	if in.EmailChannelInvites != nil {
		add("email_channel_invites", *in.EmailChannelInvites)
	}
	if in.EmailOnlyWhenOffline != nil {
		add("email_only_when_offline", *in.EmailOnlyWhenOffline)
	}
	if in.EmailDigestFrequency != nil {
		add("email_digest_frequency", *in.EmailDigestFrequency)
	}
	if in.QuietHoursEnabled != nil {
		add("quiet_hours_enabled", *in.QuietHoursEnabled)
	}
	if in.QuietHoursStart != nil {
		add("quiet_hours_start", *in.QuietHoursStart)
	}
	if in.QuietHoursEnd != nil {
		add("quiet_hours_end", *in.QuietHoursEnd)
	}
	if in.QuietHoursTZ != nil {
		add("quiet_hours_tz", *in.QuietHoursTZ)
	}
	if len(sets) == 0 {
		return nil
	}

	sets = append(sets, "updated_at = NOW()")
	args = append(args, userID)
	query := "UPDATE users_notification_preferences SET " +
		strings.Join(sets, ", ") +
		" WHERE user_id = $" + strconv.Itoa(idx)

	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UserNotificationPreference/Update failed err: %+v", err)
		return err
	}
	return nil
}

// SetEmailEnabledByToken flips email_enabled. Used by the public unsubscribe link.
func SetEmailEnabledByToken(token string, enabled bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	query := `UPDATE users_notification_preferences
	          SET email_enabled = $1, updated_at = NOW()
	          WHERE unsubscribe_token = $2`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, query, enabled, token)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UserNotificationPreference/SetEmailEnabledByToken failed err: %+v", err)
		return err
	}
	return nil
}

// GetByUserIDs is the batched twin of GetByUserID. Returns a map keyed
// by user_id; missing keys signal "no row exists, use defaults".
//
// Used by the notification dispatcher fan-out so a single channel
// mention doesn't spawn N PG round-trips against the prefs table.
func GetByUserIDs(userIDs []uuid.UUID) (map[uuid.UUID]*UserNotificationPreference, error) {
	if len(userIDs) == 0 {
		return map[uuid.UUID]*UserNotificationPreference{}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Convert to []string for ANY-array. Postgres will cast back to UUID.
	ids := make([]string, len(userIDs))
	for i, id := range userIDs {
		ids[i] = id.String()
	}

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx,
		`SELECT `+allColumns+` FROM users_notification_preferences WHERE user_id = ANY($1::uuid[])`,
		"{"+strings.Join(ids, ",")+"}")
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UserNotificationPreference/GetByUserIDs err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	out := make(map[uuid.UUID]*UserNotificationPreference, len(userIDs))
	for rows.Next() {
		var p UserNotificationPreference
		err := rows.Scan(
			&p.ID, &p.UserID, &p.EmailEnabled, &p.EmailMentions, &p.EmailDMs,
			&p.EmailTaskAssigned, &p.EmailTaskStatus, &p.EmailComments, &p.EmailCalls,
			&p.EmailChannelInvites, &p.EmailOnlyWhenOffline, &p.EmailDigestFrequency,
			&p.QuietHoursEnabled, &p.QuietHoursStart, &p.QuietHoursEnd, &p.QuietHoursTZ,
			&p.UnsubscribeToken, &p.CreatedAt, &p.UpdatedAt, &p.NotificationsPausedUntil, &p.NotificationsPausedUntil,
		)
		if err != nil {
			return nil, err
		}
		out[p.UserID] = &p
	}
	return out, rows.Err()
}

// SetPausedUntil pauses a person's notifications until a time, or resumes them
// (nil). The row must exist (domain LoadOrCreate makes it).
func SetPausedUntil(userID uuid.UUID, until *time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`UPDATE users_notification_preferences SET notifications_paused_until = $2, updated_at = NOW() WHERE user_id = $1`, userID, until)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UserNotificationPreference/SetPausedUntil err: %+v", err)
	}
	return err
}

// KnownTimeZone says whether Postgres can read the zone name. Go's zone list
// and Postgres's can differ, and the push gate converts times in SQL, where a
// name Postgres can't read would fail every lookup that touches the row.
func KnownTimeZone(name string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	var ok bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM pg_timezone_names WHERE name = $1)`, name).Scan(&ok)
	return ok, err
}
