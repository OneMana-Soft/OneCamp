// Package models (Guest) is the Postgres data-access layer for guest access
// grants (migration 97). Only the SHA-256 hash of a link token is stored;
// lookups are by hash. The business layer generates tokens, hashes them, and
// enforces the workspace guest-access policy.
//
// A guest grant authorizes an external person (no OneCamp account) to reach
// exactly one resource for a bounded time. Guests are never written to the
// users table, so they cannot leak into rosters, search, memory, or mentions.
package models

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// Resource types and capabilities.
const (
	ResourceMeeting = "meeting"
	ResourceDoc     = "doc"
	ResourceBoard   = "board"
	ResourceTable   = "table"
	ResourceChannel = "channel"
	// ResourceProject is a client's view of one project's tasks.
	ResourceProject = "project"

	CapabilityJoin    = "join"
	CapabilityView    = "view"
	CapabilityComment = "comment"
	// CapabilityPost lets a channel guest write as well as read.
	CapabilityPost = "post"
)

// GuestGrant mirrors a row of guest_grants. The raw token is never stored.
type GuestGrant struct {
	Id           uuid.UUID `json:"id"`
	ResourceType string    `json:"resource_type"`
	ResourceID   string    `json:"resource_id"`
	Capability   string    `json:"capability"`
	CreatedBy    uuid.UUID `json:"created_by"`
	// ExpiresAt nil means the grant lasts until somebody revokes it. A sentinel
	// far-future date was the alternative and would read as a real expiry in the
	// admin's grant list.
	ExpiresAt *time.Time `json:"expires_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`

	// A doc's view grant published as a web page (docs C6, migration 350):
	// its address, whether search engines may find it, whether members may
	// copy it, its password (bcrypt) and the version every unlock is for.
	// Slug nil: not published.
	Slug          *string    `json:"slug,omitempty"`
	Indexable     bool       `json:"indexable"`
	AllowCopies   bool       `json:"allow_copies"`
	PasswordHash  *string    `json:"-"`
	UnlockVersion int        `json:"-"`
	PublishedAt   *time.Time `json:"published_at,omitempty"`
	PublishedBy   *uuid.UUID `json:"published_by,omitempty"`
}

// ErrSlugTaken is an address another live grant has.
var ErrSlugTaken = errors.New("guest: that address is taken")

// LinkCapDays is the workspace's longest link lifetime in days (the policy
// guest.link.max_days, 0 for none). business/Guest sets it; the model only
// reads it, so every query that asks "is this grant live" asks the same thing.
var LinkCapDays = func(context.Context) int { return 0 }

// StandingSQL is the one SQL definition of a standing link, which the cap
// leaves alone: a meeting link with an address, a personal room (calls
// CALLS-2). IsStanding is its Go side; the two are held together by tests.
const StandingSQL = `(resource_type = 'meeting' AND slug IS NOT NULL)`

// IsStanding reports whether a grant is a standing link. Pure.
func IsStanding(g *GuestGrant) bool {
	return g != nil && g.ResourceType == ResourceMeeting && g.Slug != nil
}

// liveSQL is the one SQL predicate for a live grant, the cap as parameter n:
// not revoked, not past its own expiry, and, unless standing or there is no
// cap, made less than the cap's days ago. business/Guest grantExpired is its
// Go side.
func liveSQL(n int) string {
	p := fmt.Sprintf("$%d::int", n)
	return `revoked_at IS NULL AND (expires_at IS NULL OR expires_at > NOW()) AND (` + p + ` = 0 OR ` + StandingSQL +
		` OR created_at > NOW() - make_interval(days => ` + p + `))`
}

type scanner interface {
	Scan(dest ...any) error
}

const grantColumns = `id, resource_type, resource_id, capability, created_by, expires_at, revoked_at, created_at,
	slug, indexable, allow_copies, password_hash, unlock_version, published_at, published_by`

// grantRow is scanGrant for pgexec's reads.
func grantRow(r pgexec.Row) (*GuestGrant, error) { return scanGrant(r) }

func scanGrant(s scanner) (*GuestGrant, error) {
	var g GuestGrant
	var revokedAt, expiresAt, publishedAt sql.NullTime
	var slug, password sql.NullString
	var publishedBy uuid.NullUUID
	if err := s.Scan(&g.Id, &g.ResourceType, &g.ResourceID, &g.Capability,
		&g.CreatedBy, &expiresAt, &revokedAt, &g.CreatedAt,
		&slug, &g.Indexable, &g.AllowCopies, &password, &g.UnlockVersion, &publishedAt, &publishedBy); err != nil {
		return nil, err
	}
	if revokedAt.Valid {
		g.RevokedAt = &revokedAt.Time
	}
	if expiresAt.Valid {
		g.ExpiresAt = &expiresAt.Time
	}
	if slug.Valid {
		g.Slug = &slug.String
	}
	if password.Valid {
		g.PasswordHash = &password.String
	}
	if publishedAt.Valid {
		g.PublishedAt = &publishedAt.Time
	}
	if publishedBy.Valid {
		g.PublishedBy = &publishedBy.UUID
	}
	return &g, nil
}

// CreateGrant inserts a new grant (token hash + scope) and returns its id.
// expiresAt nil creates a grant that lasts until it is revoked.
func CreateGrant(ctx context.Context, tokenHash []byte, resourceType, resourceID, capability string, createdBy uuid.UUID, expiresAt *time.Time) (uuid.UUID, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	const q = `INSERT INTO guest_grants (id, token_hash, resource_type, resource_id, capability, created_by, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, tokenHash, resourceType, resourceID, capability, createdBy, expiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/CreateGrant err: %+v", err)
		return uuid.Nil, err
	}
	return id, nil
}

// GetActiveByHash returns the active (non-revoked, non-expired) grant for a
// token hash, or (nil, nil) if none. Used on every guest request.
func GetActiveByHash(ctx context.Context, tokenHash []byte) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + grantColumns + ` FROM guest_grants
		WHERE token_hash=$1 AND ` + liveSQL(2)
	g, err := scanGrant(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, tokenHash, LinkCapDays(ctx)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/GetActiveByHash err: %+v", err)
		return nil, err
	}
	return g, nil
}

// GetByID returns a grant by id regardless of state (admin lookup).
func GetByID(ctx context.Context, id uuid.UUID) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + grantColumns + ` FROM guest_grants WHERE id=$1`
	g, err := scanGrant(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/GetByID err: %+v", err)
		return nil, err
	}
	return g, nil
}

// Revoke marks a grant revoked (idempotent). Admin-scoped: any admin may
// revoke any grant, so there is no created_by constraint here (the controller
// enforces the admin gate).
func Revoke(ctx context.Context, id uuid.UUID) error {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE guest_grants SET revoked_at=NOW() WHERE id=$1 AND revoked_at IS NULL`
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/Revoke err: %+v", err)
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// RevokeLiveForResource turns off every live grant to one resource (revoked
// as Revoke does; expired ones are left as they are) and answers how many.
func RevokeLiveForResource(ctx context.Context, resourceType, resourceID string) (int64, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `UPDATE guest_grants SET revoked_at=NOW()
		WHERE resource_type = $1 AND resource_id = $2 AND ` + liveSQL(3)
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, resourceType, resourceID, LinkCapDays(ctx))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/RevokeLiveForResource err: %+v", err)
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ResourceExists reports whether ANY guest grant (active or not) exists for a
// resource id. Used by the LiveKit webhook router to decide whether a `meet-`
// room belongs to this instance (mirrors the DM "any participant in our DB"
// ownership check for a shared-LiveKit deployment).
func ResourceExists(ctx context.Context, resourceID string) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT EXISTS (SELECT 1 FROM guest_grants WHERE resource_id=$1)`
	var exists bool
	if err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, resourceID).Scan(&exists); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/ResourceExists err: %+v", err)
		return false, err
	}
	return exists, nil
}

// ResourceCreator is who made the first grant to a resource, live or not: for
// an instant meeting, its host. ok is false when there is none.
func ResourceCreator(ctx context.Context, resourceType, resourceID string) (uuid.UUID, bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT created_by FROM guest_grants
		WHERE resource_type = $1 AND resource_id = $2 ORDER BY created_at ASC LIMIT 1`
	var by uuid.UUID
	err := postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, resourceType, resourceID).Scan(&by)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/ResourceCreator err: %+v", err)
		return uuid.Nil, false, err
	}
	return by, true, nil
}

// Used by the admin guest-grants view.
func ListActive(ctx context.Context) ([]*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + grantColumns + ` FROM guest_grants
		WHERE ` + liveSQL(1) + `
		ORDER BY created_at DESC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, LinkCapDays(ctx))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/ListActive err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*GuestGrant
	for rows.Next() {
		g, scanErr := scanGrant(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// WithActiveGrants is which of ids, resources of one type, have a live grant:
// not revoked, not expired. One query for a page of a list (the active-grants
// index covers it); none for no ids.
func WithActiveGrants(ctx context.Context, resourceType string, ids []string) (map[string]bool, error) {
	live := map[string]bool{}
	if len(ids) == 0 {
		return live, nil
	}
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT DISTINCT resource_id FROM guest_grants
		WHERE resource_type = $1 AND resource_id = ANY($2) AND ` + liveSQL(3)
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, resourceType, pq.Array(ids), LinkCapDays(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		live[id] = true
	}
	return live, rows.Err()
}

// ListActiveForResource is the live grants to one resource, newest first.
func ListActiveForResource(ctx context.Context, resourceType, resourceID string) ([]*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	q := `SELECT ` + grantColumns + ` FROM guest_grants
		WHERE resource_type = $1 AND resource_id = $2 AND ` + liveSQL(3) + `
		ORDER BY created_at DESC LIMIT 100`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, resourceType, resourceID, LinkCapDays(ctx))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*GuestGrant{}
	for rows.Next() {
		g, err := scanGrant(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GetLiveBySlug is the grant a published address names: nobody revoked it.
// An expired one is answered too, so its page can say it expired; nil when
// no live grant has the address.
func GetLiveBySlug(ctx context.Context, slug string) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT ` + grantColumns + ` FROM guest_grants WHERE slug = $1 AND revoked_at IS NULL`
	g, err := scanGrant(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, slug))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/GetLiveBySlug err: %+v", err)
		return nil, err
	}
	return g, nil
}

// Publication is what publishing sets on a grant.
type Publication struct {
	Slug        string
	Indexable   bool
	AllowCopies bool
	// PasswordHash is the password to keep; nil for none.
	PasswordHash *string
	// NewUnlock ends every unlock given before (a new password or address).
	NewUnlock   bool
	PublishedBy uuid.UUID
}

// Publish publishes a live grant at an address, and takes the address off
// any other live grant to the same resource: a doc has one published page.
// ErrSlugTaken when another resource's live grant has the address.
func Publish(ctx context.Context, id uuid.UUID, p Publication) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	tx, err := postgresInit.DBConn.SqlDB.BeginTx(dbctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	const others = `UPDATE guest_grants SET slug = NULL, indexable = false, allow_copies = false, password_hash = NULL,
		unlock_version = unlock_version + 1, published_at = NULL, published_by = NULL
		WHERE id <> $1 AND slug IS NOT NULL AND revoked_at IS NULL
		  AND (resource_type, resource_id) = (SELECT resource_type, resource_id FROM guest_grants WHERE id = $1)`
	if _, err := tx.ExecContext(dbctx, others, id); err != nil {
		return nil, err
	}
	bump := 0
	if p.NewUnlock {
		bump = 1
	}
	const q = `UPDATE guest_grants SET slug = $2, indexable = $3, allow_copies = $4, password_hash = $5,
		unlock_version = unlock_version + $6, published_at = COALESCE(published_at, NOW()), published_by = COALESCE(published_by, $7)
		WHERE id = $1 AND revoked_at IS NULL RETURNING ` + grantColumns
	g, err := scanGrant(tx.QueryRowContext(dbctx, q, id, p.Slug, p.Indexable, p.AllowCopies, p.PasswordHash, bump, p.PublishedBy))
	var pqErr *pq.Error
	switch {
	case errors.As(err, &pqErr) && pqErr.Code == "23505":
		return nil, ErrSlugTaken
	case errors.Is(err, sql.ErrNoRows):
		return nil, nil
	case err != nil:
		helpers.LogErrorWithContext(ctx, "models/Guest/Publish err: %+v", err)
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return g, nil
}

// Unpublish takes a grant's address away, and its page with it; the link's
// token keeps working until it is revoked. Nil when the grant isn't live.
func Unpublish(ctx context.Context, id uuid.UUID) (*GuestGrant, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `UPDATE guest_grants SET slug = NULL, indexable = false, allow_copies = false, password_hash = NULL,
		unlock_version = unlock_version + 1, published_at = NULL, published_by = NULL
		WHERE id = $1 AND revoked_at IS NULL RETURNING ` + grantColumns
	g, err := scanGrant(postgresInit.DBConn.SqlDB.QueryRowContext(dbctx, q, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/Unpublish err: %+v", err)
		return nil, err
	}
	return g, nil
}
