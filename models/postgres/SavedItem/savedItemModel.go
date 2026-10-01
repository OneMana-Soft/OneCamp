// Package models (SavedItem) stores what a member saved for later
// (migration 166). Every query is scoped to the member: nobody else can see,
// change or be reminded of another member's saved items.
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// SavedItem is one saved thing.
type SavedItem struct {
	ID         uuid.UUID  `json:"id"`
	UserID     uuid.UUID  `json:"-"`
	ItemType   string     `json:"item_type"`
	ItemID     string     `json:"item_id"`
	Link       string     `json:"link"`
	Title      string     `json:"title"`
	Context    string     `json:"context"`
	RemindAt   *time.Time `json:"remind_at,omitempty"`
	RemindedAt *time.Time `json:"reminded_at,omitempty"`
	DoneAt     *time.Time `json:"done_at,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
}

// ErrNotFound is returned when the item does not exist for this member.
var ErrNotFound = errors.New("saved item not found")

const columns = `id, user_id, item_type, item_id, link, title, context, remind_at, reminded_at, done_at, created_at, updated_at`

func scan(row interface{ Scan(...any) error }) (*SavedItem, error) {
	var s SavedItem
	var remindAt, remindedAt, doneAt sql.NullTime
	if err := row.Scan(&s.ID, &s.UserID, &s.ItemType, &s.ItemID, &s.Link, &s.Title, &s.Context, &remindAt, &remindedAt, &doneAt, &s.CreatedAt, &s.UpdatedAt); err != nil {
		return nil, err
	}
	if remindAt.Valid {
		s.RemindAt = &remindAt.Time
	}
	if remindedAt.Valid {
		s.RemindedAt = &remindedAt.Time
	}
	if doneAt.Valid {
		s.DoneAt = &doneAt.Time
	}
	return &s, nil
}

// Save stores an item, or refreshes it if the member saved the same thing
// before: its preview and link are updated, it is reopened if it was done, and
// its reminder is replaced by the one given (nil keeps no reminder).
func Save(ctx context.Context, s *SavedItem) (*SavedItem, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	row := postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO saved_items (id, user_id, item_type, item_id, link, title, context, remind_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (user_id, item_type, item_id) DO UPDATE SET
			link = EXCLUDED.link, title = EXCLUDED.title, context = EXCLUDED.context,
			remind_at = EXCLUDED.remind_at, reminded_at = NULL, done_at = NULL, updated_at = NOW()
		RETURNING `+columns,
		uuid.New(), s.UserID, s.ItemType, s.ItemID, s.Link, s.Title, s.Context, s.RemindAt)
	out, err := scan(row)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SavedItem Save err: %+v", err)
	}
	return out, err
}

// Get returns one of the member's items.
func Get(ctx context.Context, userID, id uuid.UUID) (*SavedItem, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+columns+` FROM saved_items WHERE id = $1 AND user_id = $2`, id, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// GetByID returns an item by id alone, for the reminder job, which runs
// without a member's session and checks the owner itself.
func GetByID(ctx context.Context, id uuid.UUID) (*SavedItem, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c,
		`SELECT `+columns+` FROM saved_items WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// List returns the member's open items (done=false) or finished ones.
// Open items that are due come first, soonest due first, then the rest newest
// first: what needs the member now is at the top.
func List(ctx context.Context, userID uuid.UUID, done bool, now time.Time, limit int) ([]*SavedItem, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	query := `SELECT ` + columns + ` FROM saved_items WHERE user_id = $1 AND done_at IS NULL
		ORDER BY (remind_at IS NOT NULL AND remind_at <= $2) DESC,
		         CASE WHEN remind_at IS NOT NULL AND remind_at <= $2 THEN remind_at END ASC,
		         created_at DESC
		LIMIT $3`
	args := []any{userID, now, limit}
	if done {
		query = `SELECT ` + columns + ` FROM saved_items WHERE user_id = $1 AND done_at IS NOT NULL
			ORDER BY done_at DESC LIMIT $2`
		args = []any{userID, limit}
	}
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SavedItem List err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	out := []*SavedItem{}
	for rows.Next() {
		s, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// Counts reports how many open items the member has, and how many are due.
func Counts(ctx context.Context, userID uuid.UUID, now time.Time) (open, due int, err error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		SELECT COUNT(*), COUNT(*) FILTER (WHERE remind_at IS NOT NULL AND remind_at <= $2)
		FROM saved_items WHERE user_id = $1 AND done_at IS NULL`, userID, now).Scan(&open, &due)
	return
}

// SetReminder replaces an item's reminder (nil clears it) and reopens it.
func SetReminder(ctx context.Context, userID, id uuid.UUID, remindAt *time.Time) (*SavedItem, error) {
	return update(ctx, `UPDATE saved_items SET remind_at = $3, reminded_at = NULL, done_at = NULL, updated_at = NOW()
		WHERE id = $1 AND user_id = $2 RETURNING `+columns, id, userID, remindAt)
}

// SetDone marks an item finished, or reopens it. Finishing clears the
// reminder, so a finished item never bubbles up again.
func SetDone(ctx context.Context, userID, id uuid.UUID, done bool) (*SavedItem, error) {
	if done {
		return update(ctx, `UPDATE saved_items SET done_at = NOW(), remind_at = NULL, updated_at = NOW()
			WHERE id = $1 AND user_id = $2 RETURNING `+columns, id, userID)
	}
	return update(ctx, `UPDATE saved_items SET done_at = NULL, updated_at = NOW()
		WHERE id = $1 AND user_id = $2 RETURNING `+columns, id, userID)
}

// MarkReminded records that the reminder due at remindAt went out. It only
// matches while the item still has that reminder and is open, so a reminder
// changed or finished after the job was queued is not reported as sent.
func MarkReminded(ctx context.Context, id uuid.UUID, remindAt time.Time) (bool, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		UPDATE saved_items SET reminded_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND done_at IS NULL AND remind_at = $2 AND reminded_at IS NULL`, id, remindAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// Delete removes one of the member's items.
func Delete(ctx context.Context, userID, id uuid.UUID) error {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(c, `DELETE FROM saved_items WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func update(ctx context.Context, query string, args ...any) (*SavedItem, error) {
	c, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	out, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(c, query, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SavedItem update err: %+v", err)
	}
	return out, err
}
