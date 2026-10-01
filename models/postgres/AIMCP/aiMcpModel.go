// Package models (AIMCP) is the Postgres data-access layer for registered MCP
// (Model Context Protocol) servers (migration 90). An MCP server is an external
// HTTP endpoint that exposes tools; we introspect those tools and register them
// into the shared AI tool registry. This package only persists/reads server
// rows; the MCP client + registry wiring live in the business layer.
//
// Auth secrets (bearer token / header value) are encrypted at the application
// layer with the AI_CONFIG_KEK before insert and decrypted on read, so a DB
// dump never exposes them in plaintext.
package models

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	aiModels "github.com/akashc777/OneCamp/models/postgres/AI"
	"github.com/google/uuid"
)

// Transport + auth constants (aligned with the migration CHECKs).
const (
	TransportHTTP = "http"

	AuthNone   = "none"
	AuthBearer = "bearer"
	AuthHeader = "header"
)

// ValidTransport / ValidAuthType validate inputs before persisting.
func ValidTransport(t string) bool { return t == TransportHTTP }
func ValidAuthType(a string) bool {
	switch a {
	case AuthNone, AuthBearer, AuthHeader:
		return true
	default:
		return false
	}
}

// McpServer mirrors a row of the ai_mcp_servers table. AuthSecret is the
// decrypted secret in memory (never serialized to clients); the DB column is
// the encrypted blob.
type McpServer struct {
	Id             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	Description    *string   `json:"description,omitempty"`
	URL            string    `json:"url"`
	Transport      string    `json:"transport"`
	AuthType       string    `json:"auth_type"`
	AuthHeaderName *string   `json:"auth_header_name,omitempty"`
	AuthSecret     string    `json:"-"` // decrypted; never sent to clients
	HasAuthSecret  bool      `json:"has_auth_secret"`
	// AuthSecretUnreadable means a secret IS stored but could not be decrypted, so it cannot be
	// used and must be re-entered. Distinct from HasAuthSecret=false, which means none was ever
	// set — an admin needs the difference, because the usual cause is an AI_CONFIG_KEK change
	// rather than anything they did. Mirrors AIProvider.KeyUnreadable.
	AuthSecretUnreadable bool       `json:"auth_secret_unreadable,omitempty"`
	Enabled              bool       `json:"enabled"`
	ToolPrefix           string     `json:"tool_prefix"`
	ToolsCache           string     `json:"tools_cache"` // raw JSON array
	LastIntrospectedAt   *time.Time `json:"last_introspected_at,omitempty"`
	LastError            *string    `json:"last_error,omitempty"`
	CreatedBy            *uuid.UUID `json:"created_by,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
	DeletedAt            *time.Time `json:"deleted_at,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

const mcpColumns = `id, name, description, url, transport, auth_type, auth_header_name,
	auth_secret_encrypted, enabled, tool_prefix, tools_cache, last_introspected_at,
	last_error, created_by, created_at, updated_at, deleted_at`

func scanServer(s scanner) (*McpServer, error) {
	var m McpServer
	var description, authHeaderName, lastError sql.NullString
	var authSecret []byte
	var lastIntrospectedAt, deletedAt sql.NullTime
	var createdBy uuid.NullUUID

	err := s.Scan(
		&m.Id,
		&m.Name,
		&description,
		&m.URL,
		&m.Transport,
		&m.AuthType,
		&authHeaderName,
		&authSecret,
		&m.Enabled,
		&m.ToolPrefix,
		&m.ToolsCache,
		&lastIntrospectedAt,
		&lastError,
		&createdBy,
		&m.CreatedAt,
		&m.UpdatedAt,
		&deletedAt,
	)
	if err != nil {
		return nil, err
	}
	if description.Valid {
		m.Description = &description.String
	}
	if authHeaderName.Valid {
		m.AuthHeaderName = &authHeaderName.String
	}
	if lastError.Valid {
		m.LastError = &lastError.String
	}
	if lastIntrospectedAt.Valid {
		m.LastIntrospectedAt = &lastIntrospectedAt.Time
	}
	if deletedAt.Valid {
		m.DeletedAt = &deletedAt.Time
	}
	if createdBy.Valid {
		m.CreatedBy = &createdBy.UUID
	}
	if len(authSecret) > 0 {
		if plain, derr := aiModels.DecryptAPIKey(authSecret); derr == nil {
			m.HasAuthSecret = true
			m.AuthSecret = plain
		} else {
			// HasAuthSecret STAYS FALSE, and that is the fix.
			//
			// This used to set HasAuthSecret = true before attempting the decrypt, then log the
			// failure and carry on. The row therefore claimed to hold a usable credential while
			// AuthSecret was empty, and the registry went on to register the server's tools —
			// beta logged "decrypt auth secret failed" immediately followed by "registered 44
			// tool(s) from 1 server(s)". Every subsequent call to that server would have gone out
			// with an empty auth header: either refused by the remote for a reason that points at
			// the wrong thing, or, if the remote is permissive, executed with no authentication at
			// all against an external endpoint carrying workspace data.
			//
			// An undecryptable secret is exactly as unusable as no secret, so it is reported as
			// none. AuthSecretUnreadable carries the difference an operator needs: the secret is
			// present in the row and must be re-entered, rather than never having been set. The
			// usual cause is an AI_CONFIG_KEK change, which also makes every AI provider key
			// unreadable — see models/postgres/AI.scanProvider, which handles it the same way.
			m.AuthSecretUnreadable = true
			// ONCE PER SERVER PER PROCESS, not once per row read.
			//
			// scanServer runs for every row of every listing, so an admin sitting on the MCP
			// settings page produced a fresh ERROR line per refresh — beta logged three in eight
			// minutes for one server, and those lines are batched to the collector. The condition
			// is static until someone re-enters the secret, so repeating it adds no information.
			//
			// The severity stays ERROR: capability is being withheld and an admin must act. What
			// changes is that it is said once. The registry reports it again when it drops the
			// server's tools, and NewClient again if something tries to call it — both of those
			// are events, so they are the right places to be repetitive.
			if _, alreadyLogged := unreadableSecretLogged.LoadOrStore(m.Id, struct{}{}); !alreadyLogged {
				helpers.LogErrorWithContext(context.Background(),
					"models/AIMCP: auth secret for server %s cannot be decrypted (usually an "+
						"AI_CONFIG_KEK change); the server is treated as unusable until the secret "+
						"is re-entered: %v", m.Id, derr)
			}
		}
	}
	if strings.TrimSpace(m.ToolsCache) == "" {
		m.ToolsCache = "[]"
	}
	return &m, nil
}

// encryptSecret encrypts a non-empty secret for storage, or returns nil for an
// empty secret (auth_type none, or "leave unchanged" handled by the caller).
func encryptSecret(secret string) ([]byte, error) {
	if strings.TrimSpace(secret) == "" {
		return nil, nil
	}
	return aiModels.EncryptAPIKey(secret)
}

// CreateServer inserts a new MCP server and returns the generated id. The
// plaintext AuthSecret on the struct is encrypted here.
func CreateServer(ctx context.Context, m *McpServer) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	enc, err := encryptSecret(m.AuthSecret)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP CreateServer encrypt err: %+v", err)
		return uuid.Nil, err
	}
	if strings.TrimSpace(m.ToolsCache) == "" {
		m.ToolsCache = "[]"
	}
	id := uuid.New()
	const q = `INSERT INTO ai_mcp_servers
		(id, name, description, url, transport, auth_type, auth_header_name,
		 auth_secret_encrypted, enabled, tool_prefix, tools_cache, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	_, err = postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, m.Name, m.Description, m.URL, m.Transport, m.AuthType, m.AuthHeaderName,
		enc, m.Enabled, m.ToolPrefix, m.ToolsCache, m.CreatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP CreateServer err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// UpdateServer updates editable fields. When updateSecret is false the stored
// secret is left unchanged (so the UI can omit it on edit); when true, an empty
// AuthSecret clears it.
func UpdateServer(ctx context.Context, m *McpServer, updateSecret bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(m.ToolsCache) == "" {
		m.ToolsCache = "[]"
	}
	if updateSecret {
		enc, err := encryptSecret(m.AuthSecret)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/AIMCP UpdateServer encrypt err: %+v", err)
			return err
		}
		const q = `UPDATE ai_mcp_servers
			SET name=$2, description=$3, url=$4, transport=$5, auth_type=$6,
			    auth_header_name=$7, auth_secret_encrypted=$8, enabled=$9,
			    tool_prefix=$10, updated_at=NOW()
			WHERE id=$1 AND deleted_at IS NULL`
		res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
			m.Id, m.Name, m.Description, m.URL, m.Transport, m.AuthType,
			m.AuthHeaderName, enc, m.Enabled, m.ToolPrefix)
		if err != nil {
			helpers.LogErrorWithContext(ctx, "models/AIMCP UpdateServer err: %+v", err)
			return err
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			return sql.ErrNoRows
		}
		// The secret was just replaced, so any earlier "cannot be decrypted" report is spent.
		// Clearing the guard means a FUTURE failure on this same server is announced again rather
		// than swallowed by a flag that was set before the admin fixed it.
		forgetUnreadableSecret(m.Id)
		return nil
	}

	const q = `UPDATE ai_mcp_servers
		SET name=$2, description=$3, url=$4, transport=$5, auth_type=$6,
		    auth_header_name=$7, enabled=$8, tool_prefix=$9, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		m.Id, m.Name, m.Description, m.URL, m.Transport, m.AuthType,
		m.AuthHeaderName, m.Enabled, m.ToolPrefix)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP UpdateServer(no-secret) err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetEnabled toggles a server's enabled flag.
func SetEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_mcp_servers SET enabled=$2, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, enabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP SetEnabled err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SaveIntrospection stores the latest tool list (or error) from a connect probe.
func SaveIntrospection(ctx context.Context, id uuid.UUID, toolsJSON, errMsg string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	if strings.TrimSpace(toolsJSON) == "" {
		toolsJSON = "[]"
	}
	var errArg interface{}
	if errMsg != "" {
		errArg = errMsg
	}
	const q = `UPDATE ai_mcp_servers
		SET tools_cache=$2, last_introspected_at=NOW(), last_error=$3, updated_at=NOW()
		WHERE id=$1 AND deleted_at IS NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, toolsJSON, errArg)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP SaveIntrospection err: %+v", err)
		return err
	}
	return nil
}

// SoftDeleteServer marks a server deleted (and disabled).
func SoftDeleteServer(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `UPDATE ai_mcp_servers SET deleted_at=NOW(), enabled=false, updated_at=NOW() WHERE id=$1 AND deleted_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP SoftDeleteServer err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetServerByID returns a single non-deleted server, or (nil, nil) if absent.
func GetServerByID(ctx context.Context, id uuid.UUID) (*McpServer, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + mcpColumns + ` FROM ai_mcp_servers WHERE id=$1 AND deleted_at IS NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id)
	m, err := scanServer(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP GetServerByID err: %+v", err)
		return nil, err
	}
	return m, nil
}

// ListServers returns all non-deleted servers, newest first.
func ListServers(ctx context.Context) ([]*McpServer, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + mcpColumns + ` FROM ai_mcp_servers WHERE deleted_at IS NULL ORDER BY created_at DESC`
	return queryServers(dbctx, q)
}

// ListEnabledServers returns enabled, non-deleted servers (registry hot path).
func ListEnabledServers(ctx context.Context) ([]*McpServer, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `SELECT ` + mcpColumns + ` FROM ai_mcp_servers WHERE enabled=true AND deleted_at IS NULL ORDER BY created_at DESC`
	return queryServers(dbctx, q)
}

func queryServers(ctx context.Context, q string, args ...interface{}) ([]*McpServer, error) {
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(ctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/AIMCP queryServers err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*McpServer
	for rows.Next() {
		m, scanErr := scanServer(rows)
		if scanErr != nil {
			helpers.LogErrorWithContext(ctx, "models/AIMCP queryServers scan err: %+v", scanErr)
			return nil, scanErr
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// unreadableSecretLogged remembers which servers have already had their undecryptable secret
// reported, so a static condition is not re-announced on every row read.
//
// Process-scoped on purpose: a restart should say it again, because a restart is when an operator
// is most likely to be reading the logs and the state may have been fixed in between. Cleared
// whenever a secret is successfully written, so re-entering one restores the ability to complain
// if it breaks again.
var unreadableSecretLogged sync.Map

// forgetUnreadableSecret lets the write path clear the once-only guard for a server, so a later
// failure on the same id is reported again rather than silently swallowed.
func forgetUnreadableSecret(id uuid.UUID) { unreadableSecretLogged.Delete(id) }
