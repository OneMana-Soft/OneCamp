package models

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/akashc777/OneCamp/models/pgexec"
	"github.com/google/uuid"
)

type Comment struct {
	Id        uuid.UUID `gorm:"type:uuid;default:uuid_generate_v4()"`
	CreatedBy uuid.UUID `json:"user_name"`
	CreatedAt time.Time
	UpdatedAt time.Time
	DeletedAt time.Time
}

func CreateComment(query string, id uuid.UUID, createdBy uuid.UUID, currentTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		id,
		createdBy,
		currentTime,
	)
	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/CreateComment Failed to create new post err: %+v",
			err)
		return
	}

	return
}

func UpdateCommentByUUID(query string, commentUUID uuid.UUID, currentTime time.Time) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		currentTime,
		commentUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/UpdateCommentByUUID Failed to update post err: %+v",
			err)
		return
	}

	return
}

func HardDeleteCommentByUUID(query string, commentUUID uuid.UUID) (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err = postgresInit.DBConn.SqlDB.ExecContext(
		ctx,
		query,
		commentUUID,
	)

	if err != nil {
		helpers.LogErrorWithContext(ctx,
			"models/HardDeleteCommentByUUID Failed to hard delete post err: %+v",
			err)
		return
	}

	return
}

// Hide marks a comment deleted at at, and answers whether it is hidden at at
// now: it was live, or this hid it before (a retry). A comment deleted
// otherwise is left as it is. at is kept to the microsecond, as Postgres
// keeps it, so Show finds it.
func Hide(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE comments SET deleted_at = $2 WHERE id = $1 AND (deleted_at IS NULL OR deleted_at = $2)`,
		id, at.Truncate(time.Microsecond))
	return n == 1, err
}

// Show brings back a comment Hide hid at at, and answers whether it is live
// now: it was still hidden as Hide left it, or this brought it back before (a
// retry). One deleted otherwise since stays deleted.
func Show(ctx context.Context, id uuid.UUID, at time.Time) (bool, error) {
	n, err := pgexec.Exec(ctx, `UPDATE comments SET deleted_at = NULL WHERE id = $1 AND (deleted_at = $2 OR deleted_at IS NULL)`,
		id, at.Truncate(time.Microsecond))
	return n == 1, err
}
