// Package models (DataSource) is the Postgres data-access layer for external,
// read-only data-source connections (migration 126). A data source is an
// admin-configured connection to an external SQL database (Postgres to start)
// that an agent/assistant can query — read-only — the same governed way it
// queries native Tables.
//
// This package only persists and reads the CONNECTION CONFIG. The connection
// password is stored encrypted (base64 AES-256-GCM via helpers.EncryptSecret)
// and is never exposed by the row's JSON tag. The business layer adds
// validation, permission checks, and the actual (read-only) connector.
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

// Engine constants. The set of VALID engines is governed by the application
// (see business/DataSource connector registry), not a DB CHECK (migration 127),
// so adding an engine is a pure code change.
const (
	EnginePostgres = "postgres"
	EngineMySQL    = "mysql"
)

// DefaultPort returns the conventional port for an engine (0 if unknown).
func DefaultPort(engine string) int {
	switch engine {
	case EnginePostgres:
		return 5432
	case EngineMySQL:
		return 3306
	default:
		return 0
	}
}

// SSL mode constants (aligned with the migration CHECK; libpq semantics).
const (
	SSLDisable    = "disable"
	SSLRequire    = "require"
	SSLVerifyCA   = "verify-ca"
	SSLVerifyFull = "verify-full"
)

// Visibility constants (aligned with the migration CHECK). Mirrors Tables:
// private = creator + admins; workspace = any member may query.
const (
	VisibilityPrivate   = "private"
	VisibilityWorkspace = "workspace"
)

// ValidEngine / ValidSSLMode / ValidVisibility validate inputs. ValidEngine is
// the single source of truth for which engines are implemented.
func ValidEngine(e string) bool {
	switch e {
	case EnginePostgres, EngineMySQL:
		return true
	default:
		return false
	}
}

func ValidSSLMode(m string) bool {
	switch m {
	case SSLDisable, SSLRequire, SSLVerifyCA, SSLVerifyFull:
		return true
	default:
		return false
	}
}

func ValidVisibility(v string) bool {
	return v == VisibilityPrivate || v == VisibilityWorkspace
}

// DataSource mirrors a row of data_sources. PasswordEnc is deliberately NOT
// serialized (json:"-") so a decrypted or ciphertext credential can never leak
// through an API response; the FE only ever learns HasPassword via the adapter.
type DataSource struct {
	Id          uuid.UUID  `json:"id"`
	Name        string     `json:"name"`
	Engine      string     `json:"engine"`
	Host        string     `json:"host"`
	Port        int        `json:"port"`
	Database    string     `json:"database"`
	Username    string     `json:"username"`
	PasswordEnc *string    `json:"-"`
	SSLMode     string     `json:"ssl_mode"`
	Visibility  string     `json:"visibility"`
	Enabled     bool       `json:"enabled"`
	CreatedBy   uuid.UUID  `json:"created_by"`
	CreatedAt   time.Time  `json:"created_at"`
	UpdatedAt   time.Time  `json:"updated_at"`
	DeletedAt   *time.Time `json:"deleted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

const dataSourceColumns = `id, name, engine, host, port, database_name, username, password_enc, ssl_mode, visibility, enabled, created_by, created_at, updated_at, deleted_at`

func scanDataSource(s scanner) (*DataSource, error) {
	var d DataSource
	var passwordEnc sql.NullString
	var deletedAt sql.NullTime
	if err := s.Scan(&d.Id, &d.Name, &d.Engine, &d.Host, &d.Port, &d.Database, &d.Username,
		&passwordEnc, &d.SSLMode, &d.Visibility, &d.Enabled, &d.CreatedBy,
		&d.CreatedAt, &d.UpdatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if passwordEnc.Valid {
		d.PasswordEnc = &passwordEnc.String
	}
	if deletedAt.Valid {
		d.DeletedAt = &deletedAt.Time
	}
	return &d, nil
}

// Create inserts a new data source and returns the generated id.
func Create(ctx context.Context, d *DataSource) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO data_sources
		(id, name, engine, host, port, database_name, username, password_enc, ssl_mode, visibility, enabled, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, d.Name, d.Engine, d.Host, d.Port, d.Database, d.Username, d.PasswordEnc,
		d.SSLMode, d.Visibility, d.Enabled, d.CreatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DataSource Create err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// Update updates a data source's editable fields. The password is only touched
// when updatePassword is true (so "leave unchanged" is the default and an empty
// PasswordEnc doesn't wipe a stored credential by accident).
func Update(ctx context.Context, d *DataSource, updatePassword bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if updatePassword {
		const q = `UPDATE data_sources SET name=$2, engine=$3, host=$4, port=$5, database_name=$6,
			username=$7, password_enc=$8, ssl_mode=$9, visibility=$10, enabled=$11, updated_at=NOW()
			WHERE id=$1 AND deleted_at IS NULL`
		res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
			d.Id, d.Name, d.Engine, d.Host, d.Port, d.Database, d.Username, d.PasswordEnc,
			d.SSLMode, d.Visibility, d.Enabled)
		return affected(res, err, ctx)
	}
	const q = `UPDATE data_sources SET name=$2, engine=$3, host=$4, port=$5, database_name=$6,
		username=$7, ssl_mode=$8, visibility=$9, enabled=$10, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		d.Id, d.Name, d.Engine, d.Host, d.Port, d.Database, d.Username,
		d.SSLMode, d.Visibility, d.Enabled)
	return affected(res, err, ctx)
}

func affected(res sql.Result, err error, ctx context.Context) error {
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DataSource update err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetEnabled toggles a source's enabled flag without touching anything else.
func SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_sources SET enabled=$2, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, enabled)
	return affected(res, err, ctx)
}

// SoftDelete marks a source deleted.
func SoftDelete(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE data_sources SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	return affected(res, err, ctx)
}

// GetByID returns a single non-deleted data source, or (nil, nil) if absent.
func GetByID(ctx context.Context, id uuid.UUID) (*DataSource, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + dataSourceColumns + ` FROM data_sources WHERE id=$1 AND deleted_at IS NULL`
	d, err := scanDataSource(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DataSource GetByID err: %+v", err)
		return nil, err
	}
	return d, nil
}

// List returns all non-deleted data sources, newest first. Visibility filtering
// for the QUERY path is applied by the business layer per actor; this returns
// the full config list for the admin management surface.
func List(ctx context.Context) ([]*DataSource, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + dataSourceColumns + ` FROM data_sources WHERE deleted_at IS NULL ORDER BY created_at DESC LIMIT 500`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DataSource List err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*DataSource
	for rows.Next() {
		d, scanErr := scanDataSource(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// TrimName normalizes a display name (small shared helper for the business layer).
func TrimName(s string) string { return strings.TrimSpace(s) }
