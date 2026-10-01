package models

// User reads for the SCIM surface.
//
// SEPARATE FROM models/postgres/User FOR ONE REASON: every user lookup in that package filters
// `deleted_at IS NULL`, which is right for the application and wrong here.
//
// A directory needs to see a DEACTIVATED user. SCIM represents deactivation as `active: false`, not as
// absence, and the practical consequence is concrete: users.email_id is NOT NULL UNIQUE and a
// soft-deleted row keeps its address, so re-creating a leaver fails on the constraint forever. The only
// way back is for the IdP to FIND that user and set active:true — and it can only find them if this
// layer returns them. Hiding them would make re-hiring impossible through the very integration whose
// job it is.
//
// The same queries exclude bot and external identities, which is the opposite judgement for the
// opposite reason: those are not directory identities at all. is_external marks attribution-only ghosts
// (unmapped GitHub commit authors, imported Slack users) that cannot sign in, and there are potentially
// thousands of them. Exposing them would present an IdP administrator with a user list mostly composed
// of things that are not people and cannot be provisioned.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// ScimUser is one directory identity, including deactivated ones.
type ScimUser struct {
	Id       uuid.UUID
	EmailID  string
	Username *string
	// DisplayName is the user's chosen full name where they have set one.
	DisplayName *string
	CreatedAt   time.Time
	UpdatedAt   time.Time
	// DeletedAt is nil for a live user. This is what SCIM's `active` is derived from; there is no
	// separate active column on users.
	DeletedAt *time.Time
}

const scimUserColumns = `id, email_id, username, display_name, created_at, updated_at, deleted_at`

// realPeopleOnly excludes the identities that are not directory-manageable. Written once and shared by
// every query below, because a filter that is applied to the list and forgotten on the count produces a
// totalResults an IdP will page past the end of.
const realPeopleOnly = ` AND is_external = false AND is_bot = false `

func scanScimUser(s scanner) (*ScimUser, error) {
	var u ScimUser
	var username, displayName sql.NullString
	var createdAt, updatedAt, deletedAt sql.NullTime
	if err := s.Scan(&u.Id, &u.EmailID, &username, &displayName, &createdAt, &updatedAt, &deletedAt); err != nil {
		return nil, err
	}
	if username.Valid {
		u.Username = &username.String
	}
	if displayName.Valid {
		u.DisplayName = &displayName.String
	}
	if createdAt.Valid {
		u.CreatedAt = createdAt.Time
	}
	if updatedAt.Valid {
		u.UpdatedAt = updatedAt.Time
	}
	if deletedAt.Valid {
		u.DeletedAt = &deletedAt.Time
	}
	return &u, nil
}

// GetScimUserByID returns one identity by uuid, deactivated or not, or (nil, nil) if there is none.
func GetScimUserByID(ctx context.Context, id uuid.UUID) (*ScimUser, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + scimUserColumns + ` FROM users WHERE id=$1` + realPeopleOnly
	u, err := scanScimUser(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetScimUserByID err: %+v", err)
		return nil, err
	}
	return u, nil
}

// GetScimUserByEmail returns one identity by email address, deactivated or not.
//
// Case-insensitive, because an IdP does not guarantee the casing it sends and email addresses are not
// case-sensitive in practice. A case-sensitive match here would let "J.Smith@corp.com" create a second
// account for someone already provisioned as "j.smith@corp.com" — two accounts for one person, which is
// the specific failure SCIM exists to prevent.
func GetScimUserByEmail(ctx context.Context, email string) (*ScimUser, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + scimUserColumns + ` FROM users WHERE LOWER(email_id)=LOWER($1)` + realPeopleOnly
	u, err := scanScimUser(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, email))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/GetScimUserByEmail err: %+v", err)
		return nil, err
	}
	return u, nil
}

// ListScimUsers returns one page of identities plus the unpaged total.
//
// Ordered by created_at THEN id. The id tiebreak is not decoration: created_at has a default of NOW()
// and bulk provisioning writes many rows inside the same clock tick, so ordering by it alone is not a
// total order and Postgres may return those rows differently between two queries. An IdP paging through
// a non-deterministic order silently skips and repeats users.
func ListScimUsers(ctx context.Context, offset int, limit int) ([]*ScimUser, int, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()

	const countQ = `SELECT COUNT(*) FROM users WHERE true` + realPeopleOnly
	var total int
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, countQ).Scan(&total); err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListScimUsers count err: %+v", err)
		return nil, 0, err
	}

	const q = `SELECT ` + scimUserColumns + ` FROM users WHERE true` + realPeopleOnly + `
		ORDER BY created_at, id LIMIT $1 OFFSET $2`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, limit, offset)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/ListScimUsers err: %+v", err)
		return nil, 0, err
	}
	defer rows.Close()

	var out []*ScimUser
	for rows.Next() {
		u, scanErr := scanScimUser(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		out = append(out, u)
	}
	return out, total, rows.Err()
}

// UpdateScimUserNames sets the display name and username for an identity.
//
// Both are optional; a nil argument leaves that column alone rather than blanking it. An IdP that does
// not manage an attribute omits it, and treating "not sent" as "set to empty" would let a PUT wipe a
// name the user chose in OneCamp.
func UpdateScimUserNames(ctx context.Context, id uuid.UUID, username *string, displayName *string) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE users
		SET username = COALESCE($2, username),
		    display_name = COALESCE($3, display_name),
		    updated_at = NOW()
		WHERE id = $1`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, username, displayName); err != nil {
		helpers.LogErrorWithContext(ctx, "models/UpdateScimUserNames err: %+v", err)
		return err
	}
	return nil
}
