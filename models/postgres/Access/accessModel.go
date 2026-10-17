// Package models (Access) stores what the Access Resolver can't keep with a
// resource (the access_requests_and_expiry migration): requests for access,
// grants that end at a moment, and admin recoveries. Who may do what lives with the resource itself (a
// doc's grants are edges in the graph).
package models

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
	"github.com/lib/pq"
)

// ErrNotFound is a request that isn't there, or one no longer open.
var ErrNotFound = errors.New("access: no such request")

// Request statuses.
const (
	StatusPending  = "pending"
	StatusApproved = "approved"
	StatusDeclined = "declined"
)

// Request is someone asking for access to a resource.
type Request struct {
	ID            uuid.UUID  `json:"id"`
	ResourceKind  string     `json:"resource_kind"`
	ResourceUUID  uuid.UUID  `json:"resource_uuid"`
	RequesterUUID uuid.UUID  `json:"requester_uuid"`
	LevelWanted   string     `json:"level_wanted"`
	Note          string     `json:"note"`
	Status        string     `json:"status"`
	LevelGranted  *string    `json:"level_granted,omitempty"`
	DecidedBy     *uuid.UUID `json:"decided_by,omitempty"`
	DecidedAt     *time.Time `json:"decided_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const requestColumns = `id, resource_kind, resource_uuid, requester_uuid, level_wanted, note, status, level_granted, decided_by, decided_at, created_at, updated_at`

func scanRequest(row interface{ Scan(...any) error }) (*Request, error) {
	var r Request
	var granted sql.NullString
	var decidedBy uuid.NullUUID
	var decidedAt sql.NullTime
	if err := row.Scan(&r.ID, &r.ResourceKind, &r.ResourceUUID, &r.RequesterUUID, &r.LevelWanted, &r.Note, &r.Status,
		&granted, &decidedBy, &decidedAt, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if granted.Valid {
		r.LevelGranted = &granted.String
	}
	if decidedBy.Valid {
		r.DecidedBy = &decidedBy.UUID
	}
	if decidedAt.Valid {
		r.DecidedAt = &decidedAt.Time
	}
	return &r, nil
}

func timeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
}

// AskFor records a request, or updates the person's open one on the same
// resource (the level they want and their note): one open request each.
func AskFor(ctx context.Context, kind string, resource, requester uuid.UUID, levelWanted, note string, at time.Time) (*Request, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	r, err := scanRequest(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		INSERT INTO access_requests (id, resource_kind, resource_uuid, requester_uuid, level_wanted, note, status, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, 'pending', $7, $7)
		ON CONFLICT (resource_kind, resource_uuid, requester_uuid) WHERE status = 'pending'
		DO UPDATE SET level_wanted = EXCLUDED.level_wanted, note = EXCLUDED.note, updated_at = EXCLUDED.updated_at
		RETURNING `+requestColumns,
		uuid.New(), kind, resource, requester, levelWanted, note, at))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access AskFor err: %+v", err)
	}
	return r, err
}

// GetRequest is one request.
func GetRequest(ctx context.Context, id uuid.UUID) (*Request, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	r, err := scanRequest(postgresInit.DBConn.SqlDB.QueryRowContext(c, `SELECT `+requestColumns+` FROM access_requests WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access GetRequest err: %+v", err)
	}
	return r, err
}

// RequestQuery picks requests: on one resource, or by one person, open ones
// only or all; newest first, at most Limit (1 to 200).
type RequestQuery struct {
	ResourceKind  string
	ResourceUUID  *uuid.UUID
	RequesterUUID *uuid.UUID
	OpenOnly      bool
	Limit         int
}

// ListRequests reads requests by a query.
func ListRequests(ctx context.Context, q RequestQuery) ([]*Request, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	sqlText := `SELECT ` + requestColumns + ` FROM access_requests WHERE TRUE`
	var args []any
	arg := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if q.ResourceKind != "" {
		sqlText += ` AND resource_kind = ` + arg(q.ResourceKind)
	}
	if q.ResourceUUID != nil {
		sqlText += ` AND resource_uuid = ` + arg(*q.ResourceUUID)
	}
	if q.RequesterUUID != nil {
		sqlText += ` AND requester_uuid = ` + arg(*q.RequesterUUID)
	}
	if q.OpenOnly {
		sqlText += ` AND status = 'pending'`
	}
	limit := q.Limit
	if limit < 1 || limit > 200 {
		limit = 200
	}
	sqlText += ` ORDER BY created_at DESC LIMIT ` + arg(limit)
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, sqlText, args...)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access ListRequests err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Request
	for rows.Next() {
		r, err := scanRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Decide answers an open request: approved (with the level granted) or
// declined. ErrNotFound when it isn't open any more, so two people answering
// at once answer it once.
func Decide(ctx context.Context, id uuid.UUID, status string, levelGranted *string, by uuid.UUID, at time.Time) (*Request, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	r, err := scanRequest(postgresInit.DBConn.SqlDB.QueryRowContext(c, `
		UPDATE access_requests SET status = $2, level_granted = $3, decided_by = $4, decided_at = $5, updated_at = $5
		WHERE id = $1 AND status = 'pending'
		RETURNING `+requestColumns, id, status, levelGranted, by, at))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access Decide err: %+v", err)
	}
	return r, err
}

// Expiry is a grant that ends.
type Expiry struct {
	ResourceKind  string     `json:"resource_kind"`
	ResourceUUID  uuid.UUID  `json:"resource_uuid"`
	PrincipalKind string     `json:"principal_kind"`
	PrincipalUUID uuid.UUID  `json:"principal_uuid"`
	Level         string     `json:"level"`
	ExpiresAt     time.Time  `json:"expires_at"`
	CreatedBy     *uuid.UUID `json:"created_by,omitempty"`
}

const expiryColumns = `resource_kind, resource_uuid, principal_kind, principal_uuid, level, expires_at, created_by`

func scanExpiry(row interface{ Scan(...any) error }) (*Expiry, error) {
	var e Expiry
	var by uuid.NullUUID
	if err := row.Scan(&e.ResourceKind, &e.ResourceUUID, &e.PrincipalKind, &e.PrincipalUUID, &e.Level, &e.ExpiresAt, &by); err != nil {
		return nil, err
	}
	if by.Valid {
		e.CreatedBy = &by.UUID
	}
	return &e, nil
}

// SetExpiry records that a grant ends at a moment (replacing when it ended).
func SetExpiry(ctx context.Context, e Expiry) error {
	c, cancel := timeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO access_grant_expiry (`+expiryColumns+`) VALUES ($1, $2, $3, $4, $5, $6, $7)
		ON CONFLICT (resource_kind, resource_uuid, principal_kind, principal_uuid)
		DO UPDATE SET level = EXCLUDED.level, expires_at = EXCLUDED.expires_at, created_by = EXCLUDED.created_by`,
		e.ResourceKind, e.ResourceUUID, e.PrincipalKind, e.PrincipalUUID, e.Level, e.ExpiresAt, e.CreatedBy)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access SetExpiry err: %+v", err)
	}
	return err
}

// ClearExpiry forgets that a grant ends: it was taken away, or made for good.
func ClearExpiry(ctx context.Context, kind string, resources []uuid.UUID, principalKind string, principal uuid.UUID) error {
	if len(resources) == 0 {
		return nil
	}
	c, cancel := timeout(ctx)
	defer cancel()
	ids := make([]string, len(resources))
	for i, r := range resources {
		ids[i] = r.String()
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		DELETE FROM access_grant_expiry
		WHERE resource_kind = $1 AND resource_uuid = ANY($2::uuid[]) AND principal_kind = $3 AND principal_uuid = $4`,
		kind, pq.Array(ids), principalKind, principal)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access ClearExpiry err: %+v", err)
	}
	return err
}

// ClearAllExpiry forgets the end of every grant on the resources: their
// sharing was replaced whole.
func ClearAllExpiry(ctx context.Context, kind string, resources []uuid.UUID) error {
	if len(resources) == 0 {
		return nil
	}
	c, cancel := timeout(ctx)
	defer cancel()
	ids := make([]string, len(resources))
	for i, r := range resources {
		ids[i] = r.String()
	}
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		DELETE FROM access_grant_expiry WHERE resource_kind = $1 AND resource_uuid = ANY($2::uuid[])`, kind, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access ClearAllExpiry err: %+v", err)
	}
	return err
}

// ListExpiry is the grants on a resource that end, soonest first.
func ListExpiry(ctx context.Context, kind string, resource uuid.UUID) ([]*Expiry, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `SELECT `+expiryColumns+` FROM access_grant_expiry
		WHERE resource_kind = $1 AND resource_uuid = $2 ORDER BY expires_at`, kind, resource)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access ListExpiry err: %+v", err)
		return nil, err
	}
	return scanExpiries(rows)
}

// DueExpiry is up to limit grants that have ended by now, the oldest first.
func DueExpiry(ctx context.Context, now time.Time, limit int) ([]*Expiry, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `SELECT `+expiryColumns+` FROM access_grant_expiry
		WHERE expires_at <= $1 ORDER BY expires_at LIMIT $2`, now, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access DueExpiry err: %+v", err)
		return nil, err
	}
	return scanExpiries(rows)
}

// Ended forgets an ended grant once it is taken away: only while it still
// ends at that moment, so a grant extended meanwhile is kept.
func Ended(ctx context.Context, e Expiry) error {
	c, cancel := timeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		DELETE FROM access_grant_expiry
		WHERE resource_kind = $1 AND resource_uuid = $2 AND principal_kind = $3 AND principal_uuid = $4 AND expires_at = $5`,
		e.ResourceKind, e.ResourceUUID, e.PrincipalKind, e.PrincipalUUID, e.ExpiresAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access Ended err: %+v", err)
	}
	return err
}

func scanExpiries(rows *sql.Rows) ([]*Expiry, error) {
	defer rows.Close()
	var out []*Expiry
	for rows.Next() {
		e, err := scanExpiry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Recovery is an admin taking over a resource.
type Recovery struct {
	ID            uuid.UUID  `json:"id"`
	ResourceKind  string     `json:"resource_kind"`
	ResourceUUID  uuid.UUID  `json:"resource_uuid"`
	AdminUUID     uuid.UUID  `json:"admin_uuid"`
	FromOwnerUUID *uuid.UUID `json:"from_owner_uuid,omitempty"`
	ToOwnerUUID   uuid.UUID  `json:"to_owner_uuid"`
	Reason        string     `json:"reason"`
	CreatedAt     time.Time  `json:"created_at"`
}

// RecordRecovery records an admin's recovery of a resource.
func RecordRecovery(ctx context.Context, r Recovery) error {
	c, cancel := timeout(ctx)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(c, `
		INSERT INTO access_recoveries (id, resource_kind, resource_uuid, admin_uuid, from_owner_uuid, to_owner_uuid, reason, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		r.ID, r.ResourceKind, r.ResourceUUID, r.AdminUUID, r.FromOwnerUUID, r.ToOwnerUUID, r.Reason, r.CreatedAt)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access RecordRecovery err: %+v", err)
	}
	return err
}

// ListRecoveries is a resource's recoveries, newest first.
func ListRecoveries(ctx context.Context, kind string, resource uuid.UUID, limit int) ([]*Recovery, error) {
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `
		SELECT id, resource_kind, resource_uuid, admin_uuid, from_owner_uuid, to_owner_uuid, reason, created_at
		FROM access_recoveries WHERE resource_kind = $1 AND resource_uuid = $2 ORDER BY created_at DESC LIMIT $3`,
		kind, resource, limit)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access ListRecoveries err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Recovery
	for rows.Next() {
		var r Recovery
		var from uuid.NullUUID
		if err := rows.Scan(&r.ID, &r.ResourceKind, &r.ResourceUUID, &r.AdminUUID, &from, &r.ToOwnerUUID, &r.Reason, &r.CreatedAt); err != nil {
			return nil, err
		}
		if from.Valid {
			r.FromOwnerUUID = &from.UUID
		}
		out = append(out, &r)
	}
	return out, rows.Err()
}

// EndedOn is which of the resources hold a grant whose end has passed by now
// and that the sweep hasn't taken away yet (usually none: it runs every
// minute), by resource uuid.
func EndedOn(ctx context.Context, kind string, resources []uuid.UUID, now time.Time) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(resources) == 0 {
		return out, nil
	}
	ids := make([]string, len(resources))
	for i, r := range resources {
		ids[i] = r.String()
	}
	c, cancel := timeout(ctx)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(c, `SELECT DISTINCT resource_uuid FROM access_grant_expiry
		WHERE expires_at <= $1 AND resource_kind = $2 AND resource_uuid = ANY($3::uuid[])`, now, kind, pq.Array(ids))
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Access EndedOn err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
