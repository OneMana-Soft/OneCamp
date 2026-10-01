// Package models (SlashCommand) is the Postgres data-access layer for the
// user-facing slash command registry and installed apps. Built-in commands
// have AppId == nil; app-provided commands link to an apps row. OAuth tokens
// for installed apps live in the existing `integrations` table.
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

// Exec modes.
const (
	ExecInline      = "inline"      // resolves synchronously server-side
	ExecInteractive = "interactive" // returns an ephemeral Block Kit card
	ExecDeferred    = "deferred"    // enqueues a scheduled job (/remind)
	ExecExternal    = "external"    // forwards to an app handler_url
)

// App kinds.
const (
	AppKindBuiltin  = "builtin"
	AppKindExternal = "external"
	AppKindOAuth    = "oauth"
)

// App is an installed application that can provide commands.
type App struct {
	Id            uuid.UUID  `json:"id"`
	Slug          string     `json:"slug"`
	Name          string     `json:"name"`
	Description   *string    `json:"description,omitempty"`
	IconUrl       *string    `json:"icon_url,omitempty"`
	Kind          string     `json:"kind"`
	SigningSecret *string    `json:"-"` // never serialize the secret
	HandlerUrl    *string    `json:"handler_url,omitempty"`
	OAuthConfig   *string    `json:"oauth_config,omitempty"`
	Config        *string    `json:"config,omitempty"`
	IsEnabled     bool       `json:"is_enabled"`
	InstalledBy   *uuid.UUID `json:"installed_by,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

// SlashCommand is a single command in the catalog.
type SlashCommand struct {
	Id            uuid.UUID  `json:"id"`
	Command       string     `json:"command"` // stored without leading slash
	AppId         *uuid.UUID `json:"app_id,omitempty"`
	Description   string     `json:"description"`
	UsageHint     *string    `json:"usage_hint,omitempty"`
	ExecMode      string     `json:"exec_mode"`
	HandlerUrl    *string    `json:"handler_url,omitempty"`
	ScopeType     string     `json:"scope_type"`
	ScopeEntityId *uuid.UUID `json:"scope_entity_id,omitempty"`
	ResponseType  string     `json:"response_type"`
	IsBuiltin     bool       `json:"is_builtin"`
	IsEnabled     bool       `json:"is_enabled"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	// AppSlug / AppName / AppIconUrl are hydrated by JOIN reads for the catalog UI.
	AppSlug    *string `json:"app_slug,omitempty"`
	AppName    *string `json:"app_name,omitempty"`
	AppIconUrl *string `json:"app_icon_url,omitempty"`
}

type scanner interface {
	Scan(dest ...any) error
}

const cmdColumns = `sc.id, sc.command, sc.app_id, sc.description, sc.usage_hint, sc.exec_mode,
	sc.handler_url, sc.scope_type, sc.scope_entity_id, sc.response_type, sc.is_builtin,
	sc.is_enabled, sc.created_at, sc.updated_at, a.slug, a.name, a.icon_url`

func scanCommand(s scanner) (*SlashCommand, error) {
	var c SlashCommand
	var appId, scopeEntityId uuid.NullUUID
	var usageHint, handlerUrl, appSlug, appName, appIconUrl sql.NullString

	err := s.Scan(
		&c.Id, &c.Command, &appId, &c.Description, &usageHint, &c.ExecMode,
		&handlerUrl, &c.ScopeType, &scopeEntityId, &c.ResponseType, &c.IsBuiltin,
		&c.IsEnabled, &c.CreatedAt, &c.UpdatedAt, &appSlug, &appName, &appIconUrl,
	)
	if err != nil {
		return nil, err
	}
	if appId.Valid {
		c.AppId = &appId.UUID
	}
	if scopeEntityId.Valid {
		c.ScopeEntityId = &scopeEntityId.UUID
	}
	if usageHint.Valid {
		c.UsageHint = &usageHint.String
	}
	if handlerUrl.Valid {
		c.HandlerUrl = &handlerUrl.String
	}
	if appSlug.Valid {
		c.AppSlug = &appSlug.String
	}
	if appName.Valid {
		c.AppName = &appName.String
	}
	if appIconUrl.Valid {
		c.AppIconUrl = &appIconUrl.String
	}
	return &c, nil
}

// ListCommandsForScopes returns every enabled command visible to a user, given
// the org-level scope plus the user's team and channel scope-entity ids. This
// is the catalog the composer typeahead consumes. Built-in (org) commands are
// always included; team/channel commands only when the entity id matches.
func ListCommandsForScopes(ctx context.Context, teamIDs []uuid.UUID, channelIDs []uuid.UUID) ([]*SlashCommand, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	// Build the scope predicate. org commands always; team/channel commands
	// when their entity id is in the user's set. We pass entity ids as
	// uuid[] text literals to avoid driver array marshalling complexity.
	q := `
		SELECT ` + cmdColumns + `
		FROM slash_commands sc
		LEFT JOIN apps a ON a.id = sc.app_id AND a.deleted_at IS NULL
		WHERE sc.deleted_at IS NULL
		  AND sc.is_enabled = true
		  AND (a.id IS NULL OR a.is_enabled = true)
		  AND (
		        sc.scope_type = 'org'
		     OR (sc.scope_type = 'team'    AND sc.scope_entity_id = ANY($1::uuid[]))
		     OR (sc.scope_type = 'channel' AND sc.scope_entity_id = ANY($2::uuid[]))
		  )
		ORDER BY sc.is_builtin DESC, sc.command ASC`

	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, uuidArrayLiteral(teamIDs), uuidArrayLiteral(channelIDs))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListCommandsForScopes err: %+v", err)
		return nil, err
	}
	defer rows.Close()

	var out []*SlashCommand
	for rows.Next() {
		c, scanErr := scanCommand(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ResolveCommand finds the best matching command for a name within the user's
// visible scopes. Channel scope beats team beats org so a channel can override
// a global command. Built-ins win ties (ordered last in the OR via priority).
func ResolveCommand(ctx context.Context, command string, teamIDs []uuid.UUID, channelIDs []uuid.UUID) (*SlashCommand, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	q := `
		SELECT ` + cmdColumns + `
		FROM slash_commands sc
		LEFT JOIN apps a ON a.id = sc.app_id AND a.deleted_at IS NULL
		WHERE sc.deleted_at IS NULL
		  AND sc.is_enabled = true
		  AND sc.command = $1
		  AND (a.id IS NULL OR a.is_enabled = true)
		  AND (
		        sc.scope_type = 'org'
		     OR (sc.scope_type = 'team'    AND sc.scope_entity_id = ANY($2::uuid[]))
		     OR (sc.scope_type = 'channel' AND sc.scope_entity_id = ANY($3::uuid[]))
		  )
		ORDER BY CASE sc.scope_type WHEN 'channel' THEN 0 WHEN 'team' THEN 1 ELSE 2 END ASC
		LIMIT 1`

	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, command, uuidArrayLiteral(teamIDs), uuidArrayLiteral(channelIDs))
	c, err := scanCommand(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ResolveCommand err: %+v", err)
		return nil, err
	}
	return c, nil
}

// --- App + command CRUD (admin) ---

// UpsertCommand inserts or updates a command by (command, scope_type, scope_entity_id).
func UpsertCommand(ctx context.Context, c *SlashCommand) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const q = `
		INSERT INTO slash_commands
			(command, app_id, description, usage_hint, exec_mode, handler_url,
			 scope_type, scope_entity_id, response_type, is_builtin, is_enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (command, scope_type, COALESCE(scope_entity_id, '00000000-0000-0000-0000-000000000000'::uuid))
		WHERE deleted_at IS NULL
		DO UPDATE SET
			app_id = EXCLUDED.app_id,
			description = EXCLUDED.description,
			usage_hint = EXCLUDED.usage_hint,
			exec_mode = EXCLUDED.exec_mode,
			handler_url = EXCLUDED.handler_url,
			response_type = EXCLUDED.response_type,
			is_enabled = EXCLUDED.is_enabled,
			updated_at = NOW()`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		c.Command, c.AppId, c.Description, c.UsageHint, c.ExecMode, c.HandlerUrl,
		c.ScopeType, c.ScopeEntityId, c.ResponseType, c.IsBuiltin, c.IsEnabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpsertCommand err: %+v", err)
	}
	return err
}

// CreateApp inserts a new installed app and returns its id.
func CreateApp(ctx context.Context, a *App) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO apps
		(id, slug, name, description, icon_url, kind, signing_secret, handler_url, oauth_config, config, is_enabled, installed_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		id, a.Slug, a.Name, a.Description, a.IconUrl, a.Kind, a.SigningSecret,
		a.HandlerUrl, a.OAuthConfig, a.Config, a.IsEnabled, a.InstalledBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateApp err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

const appColumns = `id, slug, name, description, icon_url, kind, signing_secret, handler_url,
	oauth_config, config, is_enabled, installed_by, created_at, updated_at`

func scanApp(s scanner) (*App, error) {
	var a App
	var description, iconUrl, signingSecret, handlerUrl, oauthConfig, config sql.NullString
	var installedBy uuid.NullUUID
	err := s.Scan(
		&a.Id, &a.Slug, &a.Name, &description, &iconUrl, &a.Kind, &signingSecret, &handlerUrl,
		&oauthConfig, &config, &a.IsEnabled, &installedBy, &a.CreatedAt, &a.UpdatedAt,
	)
	if err != nil {
		return nil, err
	}
	if description.Valid {
		a.Description = &description.String
	}
	if iconUrl.Valid {
		a.IconUrl = &iconUrl.String
	}
	if signingSecret.Valid {
		a.SigningSecret = &signingSecret.String
	}
	if handlerUrl.Valid {
		a.HandlerUrl = &handlerUrl.String
	}
	if oauthConfig.Valid {
		a.OAuthConfig = &oauthConfig.String
	}
	if config.Valid {
		a.Config = &config.String
	}
	if installedBy.Valid {
		a.InstalledBy = &installedBy.UUID
	}
	return &a, nil
}

// GetAppBySlug returns an installed app by its slug.
func GetAppBySlug(ctx context.Context, slug string) (*App, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + appColumns + ` FROM apps WHERE slug = $1 AND deleted_at IS NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, slug)
	a, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAppBySlug err: %+v", err)
		return nil, err
	}
	return a, nil
}

// GetAppByID returns an installed app by id.
func GetAppByID(ctx context.Context, id uuid.UUID) (*App, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + appColumns + ` FROM apps WHERE id = $1 AND deleted_at IS NULL`
	row := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id)
	a, err := scanApp(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAppByID err: %+v", err)
		return nil, err
	}
	return a, nil
}

// ListApps returns all installed apps for the admin directory.
func ListApps(ctx context.Context) ([]*App, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + appColumns + ` FROM apps WHERE deleted_at IS NULL ORDER BY name ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListApps err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, scanErr := scanApp(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAppEnabled toggles an app on/off.
func SetAppEnabled(ctx context.Context, id uuid.UUID, enabled bool) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE apps SET is_enabled = $2, updated_at = NOW() WHERE id = $1 AND deleted_at IS NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, enabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAppEnabled err: %+v", err)
	}
	return err
}

// SoftDeleteApp soft-deletes an app (CASCADE removes its commands via FK).
func SoftDeleteApp(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	now := time.Now()
	const q = `UPDATE apps SET deleted_at = $2, updated_at = $2 WHERE id = $1 AND deleted_at IS NULL`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, now); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteApp err: %+v", err)
		return err
	}
	// Soft-delete the app's commands too (FK is hard-cascade, but we keep
	// command rows soft-deleted for audit symmetry with the rest of the schema).
	const qc = `UPDATE slash_commands SET deleted_at = $2, updated_at = $2 WHERE app_id = $1 AND deleted_at IS NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, qc, id, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SoftDeleteApp (commands) err: %+v", err)
	}
	return err
}

// UpdateApp applies partial updates to an app. Non-nil fields are written.
func UpdateApp(ctx context.Context, id uuid.UUID, fields map[string]interface{}) error {
	if len(fields) == 0 {
		return nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	setClauses := make([]string, 0, len(fields)+1)
	args := make([]interface{}, 0, len(fields)+2)
	i := 1
	for col, val := range fields {
		setClauses = append(setClauses, col+" = $"+itoa(i))
		args = append(args, val)
		i++
	}
	setClauses = append(setClauses, "updated_at = NOW()")
	q := "UPDATE apps SET " + joinComma(setClauses) + " WHERE id = $" + itoa(i) + " AND deleted_at IS NULL"
	args = append(args, id)

	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateApp err: %+v", err)
	}
	return err
}

// ListCommandsByApp returns every (non-deleted) command for an app.
func ListCommandsByApp(ctx context.Context, appID uuid.UUID) ([]*SlashCommand, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `
		SELECT ` + cmdColumns + `
		FROM slash_commands sc
		LEFT JOIN apps a ON a.id = sc.app_id
		WHERE sc.app_id = $1 AND sc.deleted_at IS NULL
		ORDER BY sc.command ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, appID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListCommandsByApp err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*SlashCommand
	for rows.Next() {
		c, scanErr := scanCommand(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteCommandsByApp soft-deletes all commands for an app (used when replacing
// an app's command set).
func DeleteCommandsByApp(ctx context.Context, appID uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE slash_commands SET deleted_at = NOW(), updated_at = NOW()
		WHERE app_id = $1 AND deleted_at IS NULL`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, appID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/DeleteCommandsByApp err: %+v", err)
	}
	return err
}

// CreateAppCommand inserts a command linked to an app.
func CreateAppCommand(ctx context.Context, c *SlashCommand) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `
		INSERT INTO slash_commands
			(command, app_id, description, usage_hint, exec_mode, handler_url,
			 scope_type, scope_entity_id, response_type, is_builtin, is_enabled)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,false,$10)
		ON CONFLICT (command, scope_type, COALESCE(scope_entity_id, '00000000-0000-0000-0000-000000000000'::uuid))
		WHERE deleted_at IS NULL
		DO UPDATE SET
			app_id = EXCLUDED.app_id,
			description = EXCLUDED.description,
			usage_hint = EXCLUDED.usage_hint,
			exec_mode = EXCLUDED.exec_mode,
			handler_url = EXCLUDED.handler_url,
			response_type = EXCLUDED.response_type,
			is_enabled = EXCLUDED.is_enabled,
			deleted_at = NULL,
			updated_at = NOW()`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q,
		c.Command, c.AppId, c.Description, c.UsageHint, c.ExecMode, c.HandlerUrl,
		c.ScopeType, c.ScopeEntityId, c.ResponseType, c.IsEnabled)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/CreateAppCommand err: %+v", err)
	}
	return err
}

// uuidArrayLiteral builds a Postgres uuid[] text literal ("{u1,u2}") from a
// slice. An empty slice yields "{}", which `= ANY` treats as "matches nothing".
func uuidArrayLiteral(ids []uuid.UUID) string {
	if len(ids) == 0 {
		return "{}"
	}
	buf := make([]byte, 0, len(ids)*38+2)
	buf = append(buf, '{')
	for i, id := range ids {
		if i > 0 {
			buf = append(buf, ',')
		}
		buf = append(buf, id.String()...)
	}
	buf = append(buf, '}')
	return string(buf)
}

// itoa converts a small positive int to its decimal string (placeholder index).
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// joinComma joins clauses with ", ".
func joinComma(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}
