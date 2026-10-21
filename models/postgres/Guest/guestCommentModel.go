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

// Who wrote a guest_comments row (docs C5-2): the guest, or a member replying
// where the guest sees it.
const (
	AuthorGuest  = "guest"
	AuthorMember = "member"
)

// GuestComment mirrors a row of guest_comments.
type GuestComment struct {
	Id        uuid.UUID `json:"id"`
	GrantID   uuid.UUID `json:"grant_id"`
	DocUUID   string    `json:"doc_uuid"`
	GuestName string    `json:"guest_name"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	// ParentID is the guest comment a reply answers; nil for a guest's own.
	ParentID *uuid.UUID `json:"parent_id,omitempty"`
	// AuthorKind is AuthorGuest or AuthorMember; AuthorUserUUID the member
	// who replied, for the workspace only.
	AuthorKind     string     `json:"author_kind"`
	AuthorUserUUID *uuid.UUID `json:"-"`
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
	const q = `SELECT id, grant_id, doc_uuid, guest_name, body, created_at, parent_id, author_kind, author_user_uuid
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
		var parent, author uuid.NullUUID
		if scanErr := rows.Scan(&c.Id, &c.GrantID, &c.DocUUID, &c.GuestName, &c.Body, &c.CreatedAt, &parent, &c.AuthorKind, &author); scanErr != nil {
			return nil, scanErr
		}
		if parent.Valid {
			c.ParentID = &parent.UUID
		}
		if author.Valid {
			c.AuthorUserUUID = &author.UUID
		}
		out = append(out, &c)
	}
	return out, rows.Err()
}

// GetGuestComment is one live guest_comments row of a doc, or nil.
func GetGuestComment(ctx context.Context, docUUID string, id uuid.UUID) (*GuestComment, error) {
	all, err := ListGuestCommentsByDoc(ctx, docUUID)
	if err != nil {
		return nil, err
	}
	for _, c := range all {
		if c.Id == id {
			return c, nil
		}
	}
	return nil, nil
}

// CreateMemberReply inserts a member's reply to a guest's comment, on the
// parent's grant, and returns its id and time. name is how the guest sees the
// member (their name and workspace), never an address.
func CreateMemberReply(ctx context.Context, parent *GuestComment, authorUUID uuid.UUID, name, body string) (uuid.UUID, time.Time, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	id := uuid.New()
	now := time.Now()
	const q = `INSERT INTO guest_comments (id, grant_id, doc_uuid, guest_name, body, created_at, parent_id, author_kind, author_user_uuid)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'member',$8)`
	if _, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, q, id, parent.GrantID, parent.DocUUID, name, body, now, parent.Id, authorUUID); err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/CreateMemberReply err: %+v", err)
		return uuid.Nil, time.Time{}, err
	}
	return id, now, nil
}

// RemoveGuestComment soft-deletes a doc's guest comment and its replies; false
// when there was none.
func RemoveGuestComment(ctx context.Context, docUUID string, id uuid.UUID) (bool, error) {
	dbctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(dbctx, `UPDATE guest_comments SET deleted_at = now()
		WHERE doc_uuid = $1 AND deleted_at IS NULL AND (id = $2 OR parent_id = $2)`, docUUID, id)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "models/Guest/RemoveGuestComment err: %+v", err)
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}
