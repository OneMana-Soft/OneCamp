package models

import (
	"context"
	"encoding/json"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// TaskView is a saved set of task-list filters, sort and columns (migration 176).
type TaskView struct {
	Id        uuid.UUID       `json:"id"`
	Scope     string          `json:"scope"`
	Name      string          `json:"name"`
	State     json.RawMessage `json:"state"`
	UpdatedAt time.Time       `json:"updated_at"`
}

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

// List returns the person's views for a scope, by name.
func List(userID uuid.UUID, scope string) ([]TaskView, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT id, scope, name, state, updated_at FROM task_views
		 WHERE user_id = $1 AND scope = $2 ORDER BY lower(name)`, userID, scope)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TaskView{}
	for rows.Next() {
		var v TaskView
		if err := rows.Scan(&v.Id, &v.Scope, &v.Name, &v.State, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Count is how many views the person has in a scope.
func Count(userID uuid.UUID, scope string) (int, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var n int
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM task_views WHERE user_id = $1 AND scope = $2`, userID, scope).Scan(&n)
	return n, err
}

// Save stores a view, replacing the person's view of the same name in the scope.
func Save(userID uuid.UUID, scope, name string, state json.RawMessage) (*TaskView, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var v TaskView
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO task_views (user_id, scope, name, state) VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id, scope, name) DO UPDATE SET state = EXCLUDED.state, updated_at = NOW()
		RETURNING id, scope, name, state, updated_at`, userID, scope, name, []byte(state)).
		Scan(&v.Id, &v.Scope, &v.Name, &v.State, &v.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// Exists says whether the person already has a view of this name in the scope.
func Exists(userID uuid.UUID, scope, name string) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	var ok bool
	err := postgresInit.DBConn.SqlDB.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM task_views WHERE user_id = $1 AND scope = $2 AND name = $3)`,
		userID, scope, name).Scan(&ok)
	return ok, err
}

// Delete removes one of the person's views; false when it wasn't theirs.
func Delete(userID, id uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx,
		`DELETE FROM task_views WHERE id = $1 AND user_id = $2`, id, userID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
