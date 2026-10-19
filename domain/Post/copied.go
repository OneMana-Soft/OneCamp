package domain

import (
	"context"
	"time"

	"github.com/akashc777/OneCamp/helpers"
	"github.com/akashc777/OneCamp/initializers/postgresInit"
	"github.com/google/uuid"
)

// CreatePostAt is CreatePost for a message written at at, not now: a copy a
// shared history makes keeps its original's time, so it is never counted
// unread, and retention reads its age as the original's.
func CreatePostAt(ctx context.Context, postID uuid.UUID, createdBy uuid.UUID, channelID uuid.UUID, at time.Time) error {
	cctx, cancel := context.WithTimeout(ctx, postgresInit.DBConn.DBTimeout)
	defer cancel()
	_, err := postgresInit.DBConn.SqlDB.ExecContext(cctx, `
		INSERT INTO posts (id, post_channel, created_by, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $4)`, postID, channelID, createdBy, at)
	if err != nil {
		helpers.LogErrorWithContext(ctx, "domain/CreatePostAt Failed to create the post row err: %+v", err)
	}
	return err
}
