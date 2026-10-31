package models

import (
	"context"
	"database/sql"
	"errors"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// AppIdentity is an installed app as an identity (migration 360; platform
// P-API, design/platform/API_CONTRACT.md §3.1): its bot user, the scopes its
// credentials may carry at most and the places it may reach, beside what the
// app record (App) already holds.
type AppIdentity struct {
	Id        uuid.UUID
	Slug      string
	Name      string
	IconUrl   *string
	Enabled   bool
	Deleted   bool
	BotUserId *uuid.UUID
	// Scopes and Audience are their columns' JSON.
	Scopes   string
	Audience string
	// AccessVersion is accessVersion's value as read: a change sent from it is
	// made only while the app still has it (SetAppAccess).
	AccessVersion string
}

// accessVersion is an app's access as one value: a hash of its scopes and its
// audience as Postgres writes their JSON, which jsonb keeps in one canonical
// form, so the same access always hashes alike.
const accessVersion = `md5(scopes::text || ' ' || audience::text)`

// GetAppIdentity reads an app as an identity, removed ones included (Deleted),
// or (nil, nil) when there is no such app.
func GetAppIdentity(ctx context.Context, id uuid.UUID) (*AppIdentity, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT id, slug, name, icon_url, is_enabled, deleted_at IS NOT NULL, bot_user_id, scopes, audience, ` + accessVersion + `
		FROM apps WHERE id=$1`
	var a AppIdentity
	var icon sql.NullString
	var bot uuid.NullUUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id).Scan(&a.Id, &a.Slug, &a.Name, &icon, &a.Enabled, &a.Deleted, &bot, &a.Scopes, &a.Audience, &a.AccessVersion)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetAppIdentity err: %+v", err)
		return nil, err
	}
	if icon.Valid {
		a.IconUrl = &icon.String
	}
	if bot.Valid {
		a.BotUserId = &bot.UUID
	}
	return &a, nil
}

// SetAppAccess stores what an app's credentials may carry and where it may
// reach, while the app is live and, given the version a change was made from
// (from), while its access is still that version: one UPDATE, so of two
// changes made from the same version only the first is taken. sql.ErrNoRows
// when nothing changed; "" changes it whatever it is.
func SetAppAccess(ctx context.Context, id uuid.UUID, scopesJSON, audienceJSON, from string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE apps SET scopes=$2::jsonb, audience=$3::jsonb, updated_at=NOW()
			WHERE id=$1 AND deleted_at IS NULL AND ($4::text = '' OR `+accessVersion+` = $4::text)`, id, scopesJSON, audienceJSON, from)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAppAccess err: %+v", err)
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAppBotUser records the app's bot user, once it is provisioned.
func SetAppBotUser(ctx context.Context, id, botUser uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx,
		`UPDATE apps SET bot_user_id=$2, updated_at=NOW() WHERE id=$1`, id, botUser); err != nil {
		helpers.LogErrorWithContext(ctx, "models/SetAppBotUser err: %+v", err)
		return err
	}
	return nil
}
