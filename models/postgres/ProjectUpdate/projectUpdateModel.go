// Package models (ProjectUpdate) stores projects' updates (migration 185).
package models

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// The healths an update can give, as Asana names them.
const (
	OnTrack  = "on_track"
	AtRisk   = "at_risk"
	OffTrack = "off_track"
	OnHold   = "on_hold"
	Done     = "done"
)

// Update is one project update.
type Update struct {
	Id               uuid.UUID `json:"id"`
	ProjectUUID      uuid.UUID `json:"project_uuid"`
	AuthorUUID       uuid.UUID `json:"author_uuid"`
	Health           string    `json:"health"`
	Body             string    `json:"body"`
	SharedWithClient bool      `json:"shared_with_client"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

// MaxList is the most updates one read returns.
const MaxList = 50

const columns = `id, project_uuid, author_uuid, health, body, shared_with_client, created_at, updated_at`

func withTimeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
}

func scan(row interface{ Scan(...any) error }) (*Update, error) {
	var u Update
	if err := row.Scan(&u.Id, &u.ProjectUUID, &u.AuthorUUID, &u.Health, &u.Body, &u.SharedWithClient, &u.CreatedAt, &u.UpdatedAt); err != nil {
		return nil, err
	}
	return &u, nil
}

// Add stores a new update.
func Add(projectID, authorID uuid.UUID, health, body string, shared bool) (*Update, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	return scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		INSERT INTO project_updates (project_uuid, author_uuid, health, body, shared_with_client) VALUES ($1, $2, $3, $4, $5)
		RETURNING `+columns, projectID, authorID, health, body, shared))
}

// Edit changes an update's health, text and sharing; nil when it is gone.
func Edit(id, projectID uuid.UUID, health, body string, shared bool) (*Update, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	u, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		UPDATE project_updates SET health = $3, body = $4, shared_with_client = $5, updated_at = NOW()
		 WHERE id = $1 AND project_uuid = $2 AND deleted_at IS NULL
		RETURNING `+columns, id, projectID, health, body, shared))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// Delete removes an update; false when it was already gone.
func Delete(id, projectID uuid.UUID) (bool, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(ctx, `
		UPDATE project_updates SET deleted_at = NOW() WHERE id = $1 AND project_uuid = $2 AND deleted_at IS NULL`, id, projectID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// Get is one live update of a project, or nil.
func Get(id, projectID uuid.UUID) (*Update, error) {
	ctx, cancel := withTimeout()
	defer cancel()
	u, err := scan(postgresInit.DBConn.SqlDB.QueryRowContext(ctx, `
		SELECT `+columns+` FROM project_updates WHERE id = $1 AND project_uuid = $2 AND deleted_at IS NULL`, id, projectID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

// List is a project's live updates, newest first, at most limit (capped at
// MaxList); with sharedOnly, only those shared with the client.
func List(projectID uuid.UUID, limit int, sharedOnly bool) ([]Update, error) {
	if limit <= 0 || limit > MaxList {
		limit = MaxList
	}
	ctx, cancel := withTimeout()
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, `
		SELECT `+columns+` FROM project_updates
		 WHERE project_uuid = $1 AND deleted_at IS NULL AND (NOT $2 OR shared_with_client)
		 ORDER BY created_at DESC LIMIT $3`, projectID, sharedOnly, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Update{}
	for rows.Next() {
		u, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *u)
	}
	return out, rows.Err()
}
