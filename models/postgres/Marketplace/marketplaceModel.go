// Package models (Marketplace) is the Postgres data-access layer for shareable
// templates (agents, workflows, tables) (migration 93, simplified in 95). The
// business layer enforces permissions, instantiates templates on install, and
// computes the visible payload shape.
package models

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Kinds of marketplace templates.
const (
	KindAgent    = "agent"
	KindWorkflow = "workflow"
	KindTable    = "table"
)

// ValidKind reports whether k is a supported template kind.
func ValidKind(k string) bool {
	return k == KindAgent || k == KindWorkflow || k == KindTable
}

// Template mirrors a row of marketplace_templates.
type Template struct {
	Id          uuid.UUID  `json:"id"`
	Kind        string     `json:"kind"`
	Name        string     `json:"name"`
	Description *string    `json:"description,omitempty"`
	Icon        *string    `json:"icon,omitempty"`
	Payload     string     `json:"payload"` // raw JSON
	CreatedBy   uuid.UUID  `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"-"`

	// AuthorName is the publisher's username, joined for display.
	AuthorName string `json:"author_name,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

// ListColumns selects template columns plus the author's name. Exported so the
// domain layer can compose the same scan-coupled projection when building the
// (dynamic) list query, keeping a single source of truth for the column order
// that scanTemplate relies on.
const ListColumns = `t.id, t.kind, t.name, t.description, t.icon, t.payload,
	t.created_by, t.created_at, t.updated_at, COALESCE(u.username, '')`

// ListJoins is the join set the projection needs (author display name).
const ListJoins = `
	LEFT JOIN users u ON u.id = t.created_by`

func scanTemplate(s scanner) (*Template, error) {
	var t Template
	var desc, icon sql.NullString
	if err := s.Scan(&t.Id, &t.Kind, &t.Name, &desc, &icon, &t.Payload,
		&t.CreatedBy, &t.CreatedAt, &t.UpdatedAt, &t.AuthorName); err != nil {
		return nil, err
	}
	if desc.Valid {
		t.Description = &desc.String
	}
	if icon.Valid {
		t.Icon = &icon.String
	}
	if strings.TrimSpace(t.Payload) == "" {
		t.Payload = "{}"
	}
	return &t, nil
}

// CreateTemplate inserts a new template and returns its id.
func CreateTemplate(ctx context.Context, kind, name string, description, icon *string, payloadJSON string, createdBy uuid.UUID) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if strings.TrimSpace(payloadJSON) == "" {
		payloadJSON = "{}"
	}
	id := uuid.New()
	const q = `INSERT INTO marketplace_templates (id, kind, name, description, icon, payload, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, kind, name, description, icon, payloadJSON, createdBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateTemplate err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// ExecTemplates runs a pre-built template-list query (see domain/Marketplace,
// which owns the optional kind filter + ordering) and scans the rows.
func ExecTemplates(ctx context.Context, query string, args []any) ([]*Template, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, query, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ExecTemplates err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Template
	for rows.Next() {
		t, scanErr := scanTemplate(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// GetTemplate returns a listed template by id, or (nil, nil) if not found.
func GetTemplate(ctx context.Context, id uuid.UUID) (*Template, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + ListColumns + ` FROM marketplace_templates t` + ListJoins + `
		WHERE t.id=$1 AND t.deleted_at IS NULL`
	t, err := scanTemplate(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetTemplate err: %+v", err)
		return nil, err
	}
	return t, nil
}

// SoftDeleteTemplate unlists a template the user owns (or admin).
func SoftDeleteTemplate(ctx context.Context, id, createdBy uuid.UUID, isAdmin bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `UPDATE marketplace_templates SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	args := []any{id}
	if !isAdmin {
		q += ` AND created_by=$2`
		args = append(args, createdBy)
	}
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteTemplate err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
