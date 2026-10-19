package models

// Postgres access for stored comments (migrations/348_thread_comments.up.sql):
// the comments of threads on kinds that keep none of their own (a table's
// rows), and their reactions. business/Thread owns the rules; this file only
// reads and writes rows. Ids are made by the caller, and slices go to pgx as
// they are.

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// Comment is one thread_comments row.
type Comment struct {
	ID           uuid.UUID
	ThreadID     uuid.UUID
	ResourceKind string
	ResourceUUID string
	AuthorUUID   uuid.UUID
	// AuthorKind is "person", or "agent" for an agent acting for AuthorUUID.
	AuthorKind string
	AgentUUID  *uuid.UUID
	Body       string
	CreatedAt  time.Time
	EditedAt   *time.Time
	DeletedAt  *time.Time
	DeletedBy  *uuid.UUID
}

// Reaction is one thread_comment_reactions row.
type Reaction struct {
	ID        uuid.UUID
	CommentID uuid.UUID
	UserUUID  uuid.UUID
	EmojiID   string
	CreatedAt time.Time
}

const commentColumns = `id, thread_id, resource_kind, resource_uuid, author_uuid, author_kind, agent_uuid,
	body, created_at, edited_at, deleted_at, deleted_by`

func scanComment(s interface{ Scan(...any) error }) (*Comment, error) {
	var c Comment
	if err := s.Scan(&c.ID, &c.ThreadID, &c.ResourceKind, &c.ResourceUUID, &c.AuthorUUID, &c.AuthorKind, &c.AgentUUID,
		&c.Body, &c.CreatedAt, &c.EditedAt, &c.DeletedAt, &c.DeletedBy); err != nil {
		return nil, err
	}
	return &c, nil
}

// InsertComment writes a new comment.
func InsertComment(ctx context.Context, c *Comment) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, `INSERT INTO thread_comments (`+commentColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, NULL, NULL, NULL)`,
		c.ID, c.ThreadID, c.ResourceKind, c.ResourceUUID, c.AuthorUUID, c.AuthorKind, c.AgentUUID, c.Body, c.CreatedAt)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread InsertComment failed: %+v", err)
	}
	return err
}

// CommentsOf is a resource's live comments, oldest first.
func CommentsOf(ctx context.Context, kind, resourceUUID string) ([]*Comment, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, `SELECT `+commentColumns+` FROM thread_comments
		WHERE resource_kind = $1 AND resource_uuid = $2 AND deleted_at IS NULL
		ORDER BY created_at, id`, kind, resourceUUID)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread CommentsOf failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	var out []*Comment
	for rows.Next() {
		c, err := scanComment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetComment is one comment, removed or not, or nil when there is none.
func GetComment(ctx context.Context, id uuid.UUID) (*Comment, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	c, err := scanComment(postgresInit.DBConn.SqlDB.QueryRowContext(cctx,
		`SELECT `+commentColumns+` FROM thread_comments WHERE id = $1`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread GetComment failed: %+v", err)
		return nil, err
	}
	return c, nil
}

// EditComment gives a live comment a new body; false when it is gone.
func EditComment(ctx context.Context, id uuid.UUID, body string, at time.Time) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE thread_comments SET body = $2, edited_at = $3 WHERE id = $1 AND deleted_at IS NULL`, id, body, at)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread EditComment failed: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RemoveComment takes a live comment from every reader; false when it is gone
// already. The row is kept: nothing is purged until the owner decides how long
// anything removed is kept.
func RemoveComment(ctx context.Context, id, by uuid.UUID, at time.Time) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`UPDATE thread_comments SET deleted_at = $3, deleted_by = $2 WHERE id = $1 AND deleted_at IS NULL`, id, by, at)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread RemoveComment failed: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// ReactionsOf is the reactions on the comments, by comment, oldest first.
func ReactionsOf(ctx context.Context, commentIDs []uuid.UUID) (map[uuid.UUID][]Reaction, error) {
	out := map[uuid.UUID][]Reaction{}
	if len(commentIDs) == 0 {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, `SELECT id, comment_id, user_uuid, emoji_id, created_at
		FROM thread_comment_reactions WHERE comment_id = ANY($1) ORDER BY created_at, id`, commentIDs)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread ReactionsOf failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var r Reaction
		if err := rows.Scan(&r.ID, &r.CommentID, &r.UserUUID, &r.EmojiID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out[r.CommentID] = append(out[r.CommentID], r)
	}
	return out, rows.Err()
}

// SetReaction is a person's reaction on a live comment: added, or its emoji
// changed when they have one there already (one a person). It answers the
// reaction's id; ok is false when the comment is gone.
func SetReaction(ctx context.Context, id, commentID, user uuid.UUID, emojiID string, at time.Time) (got uuid.UUID, ok bool, err error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	err = postgresInit.DBConn.SqlDB.QueryRowContext(cctx, `INSERT INTO thread_comment_reactions (id, comment_id, user_uuid, emoji_id, created_at)
		SELECT $1, c.id, $3, $4, $5 FROM thread_comments c WHERE c.id = $2 AND c.deleted_at IS NULL
		ON CONFLICT (comment_id, user_uuid) DO UPDATE SET emoji_id = EXCLUDED.emoji_id, created_at = EXCLUDED.created_at
		RETURNING id`, id, commentID, user, emojiID, at).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread SetReaction failed: %+v", err)
		return uuid.Nil, false, err
	}
	return got, true, nil
}

// RemoveReaction takes back a person's own reaction; false when they have none
// by that id on that comment.
func RemoveReaction(ctx context.Context, commentID, reactionID, user uuid.UUID) (bool, error) {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	res, err := postgresInit.DBConn.SqlDB.ExecContext(cctx,
		`DELETE FROM thread_comment_reactions WHERE id = $1 AND comment_id = $2 AND user_uuid = $3`, reactionID, commentID, user)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread RemoveReaction failed: %+v", err)
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// OpenCommentCounts is, for each of the resources of one kind, how many live
// comments its open threads hold; a resource with none is left out. Counted
// for many resources in one read (a page of a table's rows).
func OpenCommentCounts(ctx context.Context, kind string, resourceUUIDs []string) (map[string]int, error) {
	out := map[string]int{}
	if len(resourceUUIDs) == 0 {
		return out, nil
	}
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	rows, err := postgresInit.DBConn.SqlDB.QueryContext(cctx, `SELECT c.resource_uuid, count(*)
		FROM thread_comments c JOIN threads t ON t.id = c.thread_id AND t.status = 'open'
		WHERE c.resource_kind = $1 AND c.resource_uuid = ANY($2) AND c.deleted_at IS NULL
		GROUP BY c.resource_uuid`, kind, resourceUUIDs)
	if err != nil {
		helpers.LogErrorWithContext(cctx, "models/Thread OpenCommentCounts failed: %+v", err)
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}
