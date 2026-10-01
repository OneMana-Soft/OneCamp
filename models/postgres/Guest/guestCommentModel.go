package models

// Guest comments (migration 100): comments posted by an external guest holding
// a doc grant with capability = 'comment'. Stored in an ISOLATED table that is
// deliberately decoupled from the core `comments` table (which FKs to users).
// A guest has no users row, so these never touch member identity, the Dgraph
// comment_by node, OpenSearch, AI embeddings, or member notifications.
//
// Bodies are stored as PLAIN TEXT (HTML stripped at the business layer): guest
// comments render inside authenticated member sessions, so raw HTML would be a
// stored-XSS vector.

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// GuestComment mirrors a row of guest_comments.
type GuestComment struct {
	Id        uuid.UUID `json:"id"`
	GrantID   uuid.UUID `json:"grant_id"`
	DocUUID   string    `json:"doc_uuid"`
	GuestName string    `json:"guest_name"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// CreateGuestComment inserts a guest comment and returns its id + created time.
func CreateGuestComment(ctx context.Context, grantID uuid.UUID, docUUID, guestName, body string) (uuid.UUID, time.Time, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	now := time.Now()
	const q = `INSERT INTO guest_comments (id, grant_id, doc_uuid, guest_name, body, created_at)
		VALUES ($1,$2,$3,$4,$5,$6)`
	_, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, grantID, docUUID, guestName, body, now)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/CreateGuestComment err: %+v", err)
		return uuid.Nil, time.Time{}, err
	}
	return id, now, nil
}

// ListGuestCommentsByDoc returns all live guest comments for a doc, oldest
// first (chronological thread order, matching how the doc comment list reads).
func ListGuestCommentsByDoc(ctx context.Context, docUUID string) ([]*GuestComment, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	const q = `SELECT id, grant_id, doc_uuid, guest_name, body, created_at
		FROM guest_comments
		WHERE doc_uuid=$1 AND deleted_at IS NULL
		ORDER BY created_at ASC`
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(dbctx, q, docUUID)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/ListGuestCommentsByDoc err: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*GuestComment
	for rows.Next() {
		var c GuestComment
		if scanErr := rows.Scan(&c.Id, &c.GrantID, &c.DocUUID, &c.GuestName, &c.Body, &c.CreatedAt); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}
